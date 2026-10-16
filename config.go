package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
)

const (
	configFileName         = ".deploy.json"
	deployDirectoryName    = ".deploy"
	supportedConfigVersion = 1
	defaultRetention       = 3
	defaultEnvironmentName = "production"
)

var (
	projectIDPattern    = regexp.MustCompile(`^[0-9a-f]{8}$`)
	serviceNamePattern  = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)
	dependsOnConditions = []string{"completed", "healthy", "started"}
)

type Project struct {
	Version      int               `json:"version"`
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	GitStorage   string            `json:"gitStorage,omitempty"`
	Destination  string            `json:"destination,omitempty"`
	Destinations map[string]string `json:"destinations,omitempty"`
	Network      *ManagedNetwork   `json:"network,omitempty"`
	Retention    int               `json:"retention,omitempty"`
	// BuildOnDestination belongs in the file rather than only on the command
	// line because whether a project can be cross built is a fact about the
	// project, not about the mood of whoever is deploying it today.
	BuildOnDestination bool                   `json:"buildOnDestination,omitempty"`
	Notify             *Notify                `json:"notify,omitempty"`
	Services           map[string]Service     `json:"services"`
	Release            map[string]Service     `json:"release,omitempty"`
	Environments       map[string]Environment `json:"environments,omitempty"`
}

type Environment struct {
	Network  *ManagedNetwork    `json:"network,omitempty"`
	Services map[string]Service `json:"services,omitempty"`
	Release  map[string]Service `json:"release,omitempty"`
}

// Extra carries every key deploy does not own, so the compose renderer can hand
// them through untouched rather than us chasing compose's schema forever.
type Service struct {
	Destination string
	Endpoints   map[string]Endpoint
	Connections map[string]Connection
	Image       string
	Build       json.RawMessage
	Stateful    *bool
	Env         []string
	Host        *Host
	DependsOn   map[string]string
	Healthcheck json.RawMessage
	// Enabled false leaves the service out of the resolved project, so an environment can drop one it inherits.
	Enabled *bool
	Extra   map[string]json.RawMessage
}

type Host struct {
	Domain          string `json:"domain"`
	Port            int    `json:"port"`
	TunnelTokenFrom string `json:"tunnelTokenFrom"`
	// TunnelEnv names an env file that only cloudflared's token is read from,
	// so the token never reaches the hosted service's own environment.
	TunnelEnv string `json:"tunnelEnv,omitempty"`
}

type ResolvedProject struct {
	observedHosts map[string]string
	// whole is the project a destination's view was cut from; see on
	whole              *ResolvedProject
	Version            int                `json:"version"`
	ID                 string             `json:"id"`
	Name               string             `json:"name"`
	GitStorage         string             `json:"gitStorage,omitempty"`
	Destination        string             `json:"destination,omitempty"`
	Destinations       map[string]string  `json:"destinations,omitempty"`
	Network            *ManagedNetwork    `json:"network,omitempty"`
	Retention          int                `json:"retention"`
	BuildOnDestination bool               `json:"buildOnDestination,omitempty"`
	Notify             *Notify            `json:"notify,omitempty"`
	Environment        string             `json:"environment"`
	Services           map[string]Service `json:"services"`
	Release            map[string]Service `json:"release,omitempty"`
}

func (service Service) IsStateful() bool {
	return service.Stateful != nil && *service.Stateful
}

// buildPlan works out how one service gets built, which is either a Dockerfile
// somebody wrote or one deploy renders from the inline shorthand. The generated
// file is returned as context to add rather than written anywhere, so nobody's
// checkout grows a file they did not put there.
func buildPlan(serviceName string, service Service) (dockerfile string, extraContext map[string][]byte, err error) {
	inline, isInline, err := ParseInlineBuild(service.Build)
	if err != nil {
		return "", nil, fmt.Errorf("service %q: %w", serviceName, err)
	}
	if isInline {
		rendered, err := inline.RenderDockerfile(serviceName)
		if err != nil {
			return "", nil, err
		}
		name := generatedDockerfileName(serviceName)

		return name, map[string][]byte{name: []byte(rendered)}, nil
	}

	var asPath string
	if err := json.Unmarshal(service.Build, &asPath); err == nil {
		return asPath, nil, nil
	}

	var asObject struct {
		Dockerfile string `json:"dockerfile"`
	}
	if err := json.Unmarshal(service.Build, &asObject); err != nil {
		return "", nil, fmt.Errorf("service %q has an unreadable build block: %w", serviceName, err)
	}
	if asObject.Dockerfile == "" {
		return "", nil, fmt.Errorf(
			"service %q has a build block with neither a dockerfile nor a from, so deploy cannot tell how to build it",
			serviceName,
		)
	}

	return asObject.Dockerfile, nil, nil
}

