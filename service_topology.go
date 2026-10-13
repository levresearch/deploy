package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"
)

// on is one destination's view of the project: only the services and release
// tasks placed there, so building, env files, the shared stack and tunnels
// stay on that destination. Connections are still resolved against the whole
// project through topology.
func (resolved ResolvedProject) on(destination string) ResolvedProject {
	whole := resolved.topology()
	view := whole
	view.whole = &whole
	view.Services, view.Release = map[string]Service{}, map[string]Service{}
	for name, service := range whole.Services {
		if service.Destination == destination {
			view.Services[name] = service
		}
	}
	for name, task := range whole.Release {
		if task.Destination == destination {
			view.Release[name] = task
		}
	}

	return view
}

// topology is the whole project, whether resolved is a destination's view or
// already the whole.
func (resolved ResolvedProject) topology() ResolvedProject {
	if resolved.whole != nil {
		return *resolved.whole
	}

	return resolved
}

// loadTopology loads a version 2 config for the commands that change every
// destination it names, and refuses what deploy cannot run yet.
func loadTopology(repositoryPath, environmentName string) (ResolvedProject, map[string]Destination, error) {
	project, err := loadPlanningProject(path.Join(repositoryPath, configFileName))
	if err != nil {
		return ResolvedProject{}, nil, err
	}
	if project.Version != 2 {
		return ResolvedProject{}, nil, errors.New("this command needs a version 2 config")
	}
	if environmentName == "" {
		environmentName = defaultEnvironmentName
	}
	resolved, err := project.ResolveEnvironment(environmentName)
	if err != nil {
		return ResolvedProject{}, nil, err
	}
	if err := resolved.Validate(); err != nil {
		return ResolvedProject{}, nil, fmt.Errorf("%s is not valid:\n  %s", configFileName, strings.ReplaceAll(err.Error(), "\n", "\n  "))
	}

	var problems []error
	for _, name := range ServiceNames(resolved.Services) {
		service := resolved.Services[name]
		if !service.IsStateful() {
			continue
		}
		// each destination's shared stack is up before any release, so it can
		// wait only on what that same stack holds
		for dependency := range service.DependsOn {
			if dependsOn, found := resolved.Services[dependency]; !found || !dependsOn.IsStateful() || dependsOn.Destination != service.Destination {
				problems = append(problems, fmt.Errorf("stateful %s cannot depend on %s, which is not a stateful service on the same destination", name, dependency))
			}
		}
	}
	if err := errors.Join(problems...); err != nil {
		return ResolvedProject{}, nil, err
	}

	destinations := map[string]Destination{}
	for _, name := range slices.Sorted(maps.Keys(resolved.Destinations)) {
		destination, err := topologyDestination(resolved.Destinations[name])
		if err != nil {
			return ResolvedProject{}, nil, fmt.Errorf("destination %s: %w", name, err)
		}
		destinations[name] = destination
	}

	return resolved, destinations, nil
}

// destinationRun is one destination of a command that changes several: the
// connection holding its lock, its layout, its own recorded state and its
// view of the project.
type destinationRun struct {
	name        string
	destination Destination
	view        ResolvedProject
	runner      Runner
	layout      Layout
	state       ServiceState
}

// observeTopology observes every destination, refuses two of them on one
// host, and records the host identities so connections are classified by
// the machines they actually run on.
func observeTopology(ctx context.Context, resolved *ResolvedProject, destinations map[string]Destination, observe func(context.Context, Destination) (DestinationObservation, error)) (map[string]DestinationObservation, error) {
	observations := map[string]DestinationObservation{}
	byHost := map[string]string{}
	for _, name := range slices.Sorted(maps.Keys(destinations)) {
		observed, err := observe(ctx, destinations[name])
		if err != nil {
			return nil, fmt.Errorf("destination %s: %w", name, err)
		}
		if err := checkObservedIdentity(name, observed, observations); err != nil {
			return nil, err
		}
		if other, taken := byHost[observed.HostIdentity]; taken {
			return nil, fmt.Errorf("destinations %s and %s are on the same host; deploy runs one destination per host, so place their services under one destination", other, name)
		}
		byHost[observed.HostIdentity] = name
		observations[name] = observed
	}
	resolved.observedHosts = observedHostIDs(observations)
	// classifying connections by observed hosts can change what is valid
	if err := resolved.Validate(); err != nil {
		return nil, fmt.Errorf("%s is not valid on the observed hosts:\n  %s", configFileName, strings.ReplaceAll(err.Error(), "\n", "\n  "))
	}

	return observations, nil
}

// lockTopology takes every destination's lock in the one global order and
// returns a run per destination in that order, each with the state it holds.
func lockTopology(ctx context.Context, resolved ResolvedProject, destinations map[string]Destination, observations map[string]DestinationObservation) (*LockSet, []*destinationRun, error) {
	generations := map[string]int{}
	for name, destination := range destinations {
		layout := NewLayout(observations[name].CanonicalPath, resolved.RuntimeID())
		reader := stateReader(observedReader{ctx: ctx, destination: destination})
		if !destination.IsRemote() {
			reader = LocalRunner{}
		}
		before, _, err := ReadServiceState(reader, layout)
		if err != nil {
			return nil, nil, fmt.Errorf("destination %s: %w", name, err)
		}
		generations[name] = before.Generation
	}
	targets, err := lockOrder(destinations, observations)
	if err != nil {
		return nil, nil, err
	}
	set, err := acquireLockSet(ctx, targets, lockSetOptions{
		runtimeID: resolved.RuntimeID(), generations: generations, open: OpenRunner, observe: observeDestination,
	})
	if err != nil {
		return nil, nil, err
	}

	runs := make([]*destinationRun, 0, len(targets))
	for index, target := range targets {
		layout := NewLayout(target.Observed.CanonicalPath, resolved.RuntimeID())
		destination := target.Destination
		destination.Path = path.Dir(layout.Root)
		runs = append(runs, &destinationRun{
			name: target.Name, destination: destination, view: resolved.on(target.Name),
			runner: set.Runner(index), layout: layout,
		})
	}

	return set, runs, nil
}
