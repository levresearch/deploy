package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path"
	"slices"
)

// composeServices lists what a rendered stack really contains. Version 1
// renders compose files as json, so no yaml parser is needed.
func composeServices(runner Runner, file string) ([]string, error) {
	raw, err := runner.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", file, err)
	}
	var project ComposeProject
	if err := json.Unmarshal(raw, &project); err != nil {
		return nil, fmt.Errorf("%s was not rendered by deploy: %w", file, err)
	}

	return slices.Sorted(maps.Keys(project.Services)), nil
}

// adoptDeployment records a version 1 deployment per service. It records what
// the live commit stack actually runs, not only what today's config names, so
// a service dropped from the config is kept on record instead of forgotten.
func adoptDeployment(runner Runner, resolved ResolvedProject, layout Layout, name string) (ServiceState, error) {
	legacy, err := checkEnvironment(runner, resolved, layout)
	if err != nil {
		return ServiceState{}, err
	}
	if legacy.Current == "" {
		return ServiceState{}, errors.New("there is no version 1 deployment here to adopt; deploy with the version 2 config directly")
	}
	adopted, err := adoptLegacyState(legacy, resolved, name)
	if err != nil {
		return ServiceState{}, err
	}

	stateful, _ := SplitServices(resolved.Services)
	live, err := composeServices(runner, path.Join(layout.Root, legacyComposePath(legacy.Current)))
	if err != nil && len(stateful) < len(resolved.Services) {
		return ServiceState{}, err
	}
	for service, record := range adopted.Services {
		if !record.Stateful && !slices.Contains(live, service) {
			delete(adopted.Services, service)
		}
	}
	var previous []string
	if legacy.Previous != "" {
		if previous, err = composeServices(runner, path.Join(layout.Root, legacyComposePath(legacy.Previous))); err != nil {
			previous = nil
		}
	}
	for _, service := range live {
		record, found := adopted.Services[service]
		if !found {
			current := legacyRelease(adopted.Project, legacy.Current)
			record = ServiceRecord{Outcome: outcomeLive, Current: &current, Releases: slices.Clone(legacy.Releases)}
		}
		if record.Previous != nil && !slices.Contains(previous, service) {
			record.Previous = nil
		}
		adopted.Services[service] = record
	}

	return adopted, nil
}

func RunAdopt(options DeployOptions, write bool) (exitCode int, err error) {
	startPath := options.Context
	if startPath == "" {
		if startPath, err = os.Getwd(); err != nil {
			return exitPreconditionNotMet, err
		}
	}
	repositoryPath, err := FindRepository(startPath)
	if err != nil {
		return exitPreconditionNotMet, err
	}
	resolved, name, destination, err := loadServiceConfig(repositoryPath, options.Environment)
	if err != nil {
		return exitPreconditionNotMet, err
	}

	set, layout, err := lockServiceDestination(resolved, name, destination)
	if err != nil {
		return exitPreconditionNotMet, err
	}
	runner := set.Runner(0)
	if _, found, err := ReadServiceState(runner, layout); err != nil || found {
		set.Release()
		if err == nil {
			err = fmt.Errorf("%s is already recorded per service", layout.Root)
		}
		return exitPreconditionNotMet, err
	}
	adopted, err := adoptDeployment(runner, resolved, layout, name)
	if err != nil {
		set.Release()
		return exitPreconditionNotMet, err
	}

	fmt.Printf("%s/%s at %s:\n", resolved.Name, resolved.Environment, layout.Root)
	for _, service := range slices.Sorted(maps.Keys(adopted.Services)) {
		record := adopted.Services[service]
		line := fmt.Sprintf("  %s: %s in %s", service, record.Current.Commit, record.Current.Project)
		if record.Previous != nil {
			line += ", previous " + record.Previous.Commit
		}
		if _, configured := resolved.Services[service]; !configured {
			line += " (no longer in the config, kept on record)"
		}
		fmt.Println(line)
	}
	if !write {
		set.Release()
		fmt.Println("nothing was written. after this, version 1 deploy refuses this deployment; run deploy adopt --write to record it")
		return exitOK, nil
	}

	if _, err := set.Begin("adopt", ""); err != nil {
		set.Release()
		return exitPreconditionNotMet, err
	}
	defer set.Finish(&exitCode)
	if _, err := WriteServiceState(runner, layout, adopted); err != nil {
		return exitDeployFailed, err
	}
	fmt.Println("recorded per service; nothing was restarted, renamed or moved")

	return exitOK, nil
}
