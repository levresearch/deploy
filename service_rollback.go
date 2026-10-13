package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"
)

// retainedRelease finds a recorded service's release at a commit, using the
// files that are actually on disk, since a release outlives its record only
// as long as its compose file does.
func retainedRelease(runner Runner, layout Layout, id, name string, record ServiceRecord, commit string) (ServiceRelease, error) {
	if record.Previous != nil && record.Previous.Commit == commit {
		if _, err := runner.ReadFile(path.Join(layout.Root, record.Previous.ComposeFile)); err == nil {
			return *record.Previous, nil
		}
	}
	if slices.Contains(record.Releases, commit) {
		own := serviceRelease(id, name, commit, "")
		if _, err := runner.ReadFile(path.Join(layout.Root, own.ComposeFile)); err == nil {
			return own, nil
		}
		legacy := legacyRelease(id, commit)
		if services, err := composeServices(runner, path.Join(layout.Root, legacy.ComposeFile)); err == nil && slices.Contains(services, name) {
			return legacy, nil
		}
	}

	var available []string
	for _, retained := range record.Releases {
		if record.Current == nil || retained != record.Current.Commit {
			available = append(available, retained)
		}
	}
	if len(available) == 0 {
		available = []string{"none"}
	}

	return ServiceRelease{}, fmt.Errorf("%s is not available to roll %s back to. these still are: %s", commit, name, strings.Join(available, ", "))
}

type rollbackStep struct {
	name   string
	live   ServiceRelease
	target ServiceRelease
}

// planServiceRollback picks each service's target: the requested commit, or
// its own previous release. Stateful services are never rolled back, since
// their data does not go back with them.
func planServiceRollback(runner Runner, current ResolvedProject, layout Layout, state ServiceState, requested string, only []string) ([]rollbackStep, error) {
	selected := only
	if len(selected) == 0 {
		for _, name := range slices.Sorted(maps.Keys(state.Services)) {
			if record := state.Services[name]; !record.Stateful && record.Current != nil {
				selected = append(selected, name)
			}
		}
	}

	var steps []rollbackStep
	for _, name := range selected {
		record, found := state.Services[name]
		explicit := len(only) > 0
		switch {
		case !found || record.Current == nil:
			return nil, fmt.Errorf("%s has never been deployed here, so there is nothing to roll back", name)
		case record.Stateful:
			return nil, fmt.Errorf("%s is stateful; its data cannot be rolled back with a release", name)
		}

		target := ShortCommit(requested)
		if target == "" {
			if record.Previous == nil {
				if explicit {
					return nil, fmt.Errorf("%s is the only release of %s deploy knows about, so there is nothing to roll back to", record.Current.Commit, name)
				}
				fmt.Printf("  %s has no previous release, leaving it on %s\n", name, record.Current.Commit)
				continue
			}
			target = record.Previous.Commit
		}
		if target == record.Current.Commit {
			if explicit || requested == "" {
				return nil, fmt.Errorf("%s is already running %s", name, target)
			}
			fmt.Printf("  %s is already on %s\n", name, target)
			continue
		}

		release, err := retainedRelease(runner, layout, state.Project, name, record, target)
		if err != nil {
			if !explicit && requested != "" && !slices.Contains(record.Releases, target) {
				fmt.Printf("  %s never ran %s, leaving it on %s\n", name, target, record.Current.Commit)
				continue
			}
			return nil, err
		}
		if revoked := revokedPaths(current, name, record.Paths[release.Commit]); len(revoked) > 0 {
			return nil, fmt.Errorf("%s %s used %s on the managed network, which the current config no longer provides; rolling back would not bring that access back, so deploy a release instead", name, release.Commit, strings.Join(revoked, ", "))
		}
		steps = append(steps, rollbackStep{name: name, live: *record.Current, target: release})
	}

	return steps, nil
}

