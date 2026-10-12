package main

import (
	"fmt"
	"path"
	"regexp"
	"strings"
)

var environmentPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$`)

func validEnvironment(name string) bool {
	return len(name) <= 32 && environmentPattern.MatchString(name)
}

func (resolved ResolvedProject) RuntimeID() string {
	if resolved.Environment == "" || resolved.Environment == defaultEnvironmentName {
		return resolved.ID
	}
	return resolved.ID + "-" + resolved.Environment
}

func validRuntimeID(id string) bool {
	base, environment, suffixed := strings.Cut(id, "-")
	return projectIDPattern.MatchString(base) && (!suffixed || (environment != defaultEnvironmentName && validEnvironment(environment)))
}

// legacy staging data must never be adopted as production or silently relocated.
func checkEnvironment(runner stateReader, resolved ResolvedProject, layout Layout) (State, error) {
	if resolved.RuntimeID() != resolved.ID {
		legacy, err := readEnvironmentState(runner, NewLayout(path.Dir(layout.Root), resolved.ID))
		if err != nil {
			return State{}, err
		}
		if legacy.Environment == resolved.Environment {
			return State{}, fmt.Errorf("legacy %s deployment uses %s; transition its data explicitly before using the environment namespace", resolved.Environment, resolved.ID)
		}
	}
	state, err := readEnvironmentState(runner, layout)
	if err != nil {
		return state, err
	}
	if state.Environment != "" && state.Environment != resolved.Environment {
		return state, fmt.Errorf("deployment belongs to %s, not %s; refusing to change its resources", state.Environment, resolved.Environment)
	}
	return state, nil
}

func readEnvironmentState(runner stateReader, layout Layout) (State, error) {
	state, err := ReadState(runner, layout)
	if err != nil {
		return state, err
	}
	if state.Environment == "" {
		for _, directory := range []string{layout.Releases(), path.Dir(layout.SharedComposeFile()), layout.EnvDirectory()} {
			entries, err := runner.ListDirectory(directory)
			if err != nil {
				return state, err
			}
			if len(entries) > 0 || state.Current != "" {
				return state, fmt.Errorf("deployment environment is unknown at %s; resolve its ownership before proceeding", layout.Root)
			}
		}
	}
	return state, nil
}

// record ownership before the first secrets upload or partial deployment.
func claimEnvironment(runner Runner, resolved ResolvedProject, layout Layout) (State, error) {
	state, err := checkEnvironment(runner, resolved, layout)
	if err != nil {
		return state, err
	}
	if state.Environment == "" {
		state.Environment = resolved.Environment
		state.Name = resolved.Name
		err = WriteState(runner, layout, state)
	}
	return state, err
}
