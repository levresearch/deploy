package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path"
	"slices"
	"strings"
)

// openedDestination is one destination opened for a read-only command.
type openedDestination struct {
	name        string
	destination Destination
	view        ResolvedProject
	runner      Runner
	layout      Layout
}

// openServiceDestinations opens every destination of a version 2 config, or
// only those named, for the read-only commands. It takes no lock, and still
// refuses state recorded for anyone else.
func openServiceDestinations(repositoryPath string, options DeployOptions, only ...string) (ResolvedProject, []openedDestination, func(), error) {
	if options.Destination != "" {
		return ResolvedProject{}, nil, nil, errors.New("version 2 takes its destinations from the config; --destination applies to version 1")
	}
	resolved, destinations, err := loadTopology(repositoryPath, options.Environment)
	if err != nil {
		return ResolvedProject{}, nil, nil, err
	}
	var opened []openedDestination
	var closers []func()
	closeAll := func() {
		for index := len(closers) - 1; index >= 0; index-- {
			closers[index]()
		}
	}
	for _, name := range slices.Sorted(maps.Keys(destinations)) {
		if len(only) > 0 && !slices.Contains(only, name) {
			continue
		}
		runner, closeRunner, err := OpenRunner(destinations[name])
		if err != nil {
			closeAll()
			return ResolvedProject{}, nil, nil, fmt.Errorf("destination %s: %w", name, err)
		}
		closers = append(closers, closeRunner)
		destination, err := AbsoluteDestination(runner, destinations[name])
		if err != nil {
			closeAll()
			return ResolvedProject{}, nil, nil, fmt.Errorf("destination %s: %w", name, err)
		}
		layout := NewLayout(destination.Path, resolved.RuntimeID())
		if _, _, err := readDestinationState(runner, resolved, layout, name); err != nil {
			closeAll()
			return ResolvedProject{}, nil, nil, fmt.Errorf("destination %s: %w", name, err)
		}
		opened = append(opened, openedDestination{name: name, destination: destination, view: resolved.on(name), runner: runner, layout: layout})
	}

	return resolved, opened, closeAll, nil
}

// openServiceProject opens a version 2 config's only destination.
func openServiceProject(repositoryPath string, options DeployOptions) (ResolvedProject, Layout, Runner, func(), Destination, error) {
	_, opened, closeAll, err := openServiceDestinations(repositoryPath, options)
	if err != nil {
		return ResolvedProject{}, Layout{}, nil, nil, Destination{}, err
	}
	if len(opened) != 1 {
		closeAll()
		return ResolvedProject{}, Layout{}, nil, nil, Destination{}, errors.New("this command works on one destination for now")
	}

	return opened[0].view, opened[0].layout, opened[0].runner, closeAll, opened[0].destination, nil
}

// openServiceFor opens the destination a service runs on: the one the config
// places it on, or for a service the config dropped, the one whose state still
// records it.
func openServiceFor(repositoryPath string, options DeployOptions, serviceName string) (ResolvedProject, Layout, Runner, func(), error) {
	resolved, opened, closeAll, err := openServiceDestinations(repositoryPath, options)
	if err != nil {
		return ResolvedProject{}, Layout{}, nil, nil, err
	}
	placed := resolved.nodeDestination(serviceName)
	if placed == "" && len(opened) == 1 {
		// the one destination says what it does have
		return opened[0].view, opened[0].layout, opened[0].runner, closeAll, nil
	}
	for _, candidate := range opened {
		if placed != "" {
			if candidate.name == placed {
				return candidate.view, candidate.layout, candidate.runner, closeAll, nil
			}
			continue
		}
		if state, _, err := ReadServiceState(candidate.runner, candidate.layout); err == nil && state.Services[serviceName].Current != nil {
			return candidate.view, candidate.layout, candidate.runner, closeAll, nil
		}
	}
	closeAll()

	return ResolvedProject{}, Layout{}, nil, nil, fmt.Errorf("no service called %q on any destination. this project has %s", serviceName, strings.Join(ServiceNames(resolved.Services), ", "))
}

// inspectedServiceState reads version 2 state for the inspection commands. A version 1
// deployment that was never adopted is described, not guessed at.
func inspectedServiceState(runner Runner, resolved ResolvedProject, layout Layout) (ServiceState, error) {
	state, found, err := ReadServiceState(runner, layout)
	if err != nil {
		return ServiceState{}, err
	}
	if !found {
		legacy, err := checkEnvironment(runner, resolved, layout)
		if err != nil {
			return ServiceState{}, err
		}
		if legacy.Current != "" {
			return ServiceState{}, fmt.Errorf("%s holds a version 1 deployment of %s; run deploy adopt to record it per service first", layout.Root, legacy.Current)
		}
	}

	return state, nil
}

