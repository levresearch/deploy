package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path"
	"slices"
	"strings"
	"sync"
	"time"
)

// configVersion peeks at the version so deploy can choose an executor before
// it loads anything else.
func configVersion(repositoryPath string) (int, error) {
	raw, err := os.ReadFile(path.Join(repositoryPath, configFileName))
	if err != nil {
		return 0, err
	}
	var version struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(raw, &version); err != nil {
		return 0, fmt.Errorf("%s is unreadable: %w", configFileName, err)
	}

	return version.Version, nil
}

// loadServiceConfig loads a version 2 config and refuses what one destination
// cannot run yet.
func loadServiceConfig(repositoryPath, environmentName string) (ResolvedProject, string, Destination, error) {
	project, err := loadPlanningProject(path.Join(repositoryPath, configFileName))
	if err != nil {
		return ResolvedProject{}, "", Destination{}, err
	}
	if project.Version != 2 {
		return ResolvedProject{}, "", Destination{}, errors.New("this command needs a version 2 config")
	}
	if environmentName == "" {
		environmentName = defaultEnvironmentName
	}
	resolved, err := project.ResolveEnvironment(environmentName)
	if err != nil {
		return ResolvedProject{}, "", Destination{}, err
	}
	if err := resolved.Validate(); err != nil {
		return ResolvedProject{}, "", Destination{}, fmt.Errorf("%s is not valid:\n  %s", configFileName, strings.ReplaceAll(err.Error(), "\n", "\n  "))
	}
	if len(resolved.Destinations) != 1 {
		return ResolvedProject{}, "", Destination{}, errors.New("version 2 deploys one destination for now; deployment across several destinations arrives with managed networking")
	}

	var problems []error
	for _, name := range ServiceNames(resolved.Services) {
		service := resolved.Services[name]
		for _, endpoint := range slices.Sorted(maps.Keys(service.Endpoints)) {
			if service.Endpoints[endpoint].HostPort != 0 {
				problems = append(problems, fmt.Errorf("%s endpoint %s publishes a host port, which needs managed networking that is not available yet", name, endpoint))
			}
		}
		if !service.IsStateful() {
			continue
		}
		// the shared stack is up before any release, so it cannot wait on one
		for dependency := range service.DependsOn {
			if dependsOn, found := resolved.Services[dependency]; !found || !dependsOn.IsStateful() {
				problems = append(problems, fmt.Errorf("stateful %s cannot depend on %s, which starts after it", name, dependency))
			}
		}
	}
	if err := errors.Join(problems...); err != nil {
		return ResolvedProject{}, "", Destination{}, err
	}

	name := slices.Collect(maps.Keys(resolved.Destinations))[0]
	destination, err := topologyDestination(resolved.Destinations[name])
	if err != nil {
		return ResolvedProject{}, "", Destination{}, err
	}

	return resolved, name, destination, nil
}

// nodeDigest identifies one service's or task's configuration, so a recorded
// release says what it was deployed with and not just from which commit.
func nodeDigest(service Service) string {
	encoded, _ := json.Marshal(service)
	digest := sha256.Sum256(encoded)

	return hex.EncodeToString(digest[:])
}

// lockServiceDestination observes the destination, takes its lock and checks
// it is still what was observed.
func lockServiceDestination(resolved ResolvedProject, name string, destination Destination) (*LockSet, Layout, error) {
	ctx := context.Background()
	observed, err := observeDestination(ctx, destination)
	if err != nil {
		return nil, Layout{}, fmt.Errorf("destination %s: %w", name, err)
	}
	layout := NewLayout(observed.CanonicalPath, resolved.RuntimeID())
	reader := stateReader(observedReader{ctx: ctx, destination: destination})
	if !destination.IsRemote() {
		reader = LocalRunner{}
	}
	before, _, err := ReadServiceState(reader, layout)
	if err != nil {
		return nil, Layout{}, err
	}

	targets, err := lockOrder(map[string]Destination{name: destination}, map[string]DestinationObservation{name: observed})
	if err != nil {
		return nil, Layout{}, err
	}
	set, err := acquireLockSet(ctx, targets, lockSetOptions{
		runtimeID:   resolved.RuntimeID(),
		generations: map[string]int{name: before.Generation},
		open:        OpenRunner,
		observe:     observeDestination,
	})
	if err != nil {
		return nil, Layout{}, err
	}

	return set, layout, nil
}

