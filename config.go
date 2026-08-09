package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
)

const (
	configFileName         = ".deploy.json"
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
	Version      int                    `json:"version"`
	ID           string                 `json:"id"`
	Name         string                 `json:"name"`
	GitStorage   string                 `json:"gitStorage,omitempty"`
	Destination  string                 `json:"destination,omitempty"`
	Retention    int                    `json:"retention,omitempty"`
	Services     map[string]Service     `json:"services"`
	Release      map[string]Service     `json:"release,omitempty"`
	Environments map[string]Environment `json:"environments,omitempty"`
}

type Environment struct {
	Services map[string]Service `json:"services,omitempty"`
	Release  map[string]Service `json:"release,omitempty"`
}

// Extra carries every key deploy does not own, so the compose renderer can hand
// them through untouched rather than us chasing compose's schema forever.
type Service struct {
	Image       string
	Build       json.RawMessage
	Stateful    *bool
	Env         []string
	Host        *Host
	DependsOn   map[string]string
	Healthcheck json.RawMessage
	Extra       map[string]json.RawMessage
}

type Host struct {
	Domain          string `json:"domain"`
	Port            int    `json:"port"`
	TunnelTokenFrom string `json:"tunnelTokenFrom"`
}

type ResolvedProject struct {
	Version     int                `json:"version"`
	ID          string             `json:"id"`
	Name        string             `json:"name"`
	GitStorage  string             `json:"gitStorage,omitempty"`
	Destination string             `json:"destination,omitempty"`
	Retention   int                `json:"retention"`
	Environment string             `json:"environment"`
	Services    map[string]Service `json:"services"`
	Release     map[string]Service `json:"release,omitempty"`
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

// serviceFields is the marshalling shape of the keys deploy owns. Service itself
// cannot carry these tags because Extra has to merge in on top.
type serviceFields struct {
	Image       string            `json:"image,omitempty"`
	Build       json.RawMessage   `json:"build,omitempty"`
	Stateful    *bool             `json:"stateful,omitempty"`
	Env         []string          `json:"env,omitempty"`
	Host        *Host             `json:"host,omitempty"`
	DependsOn   map[string]string `json:"dependsOn,omitempty"`
	Healthcheck json.RawMessage   `json:"healthcheck,omitempty"`
}

func (service *Service) UnmarshalJSON(raw []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}

	for _, key := range slices.Sorted(maps.Keys(fields)) {
		value := fields[key]
		var err error

		switch key {
		case "image":
			err = json.Unmarshal(value, &service.Image)
		case "build":
			service.Build = value
		case "stateful":
			var stateful bool
			if err = json.Unmarshal(value, &stateful); err == nil {
				service.Stateful = &stateful
			}
		case "env":
			err = json.Unmarshal(value, &service.Env)
		case "host":
			err = json.Unmarshal(value, &service.Host)
		case "dependsOn":
			err = json.Unmarshal(value, &service.DependsOn)
		case "healthcheck":
			service.Healthcheck = value
		default:
			if service.Extra == nil {
				service.Extra = map[string]json.RawMessage{}
			}
			service.Extra[key] = value
		}

		if err != nil {
			return fmt.Errorf("key %q: %w", key, err)
		}
	}

	return nil
}

func (service Service) MarshalJSON() ([]byte, error) {
	encoded, err := json.Marshal(serviceFields{
		Image:       service.Image,
		Build:       service.Build,
		Stateful:    service.Stateful,
		Env:         service.Env,
		Host:        service.Host,
		DependsOn:   service.DependsOn,
		Healthcheck: service.Healthcheck,
	})
	if err != nil {
		return nil, err
	}
	if len(service.Extra) == 0 {
		return encoded, nil
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		return nil, err
	}
	maps.Copy(fields, service.Extra)

	return json.Marshal(fields)
}

func LoadProject(path string) (Project, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Project{}, err
	}

	var project Project
	if err := json.Unmarshal(raw, &project); err != nil {
		return Project{}, fmt.Errorf("parsing %s: %w", path, err)
	}
	if project.Version != supportedConfigVersion {
		return Project{}, fmt.Errorf(
			"%s declares version %d, this deploy understands version %d",
			path, project.Version, supportedConfigVersion,
		)
	}

	return project, nil
}

// ResolveEnvironment layers an environment's overrides onto the base services. A
// project with no environments block resolves to its base services as written.
func (project Project) ResolveEnvironment(environmentName string) (ResolvedProject, error) {
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
		for name, override := range environment.Services {
			services[name] = mergeService(services[name], override)
		}
		for name, override := range environment.Release {
			release[name] = mergeService(release[name], override)
		}
	}

	retention := project.Retention
	if retention == 0 {
		retention = defaultRetention
	}

	return ResolvedProject{
		Version:     project.Version,
		ID:          project.ID,
		Name:        project.Name,
		GitStorage:  project.GitStorage,
		Destination: project.Destination,
		Retention:   retention,
		Environment: environmentName,
		Services:    services,
		Release:     release,
	}, nil
}

