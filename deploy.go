package main

import (
	"fmt"
	"os"
	"path"
	"slices"
	"strings"
)

type DeployOptions struct {
	Context     string
	Destination string
	Environment string
	AllowDirty  bool
	ForceUnlock bool
}

// Layout is where everything for one project lives on the destination. Volumes
// and env sit outside releases because pruning must never be able to reach them.
type Layout struct {
	Root string
}

func NewLayout(destinationPath, projectID string) Layout {
	return Layout{Root: path.Join(destinationPath, projectID)}
}

func (layout Layout) Releases() string {
	return path.Join(layout.Root, "releases")
}

func (layout Layout) Release(commit string) string {
	return path.Join(layout.Releases(), ShortCommit(commit))
}

func RunDeploy(options DeployOptions) (int, error) {
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

	commit, err := ResolveCommit(repositoryPath, "HEAD")
	if err != nil {
		return exitPreconditionNotMet, err
	}

	if !options.AllowDirty {
		dirty, err := IsWorkingTreeDirty(repositoryPath)
		if err != nil {
			return exitPreconditionNotMet, err
		}
		if dirty {
			return exitPreconditionNotMet, fmt.Errorf(
				"the working tree has uncommitted changes, so deploying %s would ship something nobody can reproduce later. commit them, or pass --allow-dirty",
				ShortCommit(commit),
			)
		}
	}

	destinationText := options.Destination
	if destinationText == "" {
		destinationText = resolved.Destination
	}
	destination, err := ParseDestination(destinationText)
	if err != nil {
		return exitPreconditionNotMet, err
	}
	if destination.IsRemote() {
		return exitPreconditionNotMet, fmt.Errorf(
			"destination %s is remote, and ssh transport is not implemented yet. point -D at a local path for now",
			destination,
		)
	}

	runner := LocalRunner{}
	if err := CheckRequirements(runner); err != nil {
		return exitPreconditionNotMet, err
	}

	fmt.Printf("deploying %s %s to %s\n", resolved.Name, ShortCommit(commit), destination)

	layout := NewLayout(destination.Path, resolved.ID)

	lock, err := AcquireLock(runner, layout, commit, options.ForceUnlock)
	if err != nil {
		return exitPreconditionNotMet, err
	}
	defer lock.Release()

	state, err := ReadState(runner, layout)
	if err != nil {
		return exitPreconditionNotMet, err
	}

	releaseDirectory := layout.Release(commit)
	if err := PlaceRelease(runner, repositoryPath, commit, releaseDirectory); err != nil {
		return exitDeployFailed, err
	}
	fmt.Printf("  placed release in %s\n", releaseDirectory)

	if err := buildServices(runner, resolved, repositoryPath, commit); err != nil {
		return exitDeployFailed, err
	}

	composeFile := path.Join(releaseDirectory, composeFileName)
	rendered, err := RenderCompose(resolved, commit)
	if err != nil {
		return exitDeployFailed, err
	}
	if err := runner.WriteFile(composeFile, rendered); err != nil {
		return exitDeployFailed, err
	}

	if err := startStack(runner, resolved, composeFile, commit); err != nil {
		return exitDeployFailed, err
	}
	fmt.Printf("  %s is up\n", ProjectName(resolved.ID, commit))

	// everything past here has already put the new release in service, so a
	// failure is reported and never rolled back
	state = state.RecordRelease(commit, resolved.Environment)
	state, pruneErr := PruneReleases(runner, layout, resolved.ID, state, resolved.Retention)

	if err := WriteState(runner, layout, state); err != nil {
		return exitLiveButNeedsAHuman, fmt.Errorf(
			"%s is deployed and running, but recording it failed, so deploy has lost track of what is current: %w",
			ShortCommit(commit), err,
		)
	}
	if pruneErr != nil {
		return exitLiveButNeedsAHuman, fmt.Errorf(
			"%s is deployed and running, but pruning old releases failed: %w", ShortCommit(commit), pruneErr,
		)
	}

	return exitOK, nil
}