func RunServiceRollback(options DeployOptions, repositoryPath, requested string, only []string) (exitCode int, err error) {
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
	notifier, err := NewNotifier(repositoryPath, resolved.Notify)
	if err != nil {
		return exitPreconditionNotMet, err
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
	set, runs, err := lockTopology(ctx, resolved, destinations, observations)
	if err != nil {
		return exitPreconditionNotMet, err
	}

	// each named service is rolled back where it is recorded
	plans := map[string][]rollbackStep{}
	found := map[string]bool{}
	total := 0
	for _, run := range runs {
		if run.state, err = destinationServiceState(run.runner, resolved, run.layout, run.name); err != nil {
			set.Release()
			return exitPreconditionNotMet, fmt.Errorf("destination %s: %w", run.name, err)
		}
		var local []string
		for _, name := range only {
			if _, recorded := run.state.Services[name]; recorded {
				local = append(local, name)
				found[name] = true
			}
		}
		if len(only) > 0 && len(local) == 0 {
			continue
		}
		steps, err := planServiceRollback(run.runner, run.view, run.layout, run.state, requested, local)
		if err != nil {
			set.Release()
			if len(runs) > 1 {
				err = fmt.Errorf("destination %s: %w", run.name, err)
			}
			return exitPreconditionNotMet, err
		}
		plans[run.name] = steps
		total += len(steps)
	}
	for _, name := range only {
		if !found[name] {
			set.Release()
			return exitPreconditionNotMet, fmt.Errorf("%s has never been deployed here, so there is nothing to roll back", name)
		}
	}
	if total == 0 {
		set.Release()
		return exitPreconditionNotMet, errors.New("nothing to roll back")
	}
	if _, err := set.Begin("rollback", requested); err != nil {
		set.Release()
		return exitPreconditionNotMet, err
	}
	set.ReleaseOnSignal()
	defer set.Finish(&exitCode)

	var affected, leaving, landing []string
	for _, run := range runs {
		for _, step := range plans[run.name] {
			affected = append(affected, step.name)
			leaving = append(leaving, step.name+" "+step.live.Commit)
			landing = append(landing, step.name+" "+step.target.Commit)
		}
	}
	where := runs[0].destination.String()
	if len(runs) > 1 {
		where = fmt.Sprintf("%d destinations", len(runs))
	}
	fmt.Printf("rolling %s back on %s: %s\n", resolved.Name, where, strings.Join(landing, ", "))
	notifier.SendStage(stageRollingBack, resolved.Name, affected, StageVersions{Previous: strings.Join(landing, ", "), Incoming: strings.Join(leaving, ", ")})

	var rolled []string
	var failure error
	for _, run := range runs {
		if failure != nil {
			break
		}
		executor := serviceExecutor{runner: run.runner, resolved: run.view, layout: run.layout}
		for _, step := range plans[run.name] {
			if err := executor.rollService(step); err != nil {
				failure = fmt.Errorf("%s: %w", step.name, err)
				if len(runs) > 1 {
					failure = fmt.Errorf("destination %s: %w", run.name, failure)
				}
				break
			}
			record := run.state.Services[step.name]
			live, target := step.live, step.target
			record.Previous, record.Current, record.Outcome = &live, &target, outcomeLive
			run.state.Services[step.name] = record
			rolled = append(rolled, step.name)
		}
	}

	for _, run := range runs {
		if _, err := WriteServiceState(run.runner, run.layout, run.state); err != nil {
			return exitLiveButNeedsAHuman, fmt.Errorf("%s rolled back, but recording it on %s failed, so deploy has lost track of what is live: %w", strings.Join(rolled, ", "), run.name, err)
		}
	}
	if len(hosts) > 0 && len(rolled) > 0 {
		if err := recordWorkloads(ctx, resolved, hosts, runs); err != nil {
			return exitLiveButNeedsAHuman, fmt.Errorf("%s rolled back, but recording the containers to start at boot failed, so a reboot would leave them stopped: %w", strings.Join(rolled, ", "), err)
		}
	}
	if failure != nil {
		notifier.SendStage(stageRollbackFailed, resolved.Name, affected, StageVersions{Incoming: strings.Join(leaving, ", ")})
		moved := "nothing was rolled back"
		if len(rolled) > 0 {
			moved = strings.Join(rolled, ", ") + " rolled back and stay there"
		}
		return exitDeployFailed, fmt.Errorf("%w; %s, the rest keep their releases", failure, moved)
	}

	fmt.Printf("  rolled back %s; rolling back again returns them\n", strings.Join(rolled, ", "))
	fmt.Fprintln(os.Stderr, "\nNOTE: this moved containers, not data. a release that ran a migration leaves the older code running against the newer schema.")

	return exitOK, nil
}

// rollService starts the retained release of one service and switches to it
// the way deploy switches to a candidate. A release that will not start is
// removed again and nothing serving is touched.
func (executor *serviceExecutor) rollService(step rollbackStep) error {
	compose := []string{"docker", "compose", "--file", executor.composeFile(step.target), "--project-name", step.target.Project}
	up := append(slices.Clone(compose), "up", "--detach", "--wait", "--no-deps", step.name)
	if executor.fixedPort(step.name) {
		return executor.rollInPlace(step, compose, up)
	}
	if err := executor.runner.Stream(up, os.Stdout); err != nil {
		executor.runner.Run(append(slices.Clone(compose), "rm", "--stop", "--force", step.name))
		return fmt.Errorf("%s %s never became healthy, so it was removed and nothing was switched", step.target.Project, step.name)
	}
	fmt.Printf("  %s %s is up\n", step.target.Project, step.name)

	return executor.switchService(step.name, step.live)
}

// rollInPlace rolls back a fixed-port service the way deploy replaces one:
// the live release stops first, and starts again if the target does not
// become healthy.
func (executor *serviceExecutor) rollInPlace(step rollbackStep, compose, up []string) error {
	live := []string{"docker", "compose", "--file", executor.composeFile(step.live), "--project-name", step.live.Project}
	fmt.Printf("  %s publishes a fixed port, so it is replaced in place; it is down until %s is healthy\n", step.name, step.target.Commit)
	if output, err := executor.runner.Run(append(slices.Clone(live), "stop", step.name)); err != nil {
		return fmt.Errorf("stopping %s %s: %s", step.live.Project, step.name, firstLine(output))
	}
	if err := executor.runner.Stream(up, os.Stdout); err != nil {
		executor.runner.Run(append(slices.Clone(compose), "rm", "--stop", "--force", step.name))
		if output, startErr := executor.runner.Run(append(slices.Clone(live), "start", step.name)); startErr != nil {
			return fmt.Errorf("%s %s never became healthy, and %s could not be started again either, so nothing is serving it: %s", step.target.Project, step.name, step.live.Project, firstLine(output))
		}
		return fmt.Errorf("%s %s never became healthy, so it was removed and %s is serving again", step.target.Project, step.name, step.live.Project)
	}
	if output, err := executor.runner.Run(append(slices.Clone(live), "rm", "--force", step.name)); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not remove the stopped %s from %s: %s\n", step.name, step.live.Project, firstLine(output))
	}
	fmt.Printf("  %s %s is up\n", step.target.Project, step.name)

	return nil
}

