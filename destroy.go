package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path"
	"reflect"
	"slices"
	"strings"
)

// projectPathWithin is the delete fence for destroy, and it is the same shape as
// the pruner's. It takes the root it is allowed to delete under as a parameter
// rather than reaching for one, which is both why it is safe and why it can be
// tested against a throwaway tree.
//
// NOTE: this is not the defensive validation CONTRIBUTING bans. Destroy removes
// more than anything else here, and the thing on the other side of this check is
// a sibling project somebody else is still running.
func projectPathWithin(destinationPath, projectID string) (string, error) {
	if !validRuntimeID(projectID) {
		return "", fmt.Errorf("refusing to remove %q, which is not a deploy id", projectID)
	}

	candidate := path.Join(destinationPath, projectID)
	if path.Clean(candidate) != candidate {
		return "", fmt.Errorf("refusing to remove %q, which does not resolve to a plain path", candidate)
	}
	if !strings.HasPrefix(candidate, destinationPath+"/") {
		return "", fmt.Errorf("refusing to remove %q, which is outside %s", candidate, destinationPath)
	}

	return candidate, nil
}

// DestroyPlan is everything destroy is about to remove, worked out before
// anything is removed so it can be shown to whoever has to confirm it.
type DestroyPlan struct {
	ProjectName     string
	ProjectID       string
	Root            string
	Releases        []string
	SharedProject   string
	Network         string
	Volumes         []string
	RemoveVolumes   bool
	KeptDirectories []string
	// Projects is set for a per-service deployment: every compose project deploy
	// created for it, found by name on the Docker engine
	Projects []string
}

func PlanDestroy(
	resolved ResolvedProject,
	layout Layout,
	destinationPath string,
	state State,
	onDisk []string,
	removeVolumes bool,
) (DestroyPlan, error) {
	root, err := projectPathWithin(destinationPath, resolved.RuntimeID())
	if err != nil {
		return DestroyPlan{}, err
	}

	// every release deploy knows about and every one still on disk, since a
	// half tidied project has stacks in both
	releases := slices.Clone(state.Releases)
	for _, release := range onDisk {
		if !slices.Contains(releases, release) {
			releases = append(releases, release)
		}
	}
	slices.Sort(releases)

	plan := DestroyPlan{
		ProjectName:   resolved.Name,
		ProjectID:     resolved.RuntimeID(),
		Root:          root,
		Releases:      releases,
		SharedProject: SharedProjectName(resolved.RuntimeID()),
		Network:       NetworkName(resolved.RuntimeID()),
		Volumes:       VolumesFor(resolved),
		RemoveVolumes: removeVolumes,
	}
	if !removeVolumes {
		plan.KeptDirectories = []string{layout.EnvDirectory(), path.Join(root, "volumes")}
	}

	return plan, nil
}

func (plan DestroyPlan) Describe(out io.Writer) {
	fmt.Fprintf(out, "about to destroy %s (%s) at %s\n\n", plan.ProjectName, plan.ProjectID, plan.Root)

	if plan.Projects != nil {
		fmt.Fprintf(out, "  stopping   %d compose project(s): %s\n", len(plan.Projects), strings.Join(plan.Projects, ", "))
	} else {
		fmt.Fprintf(out, "  stopping   %d release stack(s), and %s\n", len(plan.Releases), plan.SharedProject)
	}
	fmt.Fprintf(out, "  removing   images matching deploy-%s/*\n", plan.ProjectID)
	fmt.Fprintf(out, "  removing   network %s\n", plan.Network)
	fmt.Fprintf(out, "  removing   %s/releases and %s/shared\n", plan.Root, plan.Root)

	if plan.RemoveVolumes {
		if len(plan.Volumes) > 0 {
			fmt.Fprintf(out, "  DELETING   %s\n", strings.Join(plan.Volumes, ", "))
		}
		fmt.Fprintf(out, "  DELETING   %s, including any secrets in it\n", plan.Root)
		fmt.Fprintf(out, "\nthis removes the data too, and none of it comes back.\n")
	} else {
		for _, kept := range plan.KeptDirectories {
			fmt.Fprintf(out, "  keeping    %s\n", kept)
		}
		if len(plan.Volumes) > 0 {
			fmt.Fprintf(out, "  keeping    %s\n", strings.Join(plan.Volumes, ", "))
		}
		fmt.Fprintf(out, "\npass --volumes to remove the data as well.\n")
	}
}