func LoadProject(path string) (Project, error) {
	project, err := loadPlanningProject(path)
	if err != nil {
		return Project{}, err
	}
	if project.Version == 2 {
		return Project{}, errors.New("this command is not available for version 2 yet; version 2 supports deploy, adopt, resolve, status, releases, list, logs, shell, exec and env push")
	}
	return project, nil
}

// ResolveEnvironment layers an environment's overrides onto the base services. A
// project with no environments block resolves to its base services as written.
func (project Project) ResolveEnvironment(environmentName string) (ResolvedProject, error) {
	if project.Version == 2 && len(project.Environments) == 0 && environmentName != defaultEnvironmentName {
		return ResolvedProject{}, fmt.Errorf("version 2 environment %q must be declared explicitly", environmentName)
	}
	network := project.Network
	services := maps.Clone(project.Services)
	if services == nil {
		services = map[string]Service{}
	}
	release := maps.Clone(project.Release)
	if release == nil {
		release = map[string]Service{}
	}

	if len(project.Environments) > 0 {
		environment, found := project.Environments[environmentName]
		if !found {
			return ResolvedProject{}, fmt.Errorf(
				"no environment named %q, this project defines %s",
				environmentName, strings.Join(slices.Sorted(maps.Keys(project.Environments)), ", "),
			)
		}
		if environment.Network != nil {
			network = environment.Network
		}
		for name, override := range environment.Services {
			services[name] = mergeService(services[name], override, project.Version)
		}
		for name, override := range environment.Release {
			release[name] = mergeService(release[name], override, project.Version)
		}
	}
	dropDisabled(services)
	dropDisabled(release)

	retention := project.Retention
	if retention == 0 {
		retention = defaultRetention
	}

	return ResolvedProject{
		Version:            project.Version,
		ID:                 project.ID,
		Name:               project.Name,
		GitStorage:         project.GitStorage,
		Destination:        project.Destination,
		Destinations:       maps.Clone(project.Destinations),
		Network:            network,
		Retention:          retention,
		BuildOnDestination: project.BuildOnDestination,
		Notify:             project.Notify,
		Environment:        environmentName,
		Services:           services,
		Release:            release,
	}, nil
}

// dropDisabled removes services marked enabled false and clears the flag on
// the rest, so it never changes the digest of a service that stays.
func dropDisabled(services map[string]Service) {
	for name, service := range services {
		if service.Enabled != nil && !*service.Enabled {
			delete(services, name)
			continue
		}
		service.Enabled = nil
		services[name] = service
	}
}

// mergeService overrides field by field rather than replacing the whole service,
// so an environment can add a host block without restating the build.
func mergeService(base, override Service, version int) Service {
	merged := base
	if override.Destination != "" {
		merged.Destination = override.Destination
	}
	if override.Endpoints != nil {
		merged.Endpoints = mergeEntries(base.Endpoints, override.Endpoints)
	}
	if override.Connections != nil {
		merged.Connections = mergeEntries(base.Connections, override.Connections)
	}

	if override.Image != "" {
		merged.Image = override.Image
	}
	if override.Build != nil {
		merged.Build = override.Build
	}
	if override.Stateful != nil {
		merged.Stateful = override.Stateful
	}
	if len(override.Env) > 0 {
		merged.Env = override.Env
	}
	if override.Host != nil {
		merged.Host = override.Host
	}
	if len(override.DependsOn) > 0 || (version == 2 && override.DependsOn != nil) {
		merged.DependsOn = override.DependsOn
	}
	if override.Healthcheck != nil {
		merged.Healthcheck = override.Healthcheck
	}
	if override.Enabled != nil {
		merged.Enabled = override.Enabled
	}

	if len(override.Extra) > 0 {
		merged.Extra = make(map[string]json.RawMessage, len(base.Extra)+len(override.Extra))
		maps.Copy(merged.Extra, base.Extra)
		maps.Copy(merged.Extra, override.Extra)
	}

	return merged
}
