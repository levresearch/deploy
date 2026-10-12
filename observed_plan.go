package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
)

func observePlanningProject(ctx context.Context, project Project, observe func(context.Context, Destination) (DestinationObservation, error)) (map[string]DestinationObservation, error) {
	if project.Version != 2 {
		return nil, errors.New("live observation requires version 2")
	}
	destinations := map[string]Destination{}
	for _, name := range slices.Sorted(maps.Keys(project.Destinations)) {
		if !validEnvironment(name) {
			return nil, fmt.Errorf("destination %q needs a lowercase slug", name)
		}
		destination, err := topologyDestination(project.Destinations[name])
		if err != nil {
			return nil, fmt.Errorf("destination %s: %w", name, err)
		}
		destinations[name] = destination
	}
	if len(destinations) == 0 {
		return nil, errors.New("version 2 requires named destinations")
	}
	observations := map[string]DestinationObservation{}
	for _, name := range slices.Sorted(maps.Keys(destinations)) {
		observed, err := observe(ctx, destinations[name])
		if err != nil {
			return nil, fmt.Errorf("destination %s: %w", name, err)
		}
		if err := checkObservedIdentity(name, observed, observations); err != nil {
			return nil, err
		}
		observations[name] = observed
	}
	return observations, nil
}

func observedHostIDs(observations map[string]DestinationObservation) map[string]string {
	hosts := map[string]string{}
	for name, observation := range observations {
		hosts[name] = observation.HostIdentity
	}
	return hosts
}

func planObservedTopology(project Project, environment, commit string, observations map[string]DestinationObservation) (TopologyPlan, error) {
	if len(observations) != len(project.Destinations) {
		return TopologyPlan{}, errors.New("destination observation set is incomplete")
	}
	for name := range project.Destinations {
		if observations[name].HostIdentity == "" {
			return TopologyPlan{}, fmt.Errorf("destination %s has no observed identity", name)
		}
	}
	hosts := observedHostIDs(observations)
	if err := validateObservedProject(project, hosts); err != nil {
		return TopologyPlan{}, err
	}
	resolved, err := project.ResolveEnvironment(environment)
	if err != nil {
		return TopologyPlan{}, err
	}
	resolved.observedHosts = hosts
	plan, err := planTopology(resolved, commit)
	if err != nil {
		return TopologyPlan{}, err
	}
	plan.Observations = observations
	for index := range plan.Destinations {
		destination := &plan.Destinations[index]
		destination.HostIdentity = observations[destination.Name].HostIdentity
		destination.CanonicalPath = observations[destination.Name].CanonicalPath
	}
	plan.Unverified = slices.DeleteFunc(plan.Unverified, func(item string) bool {
		return item == "host identities and SSH aliases" || item == "canonical destination paths" || item == "OS, architecture, privileges and firewall backend"
	})
	plan.Unverified = append(plan.Unverified, "privileges, systemd and firewall backend", "host facts may change; re-observe under locks before any mutation")
	return plan, nil
}
