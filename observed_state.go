package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"slices"
	"strings"
)

const observedStateLimit = 1 << 20

const (
	missingObservedPath = 7
	observedFileRead    = `test -e "$1" || test -L "$1" || exit 7
test -f "$1"
cat -- "$1"
`
	observedDirectoryList = `test -e "$1" || test -L "$1" || exit 7
test -d "$1"
ls -1A -- "$1"
`
)

// observedReader reads recorded state with the strict probe options, so
// planning never adds a host key or leaves a control socket behind.
type observedReader struct {
	ctx         context.Context
	destination Destination
}

func (reader observedReader) Describe() string {
	return reader.destination.String()
}

func (reader observedReader) read(script, target string) ([]byte, error) {
	output := boundedOutput{limit: observedStateLimit}
	status, err := runObservation(reader.ctx, reader.destination, []string{"sh", "-c", script, "deploy-observe-state", target}, &output)
	if status == missingObservedPath {
		return nil, fs.ErrNotExist
	}
	if errors.Is(err, errObservationCancelled) {
		return nil, err
	}
	if err != nil {
		return nil, fmt.Errorf("cannot read %s on %s", target, reader.Describe())
	}
	return output.buffer.Bytes(), nil
}

func (reader observedReader) ReadFile(target string) ([]byte, error) {
	return reader.read(observedFileRead, target)
}

func (reader observedReader) ListDirectory(target string) ([]string, error) {
	listing, err := reader.read(observedDirectoryList, target)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return strings.Fields(string(listing)), err
}

// applyRecordedState fills in what each destination has recorded: the live
// release of every service, task history, services that are recorded but no
// longer configured there, and any operation that never finished.
func applyRecordedState(plan *TopologyPlan, resolved ResolvedProject, readers map[string]stateReader) error {
	recorded := map[string]ServiceState{}
	sources := map[string]string{}
	for index := range plan.Destinations {
		destination := &plan.Destinations[index]
		reader := readers[destination.Name]
		if reader == nil {
			return fmt.Errorf("destination %s was not observed", destination.Name)
		}
		layout := NewLayout(destination.CanonicalPath, resolved.RuntimeID())

		operation, pending, err := readOperation(reader, operationFile(layout))
		if err != nil {
			return fmt.Errorf("destination %s: %w", destination.Name, err)
		}
		if pending {
			destination.UnresolvedOperation = unresolvedOperation(reader, operation).Error()
		}

		state, source, err := readDestinationState(reader, resolved, layout, destination.Name)
		if err != nil {
			return fmt.Errorf("destination %s: %w", destination.Name, err)
		}
		destination.RecordedState, destination.StateGeneration = source, state.Generation
		recorded[destination.Name], sources[destination.Name] = state, source
	}

	configured := map[string]bool{}
	for index := range plan.Services {
		service := &plan.Services[index]
		configured[service.Destination+"/"+service.Name] = true
		state := recorded[service.Destination]
		service.PreviousRelease = "none"
		if service.Kind == "release" {
			if runs := state.Tasks[service.Name]; len(runs) > 0 {
				last := runs[len(runs)-1]
				service.PreviousRelease = last.Commit + " (" + last.Outcome + ")"
			} else if sources[service.Destination] == "version 1, adopted in memory" {
				service.PreviousRelease = "unrecorded by version 1"
			}
			continue
		}
		if record, found := state.Services[service.Name]; found && record.Current != nil {
			service.PreviousRelease = record.Current.Commit
		}
	}

	plan.RecordedOnly = []string{}
	for _, destination := range slices.Sorted(maps.Keys(recorded)) {
		for _, name := range slices.Sorted(maps.Keys(recorded[destination].Services)) {
			if !configured[destination+"/"+name] {
				plan.RecordedOnly = append(plan.RecordedOnly, destination+"/"+name)
			}
		}
	}
	plan.Unverified = slices.DeleteFunc(plan.Unverified, func(item string) bool {
		return item == "live and retained releases"
	})

	return nil
}