// mergeService overrides field by field rather than replacing the whole service,
// so an environment can add a host block without restating the build.
func mergeService(base, override Service) Service {
	merged := base

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
	if len(override.DependsOn) > 0 {
		merged.DependsOn = override.DependsOn
	}
	if override.Healthcheck != nil {
		merged.Healthcheck = override.Healthcheck
	}

	if len(override.Extra) > 0 {
		merged.Extra = make(map[string]json.RawMessage, len(base.Extra)+len(override.Extra))
		maps.Copy(merged.Extra, base.Extra)
		maps.Copy(merged.Extra, override.Extra)
	}

	return merged
}

// Validate reports every problem it finds rather than the first, because fixing
// a config one error per run is miserable.
func (resolved ResolvedProject) Validate() error {
	var problems []error

	if !projectIDPattern.MatchString(resolved.ID) {
		problems = append(problems, fmt.Errorf(
			"id %q must be 8 hex characters", resolved.ID,
		))
	}
	if resolved.Name == "" {
		problems = append(problems, errors.New("name is required"))
	}
	if len(resolved.Services) == 0 {
		problems = append(problems, errors.New("no services defined"))
	}

	for _, name := range slices.Sorted(maps.Keys(resolved.Services)) {
		problems = append(problems, validateService(name, resolved.Services[name], resolved)...)
	}
	for _, name := range slices.Sorted(maps.Keys(resolved.Release)) {
		problems = append(problems, validateService(name, resolved.Release[name], resolved)...)
	}

	return errors.Join(problems...)
}

func validateService(name string, service Service, resolved ResolvedProject) []error {
	var problems []error

	if !serviceNamePattern.MatchString(name) {
		problems = append(problems, fmt.Errorf(
			"service %q is not a usable docker service name", name,
		))
	}

	hasImage := service.Image != ""
	hasBuild := len(service.Build) > 0
	switch {
	case hasImage && hasBuild:
		problems = append(problems, fmt.Errorf(
			"service %q sets both image and build, pick one", name,
		))
	case !hasImage && !hasBuild:
		problems = append(problems, fmt.Errorf(
			"service %q sets neither image nor build", name,
		))
	}

	// the build block is worked out here as well as at build time, so a config
	// that cannot be built is refused by deploy check rather than three steps into
	// a deploy
	if hasBuild {
		if _, _, err := buildPlan(name, service); err != nil {
			problems = append(problems, err)
		}
	}

	if service.Host != nil {
		problems = append(problems, validateHost(name, service)...)
	}

	for _, dependency := range slices.Sorted(maps.Keys(service.DependsOn)) {
		condition := service.DependsOn[dependency]
		if !slices.Contains(dependsOnConditions, condition) {
			problems = append(problems, fmt.Errorf(
				"service %q depends on %q with condition %q, expected one of %s",
				name, dependency, condition, strings.Join(dependsOnConditions, ", "),
			))
		}
		_, isService := resolved.Services[dependency]
		_, isReleaseTask := resolved.Release[dependency]
		if !isService && !isReleaseTask {
			problems = append(problems, fmt.Errorf(
				"service %q depends on %q, which this environment does not define",
				name, dependency,
			))
		}
	}

	return problems
}

func validateHost(name string, service Service) []error {
	var problems []error

	if service.Host.Domain == "" {
		problems = append(problems, fmt.Errorf("service %q has a host block with no domain", name))
	}
	if service.Host.Port == 0 {
		problems = append(problems, fmt.Errorf("service %q has a host block with no port", name))
	}
	if service.Host.TunnelTokenFrom == "" {
		problems = append(problems, fmt.Errorf(
			"service %q has a host block with no tunnelTokenFrom, which names the env var holding the token rather than the token itself",
			name,
		))
	}
	if service.IsStateful() {
		problems = append(problems, fmt.Errorf(
			"service %q is stateful and cannot be hosted, because a singleton cannot be cut over",
			name,
		))
	}
	if len(service.Healthcheck) == 0 {
		problems = append(problems, fmt.Errorf(
			"service %q is hosted so it needs a healthcheck, otherwise there is no way to know the new container serves before we send it traffic",
			name,
		))
	}
	if _, publishes := service.Extra["ports"]; publishes {
		problems = append(problems, fmt.Errorf(
			"service %q is hosted so it must not publish ports, because two commits cannot both bind the same one",
			name,
		))
	}

	return problems
}
