package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path"
	"slices"
	"strconv"
)

// ServiceReleaseProject names one service's release. Underscores separate the
// parts because runtime ids, version 2 service names and commits never contain
// one, so this can never be mistaken for a version 1 commit stack or for a
// service of another environment.
func ServiceReleaseProject(id, service, commit string) string {
	return "deploy-" + id + "_" + service + "_" + ShortCommit(commit)
}

// release artifacts are recorded relative to the project root, so a state file
// can never point a stop, start or delete outside it.
func serviceComposePath(service, commit string) string {
	return path.Join("releases", ShortCommit(commit), service+".compose.yml")
}

func legacyComposePath(commit string) string {
	return path.Join("releases", ShortCommit(commit), composeFileName)
}

func sharedComposePath() string {
	return path.Join("shared", composeFileName)
}

// serviceRelease is a new per-service release of a stateless service.
func serviceRelease(id, service, commit, configDigest string) ServiceRelease {
	return ServiceRelease{Commit: ShortCommit(commit), ConfigDigest: configDigest, Project: ServiceReleaseProject(id, service, commit), ComposeFile: serviceComposePath(service, commit)}
}

// legacyRelease is a service inside a version 1 commit stack.
func legacyRelease(id, commit string) ServiceRelease {
	return ServiceRelease{Commit: ShortCommit(commit), Project: ProjectName(id, commit), ComposeFile: legacyComposePath(commit)}
}

func sharedRelease(id, commit, configDigest string) ServiceRelease {
	return ServiceRelease{Commit: ShortCommit(commit), ConfigDigest: configDigest, Project: SharedProjectName(id), ComposeFile: sharedComposePath()}
}

// validArtifacts accepts only the names deploy itself would have produced for
// this service and commit, so an edited state file cannot aim a compose down
// at some other project or file.
func validArtifacts(id, service string, stateful bool, release ServiceRelease) bool {
	if stateful {
		return release.Project == SharedProjectName(id) && release.ComposeFile == sharedComposePath()
	}
	legacy := legacyRelease(id, release.Commit)
	if release.Project == legacy.Project && release.ComposeFile == legacy.ComposeFile {
		return true
	}

	return release.Project == ServiceReleaseProject(id, service, release.Commit) && release.ComposeFile == serviceComposePath(service, release.Commit)
}

// RenderServiceRelease is one stateless service on its own, so it can be
// started, replaced and rolled back without touching any other service.
func RenderServiceRelease(resolved ResolvedProject, layout Layout, name, commit string) ([]byte, error) {
	service, found := resolved.Services[name]
	if !found || service.IsStateful() {
		return nil, fmt.Errorf("%s is not a stateless service", name)
	}

	return renderProject(resolved, layout, ServiceReleaseProject(resolved.RuntimeID(), name, commit), map[string]Service{name: service}, commit)
}

// workloadRestart keeps Docker from starting a network workload at boot or on
// a daemon restart, before its private address, routes and firewall exist.
// The network helper starts it once they do; Docker still restarts it after a
// crash.
const workloadRestart = "on-failure"

// topologyService adds what the topology decides to one service or release
// task before it is rendered: its connection environment, the endpoints other
// hosts reach and, for a service on the managed network, its restart policy.
func topologyService(resolved ResolvedProject, name string, service Service) (Service, error) {
	if resolved.Version != 2 {
		return service, nil
	}
	environment, err := connectionEnvironment(resolved, name, service)
	if err != nil {
		return Service{}, err
	}
	ports := crossHostPorts(resolved, name, service)
	workload := networkWorkload(resolved, name, service)
	if environment == nil && len(ports) == 0 && !workload {
		return service, nil
	}

	service.Extra = maps.Clone(service.Extra)
	if service.Extra == nil {
		service.Extra = map[string]json.RawMessage{}
	}
	if environment != nil {
		service.Extra["environment"] = environment
	}
	if len(ports) > 0 {
		if _, set := service.Extra["ports"]; set {
			return Service{}, fmt.Errorf("%s: raw ports cannot be combined with endpoints reached from another host", name)
		}
		service.Extra["ports"], _ = json.Marshal(ports)
	}
	if workload {
		if _, set := service.Extra["restart"]; set {
			return Service{}, fmt.Errorf("%s is on the managed network, so deploy sets its restart policy and starts it once the network is up; remove restart", name)
		}
		service.Extra["restart"], _ = json.Marshal(workloadRestart)
	}

	return service, nil
}

