package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strings"
)

// HostNetwork is everything one host needs for a project environment's
// private network. It holds no secret: the private key stays on the host and
// is added to the rendered config there.
type HostNetwork struct {
	Interface   string        `json:"interface"`
	RuntimeID   string        `json:"runtimeId"`
	Environment string        `json:"environment"`
	CIDR        string        `json:"cidr"`
	Address     string        `json:"address"`
	ListenPort  int           `json:"listenPort"`
	Peers       []NetworkLink `json:"peers"`
	Access      []AccessRule  `json:"access"`
}

// NetworkLink is one direct WireGuard peer of the host.
type NetworkLink struct {
	Host      string `json:"host"`
	Address   string `json:"address"`
	Endpoint  string `json:"endpoint,omitempty"`
	Keepalive int    `json:"keepalive,omitempty"`
}

// AccessRule admits one consumer host to one published provider port on this
// host. Everything else aimed at an owned port is dropped.
type AccessRule struct {
	Source string `json:"source"`
	Port   int    `json:"port"`
	For    string `json:"for"`
}

// interfaceName fits the 15-character limit. The name alone never proves
// ownership; the owner manifest on the host does.
func interfaceName(runtimeID string) string {
	digest := sha256.Sum256([]byte(runtimeID))
	return "dw" + hex.EncodeToString(digest[:])[:12]
}

// chainPrefix names the owned iptables chains, within the 28-character limit.
func chainPrefix(networkInterface string) string {
	return "DEPLOY-" + strings.ToUpper(strings.TrimPrefix(networkInterface, "dw"))
}

// desiredNetworks works out each host's network from the resolved config and
// observed host identities. A host with no cross-host connection gets nothing,
// so a single-host deployment never needs WireGuard.
func desiredNetworks(resolved ResolvedProject) (map[string]HostNetwork, error) {
	plan, err := planTopology(resolved, "unverified")
	if err != nil {
		return nil, err
	}
	if plan.Network.Mode != "wireguard-required" {
		return map[string]HostNetwork{}, nil
	}

	hosts := map[string]HostNetwork{}
	peerOf := map[string]NetworkPeer{}
	for _, link := range plan.Network.Links {
		for _, destination := range link {
			host := resolved.destinationHost(destination)
			peer := resolved.Network.Peers[destination]
			peerOf[host] = peer
			if _, exists := hosts[host]; !exists {
				hosts[host] = HostNetwork{
					Interface: interfaceName(resolved.RuntimeID()), RuntimeID: resolved.RuntimeID(), Environment: resolved.Environment,
					CIDR: resolved.Network.CIDR, Address: peer.Address, ListenPort: peer.ListenPort,
					Peers: []NetworkLink{}, Access: []AccessRule{},
				}
			}
		}
	}

	for _, link := range plan.Network.Links {
		one, two := resolved.destinationHost(link[0]), resolved.destinationHost(link[1])
		for _, pair := range [][2]string{{one, two}, {two, one}} {
			local, remote := hosts[pair[0]], peerOf[pair[1]]
			if slices.ContainsFunc(local.Peers, func(existing NetworkLink) bool { return existing.Host == pair[1] }) {
				continue
			}
			local.Peers = append(local.Peers, NetworkLink{Host: pair[1], Address: remote.Address, Endpoint: remote.Endpoint, Keepalive: peerOf[pair[0]].Keepalive})
			hosts[pair[0]] = local
		}
	}

	for _, connection := range plan.Connections {
		if connection.Transport != "wireguard" {
			continue
		}
		consumer := resolved.destinationHost(resolved.nodeDestination(connection.Consumer))
		provider := resolved.destinationHost(resolved.Services[connection.Provider].Destination)
		local := hosts[provider]
		rule := AccessRule{Source: peerOf[consumer].Address, Port: connection.Port, For: connection.Consumer + "/" + connection.Name}
		if !slices.ContainsFunc(local.Access, func(existing AccessRule) bool {
			return existing.Source == rule.Source && existing.Port == rule.Port
		}) {
			local.Access = append(local.Access, rule)
		}
		hosts[provider] = local
	}

	for host, network := range hosts {
		slices.SortFunc(network.Peers, func(a, b NetworkLink) int { return strings.Compare(a.Host, b.Host) })
		slices.SortFunc(network.Access, func(a, b AccessRule) int {
			if a.Port != b.Port {
				return a.Port - b.Port
			}
			return strings.Compare(a.Source, b.Source)
		})
		hosts[host] = network
	}

	return hosts, nil
}

func (resolved ResolvedProject) nodeDestination(name string) string {
	if service, found := resolved.Services[name]; found {
		return service.Destination
	}
	return resolved.Release[name].Destination
}

