package main

import (
	"encoding/json"
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"
)

const (
	composeFileName      = "docker-compose.yml"
	releaseTasksFileName = "docker-compose.tasks.yml"
)

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
	Networks map[string]any             `json:"networks"`
	Volumes  map[string]any             `json:"volumes,omitempty"`
}

// SharedProjectName never contains a commit. Compose derives volume and container
// names from the project name, so a name that changed per deploy would detach
// every volume and start the stack empty on every single deploy.
func SharedProjectName(id string) string {
	return "deploy-" + id + "-shared"
}

func ProjectName(id, commit string) string {
	return "deploy-" + id + "-" + ShortCommit(commit)
}

func NetworkName(id string) string {
	return "deploy-" + id + "-net"
}

// VolumeName is fully qualified for the same reason the shared project name is
// stable. Compose would otherwise prefix it per project, so the same declared
// volume would be a different volume in each of the two stacks.
func VolumeName(id, declared string) string {
	return "deploy-" + id + "-" + declared
}

func ImageTag(id, serviceName, commit string) string {
	return fmt.Sprintf("deploy-%s/%s:%s", id, serviceName, ShortCommit(commit))
}

// SplitServices divides the config the way the whole design turns on. Stateful
// services are singletons shared across every commit, and stateless ones are
// versioned per commit and can run two at a time, which is what makes a cutover
// possible at all.
func SplitServices(services map[string]Service) (stateful, stateless map[string]Service) {
	stateful, stateless = map[string]Service{}, map[string]Service{}

	for name, service := range services {
		if service.IsStateful() {
			stateful[name] = service
			continue
		}
		stateless[name] = service
	}

	return stateful, stateless
}

// RenderShared is the stack that outlives every release. It owns the volumes, so
// nothing here is ever renamed and nothing here is pruned.
func RenderShared(resolved ResolvedProject, layout Layout) ([]byte, error) {
	stateful, _ := SplitServices(resolved.Services)

	rendered, err := renderProject(resolved, layout, SharedProjectName(resolved.ID), stateful, "")
	if err != nil {
		return nil, err
	}

	tunnels, err := renderTunnels(resolved)
	if err != nil {
		return nil, err
	}
	if len(tunnels) == 0 {
		return rendered, nil
	}

	// cloudflared belongs beside the stateful services rather than in a release
	// stack, because one inside a per-commit project restarts on every deploy
	var project ComposeProject
	if err := json.Unmarshal(rendered, &project); err != nil {
		return nil, err
	}
	maps.Copy(project.Services, tunnels)

	document, err := json.MarshalIndent(project, "", "  ")
	if err != nil {
		return nil, err
	}

	return append(document, '\n'), nil
}

// RenderRelease is the per-commit stack. It declares no volumes of its own, and
// any it references belong to the shared stack.
func RenderRelease(resolved ResolvedProject, layout Layout, commit string) ([]byte, error) {
	_, stateless := SplitServices(resolved.Services)

	return renderProject(resolved, layout, ProjectName(resolved.ID, commit), stateless, commit)
}

// RenderReleaseTasks is a stack nothing ever brings up. Each task is started one
// at a time with compose run, which is what gives them env files, volumes, and
// the shared network without any of it being restated here.
func RenderReleaseTasks(resolved ResolvedProject, layout Layout, commit string) ([]byte, error) {
	return renderProject(resolved, layout, ReleaseTasksProjectName(resolved.ID, commit), resolved.Release, commit)
}

func ReleaseTasksProjectName(id, commit string) string {
	return ProjectName(id, commit) + "-tasks"
}

func renderProject(
	resolved ResolvedProject,
	layout Layout,
	projectName string,
	services map[string]Service,
	commit string,
) ([]byte, error) {
	rendered := map[string]json.RawMessage{}
	declaredVolumes := map[string]any{}

	for _, name := range slices.Sorted(maps.Keys(services)) {
		service, err := renderService(resolved, layout, name, services[name], commit, services, declaredVolumes)
		if err != nil {
			return nil, err
		}
		rendered[name] = service
	}

	project := ComposeProject{
		Name:     projectName,
		Services: rendered,
		Networks: map[string]any{
			"default": map[string]any{"name": NetworkName(resolved.ID), "external": true},
		},
	}

	// both stacks reference the same volumes, and neither creates them, because
	// deploy makes them itself with a stable name that no project name prefixes
	if len(declaredVolumes) > 0 {
		project.Volumes = declaredVolumes
	}

	document, err := json.MarshalIndent(project, "", "  ")
	if err != nil {
		return nil, err
	}

	return append(document, '\n'), nil
}

