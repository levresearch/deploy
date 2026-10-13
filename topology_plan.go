package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
)

type TopologyPlan struct {
	Observations        map[string]DestinationObservation `json:"observations,omitempty"`
	Version             int                               `json:"version"`
	Project             string                            `json:"project"`
	Environment         string                            `json:"environment"`
	Commit              string                            `json:"commit"`
	ConfigDigest        string                            `json:"configDigest"`
	ConfigurationSource string                            `json:"configurationSource"`
	ExecutionAvailable  bool                              `json:"executionAvailable"`
	Destinations        []PlannedDestination              `json:"destinations"`
	Services            []PlannedService                  `json:"services"`
	Waves               [][]string                        `json:"readinessWaves"`
	Connections         []PlannedConnection               `json:"connections"`
	Network             PlannedNetwork                    `json:"network"`
	RecordedOnly        []string                          `json:"recordedOnly,omitempty"`
	Unverified          []string                          `json:"unverified"`
}

type PlannedDestination struct {
	Name                string `json:"name"`
	Target              string `json:"target"`
	HostHint            string `json:"hostHint"`
	HostIdentity        string `json:"hostIdentity"`
	CanonicalPath       string `json:"canonicalPath"`
	RecordedState       string `json:"recordedState,omitempty"`
	StateGeneration     int    `json:"stateGeneration,omitempty"`
	UnresolvedOperation string `json:"unresolvedOperation,omitempty"`
}

type PlannedService struct {
	Name            string              `json:"name"`
	Kind            string              `json:"kind"`
	Destination     string              `json:"destination"`
	Stateful        bool                `json:"stateful"`
	DependsOn       map[string]string   `json:"dependsOn,omitempty"`
	EnvFiles        []string            `json:"envFiles,omitempty"`
	Endpoints       map[string]Endpoint `json:"endpoints,omitempty"`
	UpdatePolicy    string              `json:"updatePolicy"`
	PreviousRelease string              `json:"previousRelease"`
}

type PlannedConnection struct {
	Consumer  string `json:"consumer"`
	Name      string `json:"name"`
	Provider  string `json:"provider"`
	Endpoint  string `json:"endpoint"`
	Transport string `json:"transport"`
	Host      string `json:"host"`
	Port      int    `json:"port"`
	HostEnv   string `json:"hostEnv"`
	PortEnv   string `json:"portEnv"`
}

type PlannedNetwork struct {
	Mode         string                 `json:"mode"`
	Peers        map[string]NetworkPeer `json:"peers,omitempty"`
	CIDR         string                 `json:"cidr,omitempty"`
	Links        [][2]string            `json:"links"`
	Action       string                 `json:"action"`
	Requirements []string               `json:"requirements"`
}

func planTopology(resolved ResolvedProject, commit string) (TopologyPlan, error) {
	if resolved.Version != 2 {
		return TopologyPlan{}, fmt.Errorf("plan requires version 2; preview conversion with deploy migrate")
	}
	if err := resolved.Validate(); err != nil {
		return TopologyPlan{}, err
	}
	encoded, err := json.Marshal(resolved)
	if err != nil {
		return TopologyPlan{}, err
	}
	digest := sha256.Sum256(encoded)
	waves, err := readinessWaves(resolved)
	if err != nil {
		return TopologyPlan{}, err
	}
	plan := TopologyPlan{
		Version: 2, Project: resolved.RuntimeID(), Environment: resolved.Environment,
		Commit: commit, ConfigDigest: hex.EncodeToString(digest[:]), ConfigurationSource: "working-tree", ExecutionAvailable: true,
		Waves: waves, Destinations: []PlannedDestination{}, Services: []PlannedService{}, Connections: []PlannedConnection{},
		Network:    PlannedNetwork{Mode: "docker-only", Links: [][2]string{}, Action: "observe before determining changes", Requirements: []string{"Docker"}},
		Unverified: []string{"host identities and SSH aliases", "canonical destination paths", "live and retained releases", "OS, architecture, privileges and firewall backend", "port, route and subnet conflicts", "secret-file environment variable collisions", "peer reachability and application authentication", "retained releases may still require an existing tunnel"},
	}
	for _, name := range slices.Sorted(maps.Keys(resolved.Destinations)) {
		target := resolved.Destinations[name]
		plan.Destinations = append(plan.Destinations, PlannedDestination{Name: name, Target: target, HostHint: hostHint(target), HostIdentity: "unverified", CanonicalPath: "unverified"})
	}
	links := map[[2]string]bool{}
	nodes := topologyNodes(resolved)
	for _, name := range slices.Sorted(maps.Keys(nodes)) {
		service := nodes[name]
		kind, policy := "service", "resolve from live state"
		if _, task := resolved.Release[name]; task {
			kind, policy = "release", "run once per recorded operation; indeterminate outcomes require reconciliation"
		}
		for _, endpoint := range service.Endpoints {
			if endpoint.HostPort != 0 {
				policy = "recreate private listener; downtime on replacement"
			}
		}
		plan.Services = append(plan.Services, PlannedService{Name: name, Kind: kind, Destination: service.Destination, Stateful: service.IsStateful(), DependsOn: service.DependsOn, EnvFiles: service.Env, Endpoints: service.Endpoints, UpdatePolicy: policy, PreviousRelease: "unverified"})
		for _, label := range slices.Sorted(maps.Keys(service.Connections)) {
			connection := service.Connections[label]
			provider := resolved.Services[connection.Service]
			endpoint := provider.Endpoints[connection.Endpoint]
			path := PlannedConnection{Consumer: name, Name: label, Provider: connection.Service, Endpoint: connection.Endpoint, Transport: "docker", Host: connection.Service, Port: endpoint.Port, HostEnv: connection.HostEnv, PortEnv: connection.PortEnv}
			if resolved.destinationHost(service.Destination) != resolved.destinationHost(provider.Destination) {
				path.Transport, path.Host, path.Port = "wireguard", resolved.Network.Peers[provider.Destination].Address, endpoint.HostPort
				pair := []string{service.Destination, provider.Destination}
				slices.Sort(pair)
				link := [2]string{pair[0], pair[1]}
				if !links[link] {
					plan.Network.Links = append(plan.Network.Links, link)
					links[link] = true
				}
				plan.Network.Mode = "wireguard-required"
				if plan.Network.Peers == nil {
					plan.Network.Peers = map[string]NetworkPeer{}
				}
				for _, peer := range pair {
					plan.Network.Peers[peer] = resolved.Network.Peers[peer]
				}
			}
			plan.Connections = append(plan.Connections, path)
		}
	}
	if plan.Network.Mode == "wireguard-required" {
		plan.Network.CIDR = resolved.Network.CIDR
		plan.Network.Requirements = []string{"Linux", "systemd", "rootful Docker with iptables backend", "WireGuard", "privileged owned-network setup", "direct UDP path for every peer link"}
	}
	return plan, nil
}

func validatePlanningProject(project Project) error {
	return validateObservedProject(project, nil)
}

func validateObservedProject(project Project, hosts map[string]string) error {
	names := slices.Sorted(maps.Keys(project.Environments))
	if len(names) == 0 {
		names = []string{defaultEnvironmentName}
	}
	var environments []ResolvedProject
	for _, name := range names {
		resolved, err := project.ResolveEnvironment(name)
		if err != nil {
			return err
		}
		resolved.observedHosts = hosts
		if err := resolved.Validate(); err != nil {
			return fmt.Errorf("environment %s: %w", name, err)
		}
		environments = append(environments, resolved)
	}
	return validateEnvironmentNetworks(environments)
}