// ConfirmDestroy asks for the project name rather than a yes or no, because a
// yes or no gets answered reflexively and a name has to be read first.
func ConfirmDestroy(plan DestroyPlan, in io.Reader, out io.Writer) error {
	fmt.Fprintf(out, "\ntype the project name to confirm (%s): ", plan.ProjectName)

	reader := bufio.NewReader(in)
	typed, err := reader.ReadString('\n')
	if err != nil && typed == "" {
		return fmt.Errorf("nothing typed, so nothing was destroyed")
	}

	if strings.TrimSpace(typed) != plan.ProjectName {
		return fmt.Errorf(
			"that is not %q, so nothing was destroyed", plan.ProjectName,
		)
	}

	return nil
}

func RunDestroy(options DeployOptions, removeVolumes bool, confirmation io.Reader) (exitCode int, err error) {
	var notifier *Notifier
	var projectName string
	var affected []string

	resolved, layout, runner, closeRunner, destination, err := openProject(options)
	if err != nil {
		return exitPreconditionNotMet, err
	}
	defer closeRunner()
	if resolved.Version == 2 {
		closeRunner()
		repositoryPath, _ := isServiceConfig(options)
		return runServiceDestroy(repositoryPath, options, removeVolumes, confirmation)
	}

	projectName = resolved.Name
	_, stateless := SplitServices(resolved.Services)
	affected = AffectedNames(stateless)

	// the repository is found again rather than threaded out of openProject,
	// because five other commands share that signature and none of them notify
	if repositoryPath, err := FindRepository(destroyStartPath(options)); err == nil {
		if notifier, err = NewNotifier(repositoryPath, resolved.Notify); err != nil {
			return exitPreconditionNotMet, err
		}
	}

	state, err := checkEnvironment(runner, resolved, layout)
	if err != nil {
		return exitPreconditionNotMet, err
	}

	onDisk, err := runner.ListDirectory(layout.Releases())
	if err != nil {
		return exitPreconditionNotMet, err
	}

	plan, err := PlanDestroy(resolved, layout, destination.Path, state, onDisk, removeVolumes)
	if err != nil {
		return exitPreconditionNotMet, err
	}

	plan.Describe(os.Stdout)
	if err := ConfirmDestroy(plan, confirmation, os.Stdout); err != nil {
		return exitPreconditionNotMet, err
	}

	lock, err := AcquireLock(runner, layout, "destroy", "", options.ForceUnlock)
	if err != nil {
		return exitPreconditionNotMet, err
	}
	lock.ReleaseOnSignal()
	defer lock.Finish(&exitCode)
	current, err := checkEnvironment(runner, resolved, layout)
	if err != nil {
		return exitPreconditionNotMet, err
	}
	entries, err := runner.ListDirectory(layout.Releases())
	if err != nil {
		return exitPreconditionNotMet, err
	}
	updated, err := PlanDestroy(resolved, layout, destination.Path, current, entries, removeVolumes)
	if err != nil {
		return exitPreconditionNotMet, err
	}
	if !reflect.DeepEqual(state, current) || !reflect.DeepEqual(plan, updated) {
		return exitPreconditionNotMet, fmt.Errorf("deployment changed during confirmation; run destroy again to review the new plan")
	}

	if err := ExecuteDestroy(runner, layout, plan); err != nil {
		return exitDeployFailed, err
	}

	// only once it is actually gone, and only that it is gone. whether the data
	// went with it is the operator's business and not something to broadcast
	notifier.SendStage(stageDowntime, projectName, affected, StageVersions{})

	return exitOK, nil
}

