package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// loadServiceConfig loads a version 2 config for the commands that still work
// on one destination.
func loadServiceConfig(repositoryPath, environmentName string) (ResolvedProject, string, Destination, error) {
	resolved, destinations, err := loadTopology(repositoryPath, environmentName)
	if err != nil {
		return ResolvedProject{}, "", Destination{}, err
	}
	if len(destinations) != 1 {
		return ResolvedProject{}, "", Destination{}, errors.New("this command works on one destination for now; deploy itself handles several")
	}
	name := slices.Collect(maps.Keys(destinations))[0]

	return resolved, name, destinations[name], nil
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
	resolved, destinations, err := loadTopology(repositoryPath, options.Environment)
	if err != nil {
		return exitPreconditionNotMet, err
	}
	if options.Destination != "" {
		return exitPreconditionNotMet, errors.New("version 2 takes its destinations from the config; --destination applies to version 1")
	}
	if options.ForceUnlock {
		return exitPreconditionNotMet, errors.New("--force-unlock is for legacy version 1 lock metadata and does not apply to version 2")
	}
	selected, err := selectNodes(resolved, options.Services)
	if err != nil {
		return exitPreconditionNotMet, err
	}

	var notifier *Notifier
	var announced bool
	_, stateless := SplitServices(resolved.Services)
	if selected != nil {
		stateless = maps.Clone(stateless)
		maps.DeleteFunc(stateless, func(name string, _ Service) bool { return !selected[name] })
	}
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
	builds := false
	for _, service := range buildableServices(resolved) {
		builds = builds || len(service.Build) > 0
	}
	for _, name := range slices.Sorted(maps.Keys(destinations)) {
		if err := CheckLocalRequirements(LocalRunner{}, destinations[name], builds); err != nil {
			return exitPreconditionNotMet, err
		}
	}

	ctx := context.Background()
	observations, err := observeTopology(ctx, &resolved, destinations, observeDestination)
	if err != nil {
		return exitPreconditionNotMet, err
	}
	hosts, err := networkHosts(resolved)
	if err != nil {
		return exitPreconditionNotMet, err
	}
	// a missing or outdated helper is found before anything is locked
	if len(hosts) > 0 {
		if _, err := inspectNetwork(ctx, resolved, hosts, callNetworkHelper); err != nil {
			return exitPreconditionNotMet, err
		}
	}

	set, runs, err := lockTopology(ctx, resolved, destinations, observations)
	if err != nil {
		return exitPreconditionNotMet, err
	}
	for _, run := range runs {
		run.state, err = destinationServiceState(run.runner, resolved, run.layout, run.name)
		if err == nil {
			err = pendingTasks(run.state)
		}
		if err != nil {
			set.Release()
			return exitPreconditionNotMet, fmt.Errorf("destination %s: %w", run.name, err)
		}
	}
	if err := selectionIsReady(resolved, selected, runs); err != nil {
		set.Release()
		return exitPreconditionNotMet, err
	}
	retiring, err := plannedRetirements(resolved, runs, selected)
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

	builders := map[string]*Builder{}
	for _, run := range runs {
		// recorded before anything changes, so the root is owned per service
		// from the first step and a version 1 deploy can no longer touch it
		if err := run.runner.MkdirAll(run.layout.Root); err != nil {
			return exitDeployFailed, err
		}
		if run.state, err = WriteServiceState(run.runner, run.layout, run.state); err != nil {
			return exitDeployFailed, err
		}
		facts, err := CheckDestination(run.runner, run.view, run.layout)
		if err != nil {
			return exitPreconditionNotMet, fmt.Errorf("destination %s: %w", run.name, err)
		}
		if !builds {
			fmt.Printf("deploying %s/%s %s to %s (published images, nothing to build)\n", resolved.Name, resolved.Environment, ShortCommit(commit), run.destination)
			continue
		}
		builder, err := NewBuilder(run.runner, facts, resolved.RuntimeID(), repositoryPath, run.destination.IsRemote(), resolveBuildOnDestination(options.BuildOnDest, resolved.BuildOnDestination))
		if err != nil {
			return exitPreconditionNotMet, fmt.Errorf("destination %s: %w", run.name, err)
		}
		builders[run.name] = builder
		fmt.Printf("deploying %s/%s %s to %s (%s)\n", resolved.Name, resolved.Environment, ShortCommit(commit), run.destination, builder.Describe())
	}

	for _, run := range runs {
		releaseDirectory := run.layout.Release(commit)
		if err := PlaceRelease(run.runner, repositoryPath, gitStorage, run.destination, commit, releaseDirectory); err != nil {
			return exitDeployFailed, fmt.Errorf("destination %s: %w", run.name, err)
		}
		fmt.Printf("  placed release in %s\n", releaseDirectory)
		if !builds {
			continue
		}
		var reuse map[string]string
		if options.Affected {
			if reuse, err = planServiceReuse(builders[run.name], run.view, repositoryPath, run.state, commit); err != nil {
				return exitDeployFailed, err
			}
		}
		builders[run.name].SetReleaseDirectory(releaseDirectory)
		if err := buildServices(builders[run.name], run.view, repositoryPath, commit, reuse); err != nil {
			return exitDeployFailed, err
		}
	}

	// the private network exists before anything binds to it or calls across it
	if len(hosts) > 0 {
		fmt.Println("  managed network")
		runners := map[string]Runner{}
		for _, run := range runs {
			runners[run.name] = run.runner
		}
		if err := applyNetwork(ctx, resolved, hosts, runners, observations, operation.ID, indented{os.Stdout}); err != nil {
			return exitDeployFailed, fmt.Errorf("the managed network could not be applied, so nothing was started: %w", err)
		}
	}

	for _, run := range runs {
		if _, err := startShared(run.runner, run.layout, run.view, commit); err != nil {
			return exitDeployFailed, fmt.Errorf("destination %s: %w", run.name, err)
		}
		stateful, _ := SplitServices(run.view.Services)
		for _, service := range ServiceNames(stateful) {
			record := run.state.Services[service]
			digest := nodeDigest(stateful[service])
			if record.Current == nil || record.Current.ConfigDigest != digest {
				current := sharedRelease(resolved.RuntimeID(), commit, digest)
				record.Current = &current
			}
			record.Stateful, record.Outcome, record.Previous, record.Releases = true, outcomeLive, nil, nil
			run.state.Services[service] = record
		}
	}

	if len(stateless) > 0 {
		var previous []string
		for _, run := range runs {
			_, local := SplitServices(run.view.Services)
			if commits := liveCommits(run.state, local); commits != "" && !slices.Contains(previous, commits) {
				previous = append(previous, commits)
			}
		}
		versions = StageVersions{Previous: strings.Join(previous, ", "), Incoming: ShortCommit(commit)}
		notifier.SendStage(stageReady, resolved.Name, affected, versions)
		announced = true
	}

	executors := map[string]*serviceExecutor{}
	for _, run := range runs {
		executor := &serviceExecutor{runner: run.runner, resolved: run.view, layout: run.layout, commit: commit, operation: operation.ID, live: map[string]string{}}
		for service, record := range run.state.Services {
			if record.Current != nil {
				executor.live[service] = record.Current.Project
			}
		}
		executors[run.name] = executor
	}
	// records what each destination actually has, whatever happened, so a
	// failure part way leaves every destination's state true
	recordAll := func() error {
		var failures []error
		for _, run := range runs {
			run.state = executors[run.name].recordCutOver(run.state)
			if _, err := WriteServiceState(run.runner, run.layout, run.state); err != nil {
				failures = append(failures, fmt.Errorf("destination %s: %w", run.name, err))
			}
		}
		return errors.Join(failures...)
	}
	discardAll := func() {
		for _, run := range runs {
			executors[run.name].discardCandidates()
		}
	}

	if err := runWaves(resolved, runs, executors, selected); err != nil {
		discardAll()
		if writeErr := recordAll(); writeErr != nil {
			return exitLiveButNeedsAHuman, fmt.Errorf("%w; recording what is live also failed: %v", err, writeErr)
		}
		return exitDeployFailed, err
	}
	if err := checkCrossHostPaths(resolved, runs, executors); err != nil {
		discardAll()
		if writeErr := recordAll(); writeErr != nil {
			return exitLiveButNeedsAHuman, fmt.Errorf("%w; recording what is live also failed: %v", err, writeErr)
		}
		return exitDeployFailed, err
	}

	if len(stateless) > 0 {
		notifier.SendStage(stageSwitching, resolved.Name, affected, versions)
	}
	var cutoverErr error
	for _, run := range runs {
		if cutoverErr != nil {
			executors[run.name].discardCandidates()
			continue
		}
		if err := executors[run.name].cutover(&run.state); err != nil {
			cutoverErr = err
			if len(runs) > 1 {
				cutoverErr = fmt.Errorf("destination %s: %w", run.name, err)
			}
		}
	}
	var pruneErrs []error
	if cutoverErr == nil {
		for _, run := range runs {
			retire(run, retiring[run.name])
		}
	}
	for _, run := range runs {
		run.state = executors[run.name].recordCutOver(run.state)
		var pruneErr error
		if run.state, pruneErr = pruneServiceReleases(run.runner, run.layout, resolved.RuntimeID(), run.state, resolved.Retention); pruneErr != nil {
			pruneErrs = append(pruneErrs, pruneErr)
		}
		if _, err := WriteServiceState(run.runner, run.layout, run.state); err != nil {
			return exitLiveButNeedsAHuman, fmt.Errorf("%s is running on %s, but recording it failed, so deploy has lost track of what is live: %w", ShortCommit(commit), run.name, err)
		}
	}
	if len(hosts) > 0 {
		if err := recordWorkloads(ctx, resolved, hosts, runs); err != nil {
			return exitLiveButNeedsAHuman, fmt.Errorf("%s is live, but recording the containers to start at boot failed, so a reboot would leave them stopped: %w", ShortCommit(commit), err)
		}
	}
	if cutoverErr != nil {
		return exitDeployFailed, cutoverErr
	}
	if announced {
		notifier.SendStage(stageLive, resolved.Name, affected, versions)
	}
	if err := errors.Join(pruneErrs...); err != nil {
		return exitLiveButNeedsAHuman, fmt.Errorf("%s is live, but pruning old releases failed: %w", ShortCommit(commit), err)
	}

	return exitOK, nil
}

