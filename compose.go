package main

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
)

const composeFileName = "docker-compose.yml"

// dependsOnConditionsToCompose maps our three conditions onto what compose calls
// them. Ours are shorter because the compose spelling reads like an enum nobody
// asked for.
var dependsOnConditionsToCompose = map[string]string{
	"completed": "service_completed_successfully",
	"healthy":   "service_healthy",
	"started":   "service_started",
}

// ComposeProject is rendered as json rather than yaml. Compose parses json in a
// .yml file happily, and json is in the standard library, so this avoids a
// dependency for a file nobody hand edits.
type ComposeProject struct {
	Name     string                     `json:"name"`
	Services map[string]json.RawMessage `json:"services"`
}

func ProjectName(id, commit string) string {
	return "deploy-" + id + "-" + ShortCommit(commit)
}

func ImageTag(id, serviceName, commit string) string {
	return fmt.Sprintf("deploy-%s/%s:%s", id, serviceName, ShortCommit(commit))
}

// RenderCompose turns the resolved config into a compose file. Every service
// arrives as an image, because anything with a build key was built and tagged
// before this runs, so compose never builds anything itself.
func RenderCompose(resolved ResolvedProject, commit string) ([]byte, error) {
	services := map[string]json.RawMessage{}

	for _, name := range slices.Sorted(maps.Keys(resolved.Services)) {
		rendered, err := renderService(resolved, name, resolved.Services[name], commit)
		if err != nil {
			return nil, err
		}
		services[name] = rendered
	}

	document, err := json.MarshalIndent(ComposeProject{
		Name:     ProjectName(resolved.ID, commit),
		Services: services,
	}, "", "  ")
	if err != nil {
		return nil, err
	}

	return append(document, '\n'), nil
}

func renderService(resolved ResolvedProject, name string, service Service, commit string) (json.RawMessage, error) {
	fields := map[string]json.RawMessage{}

	// unowned keys first, so anything deploy actually decides wins over a stray
	// image or depends_on someone left in the config
	maps.Copy(fields, service.Extra)

	image := service.Image
	if len(service.Build) > 0 {
		image = ImageTag(resolved.ID, name, commit)
	}
	if err := setField(fields, "image", image); err != nil {
		return nil, err
	}

	if len(service.Env) > 0 {
		if err := setField(fields, "env_file", service.Env); err != nil {
			return nil, err
		}
	}

	if len(service.Healthcheck) > 0 {
		healthcheck, err := renderHealthcheck(name, service.Healthcheck)
		if err != nil {
			return nil, err
		}
		fields["healthcheck"] = healthcheck
	}

	if len(service.DependsOn) > 0 {
		dependsOn := map[string]map[string]string{}
		for _, dependency := range slices.Sorted(maps.Keys(service.DependsOn)) {
			dependsOn[dependency] = map[string]string{
				"condition": dependsOnConditionsToCompose[service.DependsOn[dependency]],
			}
		}
		if err := setField(fields, "depends_on", dependsOn); err != nil {
			return nil, err
		}
	}

	return json.Marshal(fields)
}

// renderHealthcheck translates our camelCase spelling into compose's snake_case,
// and calls the command `test`, which is what compose named it.
func renderHealthcheck(serviceName string, raw json.RawMessage) (json.RawMessage, error) {
	var declared map[string]json.RawMessage
	if err := json.Unmarshal(raw, &declared); err != nil {
		return nil, fmt.Errorf("service %q has an unreadable healthcheck: %w", serviceName, err)
	}

	renamed := map[string]string{
		"command":     "test",
		"startPeriod": "start_period",
		"startAll":    "start_interval",
	}

	healthcheck := map[string]json.RawMessage{}
	for _, key := range slices.Sorted(maps.Keys(declared)) {
		name := key
		if replacement, found := renamed[key]; found {
			name = replacement
		}
		healthcheck[name] = declared[key]
	}

	return json.Marshal(healthcheck)
}

func setField(fields map[string]json.RawMessage, name string, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("rendering %s: %w", name, err)
	}
	fields[name] = encoded

	return nil
}

// ServiceNames is the order things get reported in, which is alphabetical so two
// runs read the same.
func ServiceNames(services map[string]Service) []string {
	return slices.Sorted(maps.Keys(services))
}