// destinationServiceState reads the recorded state under the lock. A version 1
// deployment must be adopted first, so its releases are never mistaken for
// nothing and started over.
func destinationServiceState(runner Runner, resolved ResolvedProject, layout Layout, name string) (ServiceState, error) {
	state, found, err := ReadServiceState(runner, layout)
	if err != nil {
		return ServiceState{}, err
	}
	if found {
		if state.Project != resolved.RuntimeID() || state.Environment != resolved.Environment || state.Destination != name {
			return ServiceState{}, fmt.Errorf("%s belongs to %s %s at destination %s; refusing to change it", layout.ServiceStateFile(), state.Project, state.Environment, state.Destination)
		}
		return state, nil
	}

	legacy, err := checkEnvironment(runner, resolved, layout)
	if err != nil {
		return ServiceState{}, err
	}
	if legacy.Current != "" {
		return ServiceState{}, fmt.Errorf("%s holds a version 1 deployment of %s; run deploy adopt to record it per service first", layout.Root, legacy.Current)
	}

	return ServiceState{
		Version: serviceStateVersion, Project: resolved.RuntimeID(), Name: resolved.Name,
		Environment: resolved.Environment, Destination: name, Naming: legacyNaming,
		Services: map[string]ServiceRecord{},
	}, nil
}

// pendingTasks refuses to start over a release task whose last run was never
// seen to finish, since running it again could repeat whatever it half did.
func pendingTasks(state ServiceState) error {
	for _, name := range slices.Sorted(maps.Keys(state.Tasks)) {
		runs := state.Tasks[name]
		if last := runs[len(runs)-1]; last.Outcome == outcomeIndeterminate {
			return fmt.Errorf("release task %s did not finish in operation %s; inspect what it did, then run deploy resolve %s", name, last.Operation, last.Operation)
		}
	}

	return nil
}

