package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

const (
	stateFileName = "state.json"
	lockFileName  = "deploy.lock"
)

// commitPattern is the first half of the delete fence. A release directory is
// always named after a commit, so anything that is not one cannot be a path we
// are allowed to remove, and this alone rules out "..", "/", and every other
// traversal trick before any path handling happens.
var commitPattern = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

type State struct {
	Version int `json:"version,omitempty"`
	// Name is here so deploy list can say what a project is without reading a
	// config it has no path to. A destination full of hex ids is unreadable.
	Name        string    `json:"name,omitempty"`
	Current     string    `json:"current"`
	Previous    string    `json:"previous,omitempty"`
	Environment string    `json:"environment"`
	Releases    []string  `json:"releases"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

func (layout Layout) StateFile() string {
	return path.Join(layout.Root, stateFileName)
}

func (layout Layout) LockFile() string {
	return path.Join(layout.Root, lockFileName)
}

// ReadState tolerates a project that has never been deployed, since the first
// deploy has no state to find and that is not an error.
func ReadState(runner Runner, layout Layout) (State, error) {
	raw, err := runner.ReadFile(layout.StateFile())
	if errors.Is(err, fs.ErrNotExist) {
		return State{}, nil
	}
	if err != nil {
		return State{}, fmt.Errorf("reading %s: %w", layout.StateFile(), err)
	}

	var state State
	if err := json.Unmarshal(raw, &state); err != nil {
		return State{}, fmt.Errorf("%s is unreadable: %w", layout.StateFile(), err)
	}
	if state.Version != 0 && state.Version != 1 {
		return State{}, fmt.Errorf("%s uses unsupported state version %d; use a compatible deploy version", layout.StateFile(), state.Version)
	}

	return state, nil
}

func WriteState(runner Runner, layout Layout, state State) error {
	if state.Version != 0 && state.Version != 1 {
		return fmt.Errorf("refusing to write unsupported state version %d", state.Version)
	}
	state.Version = 1
	state.UpdatedAt = time.Now().UTC().Truncate(time.Second)

	encoded, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}

	return writeStateFile(runner, layout.StateFile(), append(encoded, '\n'))
}

// RecordRelease moves a newly deployed commit to the front. Redeploying the
// commit that is already current leaves previous alone, because the release
// before it is still the thing to roll back to.
func (state State) RecordRelease(commit, environmentName string) State {
	commit = ShortCommit(commit)

	if state.Current != commit {
		state.Previous = state.Current
	}
	state.Current = commit
	state.Environment = environmentName

	state.Releases = append(
		[]string{commit},
		slices.DeleteFunc(slices.Clone(state.Releases), func(existing string) bool {
			return existing == commit
		})...,
	)

	return state
}

// ReleasesToPrune is deliberately separate from the removal, so the decision can
// be tested without deleting anything at all.
func (state State) ReleasesToPrune(retention int) []string {
	if retention < 1 {
		retention = 1
	}

	var prunable []string
	for _, release := range state.Releases {
		// current is serving and previous is the rollback target, so neither is
		// ever a candidate no matter what retention says
		if release == state.Current || release == state.Previous {
			continue
		}
		prunable = append(prunable, release)
	}

	keep := retention - 1
	if state.Previous != "" {
		keep = retention - 2
	}
	if keep < 0 {
		keep = 0
	}
	if len(prunable) <= keep {
		return nil
	}

	return prunable[keep:]
}

// releasePathWithin is the delete fence. Every path deploy is about to remove
// goes through here, and it takes the root it is allowed to delete under as a
// parameter rather than reaching for one, which is also what makes it testable
// against a throwaway tree.
//
// NOTE: this looks like the defensive validation CONTRIBUTING bans, and it is
// not. The failure it prevents is deleting something outside the project, so it
// does not get cleaned up later.
func releasePathWithin(releasesRoot, commit string) (string, error) {
	if !commitPattern.MatchString(commit) {
		return "", fmt.Errorf("refusing to remove %q, which is not a commit", commit)
	}

	candidate := path.Join(releasesRoot, commit)

	// NOTE: these two are unreachable while commitPattern stays as strict as it
	// is, since a name of only hex digits cannot contain a slash or a dot and so
	// cannot escape the join. They stay anyway, because they are what still holds
	// if anyone ever loosens that pattern to allow tags or branch names, and the
	// thing on the other side of this fence is somebody's database. No test can
	// reach them, which is why this comment exists rather than a test name.
	if path.Clean(candidate) != candidate {
		return "", fmt.Errorf("refusing to remove %q, which does not resolve to a plain path", candidate)
	}
	if !strings.HasPrefix(candidate, releasesRoot+"/") {
		return "", fmt.Errorf("refusing to remove %q, which is outside %s", candidate, releasesRoot)
	}

	return candidate, nil
}

// resolvedWithinRoot follows symlinks before deleting, because a release
// directory replaced with a link pointing elsewhere would otherwise carry the
// fence's blessing to whatever it points at.
func resolvedWithinRoot(releasesRoot, target string) error {
	realRoot, err := filepath.EvalSymlinks(releasesRoot)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("resolving %s: %w", releasesRoot, err)
	}

	realTarget, err := filepath.EvalSymlinks(target)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("resolving %s: %w", target, err)
	}

	if !strings.HasPrefix(realTarget, realRoot+string(filepath.Separator)) {
		return fmt.Errorf(
			"refusing to remove %s, which resolves to %s, outside %s", target, realTarget, realRoot,
		)
	}

	return nil
}

// PruneReleases removes old release directories and the images built for them.
// It only ever removes things under releases/, so volumes/ and env/ are not
// reachable from here even if the state file is wrong.
func PruneReleases(runner Runner, layout Layout, projectID string, state State, retention int) (State, error) {
	for _, commit := range state.ReleasesToPrune(retention) {
		directory, err := releasePathWithin(layout.Releases(), commit)
		if err != nil {
			return state, err
		}
		if err := resolvedWithinRoot(layout.Releases(), directory); err != nil {
			return state, err
		}

		// the containers have to go before the directory does, since the compose
		// file that names them lives inside it, and removing the directory first
		// would strand them running with no way left to address them
		stopRelease(runner, projectID, commit, path.Join(directory, composeFileName))

		if err := runner.RemoveAll(directory); err != nil {
			return state, fmt.Errorf("pruning %s: %w", directory, err)
		}

		pruneImages(runner, projectID, commit)

		state.Releases = slices.DeleteFunc(state.Releases, func(existing string) bool {
			return existing == commit
		})
		fmt.Printf("  pruned release %s\n", commit)
	}

	return state, nil
}

// stopRelease tears down a release's containers. Best effort, since a stack that
// is already gone is the normal case, and the images cannot be removed while
// anything still holds them.
func stopRelease(runner Runner, projectID, commit, composeFile string) {
	down := []string{
		"docker", "compose",
		"--file", composeFile,
		"--project-name", ProjectName(projectID, commit),
		"down",
	}
	if _, err := runner.Run(down); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not stop the containers for release %s: %v\n", commit, err)
	}
}

// pruneImages is best effort on purpose. A left over image wastes disk, and
// failing a deploy that already succeeded over one would be worse.
func pruneImages(runner Runner, projectID, commit string) {
	reference := fmt.Sprintf("deploy-%s/*:%s", projectID, commit)

	listed, err := runner.Run([]string{"docker", "images", "--filter", "reference=" + reference, "--format", "{{.Repository}}:{{.Tag}}"})
	if err != nil {
		return
	}

	for _, tag := range strings.Fields(string(listed)) {
		if _, err := runner.Run([]string{"docker", "image", "rm", tag}); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not remove image %s: %v\n", tag, err)
		}
	}
}
