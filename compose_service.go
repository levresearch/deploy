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
	for _, endpoint := range slices.Sorted(maps.Keys(service.Endpoints)) {
		if service.Endpoints[endpoint].HostPort != 0 {
			return nil, fmt.Errorf("%s endpoint %s publishes a host port, which needs managed networking that is not available yet", name, endpoint)
		}
	}

	environment, err := connectionEnvironment(resolved, name, service)
	if err != nil {
		return nil, err
	}
	if environment != nil {
		service.Extra = maps.Clone(service.Extra)
		service.Extra["environment"] = environment
	}

	return renderProject(resolved, layout, ServiceReleaseProject(resolved.RuntimeID(), name, commit), map[string]Service{name: service}, commit)
}

// connectionEnvironment adds each connection's host and port to the service's
// own environment. On one host the provider is reached by its service name on
// the project network.
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
		if provider.Destination != service.Destination {
			return nil, fmt.Errorf("%s connection %s crosses destinations, which is not available yet", name, label)
		}
		values[connection.HostEnv] = connection.Service
		values[connection.PortEnv] = strconv.Itoa(endpoint.Port)
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