// Digest identifies the desired state, so an unchanged network is recognized
// without comparing rendered files.
func (network HostNetwork) Digest() string {
	encoded, _ := json.Marshal(network)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func validWireGuardKey(key string) bool {
	raw, err := base64.StdEncoding.DecodeString(key)
	return err == nil && len(raw) == 32
}

// WireGuardConfig renders the wg-quick file without its PrivateKey line, which
// the helper inserts on the host. Table=off because routes are reconciled on
// their own; syncconf does not install them.
func (network HostNetwork) WireGuardConfig(publicKeys map[string]string) (string, error) {
	prefix, err := netip.ParsePrefix(network.CIDR)
	if err != nil {
		return "", err
	}
	var config strings.Builder
	fmt.Fprintf(&config, "[Interface]\nAddress = %s/%d\nListenPort = %d\nSaveConfig = false\nTable = off\n", network.Address, prefix.Bits(), network.ListenPort)
	for _, peer := range network.Peers {
		key := publicKeys[peer.Host]
		if !validWireGuardKey(key) {
			return "", fmt.Errorf("no valid public key for peer %s", peer.Host)
		}
		fmt.Fprintf(&config, "\n[Peer]\nPublicKey = %s\nAllowedIPs = %s/32\n", key, peer.Address)
		if peer.Endpoint != "" {
			fmt.Fprintf(&config, "Endpoint = %s\n", peer.Endpoint)
		}
		if peer.Keepalive > 0 {
			fmt.Fprintf(&config, "PersistentKeepalive = %d\n", peer.Keepalive)
		}
	}

	return config.String(), nil
}

// Routes are the host routes each peer needs through the interface.
func (network HostNetwork) Routes() []string {
	routes := make([]string, 0, len(network.Peers))
	for _, peer := range network.Peers {
		routes = append(routes, peer.Address+"/32")
	}
	return routes
}

// FirewallRules renders the owned chains as an iptables-restore --noflush
// fragment: declaring a chain flushes only that chain, so unrelated rules are
// never touched. The jumps into them are installed separately by the helper.
//
// IN admits the WireGuard listener. FWD sits under DOCKER-USER: established
// traffic first, then each consumer to its provider port by interface, source
// and original destination, then a drop for every owned port, then a reject
// for managed peer addresses that would leave by any other interface, so a
// lost route fails closed. OUT carries the same guard for the host itself.
func (network HostNetwork) FirewallRules() string {
	chain := chainPrefix(network.Interface)
	in, forward, out := chain+"-IN", chain+"-FWD", chain+"-OUT"
	var rules strings.Builder
	fmt.Fprintf(&rules, "*filter\n:%s - [0:0]\n:%s - [0:0]\n:%s - [0:0]\n", in, forward, out)
	fmt.Fprintf(&rules, "-A %s -p udp --dport %d -j ACCEPT\n", in, network.ListenPort)
	fmt.Fprintf(&rules, "-A %s -m conntrack --ctstate ESTABLISHED,RELATED -j RETURN\n", forward)
	for _, rule := range network.Access {
		fmt.Fprintf(&rules, "-A %s -i %s -s %s/32 -p tcp -m conntrack --ctorigdst %s --ctorigdstport %d -j RETURN\n", forward, network.Interface, rule.Source, network.Address, rule.Port)
	}
	var ports []int
	for _, rule := range network.Access {
		if !slices.Contains(ports, rule.Port) {
			ports = append(ports, rule.Port)
		}
	}
	for _, port := range ports {
		fmt.Fprintf(&rules, "-A %s -p tcp -m conntrack --ctorigdst %s --ctorigdstport %d -j DROP\n", forward, network.Address, port)
	}
	for _, route := range network.Routes() {
		fmt.Fprintf(&rules, "-A %s -d %s ! -o %s -j REJECT\n", forward, route, network.Interface)
		fmt.Fprintf(&rules, "-A %s -d %s ! -o %s -j REJECT\n", out, route, network.Interface)
	}
	fmt.Fprintf(&rules, "COMMIT\n")

	return rules.String()
}

// Jumps are the rules that send traffic into the owned chains, each checked
// before it is inserted so a repeated apply adds nothing.
func (network HostNetwork) Jumps() [][]string {
	chain := chainPrefix(network.Interface)
	return [][]string{
		{"INPUT", "-j", chain + "-IN"},
		{"DOCKER-USER", "-j", chain + "-FWD"},
		{"OUTPUT", "-j", chain + "-OUT"},
	}
}

// validate checks a host network before anything is rendered from it, since
// the helper will run with root on the result.
func (network HostNetwork) validate() error {
	var problems []error
	if network.Interface != interfaceName(network.RuntimeID) || !validRuntimeID(network.RuntimeID) || !validEnvironment(network.Environment) {
		problems = append(problems, errors.New("network identity does not match its runtime id"))
	}
	prefix, err := netip.ParsePrefix(network.CIDR)
	address, addressErr := netip.ParseAddr(network.Address)
	if err != nil || addressErr != nil || !prefix.Contains(address) {
		problems = append(problems, errors.New("network address must lie in its cidr"))
	}
	if !validPort(network.ListenPort) {
		problems = append(problems, errors.New("invalid listen port"))
	}
	for _, peer := range network.Peers {
		remote, err := netip.ParseAddr(peer.Address)
		if err != nil || !prefix.Contains(remote) || remote == address {
			problems = append(problems, fmt.Errorf("peer %s has an invalid address", peer.Host))
		}
	}
	for _, rule := range network.Access {
		source, err := netip.ParseAddr(rule.Source)
		if err != nil || !prefix.Contains(source) || !validPort(rule.Port) {
			problems = append(problems, fmt.Errorf("access rule %s is invalid", rule.For))
		}
	}

	return errors.Join(problems...)
}

// sortedHosts is the order networks are applied and printed in.
func sortedHosts(networks map[string]HostNetwork) []string {
	return slices.Sorted(maps.Keys(networks))
}