func RunServiceDeploy(options DeployOptions, repositoryPath string) (exitCode int, err error) {
	resolved, name, destination, err := loadServiceConfig(repositoryPath, options.Environment)
	if err != nil {
		return exitPreconditionNotMet, err
	}
	if options.Destination != "" {
		return exitPreconditionNotMet, errors.New("version 2 takes its destination from the config; --destination applies to version 1")
	}
	if options.Affected {
		return exitPreconditionNotMet, errors.New("--affected is not available for version 2 yet")
	}
	if options.ForceUnlock {
		return exitPreconditionNotMet, errors.New("--force-unlock is for legacy version 1 lock metadata and does not apply to version 2")
	}

	var notifier *Notifier
	var announced bool
	_, stateless := SplitServices(resolved.Services)
	affected := AffectedNames(stateless)
	var versions StageVersions
	defer func() {
		if announced && err != nil {
			notifier.SendStage(stageCancelled, resolved.Name, affected, versions)
		}
	}()
	notifier, err = NewNotifier(repositoryPath, resolved.Notify)
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
			return exitPreconditionNotMet, fmt.Errorf("the working tree has uncommitted changes, so deploying %s would ship something nobody can reproduce later. commit them, or pass --allow-dirty", ShortCommit(commit))
		}
	}
	gitStorage, err := resolveGitStorage(options.GitStorage, resolved.GitStorage)
	if err != nil {
		return exitPreconditionNotMet, err
	}
	if err := CheckLocalRequirements(LocalRunner{}, destination); err != nil {
		return exitPreconditionNotMet, err
	}

	set, layout, err := lockServiceDestination(resolved, name, destination)
	if err != nil {
		return exitPreconditionNotMet, err
	}
	runner := set.Runner(0)
	destination.Path = path.Dir(layout.Root)

	state, err := destinationServiceState(runner, resolved, layout, name)
	if err == nil {
		err = pendingTasks(state)
	}
	if err != nil {
		set.Release()
		return exitPreconditionNotMet, err
	}
	operation, err := set.Begin("deploy", commit)
	if err != nil {
		set.Release()
		return exitPreconditionNotMet, err
	}
	set.ReleaseOnSignal()
	defer set.Finish(&exitCode)

	// recorded before anything changes, so the root is owned per service from
	// the first step and a version 1 deploy can no longer touch it
	if err := runner.MkdirAll(layout.Root); err != nil {
		return exitDeployFailed, err
	}
	if state, err = WriteServiceState(runner, layout, state); err != nil {
		return exitDeployFailed, err
	}

	facts, err := CheckDestination(runner, resolved, layout)
	if err != nil {
		return exitPreconditionNotMet, err
	}
	builder, err := NewBuilder(runner, facts, resolved.RuntimeID(), repositoryPath, destination.IsRemote(), resolveBuildOnDestination(options.BuildOnDest, resolved.BuildOnDestination))
	if err != nil {
		return exitPreconditionNotMet, err
	}
	fmt.Printf("deploying %s/%s %s to %s (%s)\n", resolved.Name, resolved.Environment, ShortCommit(commit), destination, builder.Describe())

	releaseDirectory := layout.Release(commit)
	if err := PlaceRelease(runner, repositoryPath, gitStorage, destination, commit, releaseDirectory); err != nil {
		return exitDeployFailed, err
	}
	fmt.Printf("  placed release in %s\n", releaseDirectory)
	builder.SetReleaseDirectory(releaseDirectory)
	if err := buildServices(builder, resolved, repositoryPath, commit, nil); err != nil {
		return exitDeployFailed, err
	}

	if _, err := startShared(runner, layout, resolved, commit); err != nil {
		return exitDeployFailed, err
	}
	stateful, _ := SplitServices(resolved.Services)
	for _, service := range ServiceNames(stateful) {
		record := state.Services[service]
		digest := nodeDigest(stateful[service])
		if record.Current == nil || record.Current.ConfigDigest != digest {
			current := sharedRelease(resolved.RuntimeID(), commit, digest)
			record.Current = &current
		}
		record.Stateful, record.Outcome, record.Previous, record.Releases = true, outcomeLive, nil, nil
		state.Services[service] = record
	}

	if len(stateless) > 0 {
		versions = StageVersions{Previous: liveCommits(state, stateless), Incoming: ShortCommit(commit)}
		notifier.SendStage(stageReady, resolved.Name, affected, versions)
		announced = true
	}

	executor := serviceExecutor{runner: runner, resolved: resolved, layout: layout, commit: commit, operation: operation.ID, live: map[string]string{}}
	for service, record := range state.Services {
		if record.Current != nil {
			executor.live[service] = record.Current.Project
		}
	}
	if err := executor.startWaves(&state); err != nil {
		executor.discardCandidates()
		if _, writeErr := WriteServiceState(runner, layout, state); writeErr != nil {
			return exitLiveButNeedsAHuman, fmt.Errorf("%w; recording the release task results also failed: %v", err, writeErr)
		}
		return exitDeployFailed, err
	}

	if len(stateless) > 0 {
		notifier.SendStage(stageSwitching, resolved.Name, affected, versions)
	}
	cutoverErr := executor.cutover(&state)
	state = executor.recordCutOver(state)
	state, pruneErr := pruneServiceReleases(runner, layout, resolved.RuntimeID(), state, resolved.Retention)
	if _, err := WriteServiceState(runner, layout, state); err != nil {
		return exitLiveButNeedsAHuman, fmt.Errorf("%s is running, but recording it failed, so deploy has lost track of what is live: %w", ShortCommit(commit), err)
	}
	if cutoverErr != nil {
		return exitDeployFailed, cutoverErr
	}
	if announced {
		notifier.SendStage(stageLive, resolved.Name, affected, versions)
	}
	if pruneErr != nil {
		return exitLiveButNeedsAHuman, fmt.Errorf("%s is live, but pruning old releases failed: %w", ShortCommit(commit), pruneErr)
	}

	return exitOK, nil
}

// liveCommits summarizes what is serving before the deploy, which may be
// several commits once services move independently.
func liveCommits(state ServiceState, services map[string]Service) string {
	var commits []string
	for _, name := range ServiceNames(services) {
		if record := state.Services[name]; record.Current != nil && !slices.Contains(commits, record.Current.Commit) {
			commits = append(commits, record.Current.Commit)
		}
	}

	return strings.Join(commits, ", ")
}

type serviceExecutor struct {
	runner    Runner
	resolved  ResolvedProject
	layout    Layout
	commit    string
	operation string
	// candidates are the releases started by this deploy, in start order
	candidates []string
	cutOver    []string
	// live names each service's serving project before the deploy; a candidate
	// with the same project is a redeploy of that commit and is never discarded
	live map[string]string
}

func (executor *serviceExecutor) composeFile(release ServiceRelease) string {
	return path.Join(executor.layout.Root, release.ComposeFile)
}

// startWaves runs release tasks and starts candidates wave by wave, so every
// dependency is ready before what needs it starts. Nothing serving is stopped.
func (executor *serviceExecutor) startWaves(state *ServiceState) error {
	waves, err := readinessWaves(executor.resolved)
	if err != nil {
		return err
	}
	if len(executor.resolved.Release) > 0 {
		rendered, err := RenderReleaseTasks(executor.resolved, executor.layout, executor.commit)
		if err != nil {
			return err
		}
		if err := executor.runner.WriteFile(path.Join(executor.layout.Release(executor.commit), releaseTasksFileName), rendered); err != nil {
			return err
		}
	}

	for _, wave := range waves {
		for _, name := range wave {
			if _, task := executor.resolved.Release[name]; task {
				if err := executor.runTask(state, name); err != nil {
					return err
				}
			}
		}

		var starting []string
		for _, name := range wave {
			if service, found := executor.resolved.Services[name]; found && !service.IsStateful() {
				starting = append(starting, name)
			}
		}
		if err := executor.startCandidates(starting); err != nil {
			return err
		}
	}

	return nil
}

