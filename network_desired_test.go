package main

import (
	"encoding/base64"
	"os/exec"
	"strings"
	"testing"
)

func testKey(fill byte) string {
	key := make([]byte, 32)
	for index := range key {
		key[index] = fill
	}
	return base64.StdEncoding.EncodeToString(key)
}

func TestEachHostGetsOnlyTheNetworkItsConnectionsNeed(t *testing.T) {
	networks, err := desiredNetworks(topologyResolved(t))
	if err != nil {
		t.Fatal(err)
	}
	if hosts := sortedHosts(networks); strings.Join(hosts, ",") != "app-host,data-host" {
		t.Fatalf("hosts %v", hosts)
	}
	app, data := networks["app-host"], networks["data-host"]
	if app.Interface != data.Interface || len(app.Interface) > 15 || !strings.HasPrefix(app.Interface, "dw") || app.Interface != interfaceName("1234abcd") {
		t.Fatalf("interface %q", app.Interface)
	}
	if len(chainPrefix(app.Interface))+4 > 28 {
		t.Fatalf("chain names would exceed the iptables limit: %s", chainPrefix(app.Interface))
	}
	if len(app.Peers) != 1 || app.Peers[0] != (NetworkLink{Host: "data-host", Address: "10.77.0.2", Endpoint: "data.example.com:51820", Keepalive: 25}) {
		t.Fatalf("app peers %+v", app.Peers)
	}
	if len(data.Peers) != 1 || data.Peers[0] != (NetworkLink{Host: "app-host", Address: "10.77.0.1"}) {
		t.Fatalf("data peers %+v", data.Peers)
	}
	if len(app.Access) != 0 || len(data.Access) != 1 || data.Access[0] != (AccessRule{Source: "10.77.0.1", Port: 5432, For: "api/database"}) {
		t.Fatalf("access app=%+v data=%+v", app.Access, data.Access)
	}
	for _, network := range networks {
		if err := network.validate(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestOneHostNeedsNoWireGuard(t *testing.T) {
	resolved := topologyResolved(t)
	resolved.observedHosts = map[string]string{"app": "same", "data": "same"}
	// two aliases of one host must share their peer settings
	network := *resolved.Network
	network.Peers = map[string]NetworkPeer{"app": network.Peers["app"], "data": network.Peers["app"]}
	resolved.Network = &network
	networks, err := desiredNetworks(resolved)
	if err != nil || len(networks) != 0 {
		t.Fatalf("aliases of one host got a network: %+v %v", networks, err)
	}
	single := serviceReleaseResolved(t, serviceReleaseFixture)
	if networks, err := desiredNetworks(single); err != nil || len(networks) != 0 {
		t.Fatalf("a single destination got a network: %+v %v", networks, err)
	}
}

func TestTheWireGuardConfigCarriesNoSecretAndNoRoutes(t *testing.T) {
	networks, err := desiredNetworks(topologyResolved(t))
	if err != nil {
		t.Fatal(err)
	}
	app := networks["app-host"]
	if _, err := app.WireGuardConfig(map[string]string{}); err == nil {
		t.Fatal("a peer without a public key was rendered")
	}
	if _, err := app.WireGuardConfig(map[string]string{"data-host": "not-a-key"}); err == nil {
		t.Fatal("an invalid public key was rendered")
	}
	config, err := app.WireGuardConfig(map[string]string{"data-host": testKey(7)})
	if err != nil {
		t.Fatal(err)
	}
	want := "[Interface]\nAddress = 10.77.0.1/24\nListenPort = 51820\nSaveConfig = false\nTable = off\n\n[Peer]\nPublicKey = " + testKey(7) + "\nAllowedIPs = 10.77.0.2/32\nEndpoint = data.example.com:51820\nPersistentKeepalive = 25\n"
	if config != want {
		t.Fatalf("config:\n%s\nwant:\n%s", config, want)
	}
	if strings.Contains(config, "PrivateKey") {
		t.Fatal("the config carries a private key")
	}
	if routes := app.Routes(); len(routes) != 1 || routes[0] != "10.77.0.2/32" {
		t.Fatalf("routes %v", routes)
	}
}

func TestOwnedRulesAdmitOnlyTheConsumerAndFailClosed(t *testing.T) {
	networks, err := desiredNetworks(topologyResolved(t))
	if err != nil {
		t.Fatal(err)
	}
	data := networks["data-host"]
	chain := chainPrefix(data.Interface)
	want := strings.Join([]string{
		"*filter",
		":" + chain + "-IN - [0:0]",
		":" + chain + "-FWD - [0:0]",
		":" + chain + "-OUT - [0:0]",
		"-A " + chain + "-IN -p udp --dport 51820 -j ACCEPT",
		"-A " + chain + "-FWD -m conntrack --ctstate ESTABLISHED,RELATED -j RETURN",
		"-A " + chain + "-FWD -i " + data.Interface + " -s 10.77.0.1/32 -p tcp -m conntrack --ctorigdst 10.77.0.2 --ctorigdstport 5432 -j RETURN",
		"-A " + chain + "-FWD -p tcp -m conntrack --ctorigdst 10.77.0.2 --ctorigdstport 5432 -j DROP",
		"-A " + chain + "-FWD -d 10.77.0.1/32 ! -o " + data.Interface + " -j REJECT",
		"-A " + chain + "-OUT -d 10.77.0.1/32 ! -o " + data.Interface + " -j REJECT",
		"COMMIT",
		"",
	}, "\n")
	if got := data.FirewallRules(); got != want {
		t.Fatalf("rules:\n%s\nwant:\n%s", got, want)
	}
	if jumps := data.Jumps(); len(jumps) != 3 || jumps[1][0] != "DOCKER-USER" {
		t.Fatalf("jumps %v", jumps)
	}
}

func TestTheDigestFollowsTheDesiredStateOnly(t *testing.T) {
	first, err := desiredNetworks(topologyResolved(t))
	if err != nil {
		t.Fatal(err)
	}
	second, err := desiredNetworks(topologyResolved(t))
	if err != nil {
		t.Fatal(err)
	}
	if first["data-host"].Digest() != second["data-host"].Digest() {
		t.Fatal("the same config produced different digests")
	}
	changed := second["data-host"]
	changed.Access = append(changed.Access, AccessRule{Source: "10.77.0.1", Port: 6379, For: "api/cache"})
	if changed.Digest() == first["data-host"].Digest() {
		t.Fatal("a new access rule did not change the digest")
	}
}

func TestANetworkThatCouldEscapeItsSubnetIsRefused(t *testing.T) {
	networks, err := desiredNetworks(topologyResolved(t))
	if err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*HostNetwork){
		"foreign interface": func(network *HostNetwork) { network.Interface = "eth0" },
		"outside address":   func(network *HostNetwork) { network.Address = "192.168.1.1" },
		"outside peer":      func(network *HostNetwork) { network.Peers[0].Address = "8.8.8.8" },
		"self as peer":      func(network *HostNetwork) { network.Peers[0].Address = network.Address },
		"outside source":    func(network *HostNetwork) { network.Access[0].Source = "0.0.0.0" },
		"bad port":          func(network *HostNetwork) { network.ListenPort = 0 },
	} {
		network := networks["data-host"]
		network.Peers = append([]NetworkLink(nil), network.Peers...)
		network.Access = append([]AccessRule(nil), network.Access...)
		change(&network)
		if err := network.validate(); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// the rendered files are fed to the real tools inside a throwaway container,
// whose own network namespace keeps the host firewall untouched
func TestTheRealToolsAcceptWhatIsRendered(t *testing.T) {
	dockerAvailable(t)
	t.Parallel()
	networks, err := desiredNetworks(topologyResolved(t))
	if err != nil {
		t.Fatal(err)
	}
	data := networks["data-host"]
	config, err := data.WireGuardConfig(map[string]string{"app-host": testKey(9)})
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	writeFile(t, directory, "rules", data.FirewallRules())
	writeFile(t, directory, data.Interface+".conf", "[Interface]\nPrivateKey = "+testKey(3)+"\n"+strings.TrimPrefix(config, "[Interface]\n"))
	chain := chainPrefix(data.Interface)
	script := `set -eux
apk add --quiet --no-progress iptables wireguard-tools-wg-quick >/dev/null
iptables -N DOCKER-USER
iptables-restore --noflush < /check/rules
iptables-restore --noflush < /check/rules
test "$(iptables -S ` + chain + `-FWD | wc -l)" -eq 5
iptables -S ` + chain + `-OUT | grep -q 'REJECT'
wg-quick strip /check/` + data.Interface + `.conf | grep -q '^AllowedIPs = 10.77.0.1/32$'
`
	output, err := exec.Command("docker", "run", "--rm", "--cap-add", "NET_ADMIN", "-v", directory+":/check:ro", "alpine:3.20", "sh", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("the real tools refused the rendered network: %v\n%s", err, output)
	}
}
