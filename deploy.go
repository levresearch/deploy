package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"slices"
	"strings"
)

type DeployOptions struct {
	Context     string
	GitStorage  string
	Destination string
	Environment string
	AllowDirty  bool
	ForceUnlock bool
	BuildOnDest bool
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

// EnvDirectory holds the env files, which cannot ride along with the code
// because git archive ships tracked files only and a real .env is gitignored.
// Outside releases/, so pruning can never reach a secret.
func (layout Layout) EnvDirectory() string {
	return path.Join(layout.Root, "env")
}

// SharedComposeFile sits beside releases rather than inside one, since the stack
// it describes outlives every release and must never be pruned with one.
func (layout Layout) SharedComposeFile() string {
	return path.Join(layout.Root, "shared", composeFileName)
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

	// a project with no config gets one here rather than from a separate command,
	// so deploy is the only thing anyone has to run
	created, err := EnsureProjectConfig(repositoryPath, options.Destination)
	if err != nil {
		return exitPreconditionNotMet, err
	}

	resolved, err := loadResolvedConfig(repositoryPath, options.Environment)
	if err != nil {
		if created {
			describeFreshConfig(freshProjectID(repositoryPath))
		}

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

	gitStorage, err := resolveGitStorage(options.GitStorage, resolved.GitStorage)
	if err != nil {
		return exitPreconditionNotMet, err
	}

	if err := CheckLocalRequirements(LocalRunner{}, destination); err != nil {
		return exitPreconditionNotMet, err
	}

	runner, closeRunner, err := OpenRunner(destination)
	if err != nil {
		return exitPreconditionNotMet, err
	}
	defer closeRunner()

	layout := NewLayout(destination.Path, resolved.ID)

	facts, err := CheckDestination(runner, resolved, layout)
	if err != nil {
		return exitPreconditionNotMet, err
	}

	// the cross build toolchain is a precondition like any other, so a missing
	// buildx fails here rather than after a release has been placed
	builder, err := NewBuilder(runner, facts, resolved.ID, repositoryPath, destination.IsRemote(), options.BuildOnDest)
	if err != nil {
		return exitPreconditionNotMet, err
	}

	fmt.Printf(
		"deploying %s %s to %s (%s)\n",
		resolved.Name, ShortCommit(commit), destination, builder.Describe(),
	)

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
	if err := PlaceRelease(
		runner, repositoryPath, gitStorage, destination, commit, releaseDirectory,
	); err != nil {
		return exitDeployFailed, err
	}
	if gitStorage.Path != "" && SameHost(gitStorage, destination) {
		fmt.Printf("  placed release in %s, straight from %s\n", releaseDirectory, gitStorage.Path)
	} else {
		fmt.Printf("  placed release in %s\n", releaseDirectory)
	}

	builder.SetReleaseDirectory(releaseDirectory)
	if err := buildServices(builder, resolved, repositoryPath, commit); err != nil {
		return exitDeployFailed, err
	}

	if err := startShared(runner, layout, resolved); err != nil {
		return exitDeployFailed, err
	}

	if err := runReleaseTasks(runner, resolved, layout, releaseDirectory, commit); err != nil {
		return exitDeployFailed, err
	}

	composeFile := path.Join(releaseDirectory, composeFileName)
	rendered, err := RenderRelease(resolved, layout, commit)
	if err != nil {
		return exitDeployFailed, err
	}
	if err := runner.WriteFile(composeFile, rendered); err != nil {
		return exitDeployFailed, err
	}

	if err := startStack(runner, composeFile, ProjectName(resolved.ID, commit)); err != nil {
		return exitDeployFailed, err
	}
	fmt.Printf("  %s is up\n", ProjectName(resolved.ID, commit))

	retireSupersededRelease(runner, layout, resolved, state.Current, commit)

	// everything past here has already put the new release in service, so a
	// failure is reported and never rolled back
	state = state.RecordRelease(commit, resolved.Environment)
	state.Name = resolved.Name
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
	runner, closeRunner, err := OpenRunner(destination)
	if err != nil {
		return exitPreconditionNotMet, err
	}
	defer closeRunner()

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

// RunEnvPush places one env file on the destination. Separate from deploy because
// a secret is not something to re-upload on every release, and because it is the
// answer preflight points at when one is missing.
func RunEnvPush(options DeployOptions, localPath string) (int, error) {
	contents, err := os.ReadFile(localPath)
	if err != nil {
		return exitPreconditionNotMet, err
	}

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
	name := path.Base(localPath)

	if err := PushEnvFile(runner, layout, name, contents); err != nil {
		return exitDeployFailed, err
	}
	fmt.Printf("pushed %s to %s on %s\n", name, layout.EnvDirectory(), runner.Describe())

	return exitOK, nil
}

// freshProjectID reads back what was just written, so the message names the real
// id rather than one generated twice.
func freshProjectID(repositoryPath string) string {
	project, err := LoadProject(path.Join(repositoryPath, configFileName))
	if err != nil {
		return "(unreadable)"
	}

	return project.ID
}

// resolveGitStorage is optional, so an unset one is not an error. It only ever
// unlocks the faster path.
func resolveGitStorage(fromFlag, fromConfig string) (Destination, error) {
	raw := fromFlag
	if raw == "" {
		raw = fromConfig
	}
	if raw == "" {
		return Destination{}, nil
	}

	return ParseDestination(raw)
}

func loadResolvedConfig(repositoryPath, environmentName string) (ResolvedProject, error) {
	configPath := path.Join(repositoryPath, configFileName)

	project, err := LoadProject(configPath)
	if errors.Is(err, fs.ErrNotExist) {
		return ResolvedProject{}, fmt.Errorf(
			"no %s in %s. run deploy there and it will write one", configFileName, repositoryPath,
		)
	}
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

// buildServices builds everything with a build key. Release tasks are included,
// since a migration usually ships in the same image as the app it migrates.
func buildServices(builder *Builder, resolved ResolvedProject, repositoryPath, commit string) error {
	buildable := map[string]Service{}
	maps.Copy(buildable, resolved.Services)
	maps.Copy(buildable, resolved.Release)

	for _, name := range ServiceNames(buildable) {
		service := buildable[name]
		if len(service.Build) == 0 {
			continue
		}

		dockerfile, extraContext, err := buildPlan(name, service)
		if err != nil {
			return err
		}
		if err := builder.Build(repositoryPath, commit, name, dockerfile, extraContext); err != nil {
			return err
		}
	}

	return nil
}

// retireSupersededRelease stops the release the new one just replaced, which only
// happens after the new stack is up and --wait has already confirmed every
// healthcheck passed. Leaving it running would pile up a stack per deploy, which
// on a small box is real memory.
//
// NOTE: a project with a host block keeps both stacks up, because stopping the
// old one before the tunnel has been pointed at the new one is precisely the
// outage the cutover exists to avoid. That swap is not implemented yet, so this
// deliberately does less rather than something unsafe.
func retireSupersededRelease(
	runner Runner,
	layout Layout,
	resolved ResolvedProject,
	superseded, commit string,
) {
	if superseded == "" || superseded == ShortCommit(commit) {
		return
	}

	if hosted := HostedServices(resolved.Services); len(hosted) > 0 {
		fmt.Printf(
			"  leaving %s running, since %s is exposed and the tunnel cutover is not implemented yet\n",
			ProjectName(resolved.ID, superseded), strings.Join(hosted, ", "),
		)

		return
	}

	stopRelease(runner, resolved.ID, superseded, path.Join(layout.Release(superseded), composeFileName))
	fmt.Printf("  stopped %s, which this release replaces\n", ProjectName(resolved.ID, superseded))
}

// startShared brings up the stack that outlives every release. It is rendered
// every deploy and compared with what is already there, because "bring it up if
// it is not running" would mean a changed postgres image, a new volume, or an
// edited command silently never taking effect.
func startShared(runner Runner, layout Layout, resolved ResolvedProject) error {
	network := NetworkName(resolved.ID)
	if output, err := runner.Run([]string{"docker", "network", "create", network}); err != nil {
		// a network that already exists is the normal case on every deploy after
		// the first. anything else is a real failure and swallowing it would turn
		// a permissions problem into a confusing one about a service that cannot
		// reach its database
		if !strings.Contains(string(output), "already exists") {
			return fmt.Errorf(
				"creating network %s on %s: %s", network, runner.Describe(), firstLine(output),
			)
		}
	}
	for _, volume := range VolumesFor(resolved) {
		if _, err := runner.Run([]string{"docker", "volume", "create", volume}); err != nil {
			return fmt.Errorf("creating volume %s: %w", volume, err)
		}
	}

	stateful, _ := SplitServices(resolved.Services)
	if len(stateful) == 0 {
		return nil
	}

	rendered, err := RenderShared(resolved, layout)
	if err != nil {
		return err
	}

	sharedFile := layout.SharedComposeFile()
	previous, _ := runner.ReadFile(sharedFile)

	if len(previous) > 0 && !bytes.Equal(previous, rendered) {
		fmt.Printf(
			"  shared stack changed, so %s will be recreated. stateful services take a real outage here\n",
			strings.Join(ServiceNames(stateful), ", "),
		)
	}

	if err := runner.MkdirAll(path.Dir(sharedFile)); err != nil {
		return err
	}
	if err := runner.WriteFile(sharedFile, rendered); err != nil {
		return err
	}

	if err := startStack(runner, sharedFile, SharedProjectName(resolved.ID)); err != nil {
		return err
	}
	fmt.Printf("  shared stack up (%s)\n", strings.Join(ServiceNames(stateful), ", "))

	return nil
}

// runReleaseTasks runs the one-shot work, a migration being the obvious one,
// against the shared stack that is already up. It happens before the new stack
// starts, so a failure here means the old release is still serving and nothing
// about the deploy has been applied.
func runReleaseTasks(runner Runner, resolved ResolvedProject, layout Layout, releaseDirectory, commit string) error {
	if len(resolved.Release) == 0 {
		return nil
	}

	rendered, err := RenderReleaseTasks(resolved, layout, commit)
	if err != nil {
		return err
	}

	tasksFile := path.Join(releaseDirectory, releaseTasksFileName)
	if err := runner.WriteFile(tasksFile, rendered); err != nil {
		return err
	}

	for _, name := range ServiceNames(resolved.Release) {
		fmt.Printf("  running release task %s\n", name)

		// --no-deps because the shared stack is already up and healthy, and
		// anything this task depends on is in it rather than in this file
		run := []string{
			"docker", "compose",
			"--file", tasksFile,
			"--project-name", ReleaseTasksProjectName(resolved.ID, commit),
			"run", "--rm", "--no-deps", name,
		}

		// streamed rather than captured, since a migration log is exactly what
		// you want to read when one fails
		if err := runner.Stream(run, os.Stdout); err != nil {
			return fmt.Errorf(
				"release task %s failed, so the deploy stopped before starting anything new and the previous release is still serving: %w",
				name, err,
			)
		}
	}

	return nil
}

func startStack(runner Runner, composeFile, projectName string) error {
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
