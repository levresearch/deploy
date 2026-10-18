package main

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
)

// privateNetworkPattern keeps a private network name short enough that the
// docker network built from it stays readable next to the project network.
var privateNetworkPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// PrivateNetworkName is the docker network behind one private network. Docker
// creates it internal, so nothing on it reaches beyond it, and only services
// that are isolated in it or join it are attached at all.
func PrivateNetworkName(id, name string) string {
	return "deploy-" + id + "-" + name + "-private"
}

// serviceNetworks is the compose networks list for one service, or nil for a
// service that only sits on the project network, which compose gives it anyway.
func serviceNetworks(service Service) []string {
	if service.Isolated != "" {
		return []string{service.Isolated}
	}
	if len(service.Joins) == 0 {
		return nil
	}

	return append([]string{"default"}, slices.Sorted(slices.Values(service.Joins))...)
}

// privateNetworksOf names every private network the given services sit on.
func privateNetworksOf(groups ...map[string]Service) []string {
	found := map[string]bool{}
	for _, services := range groups {
		for _, service := range services {
			if service.Isolated != "" {
				found[service.Isolated] = true
			}
			for _, joined := range service.Joins {
				found[joined] = true
			}
		}
	}

	return slices.Sorted(maps.Keys(found))
}

// PrivateNetworksFor is every docker network deploy makes for a project's
// private networks, which is what the shared stack needs before anything starts
// and what destroy removes.
func PrivateNetworksFor(resolved ResolvedProject) []string {
	names := privateNetworksOf(resolved.Services, resolved.Release)
	networks := make([]string, 0, len(names))
	for _, name := range names {
		networks = append(networks, PrivateNetworkName(resolved.RuntimeID(), name))
	}

	return networks
}

// ensurePrivateNetworks creates each private network as an internal network.
// One that already exists has to be internal too, since a network somebody made
// by hand with outside access would quietly undo the isolation.
func ensurePrivateNetworks(runner Runner, resolved ResolvedProject) error {
	for _, network := range PrivateNetworksFor(resolved) {
		output, err := runner.Run([]string{"docker", "network", "create", "--internal", network})
		if err == nil {
			continue
		}
		if !strings.Contains(string(output), "already exists") {
			return fmt.Errorf("creating private network %s on %s: %s", network, runner.Describe(), firstLine(output))
		}
		internal, err := runner.Run([]string{"docker", "network", "inspect", "--format", "{{.Internal}}", network})
		if err != nil {
			return fmt.Errorf("inspecting private network %s on %s: %s", network, runner.Describe(), firstLine(internal))
		}
		if strings.TrimSpace(string(internal)) != "true" {
			return fmt.Errorf(
				"private network %s already exists on %s but is not internal, so it would not isolate anything; remove it and deploy again",
				network, runner.Describe(),
			)
		}
	}

	return nil
}

// validatePrivateNetworks checks that nothing which must reach an isolated
// service is left outside its network. Joining a network nothing is isolated in
// yet is allowed on purpose: deploying the joins first and the isolation after
// moves a running database without cutting off the releases still using it.
func validatePrivateNetworks(resolved ResolvedProject) []error {
	var problems []error
	all := map[string]Service{}
	maps.Copy(all, resolved.Services)
	maps.Copy(all, resolved.Release)

	isolatedIn := map[string][]string{}
	for _, name := range slices.Sorted(maps.Keys(all)) {
		service := all[name]
		if service.Isolated != "" {
			isolatedIn[service.Isolated] = append(isolatedIn[service.Isolated], name)
		}
	}

	for _, name := range slices.Sorted(maps.Keys(all)) {
		service := all[name]
		if service.Isolated != "" && len(service.Joins) > 0 {
			problems = append(problems, fmt.Errorf(
				"service %q is isolated in %q and also joins %s; an isolated service sits on its own private network only",
				name, service.Isolated, strings.Join(service.Joins, ", "),
			))
		}
		if service.Isolated != "" && !privateNetworkPattern.MatchString(service.Isolated) {
			problems = append(problems, fmt.Errorf(
				"service %q is isolated in %q, which must be a lowercase name of at most 32 characters", name, service.Isolated,
			))
		}
		seen := map[string]bool{}
		for _, joined := range service.Joins {
			switch {
			case !privateNetworkPattern.MatchString(joined):
				problems = append(problems, fmt.Errorf(
					"service %q joins %q, which must be a lowercase name of at most 32 characters", name, joined,
				))
			case seen[joined]:
				problems = append(problems, fmt.Errorf("service %q joins %q twice", name, joined))
			}
			seen[joined] = true
		}

		if service.Isolated == "" {
			continue
		}
		// an isolated service is reached only over its private network, so
		// anything that would expose it another way defeats the point
		if service.Host != nil {
			problems = append(problems, fmt.Errorf(
				"service %q is isolated, so its tunnel could never reach it; host the service that joins %q instead",
				name, service.Isolated,
			))
		}
		if _, publishes := service.Extra["ports"]; publishes {
			problems = append(problems, fmt.Errorf("service %q is isolated and must not publish ports", name))
		}
		if len(service.Endpoints) > 0 {
			problems = append(problems, fmt.Errorf(
				"service %q is isolated and cannot declare endpoints, which other hosts would reach it through", name,
			))
		}
		for _, key := range []string{"networks", "network_mode"} {
			if _, set := service.Extra[key]; set {
				problems = append(problems, fmt.Errorf("service %q is isolated, so deploy owns its %s", name, key))
			}
		}
	}

	// whatever waits on or connects to an isolated service has to be on its
	// network, and on the same machine, since a docker network is per host
	for _, network := range slices.Sorted(maps.Keys(isolatedIn)) {
		members := map[string]bool{}
		destinations := map[string]bool{}
		for _, name := range slices.Sorted(maps.Keys(all)) {
			service := all[name]
			if service.Isolated == network || slices.Contains(service.Joins, network) {
				members[name] = true
				destinations[service.Destination] = true
			}
		}
		if len(destinations) > 1 {
			problems = append(problems, fmt.Errorf(
				"private network %q spans destinations %s, but a private network lives on one host",
				network, strings.Join(slices.Sorted(maps.Keys(destinations)), ", "),
			))
		}
		for _, name := range slices.Sorted(maps.Keys(all)) {
			if members[name] {
				continue
			}
			service := all[name]
			for _, isolated := range isolatedIn[network] {
				if _, waits := service.DependsOn[isolated]; waits {
					problems = append(problems, fmt.Errorf(
						"service %q depends on %q, which is isolated in %q; add %q to its joins",
						name, isolated, network, network,
					))
				}
				for _, connection := range slices.Sorted(maps.Keys(service.Connections)) {
					if service.Connections[connection].Service == isolated {
						problems = append(problems, fmt.Errorf(
							"service %q connects to %q, which is isolated in %q; add %q to its joins",
							name, isolated, network, network,
						))
					}
				}
			}
		}
	}

	return problems
}
