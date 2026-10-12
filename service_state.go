package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"path"
	"regexp"
	"slices"
	"time"
)

const (
	serviceStateFileName = "services.json"
	serviceStateVersion  = 2
	// legacyNaming keeps the version 1 resource names: one compose project per
	// commit plus the shared stack, so adopting state never renames a volume.
	legacyNaming = "legacy"
)

const (
	outcomeLive          = "live"
	outcomeFailed        = "failed"
	outcomeUnknown       = "unknown"
	outcomeCompleted     = "completed"
	outcomeIndeterminate = "indeterminate"
	// outcomeResolved is an indeterminate run that an operator inspected and
	// settled with deploy resolve; whether it completed stays unknown
	outcomeResolved = "resolved"
)

// stateReader is the read-only part of a Runner. Planning reads state through
// it without opening a connection that could change anything.
type stateReader interface {
	Describe() string
	ReadFile(path string) ([]byte, error)
	ListDirectory(path string) ([]string, error)
}

// ServiceState is the version 2 record for one destination. Each service has
// its own releases, so a partial deploy across services or hosts can be
// written down truthfully instead of as one commit for everything.
type ServiceState struct {
	Version     int                      `json:"version"`
	Project     string                   `json:"project"`
	Name        string                   `json:"name,omitempty"`
	Environment string                   `json:"environment"`
	Destination string                   `json:"destination"`
	Naming      string                   `json:"naming"`
	Generation  int                      `json:"generation"`
	Services    map[string]ServiceRecord `json:"services"`
	Tasks       map[string][]TaskRecord  `json:"tasks,omitempty"`
	UpdatedAt   time.Time                `json:"updatedAt"`
}

// ServiceRecord stays in the state after its service leaves the config, so a
// removed or moved service is never mistaken for a new one.
type ServiceRecord struct {
	Stateful bool            `json:"stateful"`
	Current  *ServiceRelease `json:"current,omitempty"`
	Previous *ServiceRelease `json:"previous,omitempty"`
	Releases []string        `json:"releases,omitempty"`
	Outcome  string          `json:"outcome"`
}

type ServiceRelease struct {
	Commit string `json:"commit"`
	// ConfigDigest is empty for releases adopted from version 1 state, which
	// never recorded the configuration it deployed.
	ConfigDigest string `json:"configDigest,omitempty"`
	// Project and ComposeFile address the release for cutover and rollback. A
	// release adopted from version 1 points at its commit stack.
	Project     string `json:"project"`
	ComposeFile string `json:"composeFile"`
}

// TaskRecord keeps every release task run. An indeterminate run is never
// repeated automatically, since whether it changed data is unknown.
type TaskRecord struct {
	Operation    string    `json:"operation"`
	Commit       string    `json:"commit"`
	ConfigDigest string    `json:"configDigest"`
	Outcome      string    `json:"outcome"`
	RecordedAt   time.Time `json:"recordedAt"`
}

func (layout Layout) ServiceStateFile() string {
	return path.Join(layout.Root, serviceStateFileName)
}

func ReadServiceState(reader stateReader, layout Layout) (ServiceState, bool, error) {
	raw, err := reader.ReadFile(layout.ServiceStateFile())
	if errors.Is(err, fs.ErrNotExist) {
		return ServiceState{}, false, nil
	}
	if err != nil {
		return ServiceState{}, false, fmt.Errorf("reading %s: %w", layout.ServiceStateFile(), err)
	}

	var state ServiceState
	if err := decodeOwned(raw, &state); err != nil {
		return ServiceState{}, true, fmt.Errorf("%s is unreadable: %w", layout.ServiceStateFile(), err)
	}
	if state.Version != serviceStateVersion {
		return ServiceState{}, true, fmt.Errorf("%s uses unsupported state version %d; use a compatible deploy version", layout.ServiceStateFile(), state.Version)
	}
	if err := state.validate(); err != nil {
		return ServiceState{}, true, fmt.Errorf("%s: %w", layout.ServiceStateFile(), err)
	}

	return state, true, nil
}

// WriteServiceState bumps the generation, so a reader that saw an older one
// knows the destination changed after it looked.
func WriteServiceState(runner Runner, layout Layout, state ServiceState) (ServiceState, error) {
	state.Version = serviceStateVersion
	state.Generation++
	state.UpdatedAt = time.Now().UTC().Truncate(time.Second)
	if err := state.validate(); err != nil {
		return ServiceState{}, fmt.Errorf("refusing to write invalid state: %w", err)
	}

	encoded, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return ServiceState{}, err
	}
	if err := writeStateFile(runner, layout.ServiceStateFile(), append(encoded, '\n')); err != nil {
		return ServiceState{}, err
	}

	return state, nil
}

