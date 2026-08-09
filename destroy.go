package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path"
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
	if !projectIDPattern.MatchString(projectID) {
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
}

func PlanDestroy(
	resolved ResolvedProject,
	layout Layout,
	destinationPath string,
	state State,
	onDisk []string,
	removeVolumes bool,
) (DestroyPlan, error) {
	root, err := projectPathWithin(destinationPath, resolved.ID)
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
		ProjectID:     resolved.ID,
		Root:          root,
		Releases:      releases,
		SharedProject: SharedProjectName(resolved.ID),
		Network:       NetworkName(resolved.ID),
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

	fmt.Fprintf(out, "  stopping   %d release stack(s), and %s\n", len(plan.Releases), plan.SharedProject)
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

func RunDestroy(options DeployOptions, removeVolumes bool, confirmation io.Reader) (int, error) {
	resolved, layout, runner, closeRunner, destination, err := openProject(options)
	if err != nil {
		return exitPreconditionNotMet, err
	}
	defer closeRunner()

	state, err := ReadState(runner, layout)
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

	// the lock is taken so a destroy cannot interleave with a deploy, and forced
	// because a project being torn down is not a project to wait politely for
	lock, err := AcquireLock(runner, layout, "destroy", true)
	if err != nil {
		return exitPreconditionNotMet, err
	}
	defer lock.Release()

	return exitOK, ExecuteDestroy(runner, layout, plan)
}

// ExecuteDestroy takes the plan rather than working it out again, so what gets
// removed is exactly what was shown and confirmed.
func ExecuteDestroy(runner Runner, layout Layout, plan DestroyPlan) error {
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
	for _, file := range []string{layout.StateFile()} {
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
		"docker", "images", "--filter", "reference=deploy-" + projectID + "/*", "--quiet",
	})
	if err != nil {
		return
	}

	removed := 0
	for _, imageID := range strings.Fields(string(listed)) {
		if _, err := runner.Run([]string{"docker", "image", "rm", "--force", imageID}); err == nil {
			removed++
		}
	}
	if removed > 0 {
		fmt.Printf("  removed %d image(s)\n", removed)
	}
}