// runWaves runs release tasks and starts candidates wave by wave across every
// destination, so whatever a service waits on is ready first, wherever it
// runs. Candidates of one wave start together on all destinations.
func runWaves(resolved ResolvedProject, runs []*destinationRun, executors map[string]*serviceExecutor, selected map[string]bool) error {
	waves, err := readinessWaves(resolved)
	if err != nil {
		return err
	}
	for _, run := range runs {
		if err := executors[run.name].prepareTasks(); err != nil {
			return fmt.Errorf("destination %s: %w", run.name, err)
		}
	}

	for _, wave := range waves {
		if selected != nil {
			wave = slices.DeleteFunc(slices.Clone(wave), func(name string) bool { return !selected[name] })
		}
		for _, run := range runs {
			for _, name := range wave {
				if _, task := run.view.Release[name]; task {
					if err := executors[run.name].runTask(&run.state, name); err != nil {
						return err
					}
				}
			}
		}

		failures := make([]error, len(runs))
		var group sync.WaitGroup
		for index, run := range runs {
			var starting []string
			for _, name := range wave {
				if service, found := run.view.Services[name]; found && !service.IsStateful() {
					starting = append(starting, name)
				}
			}
			group.Go(func() {
				if err := executors[run.name].startCandidates(&run.state, starting); err != nil {
					failures[index] = err
					if len(runs) > 1 {
						failures[index] = fmt.Errorf("destination %s: %w", run.name, err)
					}
				}
			})
		}
		group.Wait()
		if err := errors.Join(failures...); err != nil {
			return err
		}
	}

	return nil
}

