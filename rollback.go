package main

import (
	"fmt"
	"os"
	"path"
	"slices"
	"strings"
)

// RollbackTarget works out which release to activate and refuses anything that is
// not actually there to activate. Separate from the rollback itself so the
// decision can be tested without starting a container.
func RollbackTarget(state State, requested string, onDisk []string) (string, error) {
	if state.Current == "" {
		return "", fmt.Errorf("this project has never been deployed, so there is nothing to roll back to")
	}

	target := ShortCommit(requested)
	if target == "" {
		target = state.Previous
	}
	if target == "" {
		return "", fmt.Errorf(
			"%s is the only release deploy knows about, so there is nothing to roll back to",
			state.Current,
		)
	}

	if target == state.Current {
		return "", fmt.Errorf("%s is already what is running", target)
	}

	// a release deploy has forgotten and one it pruned are the same problem to
	// whoever typed it, so they get the same answer
	if !slices.Contains(state.Releases, target) || !slices.Contains(onDisk, target) {
		return "", fmt.Errorf(
			"%s is not available to roll back to. these still are: %s",
			target, strings.Join(availableReleases(state, onDisk), ", "),
		)
	}

	return target, nil
}

func availableReleases(state State, onDisk []string) []string {
	var available []string
	for _, release := range state.Releases {
		if release != state.Current && slices.Contains(onDisk, release) {
			available = append(available, release)
		}
	}
	if len(available) == 0 {
		return []string{"none"}
	}

	return available
}

// RecordRollback swaps current and the release being activated, so rolling back
// again returns to where you were. The releases list is deploy history and is
// deliberately left alone, since rolling back is not a deploy.
func (state State) RecordRollback(target string) State {
	state.Previous = state.Current
	state.Current = target

	return state
}

func RunRollback(options DeployOptions, requested string) (int, error) {
	startPath := options.Context
	if startPath == "" {
		working, err := os.Getwd()
		if err != nil {
			return exitPreconditionNotMet, err
		}
		startPath = working
	}

	repositoryPath, err := FindRepository(startPath)
	if err != nil {
		return exitPreconditionNotMet, err
	}

	resolved, err := loadResolvedConfig(repositoryPath, options.Environment)
	if err != nil {
		return exitPreconditionNotMet, err
	}

	destinationText := options.Destination
	if destinationText == "" {
		destinationText = resolved.Destination
	}
	destination, err := ParseDestination(destinationText)
	if err != nil {
		return exitPreconditionNotMet, err
	}

	runner, closeRunner, err := OpenRunner(destination)
	if err != nil {
		return exitPreconditionNotMet, err
	}
	defer closeRunner()

	layout := NewLayout(destination.Path, resolved.ID)

	lock, err := AcquireLock(runner, layout, requested, options.ForceUnlock)
	if err != nil {
		return exitPreconditionNotMet, err
	}
	defer lock.Release()

	state, err := ReadState(runner, layout)
	if err != nil {
		return exitPreconditionNotMet, err
	}

	onDisk, err := runner.ListDirectory(layout.Releases())
	if err != nil {
		return exitPreconditionNotMet, err
	}

	target, err := RollbackTarget(state, requested, onDisk)
	if err != nil {
		return exitPreconditionNotMet, err
	}

	leaving := state.Current
	fmt.Printf("rolling %s back from %s to %s on %s\n", resolved.Name, leaving, target, destination)

	// nothing is built and nothing is transferred, because the release and its
	// images are already sitting there. that is what makes this the fastest thing
	// deploy does
	targetCompose := path.Join(layout.Release(target), composeFileName)
	if err := startStack(runner, targetCompose, ProjectName(resolved.ID, target)); err != nil {
		return exitDeployFailed, err
	}
	fmt.Printf("  %s is up\n", ProjectName(resolved.ID, target))

	// the release being left is only stopped once the older one is proven healthy,
	// so a rollback that cannot start leaves you no worse off
	stopRelease(runner, resolved.ID, leaving, path.Join(layout.Release(leaving), composeFileName))

	state = state.RecordRollback(target)
	if err := WriteState(runner, layout, state); err != nil {
		return exitLiveButNeedsAHuman, fmt.Errorf(
			"%s is running, but recording the rollback failed, so deploy has lost track of what is current: %w",
			target, err,
		)
	}

	fmt.Printf("  rolled back to %s, and %s is now what a further rollback would return to\n", target, leaving)
	fmt.Fprintf(os.Stderr,
		"\nNOTE: this moved containers, not data. if %s ran a migration, %s is now running against the newer schema.\n",
		leaving, target,
	)

	return exitOK, nil
}