// locateRecordedService finds a service through its recorded release, so a
// service that was dropped from the config but still runs can be reached too.
func locateRecordedService(resolved ResolvedProject, layout Layout, state ServiceState, name string) (ServiceLocation, error) {
	if _, task := resolved.Release[name]; task {
		return ServiceLocation{}, fmt.Errorf("%q is a release task, which runs to completion and is gone. it is not something to attach to", name)
	}
	record, recorded := state.Services[name]
	if !recorded || record.Current == nil {
		if _, configured := resolved.Services[name]; configured {
			return ServiceLocation{}, fmt.Errorf("%s has never been deployed to this destination, so nothing is running", name)
		}
		known := slices.Sorted(maps.Keys(state.Services))
		for configured := range resolved.Services {
			if !slices.Contains(known, configured) {
				known = append(known, configured)
			}
		}
		slices.Sort(known)
		return ServiceLocation{}, fmt.Errorf("no service called %q. this project has %s", name, strings.Join(known, ", "))
	}

	return ServiceLocation{ProjectName: record.Current.Project, ComposeFile: path.Join(layout.Root, record.Current.ComposeFile), Stateful: record.Stateful}, nil
}

func runServiceStatus(runner Runner, resolved ResolvedProject, layout Layout, destination Destination) (int, error) {
	state, err := inspectedServiceState(runner, resolved, layout)
	if err != nil {
		return exitPreconditionNotMet, err
	}
	if len(state.Services) == 0 {
		fmt.Printf("%s has never been deployed to %s\n", resolved.Name, destination)
		return exitOK, nil
	}

	fmt.Printf("%s on %s\n", resolved.Name, destination)
	fmt.Printf("  environment  %s\n", state.Environment)
	fmt.Printf("  recorded     %s, generation %d\n", state.UpdatedAt.Format("2006-01-02 15:04:05 MST"), state.Generation)

	statuses := map[string]map[string]ContainerStatus{}
	fmt.Printf("\n  %-12s %-9s %-9s %s\n", "SERVICE", "RELEASE", "PREVIOUS", "STATUS")
	for _, name := range slices.Sorted(maps.Keys(state.Services)) {
		record := state.Services[name]
		if record.Current == nil {
			continue
		}
		project := record.Current.Project
		if statuses[project] == nil {
			statuses[project] = map[string]ContainerStatus{}
			if listed, err := containerStatuses(runner, project); err == nil {
				for _, status := range listed {
					statuses[project][status.Service] = status
				}
			}
		}
		status := "not running"
		if running, found := statuses[project][name]; found {
			status = running.Status
		}
		previous := "-"
		if record.Previous != nil {
			previous = record.Previous.Commit
		}
		release := record.Current.Commit
		if record.Stateful {
			release = "shared"
		}
		note := ""
		if _, configured := resolved.Services[name]; !configured {
			note = "  (no longer in the config)"
		}
		fmt.Printf("  %-12s %-9s %-9s %s%s\n", name, release, previous, status, note)
	}
	for _, name := range ServiceNames(resolved.Services) {
		if record, found := state.Services[name]; !found || record.Current == nil {
			fmt.Printf("  %-12s %-9s %-9s %s\n", name, "-", "-", "not deployed yet")
		}
	}

	if hosted := HostedServices(resolved.Services); len(hosted) > 0 {
		fmt.Printf("\n  exposed\n")
		for _, name := range hosted {
			host := resolved.Services[name].Host
			fmt.Printf("    %-12s %s -> %s:%d, tunnel %s in the shared stack\n", name, host.Domain, name, host.Port, TunnelServiceName(name))
		}
	}

	return exitOK, nil
}