// plannedRetirements finds, per destination, the stateless services it still
// runs that the config has moved to another destination; they go once the
// deploy is live there. A stateful service moving is refused, since its data
// would stay behind. A service dropped from the config keeps running and stays
// recorded, as it always has, until deploy destroy removes it.
func plannedRetirements(resolved ResolvedProject, runs []*destinationRun, selected map[string]bool) (map[string][]string, error) {
	retiring := map[string][]string{}
	var problems []error
	for _, run := range runs {
		for _, name := range slices.Sorted(maps.Keys(run.state.Services)) {
			record := run.state.Services[name]
			placed, configured := resolved.Services[name]
			if record.Current == nil || !configured || placed.Destination == run.name {
				continue
			}
			if record.Stateful {
				problems = append(problems, fmt.Errorf("stateful %s moves from %s to %s, but its data stays on %s; move the data yourself, or keep it on %s", name, run.name, placed.Destination, run.name, run.name))
				continue
			}
			if selected != nil && !selected[name] {
				continue
			}
			retiring[run.name] = append(retiring[run.name], name)
		}
	}

	return retiring, errors.Join(problems...)
}

// retire removes a moved stateless service's live container from the
// destination it left, and its record with it, so its releases there are
// pruned like any other unreferenced ones.
func retire(run *destinationRun, names []string) {
	for _, name := range names {
		current := run.state.Services[name].Current
		compose := []string{"docker", "compose", "--file", path.Join(run.layout.Root, current.ComposeFile), "--project-name", current.Project}
		if output, err := run.runner.Run(append(compose, "rm", "--stop", "--force", name)); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not remove %s from %s on %s: %s\n", name, current.Project, run.name, firstLine(output))
			continue
		}
		delete(run.state.Services, name)
		fmt.Printf("  retired %s from %s; it now runs on %s\n", name, run.name, placedOn(run.view, name))
	}
}