func RunReleases(options DeployOptions) (int, error) {
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
	if destination.IsRemote() {
		return exitPreconditionNotMet, fmt.Errorf(
			"destination %s is remote, and ssh transport is not implemented yet", destination,
		)
	}

	runner := LocalRunner{}
	layout := NewLayout(destination.Path, resolved.ID)

	state, err := ReadState(runner, layout)
	if err != nil {
		return exitPreconditionNotMet, err
	}
	if state.Current == "" {
		fmt.Printf("%s has never been deployed to %s\n", resolved.Name, destination)
		return exitOK, nil
	}

	onDisk, err := runner.ListDirectory(layout.Releases())
	if err != nil {
		return exitPreconditionNotMet, err
	}

	fmt.Printf("%s on %s, environment %s\n", resolved.Name, destination, state.Environment)
	for _, release := range state.Releases {
		marker := "  "
		switch release {
		case state.Current:
			marker = "* "
		case state.Previous:
			marker = "- "
		}

		note := ""
		if !slices.Contains(onDisk, release) {
			note = "  (missing from disk)"
		}
		fmt.Printf("%s%s%s\n", marker, release, note)
	}
	fmt.Printf("\n* current, - previous, keeping %d\n", resolved.Retention)

	return exitOK, nil
}

func loadResolvedConfig(repositoryPath, environmentName string) (ResolvedProject, error) {
	project, err := LoadProject(path.Join(repositoryPath, configFileName))
	if err != nil {
		return ResolvedProject{}, err
	}

	resolved, err := project.ResolveEnvironment(environmentName)
	if err != nil {
		return ResolvedProject{}, err
	}
	if err := resolved.Validate(); err != nil {
		return ResolvedProject{}, fmt.Errorf(
			"%s is not valid:\n  %s", configFileName, strings.ReplaceAll(err.Error(), "\n", "\n  "),
		)
	}

	return resolved, nil
}

// buildServices builds from the git archive rather than the working directory, so
// the image and the release tree cannot disagree about what they contain.
func buildServices(runner Runner, resolved ResolvedProject, repositoryPath, commit string) error {
	for _, name := range ServiceNames(resolved.Services) {
		service := resolved.Services[name]
		if len(service.Build) == 0 {
			continue
		}

		dockerfile, err := dockerfilePath(name, service)
		if err != nil {
			return err
		}

		tag := ImageTag(resolved.ID, name, commit)
		fmt.Printf("  building %s\n", tag)

		build := []string{
			"docker", "build",
			"--file", path.Join(repositoryPath, dockerfile),
			"--tag", tag,
			repositoryPath,
		}
		if err := runner.Stream(build, os.Stdout); err != nil {
			return fmt.Errorf("building %s: %w", name, err)
		}
	}

	return nil
}

func startStack(runner Runner, resolved ResolvedProject, composeFile, commit string) error {
	projectName := ProjectName(resolved.ID, commit)
	up := []string{
		"docker", "compose",
		"--file", composeFile,
		"--project-name", projectName,
		"up", "--detach", "--wait",
	}

	if err := runner.Stream(up, os.Stdout); err != nil {
		// compose exits non-zero with nothing useful on stdout, and by the time
		// anyone runs deploy logs the container is often gone, so dump them here
		dumpFailureLogs(runner, composeFile, projectName)
		tearDown(runner, composeFile, projectName)

		return fmt.Errorf("%s never became healthy, so it was torn down and nothing was cut over", projectName)
	}

	return nil
}

func dumpFailureLogs(runner Runner, composeFile, projectName string) {
	fmt.Fprintf(os.Stderr, "\n--- logs from %s ---\n", projectName)

	logs := []string{
		"docker", "compose",
		"--file", composeFile,
		"--project-name", projectName,
		"logs", "--tail", "50",
	}
	if err := runner.Stream(logs, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "(could not read logs: %v)\n", err)
	}
	fmt.Fprintf(os.Stderr, "--- end logs ---\n\n")
}

func tearDown(runner Runner, composeFile, projectName string) {
	down := []string{
		"docker", "compose",
		"--file", composeFile,
		"--project-name", projectName,
		"down",
	}
	if _, err := runner.Run(down); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not tear down %s: %v\n", projectName, err)
	}
}