// destroyStartPath mirrors what openProject does with an empty context, so the
// notifier looks for secrets in the same repository the config came from.
func destroyStartPath(options DeployOptions) string {
	if options.Context != "" {
		return options.Context
	}

	working, err := os.Getwd()
	if err != nil {
		return "."
	}

	return working
}

// ExecuteDestroy takes the plan rather than working it out again, so what gets
// removed is exactly what was shown and confirmed.
func ExecuteDestroy(runner Runner, layout Layout, plan DestroyPlan) error {
	// compose finds a project's containers by its label, so no file is needed
	for _, project := range plan.Projects {
		if output, err := runner.Run([]string{"docker", "compose", "--project-name", project, "down"}); err != nil {
			return fmt.Errorf("stopping %s: %s", project, firstLine(output))
		}
	}
	if plan.Projects != nil {
		fmt.Printf("  stopped %d compose project(s)\n", len(plan.Projects))
	}
	for _, release := range plan.Releases {
		stopRelease(runner, plan.ProjectID, release, path.Join(layout.Release(release), composeFileName))
	}
	fmt.Printf("  stopped %d release stack(s)\n", len(plan.Releases))

	if _, err := runner.Run([]string{
		"docker", "compose",
		"--file", layout.SharedComposeFile(),
		"--project-name", plan.SharedProject,
		"down",
	}); err != nil {
		// a shared stack that was never brought up is the normal case here
		fmt.Printf("  shared stack was not running\n")
	} else {
		fmt.Printf("  stopped %s\n", plan.SharedProject)
	}

	removeProjectImages(runner, plan.ProjectID)

	if _, err := runner.Run([]string{"docker", "network", "rm", plan.Network}); err == nil {
		fmt.Printf("  removed network %s\n", plan.Network)
	}

	// releases and shared go whatever happens, since both are rebuildable from
	// the repository
	for _, directory := range []string{layout.Releases(), path.Dir(layout.SharedComposeFile())} {
		if err := runner.RemoveAll(directory); err != nil {
			return fmt.Errorf("removing %s: %w", directory, err)
		}
	}
	for _, file := range []string{layout.StateFile(), layout.ServiceStateFile()} {
		if err := runner.RemoveAll(file); err != nil {
			return fmt.Errorf("removing %s: %w", file, err)
		}
	}

	if !plan.RemoveVolumes {
		fmt.Printf("  removed %s/releases and %s/shared\n", plan.Root, plan.Root)
		fmt.Printf("  kept the volumes and %s\n", layout.EnvDirectory())

		return nil
	}

	for _, volume := range plan.Volumes {
		if _, err := runner.Run([]string{"docker", "volume", "rm", "--force", volume}); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not remove volume %s: %v\n", volume, err)
			continue
		}
		fmt.Printf("  removed volume %s\n", volume)
	}

	if err := runner.RemoveAll(plan.Root); err != nil {
		return fmt.Errorf("removing %s: %w", plan.Root, err)
	}
	fmt.Printf("  removed %s\n", plan.Root)

	return nil
}

func removeProjectImages(runner Runner, projectID string) {
	listed, err := runner.Run([]string{
		"docker", "images", "--filter", "reference=deploy-" + projectID + "/*", "--format", "{{.Repository}}:{{.Tag}}",
	})
	if err != nil {
		return
	}

	removed := 0
	for _, tag := range strings.Fields(string(listed)) {
		if _, err := runner.Run([]string{"docker", "image", "rm", tag}); err == nil {
			removed++
		} else {
			fmt.Fprintf(os.Stderr, "warning: could not remove image %s: %v\n", tag, err)
		}
	}
	if removed > 0 {
		fmt.Printf("  removed %d image(s)\n", removed)
	}
}