func (state ServiceState) validate() error {
	if !validRuntimeID(state.Project) {
		return fmt.Errorf("invalid project %q", state.Project)
	}
	if !validEnvironment(state.Environment) {
		return fmt.Errorf("invalid environment %q", state.Environment)
	}
	if !validEnvironment(state.Destination) {
		return fmt.Errorf("invalid destination %q", state.Destination)
	}
	if state.Naming != legacyNaming {
		return fmt.Errorf("unsupported resource naming %q", state.Naming)
	}
	if state.Generation < 1 {
		return errors.New("generation must be positive")
	}
	if state.Services == nil {
		return errors.New("services are missing")
	}

	for _, name := range slices.Sorted(maps.Keys(state.Services)) {
		if err := state.Services[name].validate(state.Project, name); err != nil {
			return fmt.Errorf("service %q: %w", name, err)
		}
		if !serviceNamePattern.MatchString(name) {
			return fmt.Errorf("invalid service name %q", name)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(state.Tasks)) {
		if !serviceNamePattern.MatchString(name) {
			return fmt.Errorf("invalid task name %q", name)
		}
		for _, run := range state.Tasks[name] {
			if err := run.validate(); err != nil {
				return fmt.Errorf("task %q: %w", name, err)
			}
		}
	}

	return nil
}

func (record ServiceRecord) validate(project, name string) error {
	if !slices.Contains([]string{outcomeLive, outcomeFailed, outcomeUnknown}, record.Outcome) {
		return fmt.Errorf("invalid outcome %q", record.Outcome)
	}
	for _, release := range []*ServiceRelease{record.Current, record.Previous} {
		if release != nil && !validRecordedRelease(*release) {
			return fmt.Errorf("invalid release %q", release.Commit)
		}
		if release != nil && !validArtifacts(project, name, record.Stateful, *release) {
			return fmt.Errorf("release %s points at %s %s, which deploy would not have created", release.Commit, release.Project, release.ComposeFile)
		}
	}
	if record.Current == nil && record.Previous != nil {
		return errors.New("a previous release needs a current one")
	}
	for _, commit := range record.Releases {
		if !commitPattern.MatchString(commit) {
			return fmt.Errorf("invalid retained release %q", commit)
		}
	}

	return nil
}

func (run TaskRecord) validate() error {
	if !operationIDPattern.MatchString(run.Operation) {
		return fmt.Errorf("invalid operation %q", run.Operation)
	}
	if !validRecordedRelease(ServiceRelease{Commit: run.Commit, ConfigDigest: run.ConfigDigest}) || run.ConfigDigest == "" {
		return fmt.Errorf("run %s needs a commit and config digest", run.Operation)
	}
	if !slices.Contains([]string{outcomeCompleted, outcomeFailed, outcomeIndeterminate, outcomeResolved}, run.Outcome) {
		return fmt.Errorf("invalid outcome %q", run.Outcome)
	}

	return nil
}

var configDigestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func validRecordedRelease(release ServiceRelease) bool {
	return commitPattern.MatchString(release.Commit) && (release.ConfigDigest == "" || configDigestPattern.MatchString(release.ConfigDigest))
}

// adoptLegacyState describes a version 1 deployment in version 2 terms without
// touching it. Version 1 kept one commit for the whole project, so every
// service placed here is recorded at that commit under the legacy names.
func adoptLegacyState(legacy State, resolved ResolvedProject, destination string) (ServiceState, error) {
	if legacy.Environment != "" && legacy.Environment != resolved.Environment {
		return ServiceState{}, fmt.Errorf("deployment belongs to %s, not %s; refusing to adopt it", legacy.Environment, resolved.Environment)
	}

	adopted := ServiceState{
		Version: serviceStateVersion, Project: resolved.RuntimeID(), Name: resolved.Name,
		Environment: resolved.Environment, Destination: destination, Naming: legacyNaming,
		Services: map[string]ServiceRecord{},
	}
	if legacy.Current == "" {
		return adopted, nil
	}

	for name, service := range resolved.Services {
		if service.Destination != "" && service.Destination != destination {
			continue
		}
		record := ServiceRecord{Stateful: service.IsStateful(), Outcome: outcomeLive}
		// the shared stack is not rolled back with a release, so a stateful
		// service has no previous release to return to
		if record.Stateful {
			current := sharedRelease(adopted.Project, legacy.Current, "")
			record.Current = &current
		} else {
			current := legacyRelease(adopted.Project, legacy.Current)
			record.Current = &current
			if legacy.Previous != "" {
				previous := legacyRelease(adopted.Project, legacy.Previous)
				record.Previous = &previous
			}
			record.Releases = slices.Clone(legacy.Releases)
		}
		adopted.Services[name] = record
	}

	return adopted, nil
}

// readDestinationState reads version 2 state, or adopts version 1 state in
// memory when that is all the destination has. Nothing is written.
func readDestinationState(reader stateReader, resolved ResolvedProject, layout Layout, destination string) (ServiceState, string, error) {
	state, found, err := ReadServiceState(reader, layout)
	if err != nil {
		return ServiceState{}, "", err
	}
	if found {
		if state.Project != resolved.RuntimeID() || state.Environment != resolved.Environment || state.Destination != destination {
			return ServiceState{}, "", fmt.Errorf("%s belongs to %s %s at destination %s; refusing to plan over it", layout.ServiceStateFile(), state.Project, state.Environment, state.Destination)
		}
		return state, "version 2", nil
	}

	legacy, err := checkEnvironment(reader, resolved, layout)
	if err != nil {
		return ServiceState{}, "", err
	}
	adopted, err := adoptLegacyState(legacy, resolved, destination)
	if err != nil {
		return ServiceState{}, "", err
	}
	if legacy.Current == "" {
		return adopted, "none", nil
	}

	return adopted, "version 1, adopted in memory", nil
}

// refuseServiceState stops the single-commit commands once version 2 state
// exists, since they would record a release it does not know about.
func refuseServiceState(reader stateReader, layout Layout) error {
	_, err := reader.ReadFile(layout.ServiceStateFile())
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading %s: %w", layout.ServiceStateFile(), err)
	}

	return fmt.Errorf("%s is recorded per service by a newer deploy; use a deploy version that supports version 2 state", layout.Root)
}