// selectNodes checks the names given to --service. nil means everything.
func selectNodes(resolved ResolvedProject, names []string) (map[string]bool, error) {
	if len(names) == 0 {
		return nil, nil
	}
	selected := map[string]bool{}
	nodes := topologyNodes(resolved)
	for _, name := range names {
		if _, found := nodes[name]; !found {
			return nil, fmt.Errorf("--service names %q, which %s does not have; it has %s", name, resolved.Environment, strings.Join(slices.Sorted(maps.Keys(nodes)), ", "))
		}
		selected[name] = true
	}

	return selected, nil
}

// selectionIsReady refuses a selected deploy whose services wait on something
// that is neither part of it nor already there. Stateful services are always
// brought up with their shared stack, so they count as there.
func selectionIsReady(resolved ResolvedProject, selected map[string]bool, runs []*destinationRun) error {
	if selected == nil {
		return nil
	}
	recorded := map[string]bool{}
	for _, run := range runs {
		for name, record := range run.state.Services {
			if record.Current != nil {
				recorded[name] = true
			}
		}
		for name, tasks := range run.state.Tasks {
			if tasks[len(tasks)-1].Outcome == outcomeCompleted {
				recorded[name] = true
			}
		}
	}
	nodes := topologyNodes(resolved)
	var problems []error
	for _, name := range slices.Sorted(maps.Keys(selected)) {
		for _, dependency := range slices.Sorted(maps.Keys(nodes[name].DependsOn)) {
			if selected[dependency] || nodes[dependency].IsStateful() || recorded[dependency] {
				continue
			}
			problems = append(problems, fmt.Errorf("%s waits on %s, which has never been deployed; add it to --service", name, dependency))
		}
	}

	return errors.Join(problems...)
}

// checkCrossHostPaths reaches each provider on another host the way its
// consumer will, from the consumer's project network, before anything is
// switched. Application authentication stays with each service's own checks.
func checkCrossHostPaths(resolved ResolvedProject, runs []*destinationRun, executors map[string]*serviceExecutor) error {
	for _, run := range runs {
		for _, name := range executors[run.name].candidates {
			service := run.view.Services[name]
			for _, label := range slices.Sorted(maps.Keys(service.Connections)) {
				connection := service.Connections[label]
				provider := resolved.Services[connection.Service]
				if resolved.destinationHost(provider.Destination) == resolved.destinationHost(service.Destination) {
					continue
				}
				address := resolved.Network.Peers[provider.Destination].Address
				port := provider.Endpoints[connection.Endpoint].HostPort
				if err := reachableOnNetwork(run.runner, NetworkName(resolved.RuntimeID()), address, port); err != nil {
					return fmt.Errorf("%s on %s cannot reach %s at %s:%d over the managed network: %w", name, run.name, connection.Service, address, port, err)
				}
				fmt.Printf("  %s reaches %s at %s:%d\n", name, connection.Service, address, port)
			}
		}
	}

	return nil
}

// recordWorkloads tells the network helper on each host which containers to
// start once its network is up, from what each destination now records as
// live there.
func recordWorkloads(ctx context.Context, resolved ResolvedProject, hosts []networkHost, runs []*destinationRun) error {
	for _, host := range hosts {
		live := map[string]ServiceRelease{}
		for _, run := range runs {
			if !slices.Contains(host.Destinations, run.name) {
				continue
			}
			for name, record := range run.state.Services {
				if record.Current != nil {
					live[name] = *record.Current
				}
			}
		}
		containers, err := workloadContainers(resolved, host.Name, live)
		if err != nil {
			return err
		}
		if _, err := callNetworkHelper(ctx, Destination{Host: host.Target}, "workloads", workloadsInput(resolved, containers)); err != nil {
			return err
		}
	}

	return nil
}

// indented prefixes every line written through it, so a stage's own output
// sits under the deploy step that ran it.
type indented struct{ out io.Writer }