// runTask records the run as indeterminate before it starts, so a crash
// part way through can never look like a task that never ran.
func (executor *serviceExecutor) runTask(state *ServiceState, name string) error {
	if state.Tasks == nil {
		state.Tasks = map[string][]TaskRecord{}
	}
	run := TaskRecord{Operation: executor.operation, Commit: ShortCommit(executor.commit), ConfigDigest: nodeDigest(executor.resolved.Release[name]), Outcome: outcomeIndeterminate, RecordedAt: time.Now().UTC().Truncate(time.Second)}
	state.Tasks[name] = append(state.Tasks[name], run)
	written, err := WriteServiceState(executor.runner, executor.layout, *state)
	if err != nil {
		return err
	}
	*state = written

	fmt.Printf("  running release task %s\n", name)
	command := []string{
		"docker", "compose",
		"--file", path.Join(executor.layout.Release(executor.commit), releaseTasksFileName),
		"--project-name", ReleaseTasksProjectName(executor.resolved.RuntimeID(), executor.commit),
		"run", "--rm", "--no-deps", name,
	}
	taskErr := executor.runner.Stream(command, os.Stdout)

	runs := state.Tasks[name]
	runs[len(runs)-1].Outcome = outcomeCompleted
	if taskErr != nil {
		runs[len(runs)-1].Outcome = outcomeFailed
	}
	written, err = WriteServiceState(executor.runner, executor.layout, *state)
	if err != nil {
		return err
	}
	*state = written
	if taskErr != nil {
		return fmt.Errorf("release task %s failed, so the deploy stopped before switching anything and every service is still on its previous release: %w", name, taskErr)
	}

	return nil
}

// startCandidates starts one wave together and waits for all of it, since
// waiting on each in turn would make independent services wait on each other.
func (executor *serviceExecutor) startCandidates(names []string) error {
	if len(names) == 0 {
		return nil
	}
	for _, name := range names {
		release := serviceRelease(executor.resolved.RuntimeID(), name, executor.commit, "")
		rendered, err := RenderServiceRelease(executor.resolved, executor.layout, name, executor.commit)
		if err != nil {
			return err
		}
		if err := executor.runner.WriteFile(executor.composeFile(release), rendered); err != nil {
			return err
		}
	}

	failures := make([]error, len(names))
	var group sync.WaitGroup
	for index, name := range names {
		release := serviceRelease(executor.resolved.RuntimeID(), name, executor.commit, "")
		executor.candidates = append(executor.candidates, name)
		group.Go(func() {
			failures[index] = startStack(executor.runner, executor.composeFile(release), release.Project)
		})
	}
	group.Wait()
	if err := errors.Join(failures...); err != nil {
		return err
	}
	fmt.Printf("  %s up\n", strings.Join(names, ", "))

	return nil
}

// discardCandidates removes what this deploy started and never switched to.
func (executor *serviceExecutor) discardCandidates() {
	for index := len(executor.candidates) - 1; index >= 0; index-- {
		name := executor.candidates[index]
		release := serviceRelease(executor.resolved.RuntimeID(), name, executor.commit, "")
		if slices.Contains(executor.cutOver, name) || executor.live[name] == release.Project {
			continue
		}
		tearDown(executor.runner, executor.composeFile(release), release.Project)
	}
}

// cutover moves each service from its live release to its candidate, one
// service at a time. The first failure stops the rest; services already moved
// stay moved and are recorded as such.
func (executor *serviceExecutor) cutover(state *ServiceState) error {
	for _, name := range executor.candidates {
		candidate := serviceRelease(executor.resolved.RuntimeID(), name, executor.commit, "")
		live := state.Services[name].Current
		if live == nil || live.Project == candidate.Project {
			executor.cutOver = append(executor.cutOver, name)
			continue
		}
		if err := executor.switchService(name, *live); err != nil {
			executor.discardCandidates()
			moved := "no service was switched"
			if len(executor.cutOver) > 0 {
				moved = strings.Join(executor.cutOver, ", ") + " switched and stay on " + ShortCommit(executor.commit)
			}
			return fmt.Errorf("%s: %w; %s, the rest stay on their previous releases", name, err, moved)
		}
		executor.cutOver = append(executor.cutOver, name)
	}

	return nil
}