// networkWorkload reports whether a service publishes onto the managed network
// or consumes across it, which is what makes the network helper start it.
func networkWorkload(resolved ResolvedProject, name string, service Service) bool {
	if _, task := resolved.Release[name]; task {
		return false
	}

	return len(crossHostPorts(resolved, name, service)) > 0 || crossesHosts(resolved, service)
}

// crossesHosts reports whether a service consumes a provider on another host.
func crossesHosts(resolved ResolvedProject, service Service) bool {
	for _, connection := range service.Connections {
		if provider, found := resolved.Services[connection.Service]; found && resolved.destinationHost(provider.Destination) != resolved.destinationHost(service.Destination) {
			return true
		}
	}

	return false
}

// crossHostPorts publishes each endpoint a consumer on another host reaches,
// bound to the provider's private address and nowhere else. An endpoint only
// its own host uses is not published at all.
func crossHostPorts(resolved ResolvedProject, name string, service Service) []string {
	if resolved.Network == nil {
		return nil
	}
	reached := map[string]bool{}
	nodes := topologyNodes(resolved)
	for _, consumer := range slices.Sorted(maps.Keys(nodes)) {
		for _, connection := range nodes[consumer].Connections {
			if connection.Service == name && resolved.destinationHost(nodes[consumer].Destination) != resolved.destinationHost(service.Destination) {
				reached[connection.Endpoint] = true
			}
		}
	}
	peer, found := resolved.Network.Peers[service.Destination]
	if !found {
		return nil
	}
	var ports []string
	for _, endpoint := range slices.Sorted(maps.Keys(reached)) {
		published := service.Endpoints[endpoint]
		if published.HostPort == 0 {
			continue
		}
		ports = append(ports, fmt.Sprintf("%s:%d:%d/tcp", peer.Address, published.HostPort, published.Port))
	}

	return ports
}

// connectionEnvironment adds each connection's host and port to the service's
// own environment. A provider on the same host is reached by its service name
// on the project network; one on another host by its private address and
// published port.
func connectionEnvironment(resolved ResolvedProject, name string, service Service) (json.RawMessage, error) {
	if len(service.Connections) == 0 {
		return nil, nil
	}

	values := map[string]string{}
	for _, label := range slices.Sorted(maps.Keys(service.Connections)) {
		connection := service.Connections[label]
		provider, found := resolved.Services[connection.Service]
		endpoint, hasEndpoint := provider.Endpoints[connection.Endpoint]
		if !found || !hasEndpoint {
			return nil, fmt.Errorf("%s connection %s requires a named service endpoint", name, label)
		}
		if resolved.destinationHost(provider.Destination) == resolved.destinationHost(service.Destination) {
			values[connection.HostEnv] = connection.Service
			values[connection.PortEnv] = strconv.Itoa(endpoint.Port)
			continue
		}
		peer, peered := NetworkPeer{}, false
		if resolved.Network != nil {
			peer, peered = resolved.Network.Peers[provider.Destination]
		}
		if !peered || endpoint.HostPort == 0 {
			return nil, fmt.Errorf("%s connection %s crosses hosts and needs a network peer for %s and a hostPort on %s", name, label, provider.Destination, connection.Endpoint)
		}
		values[connection.HostEnv] = peer.Address
		values[connection.PortEnv] = strconv.Itoa(endpoint.HostPort)
	}

	existing := service.Extra["environment"]
	if len(existing) == 0 {
		return json.Marshal(values)
	}

	var object map[string]json.RawMessage
	if err := json.Unmarshal(existing, &object); err == nil && object != nil {
		for _, key := range slices.Sorted(maps.Keys(values)) {
			if _, taken := object[key]; taken {
				return nil, fmt.Errorf("%s sets %s itself, which a connection also sets", name, key)
			}
			object[key], _ = json.Marshal(values[key])
		}
		return json.Marshal(object)
	}

	var list []string
	if err := json.Unmarshal(existing, &list); err != nil || list == nil {
		return nil, errors.New(name + ": environment must be an object or a list")
	}
	names, _ := inlineVariableNames(existing)
	for _, key := range slices.Sorted(maps.Keys(values)) {
		if names[key] {
			return nil, fmt.Errorf("%s sets %s itself, which a connection also sets", name, key)
		}
		list = append(list, key+"="+values[key])
	}

	return json.Marshal(list)
}