func (writer indented) Write(text []byte) (int, error) {
	lines := strings.SplitAfter(string(text), "\n")
	var buffer strings.Builder
	for _, line := range lines {
		if line != "" {
			buffer.WriteString("    " + line)
		}
	}
	if _, err := io.WriteString(writer.out, buffer.String()); err != nil {
		return 0, err
	}

	return len(text), nil
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

// prepareTasks writes the release task stack for this destination's tasks.
func (executor *serviceExecutor) prepareTasks() error {
	if len(executor.resolved.Release) == 0 {
		return nil
	}
	rendered, err := RenderReleaseTasks(executor.resolved, executor.layout, executor.commit)
	if err != nil {
		return err
	}

	return executor.runner.WriteFile(path.Join(executor.layout.Release(executor.commit), releaseTasksFileName), rendered)
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
// A service publishing a fixed port on the managed network cannot run beside
// its live release, so it is replaced in place instead.
func (executor *serviceExecutor) startCandidates(state *ServiceState, names []string) error {
	var together []string
	for _, name := range names {
		if executor.fixedPort(name) {
			if err := executor.recreate(state, name); err != nil {
				return err
			}
			continue
		}
		together = append(together, name)
	}
	if len(together) == 0 {
		return nil
	}
	for _, name := range together {
		release := serviceRelease(executor.resolved.RuntimeID(), name, executor.commit, "")
		rendered, err := RenderServiceRelease(executor.resolved, executor.layout, name, executor.commit)
		if err != nil {
			return err
		}
		if err := executor.runner.WriteFile(executor.composeFile(release), rendered); err != nil {
			return err
		}
	}

	failures := make([]error, len(together))
	var group sync.WaitGroup
	for index, name := range together {
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
	fmt.Printf("  %s up\n", strings.Join(together, ", "))

	return nil
}

// fixedPort reports whether a stateless service publishes a host port on the
// managed network, which two releases cannot hold at once.
func (executor *serviceExecutor) fixedPort(name string) bool {
	whole := executor.resolved.topology()
	service := whole.Services[name]

	return !service.IsStateful() && len(crossHostPorts(whole, name, service)) > 0
}

// recreate replaces a fixed-port service in place: the live release stops,
// the candidate starts and must become healthy, and if it does not, the live
// release is started again. This is downtime, and it is announced as such.
func (executor *serviceExecutor) recreate(state *ServiceState, name string) error {
	candidate := serviceRelease(executor.resolved.RuntimeID(), name, executor.commit, "")
	rendered, err := RenderServiceRelease(executor.resolved, executor.layout, name, executor.commit)
	if err != nil {
		return err
	}
	if err := executor.runner.WriteFile(executor.composeFile(candidate), rendered); err != nil {
		return err
	}

	live := state.Services[name].Current
	var compose []string
	if live != nil && live.Project != candidate.Project {
		compose = []string{"docker", "compose", "--file", executor.composeFile(*live), "--project-name", live.Project}
		fmt.Printf("  %s publishes a fixed port, so it is replaced in place; it is down until %s is healthy\n", name, ShortCommit(executor.commit))
		if output, err := executor.runner.Run(append(slices.Clone(compose), "stop", name)); err != nil {
			return fmt.Errorf("stopping %s %s: %s", live.Project, name, firstLine(output))
		}
	}
	executor.candidates = append(executor.candidates, name)
	if err := startStack(executor.runner, executor.composeFile(candidate), candidate.Project); err != nil {
		if compose != nil {
			if output, startErr := executor.runner.Run(append(slices.Clone(compose), "start", name)); startErr != nil {
				return fmt.Errorf("%w, and %s could not be started again either, so nothing is serving it: %s", err, live.Project, firstLine(output))
			}
			return fmt.Errorf("%w; %s is serving again", err, live.Project)
		}
		return err
	}
	if compose != nil {
		if output, err := executor.runner.Run(append(slices.Clone(compose), "rm", "--force", name)); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not remove the stopped %s from %s: %s\n", name, live.Project, firstLine(output))
		}
	}
	executor.cutOver = append(executor.cutOver, name)
	fmt.Printf("  %s up\n", name)

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
		if slices.Contains(executor.cutOver, name) {
			continue
		}
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
		if paths := releasePaths(executor.resolved, name); len(paths) > 0 {
			record.Paths = maps.Clone(record.Paths)
			if record.Paths == nil {
				record.Paths = map[string][]string{}
			}
			record.Paths[release.Commit] = paths
		} else {
			delete(record.Paths, release.Commit)
		}
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
		for commit := range record.Paths {
			if !slices.Contains(kept, commit) && (record.Current == nil || record.Current.Commit != commit) && (record.Previous == nil || record.Previous.Commit != commit) {
				delete(record.Paths, commit)
			}
		}
		if len(record.Paths) == 0 {
			record.Paths = nil
		}
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

// placedOn names the destination the whole config places a service on.
func placedOn(resolved ResolvedProject, name string) string {
	return resolved.topology().Services[name].Destination
}