// switchService follows the measured cutover: the candidate already holds the
// network alias, and traffic moves when the live container stops answering.
func (executor *serviceExecutor) switchService(name string, live ServiceRelease) error {
	host := executor.resolved.Services[name].Host
	if host != nil {
		if err := reachableOnNetwork(executor.runner, NetworkName(executor.resolved.RuntimeID()), name, host.Port); err != nil {
			return fmt.Errorf("the candidate is not reachable at %s:%d on the project network: %w", name, host.Port, err)
		}
	}

	compose := []string{"docker", "compose", "--file", executor.composeFile(live), "--project-name", live.Project}
	if output, err := executor.runner.Run(append(slices.Clone(compose), "stop", name)); err != nil {
		return fmt.Errorf("stopping %s %s: %s", live.Project, name, firstLine(output))
	}
	fmt.Printf("  stopped %s in %s, traffic is moving\n", name, live.Project)

	if host != nil {
		if err := verifyPublicHostname(host.Domain); err != nil {
			if output, startErr := executor.runner.Run(append(slices.Clone(compose), "start", name)); startErr != nil {
				return fmt.Errorf("%w, and %s could not be started again either, so nothing is serving it: %s", err, live.Project, firstLine(output))
			}
			return err
		}
		fmt.Printf("  https://%s answered\n", host.Domain)
	}

	// the stopped container goes, the compose file stays for rollback
	if output, err := executor.runner.Run(append(slices.Clone(compose), "rm", "--force", name)); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not remove the stopped %s from %s: %s\n", name, live.Project, firstLine(output))
	}

	return nil
}

func (executor *serviceExecutor) recordCutOver(state ServiceState) ServiceState {
	for _, name := range executor.cutOver {
		release := serviceRelease(executor.resolved.RuntimeID(), name, executor.commit, nodeDigest(executor.resolved.Services[name]))
		record := state.Services[name]
		if record.Current != nil && record.Current.Project != release.Project {
			record.Previous = record.Current
		}
		record.Current = &release
		record.Outcome = outcomeLive
		record.Releases = append([]string{release.Commit}, slices.DeleteFunc(slices.Clone(record.Releases), func(existing string) bool { return existing == release.Commit })...)
		state.Services[name] = record
	}

	return state
}

// pruneServiceReleases keeps each service's newest releases plus its current
// and previous ones. A release directory and its images go only once no
// service, stateful ones included, still refers to that commit.
func pruneServiceReleases(runner Runner, layout Layout, id string, state ServiceState, retention int) (ServiceState, error) {
	if retention < 1 {
		retention = 1
	}
	referenced := map[string]bool{}
	for _, name := range slices.Sorted(maps.Keys(state.Services)) {
		record := state.Services[name]
		var kept []string
		for _, commit := range record.Releases {
			isLive := (record.Current != nil && record.Current.Commit == commit) || (record.Previous != nil && record.Previous.Commit == commit)
			if len(kept) < retention || isLive {
				kept = append(kept, commit)
				continue
			}
			release := serviceRelease(id, name, commit, "")
			file := path.Join(layout.Root, release.ComposeFile)
			if _, err := runner.ReadFile(file); err == nil {
				tearDown(runner, file, release.Project)
				if err := runner.RemoveAll(file); err != nil {
					return state, fmt.Errorf("pruning %s: %w", file, err)
				}
			}
		}
		record.Releases = kept
		state.Services[name] = record
		for _, commit := range kept {
			referenced[commit] = true
		}
		for _, release := range []*ServiceRelease{record.Current, record.Previous} {
			if release != nil {
				referenced[release.Commit] = true
			}
		}
	}

	onDisk, err := runner.ListDirectory(layout.Releases())
	if err != nil {
		return state, err
	}
	for _, commit := range onDisk {
		if referenced[commit] || !commitPattern.MatchString(commit) {
			continue
		}
		directory, err := releasePathWithin(layout.Releases(), commit)
		if err != nil {
			return state, err
		}
		if err := resolvedWithinRoot(layout.Releases(), directory); err != nil {
			return state, err
		}
		legacy := path.Join(directory, composeFileName)
		if _, err := runner.ReadFile(legacy); err == nil {
			stopRelease(runner, id, commit, legacy)
		}
		if err := runner.RemoveAll(directory); err != nil {
			return state, fmt.Errorf("pruning %s: %w", directory, err)
		}
		pruneImages(runner, id, commit)
		fmt.Printf("  pruned release %s\n", commit)
	}

	return state, nil
}