// projectsOf matches every compose project deploy can have created for one
// runtime id and nothing else. Another environment's runtime id always adds a
// dash segment that none of these patterns allows.
func projectsOf(id string) *regexp.Regexp {
	quoted := regexp.QuoteMeta("deploy-" + id)
	return regexp.MustCompile(`^` + quoted + `(-[0-9a-f]{7}(-tasks)?|-shared|_[a-z][a-z0-9-]*_[0-9a-f]{7})$`)
}

func composeProjectsOn(runner Runner, id string) ([]string, error) {
	output, err := runner.Run([]string{"docker", "ps", "--all", "--filter", "label=com.docker.compose.project", "--format", `{{.Label "com.docker.compose.project"}}`})
	if err != nil {
		return nil, fmt.Errorf("listing compose projects on %s: %s", runner.Describe(), firstLine(output))
	}
	pattern := projectsOf(id)
	var projects []string
	for _, project := range strings.Fields(string(output)) {
		if pattern.MatchString(project) && !slices.Contains(projects, project) {
			projects = append(projects, project)
		}
	}
	slices.Sort(projects)

	return projects, nil
}

// planServiceDestroy is DestroyPlan for a per-service deployment. Projects are
// found by name on the Docker engine rather than from the state alone, so a
// candidate or a release task left behind by an interrupted run goes too.
func planServiceDestroy(runner Runner, resolved ResolvedProject, layout Layout, destinationPath string, removeVolumes bool) (DestroyPlan, ServiceState, error) {
	state, err := inspectedServiceState(runner, resolved, layout)
	if err != nil {
		return DestroyPlan{}, ServiceState{}, err
	}
	onDisk, err := runner.ListDirectory(layout.Releases())
	if err != nil {
		return DestroyPlan{}, ServiceState{}, err
	}
	plan, err := PlanDestroy(resolved, layout, destinationPath, State{}, onDisk, removeVolumes)
	if err != nil {
		return DestroyPlan{}, ServiceState{}, err
	}
	if plan.Projects, err = composeProjectsOn(runner, resolved.RuntimeID()); err != nil {
		return DestroyPlan{}, ServiceState{}, err
	}

	return plan, state, nil
}

