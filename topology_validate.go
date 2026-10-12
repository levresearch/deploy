package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path"
	"regexp"
	"slices"
	"strings"
)

var sshHostPattern = regexp.MustCompile(`^([a-zA-Z0-9._-]+@)?[a-zA-Z0-9][a-zA-Z0-9._-]*$`)
var variablePattern = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

func topologyDestination(raw string) (Destination, error) {
	destination, err := ParseDestination(raw)
	if err != nil {
		return destination, err
	}
	if destination.IsRemote() && (!sshHostPattern.MatchString(destination.Host) || strings.HasPrefix(destination.Host, "-")) {
		return destination, errors.New("destination needs a plain SSH host or user@host")
	}
	clean := path.Clean(destination.Path)
	if strings.ContainsAny(destination.Path, "\x00\r\n") || strings.HasPrefix(destination.Path, "~") || strings.HasPrefix(destination.Path, "-") || strings.TrimSpace(destination.Path) == "" || clean == "/" || clean == "." || clean == ".." {
		return destination, errors.New("destination needs an explicit directory; use a home-relative path instead of ~")
	}
	return destination, nil
}

// this is only a configuration hint; aliases need live host identity checks.
func hostHint(raw string) string {
	destination, _ := ParseDestination(raw)
	if !destination.IsRemote() {
		return "local"
	}
	_, host, found := strings.Cut(destination.Host, "@")
	if !found {
		host = destination.Host
	}
	return host
}

func (resolved ResolvedProject) destinationHost(name string) string {
	if identity, observed := resolved.observedHosts[name]; observed {
		return identity
	}
	return hostHint(resolved.Destinations[name])
}

func (resolved ResolvedProject) validateTopology() error {
	problems := validateReadiness(resolved)
	if resolved.Destination != "" {
		problems = append(problems, errors.New("version 2 does not allow a root destination"))
	}
	if len(resolved.Destinations) == 0 {
		problems = append(problems, errors.New("version 2 requires named destinations"))
	}
	if resolved.Retention < 1 {
		problems = append(problems, errors.New("retention must be positive"))
	}
	for _, name := range slices.Sorted(maps.Keys(resolved.Destinations)) {
		if !validEnvironment(name) {
			problems = append(problems, fmt.Errorf("destination %q needs a lowercase slug", name))
		}
		if _, err := topologyDestination(resolved.Destinations[name]); err != nil {
			problems = append(problems, fmt.Errorf("destination %s: %w", name, err))
		}
	}
	nodes := topologyNodes(resolved)
	ports := map[string]string{}
	for _, name := range slices.Sorted(maps.Keys(nodes)) {
		service := nodes[name]
		if !validEnvironment(name) {
			problems = append(problems, fmt.Errorf("version 2 service/task %q needs a lowercase slug of at most 32 characters", name))
		}
		if _, exists := resolved.Destinations[service.Destination]; !exists {
			problems = append(problems, fmt.Errorf("%s requires a known destination", name))
		}
		for _, endpointName := range slices.Sorted(maps.Keys(service.Endpoints)) {
			endpoint := service.Endpoints[endpointName]
			if !validEnvironment(endpointName) || endpoint.Protocol != "tcp" || !validPort(endpoint.Port) || (endpoint.HostPort != 0 && !validPort(endpoint.HostPort)) {
				problems = append(problems, fmt.Errorf("%s endpoint %s requires a lowercase name, tcp and valid ports", name, endpointName))
			}
			if endpoint.HostPort != 0 {
				key := fmt.Sprintf("%s:%d", resolved.destinationHost(service.Destination), endpoint.HostPort)
				if previous, exists := ports[key]; exists {
					problems = append(problems, fmt.Errorf("host port collision between %s and %s/%s", previous, name, endpointName))
				}
				ports[key] = name + "/" + endpointName
			}
		}
		problems = append(problems, validateConnections(name, service, resolved)...)
		problems = append(problems, validateTopologyExtra(name, service)...)
	}
	problems = append(problems, validateManagedNetwork(resolved)...)
	return errors.Join(problems...)
}

func validPort(port int) bool { return port > 0 && port <= 65535 }

func validateConnections(name string, service Service, resolved ResolvedProject) []error {
	var problems []error
	variables, err := inlineVariableNames(service.Extra["environment"])
	if err != nil {
		problems = append(problems, fmt.Errorf("%s: %w", name, err))
	}
	for _, label := range slices.Sorted(maps.Keys(service.Connections)) {
		connection := service.Connections[label]
		provider, exists := resolved.Services[connection.Service]
		endpoint, found := provider.Endpoints[connection.Endpoint]
		if !validEnvironment(label) || !exists || !found {
			problems = append(problems, fmt.Errorf("%s connection %s requires a named service endpoint", name, label))
			continue
		}
		for _, variable := range []string{connection.HostEnv, connection.PortEnv} {
			if !variablePattern.MatchString(variable) || variables[variable] {
				problems = append(problems, fmt.Errorf("%s connection %s has an invalid or colliding environment variable %q", name, label, variable))
			}
			variables[variable] = true
		}
		if resolved.destinationHost(service.Destination) != resolved.destinationHost(provider.Destination) {
			if endpoint.HostPort == 0 {
				problems = append(problems, fmt.Errorf("%s connection %s requires a stable provider hostPort", name, label))
			}
			if resolved.Network == nil {
				problems = append(problems, fmt.Errorf("%s connection %s crosses hosts and requires network", name, label))
				continue
			}
			consumerPeer, consumerFound := resolved.Network.Peers[service.Destination]
			providerPeer, providerFound := resolved.Network.Peers[provider.Destination]
			if !consumerFound || !providerFound {
				problems = append(problems, fmt.Errorf("%s connection %s needs both network peers", name, label))
			}
			if consumerPeer.Endpoint == "" && providerPeer.Endpoint == "" {
				problems = append(problems, fmt.Errorf("%s connection %s needs at least one reachable UDP endpoint", name, label))
			}
		}
	}
	return problems
}

func inlineVariableNames(raw json.RawMessage) (map[string]bool, error) {
	result := map[string]bool{}
	if len(raw) == 0 {
		return result, nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err == nil && object != nil {
		for key := range object {
			result[key] = true
		}
		return result, nil
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err != nil || list == nil {
		return result, errors.New("environment must be an object or a list")
	}
	for _, entry := range list {
		key, _, _ := strings.Cut(entry, "=")
		result[key] = true
	}
	return result, nil
}

func validateTopologyExtra(name string, service Service) []error {
	var problems []error
	for _, key := range []string{"depends_on", "links", "external_links", "network_mode", "networks", "volumes_from", "container_name", "pid", "ipc", "privileged", "cap_add", "devices", "sysctls", "env_file"} {
		if _, exists := service.Extra[key]; exists {
			problems = append(problems, fmt.Errorf("%s: raw %s is not supported in version 2; use owned topology fields", name, key))
		}
	}
	if len(service.Endpoints) > 0 {
		if _, exists := service.Extra["ports"]; exists {
			problems = append(problems, fmt.Errorf("%s: endpoints cannot be combined with raw ports", name))
		}
	}
	return problems
}