func runServiceReleases(runner Runner, resolved ResolvedProject, layout Layout, destination Destination) (int, error) {
	state, err := inspectedServiceState(runner, resolved, layout)
	if err != nil {
		return exitPreconditionNotMet, err
	}
	if len(state.Services) == 0 {
		fmt.Printf("%s has never been deployed to %s\n", resolved.Name, destination)
		return exitOK, nil
	}
	onDisk, err := runner.ListDirectory(layout.Releases())
	if err != nil {
		return exitPreconditionNotMet, err
	}

	fmt.Printf("%s on %s, environment %s\n", resolved.Name, destination, state.Environment)
	for _, name := range slices.Sorted(maps.Keys(state.Services)) {
		record := state.Services[name]
		if record.Stateful {
			if record.Current != nil {
				fmt.Printf("\n%s (stateful, shared stack since %s)\n", name, record.Current.Commit)
			}
			continue
		}
		fmt.Printf("\n%s\n", name)
		for _, commit := range record.Releases {
			marker := "  "
			switch {
			case record.Current != nil && commit == record.Current.Commit:
				marker = "* "
			case record.Previous != nil && commit == record.Previous.Commit:
				marker = "- "
			}
			note := ""
			if !slices.Contains(onDisk, commit) {
				note = "  (missing from disk)"
			}
			fmt.Printf("%s%s%s\n", marker, commit, note)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(state.Tasks)) {
		runs := state.Tasks[name]
		last := runs[len(runs)-1]
		fmt.Printf("\n%s (release task), last run %s: %s in operation %s\n", name, last.Commit, last.Outcome, last.Operation)
	}
	fmt.Printf("\n* current, - previous, keeping %d per service\n", resolved.Retention)

	return exitOK, nil
}

// listedProject describes one deployment in a destination for deploy list,
// whichever version recorded it.
func listedProject(runner Runner, destinationPath, id string) (name, environment, release, deployed string, found bool) {
	layout := NewLayout(destinationPath, id)
	if state, recorded, err := ReadServiceState(runner, layout); err == nil && recorded {
		var commits []string
		for _, record := range state.Services {
			if record.Current != nil && !record.Stateful && !slices.Contains(commits, record.Current.Commit) {
				commits = append(commits, record.Current.Commit)
			}
		}
		slices.Sort(commits)
		release = strings.Join(commits, ",")
		if release == "" {
			release = "-"
		}
		return state.Name, state.Environment, release, state.UpdatedAt.Format("2006-01-02 15:04"), true
	}
	state, err := ReadState(runner, layout)
	if err != nil || state.Current == "" {
		return "", "", "", "", false
	}
	deployed = "unknown"
	if !state.UpdatedAt.IsZero() {
		deployed = state.UpdatedAt.Format("2006-01-02 15:04")
	}

	return state.Name, state.Environment, state.Current, deployed, true
}

// runServiceEnvPush places an env file on each destination whose services or
// release tasks list it, and nowhere else, under the locks and one operation.
// A destination that was never deployed is claimed per service here, the way
// version 1 env push claims an environment.
func runServiceEnvPush(repositoryPath string, options DeployOptions, localPath string, contents []byte) (exitCode int, err error) {
	if options.Destination != "" {
		return exitPreconditionNotMet, errors.New("version 2 takes its destinations from the config; --destination applies to version 1")
	}
	if options.ForceUnlock {
		return exitPreconditionNotMet, errors.New("--force-unlock is for legacy version 1 lock metadata and does not apply to version 2")
	}
	resolved, destinations, err := loadTopology(repositoryPath, options.Environment)
	if err != nil {
		return exitPreconditionNotMet, err
	}
	file := path.Base(localPath)
	needing := envFileDestinations(resolved, file)
	if len(needing) == 0 {
		return exitPreconditionNotMet, fmt.Errorf("no service or release task in %s lists %s, so it has nowhere to go", resolved.Environment, file)
	}
	selected := map[string]Destination{}
	for _, name := range needing {
		selected[name] = destinations[name]
	}

	ctx := context.Background()
	observations, err := observeTopology(ctx, &resolved, destinations, observeDestination)
	if err != nil {
		return exitPreconditionNotMet, err
	}
	set, runs, err := lockTopology(ctx, resolved, selected, observations)
	if err != nil {
		return exitPreconditionNotMet, err
	}
	for _, run := range runs {
		if run.state, err = destinationServiceState(run.runner, resolved, run.layout, run.name); err != nil {
			set.Release()
			return exitPreconditionNotMet, fmt.Errorf("destination %s: %w", run.name, err)
		}
	}
	if _, err := set.Begin("env push", file); err != nil {
		set.Release()
		return exitPreconditionNotMet, err
	}
	set.ReleaseOnSignal()
	defer set.Finish(&exitCode)

	for _, run := range runs {
		if run.state.Generation == 0 {
			if err := run.runner.MkdirAll(run.layout.Root); err != nil {
				return exitDeployFailed, err
			}
			if _, err := WriteServiceState(run.runner, run.layout, run.state); err != nil {
				return exitDeployFailed, err
			}
		}
		if err := PushEnvFile(run.runner, run.layout, file, contents); err != nil {
			return exitDeployFailed, fmt.Errorf("destination %s: %w", run.name, err)
		}
		fmt.Printf("pushed %s to %s on %s\n", file, run.layout.EnvDirectory(), run.runner.Describe())
	}

	return exitOK, nil
}

// envFileDestinations names the destinations whose services or release tasks
// list an env file, so a secret only goes where something reads it.
func envFileDestinations(resolved ResolvedProject, file string) []string {
	var names []string
	for _, node := range topologyNodes(resolved) {
		listed := slices.Contains(node.Env, file) || (node.Host != nil && node.Host.TunnelEnv == file)
		if listed && !slices.Contains(names, node.Destination) {
			names = append(names, node.Destination)
		}
	}
	slices.Sort(names)

	return names
}

// repositoryFor finds the repository the way every command does.
func repositoryFor(options DeployOptions) (string, error) {
	startPath := options.Context
	if startPath == "" {
		working, err := os.Getwd()
		if err != nil {
			return "", err
		}
		startPath = working
	}

	return FindRepository(startPath)
}

// isServiceConfig reports whether the repository's config is version 2.
func isServiceConfig(options DeployOptions) (string, bool) {
	repositoryPath, err := repositoryFor(options)
	if err != nil {
		return "", false
	}
	version, err := configVersion(repositoryPath)

	return repositoryPath, err == nil && version == 2
}
