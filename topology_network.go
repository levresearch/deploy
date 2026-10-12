package main

import (
	"fmt"
	"maps"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
)

func validateManagedNetwork(resolved ResolvedProject) []error {
	if resolved.Network == nil {
		return nil
	}
	var problems []error
	network := resolved.Network
	prefix, err := netip.ParsePrefix(network.CIDR)
	private := false
	for _, block := range []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"} {
		allowed := netip.MustParsePrefix(block)
		if prefix.IsValid() && allowed.Contains(prefix.Addr()) && prefix.Bits() >= allowed.Bits() {
			private = true
		}
	}
	if err != nil || !private || prefix != prefix.Masked() || prefix.Bits() > 30 {
		return []error{fmt.Errorf("network.cidr must be a canonical private IPv4 subnet with usable peer addresses")}
	}
	addresses := map[netip.Addr]string{}
	hosts := map[string]NetworkPeer{}
	for _, name := range slices.Sorted(maps.Keys(network.Peers)) {
		peer := network.Peers[name]
		_, exists := resolved.Destinations[name]
		if !exists {
			problems = append(problems, fmt.Errorf("network peer %s is not a destination", name))
			continue
		}
		address, err := netip.ParseAddr(peer.Address)
		if err != nil || !address.Is4() || !prefix.Contains(address) || address == prefix.Addr() || !prefix.Contains(address.Next()) {
			problems = append(problems, fmt.Errorf("peer %s needs a usable IPv4 address in network.cidr", name))
		}
		host := resolved.destinationHost(name)
		if previous, used := addresses[address]; used && previous != host {
			problems = append(problems, fmt.Errorf("peer %s duplicates a private address", name))
		}
		addresses[address] = host
		if previous, used := hosts[host]; used && previous != peer {
			problems = append(problems, fmt.Errorf("destinations on host %s must use identical peer settings", host))
		}
		hosts[host] = peer
		if !validPort(peer.ListenPort) || peer.Keepalive < 0 || peer.Keepalive > 65535 {
			problems = append(problems, fmt.Errorf("peer %s has invalid listenPort or keepalive", name))
		}
		if peer.Endpoint != "" {
			host, portText, err := net.SplitHostPort(peer.Endpoint)
			port, portErr := strconv.Atoi(portText)
			if err != nil || portErr != nil || !validPort(port) || !sshHostPattern.MatchString(host) || strings.Contains(host, "@") {
				problems = append(problems, fmt.Errorf("peer %s endpoint must be hostname:port or IPv4:port", name))
			}
		}
	}
	return problems
}

func validateEnvironmentNetworks(environments []ResolvedProject) error {
	for index, first := range environments {
		if first.Network == nil {
			continue
		}
		for _, second := range environments[index+1:] {
			if second.Network == nil {
				continue
			}
			for firstName, a := range first.Network.Peers {
				for secondName, b := range second.Network.Peers {
					if first.destinationHost(firstName) != second.destinationHost(secondName) {
						continue
					}
					one, _ := netip.ParsePrefix(first.Network.CIDR)
					two, _ := netip.ParsePrefix(second.Network.CIDR)
					if one.Overlaps(two) || a.ListenPort == b.ListenPort {
						return fmt.Errorf("environments %s and %s share a host and need separate network subnets and listen ports", first.Environment, second.Environment)
					}
				}
			}
		}
	}
	return nil
}