func renderService(
	resolved ResolvedProject,
	layout Layout,
	name string,
	service Service,
	commit string,
	siblings map[string]Service,
	declaredVolumes map[string]any,
) (json.RawMessage, error) {
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
		if err := setField(fields, "env_file", EnvFilePaths(layout, service.Env)); err != nil {
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

	if volumes, found := fields["volumes"]; found {
		rewritten, err := rewriteVolumes(resolved.ID, name, volumes, declaredVolumes)
		if err != nil {
			return nil, err
		}
		fields["volumes"] = rewritten
	}

	if err := setDependsOn(fields, service, siblings); err != nil {
		return nil, err
	}

	return json.Marshal(fields)
}

// setDependsOn only keeps dependencies that live in the same compose project.
// Compose cannot wait on a service it does not own, and a dependency on a
// stateful service is already satisfied by ordering, since the shared stack is up
// and healthy before any release stack starts.
func setDependsOn(fields map[string]json.RawMessage, service Service, siblings map[string]Service) error {
	dependsOn := map[string]map[string]string{}

	for _, dependency := range slices.Sorted(maps.Keys(service.DependsOn)) {
		if _, sameProject := siblings[dependency]; !sameProject {
			continue
		}
		dependsOn[dependency] = map[string]string{
			"condition": dependsOnConditionsToCompose[service.DependsOn[dependency]],
		}
	}

	if len(dependsOn) == 0 {
		delete(fields, "depends_on")
		return nil
	}

	return setField(fields, "depends_on", dependsOn)
}

// rewriteVolumes qualifies every named volume and declares it external. A bind
// mount is left exactly as written, since it is a path rather than something
// docker manages.
func rewriteVolumes(
	projectID, serviceName string,
	raw json.RawMessage,
	declaredVolumes map[string]any,
) (json.RawMessage, error) {
	var entries []string
	if err := json.Unmarshal(raw, &entries); err != nil {
		// the long form is a list of objects, which we pass through untouched
		// rather than half understanding it
		return raw, nil
	}

	rewritten := make([]string, 0, len(entries))
	for _, entry := range entries {
		source, rest, found := strings.Cut(entry, ":")
		if !found || isBindMount(source) {
			rewritten = append(rewritten, entry)
			continue
		}

		qualified := VolumeName(projectID, source)
		declaredVolumes[qualified] = map[string]any{"external": true}
		rewritten = append(rewritten, qualified+":"+rest)
	}

	return json.Marshal(rewritten)
}

func isBindMount(source string) bool {
	return strings.HasPrefix(source, ".") || strings.HasPrefix(source, "/") || strings.HasPrefix(source, "~")
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

// VolumesFor is what deploy creates before either stack comes up, since both
// declare them external and neither will make them.
func VolumesFor(resolved ResolvedProject) []string {
	found := map[string]bool{}

	for _, service := range resolved.Services {
		var entries []string
		if err := json.Unmarshal(service.Extra["volumes"], &entries); err != nil {
			continue
		}
		for _, entry := range entries {
			source, _, cut := strings.Cut(entry, ":")
			if cut && !isBindMount(source) {
				found[VolumeName(resolved.ID, source)] = true
			}
		}
	}

	return slices.Sorted(maps.Keys(found))
}

// EnvFilePaths sends a bare name to the project level env directory, where the
// files actually live, since a gitignored .env is never in the release tree that
// git archive placed. Anything written as a path is left alone, so a committed
// file next to the code still works.
func EnvFilePaths(layout Layout, declared []string) []string {
	resolvedPaths := make([]string, 0, len(declared))
	for _, entry := range declared {
		if strings.ContainsRune(entry, '/') {
			resolvedPaths = append(resolvedPaths, entry)
			continue
		}
		resolvedPaths = append(resolvedPaths, path.Join(layout.EnvDirectory(), entry))
	}

	return resolvedPaths
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