func runServiceDestroy(repositoryPath string, options DeployOptions, removeVolumes bool, confirmation io.Reader) (exitCode int, err error) {
	if options.Destination != "" {
		return exitPreconditionNotMet, errors.New("version 2 takes its destination from the config; --destination applies to version 1")
	}
	resolved, name, destination, err := loadServiceConfig(repositoryPath, options.Environment)
	if err != nil {
		return exitPreconditionNotMet, err
	}
	notifier, err := NewNotifier(repositoryPath, resolved.Notify)
	if err != nil {
		return exitPreconditionNotMet, err
	}

	runner, closeRunner, err := OpenRunner(destination)
	if err != nil {
		return exitPreconditionNotMet, err
	}
	absolute, err := AbsoluteDestination(runner, destination)
	if err != nil {
		closeRunner()
		return exitPreconditionNotMet, err
	}
	layout := NewLayout(absolute.Path, resolved.RuntimeID())
	plan, state, err := planServiceDestroy(runner, resolved, layout, absolute.Path, removeVolumes)
	closeRunner()
	if err != nil {
		return exitPreconditionNotMet, err
	}
	plan.Describe(os.Stdout)
	if err := ConfirmDestroy(plan, confirmation, os.Stdout); err != nil {
		return exitPreconditionNotMet, err
	}

	set, lockedLayout, err := lockServiceDestination(resolved, name, destination)
	if err != nil {
		return exitPreconditionNotMet, err
	}
	locked := set.Runner(0)
	// the plan was confirmed against a path that may be spelled differently
	// from the canonical one the lock resolved, so compare what it found
	updated, current, err := planServiceDestroy(locked, resolved, NewLayout(path.Dir(lockedLayout.Root), resolved.RuntimeID()), path.Dir(lockedLayout.Root), removeVolumes)
	if err != nil {
		set.Release()
		return exitPreconditionNotMet, err
	}
	if current.Generation != state.Generation || !slices.Equal(updated.Projects, plan.Projects) || !slices.Equal(updated.Releases, plan.Releases) {
		set.Release()
		return exitPreconditionNotMet, errors.New("deployment changed during confirmation; run destroy again to review the new plan")
	}
	if _, err := set.Begin("destroy", ""); err != nil {
		set.Release()
		return exitPreconditionNotMet, err
	}
	set.ReleaseOnSignal()
	defer set.Finish(&exitCode)

	if err := ExecuteDestroy(locked, lockedLayout, updated); err != nil {
		return exitDeployFailed, err
	}
	var affected []string
	for service, record := range current.Services {
		if !record.Stateful {
			affected = append(affected, service)
		}
	}
	slices.Sort(affected)
	notifier.SendStage(stageDowntime, resolved.Name, affected, StageVersions{})

	return exitOK, nil
}

// planServiceReuse is --affected per service: each service is compared with
// the commit it is actually running, not with one commit for the project.
func planServiceReuse(builder *Builder, resolved ResolvedProject, repositoryPath string, state ServiceState, commit string) (map[string]string, error) {
	claims, err := ServiceClaims(resolved)
	if err != nil {
		return nil, err
	}
	if err := CheckClaims(claims); err != nil {
		return nil, err
	}

	byCommit := map[string][]string{}
	for name := range claims {
		previous := ""
		if record, found := state.Services[name]; found && record.Current != nil && !record.Stateful {
			previous = record.Current.Commit
		} else if runs := state.Tasks[name]; len(runs) > 0 && runs[len(runs)-1].Outcome == outcomeCompleted {
			previous = runs[len(runs)-1].Commit
		}
		if previous == "" {
			fmt.Printf("  --affected: %s has no recorded release, so building it\n", name)
			continue
		}
		byCommit[previous] = append(byCommit[previous], name)
	}

	reuse := map[string]string{}
	for _, previous := range slices.Sorted(maps.Keys(byCommit)) {
		candidates, err := PlanReuse(builder, resolved, repositoryPath, previous, commit)
		if err != nil {
			return nil, err
		}
		for _, name := range byCommit[previous] {
			if from, found := candidates[name]; found {
				reuse[name] = from
			}
		}
	}

	return reuse, nil
}
