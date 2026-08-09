package main

import (
	"fmt"
	"os"
	"path"
	"slices"
	"strings"
)

// ServiceLocation is where a service actually runs. Working this out is the whole
// value of these commands, because otherwise every one of them starts with
// remembering whether a service is stateful and what the current commit was.
type ServiceLocation struct {
	ProjectName string
	ComposeFile string
	Stateful    bool
}

func LocateService(
	resolved ResolvedProject,
	layout Layout,
	state State,
	serviceName string,
) (ServiceLocation, error) {
	service, found := resolved.Services[serviceName]
	if !found {
		if _, isTask := resolved.Release[serviceName]; isTask {
			return ServiceLocation{}, fmt.Errorf(
				"%q is a release task, which runs to completion and is gone. it is not something to attach to",
				serviceName,
			)
		}

		return ServiceLocation{}, fmt.Errorf(
			"no service called %q. this project has %s",
			serviceName, strings.Join(ServiceNames(resolved.Services), ", "),
		)
	}

	// a stateful service is a singleton in the shared stack, and a stateless one
	// belongs to whichever release is current
	if service.IsStateful() {
		return ServiceLocation{
			ProjectName: SharedProjectName(resolved.ID),
			ComposeFile: layout.SharedComposeFile(),
			Stateful:    true,
		}, nil
	}

	if state.Current == "" {
		return ServiceLocation{}, fmt.Errorf(
			"%s has never been deployed to this destination, so nothing is running", resolved.Name,
		)
	}

	return ServiceLocation{
		ProjectName: ProjectName(resolved.ID, state.Current),
		ComposeFile: path.Join(layout.Release(state.Current), composeFileName),
	}, nil
}

// ContainerStatus is what docker reports for one container, read through the
// compose project label so no compose file has to be found first.
type ContainerStatus struct {
	Service string
	Name    string
	Status  string
}

func containerStatuses(runner Runner, projectName string) ([]ContainerStatus, error) {
	output, err := runner.Run([]string{
		"docker", "ps", "--all",
		"--filter", "label=com.docker.compose.project=" + projectName,
		"--format", `{{.Label "com.docker.compose.service"}}\t{{.Names}}\t{{.Status}}`,
	})
	if err != nil {
		return nil, fmt.Errorf("reading container status on %s: %s", runner.Describe(), firstLine(output))
	}

	var statuses []ContainerStatus
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		columns := strings.Split(line, "\t")
		if len(columns) < 3 {
			continue
		}
		statuses = append(statuses, ContainerStatus{
			Service: columns[0], Name: columns[1], Status: columns[2],
		})
	}

	return statuses, nil
}

// openProject is the preamble every one of these commands shares. Read only, so
// it never takes the lock.
func openProject(options DeployOptions) (ResolvedProject, Layout, Runner, func(), Destination, error) {
	startPath := options.Context
	if startPath == "" {
		working, err := os.Getwd()
		if err != nil {
			return ResolvedProject{}, Layout{}, nil, nil, Destination{}, err
		}
		startPath = working
	}

	repositoryPath, err := FindRepository(startPath)
	if err != nil {
		return ResolvedProject{}, Layout{}, nil, nil, Destination{}, err
	}

	resolved, err := loadResolvedConfig(repositoryPath, options.Environment)
	if err != nil {
		return ResolvedProject{}, Layout{}, nil, nil, Destination{}, err
	}

	destinationText := options.Destination
	if destinationText == "" {
		destinationText = resolved.Destination
	}
	destination, err := ParseDestination(destinationText)
	if err != nil {
		return ResolvedProject{}, Layout{}, nil, nil, Destination{}, err
	}

	runner, closeRunner, err := OpenRunner(destination)
	if err != nil {
		return ResolvedProject{}, Layout{}, nil, nil, Destination{}, err
	}

	return resolved, NewLayout(destination.Path, resolved.ID), runner, closeRunner, destination, nil
}

func RunStatus(options DeployOptions) (int, error) {
	resolved, layout, runner, closeRunner, destination, err := openProject(options)
	if err != nil {
		return exitPreconditionNotMet, err
	}
	defer closeRunner()

	state, err := ReadState(runner, layout)
	if err != nil {
		return exitPreconditionNotMet, err
	}
	if state.Current == "" {
		fmt.Printf("%s has never been deployed to %s\n", resolved.Name, destination)
		return exitOK, nil
	}

	fmt.Printf("%s on %s\n", resolved.Name, destination)
	fmt.Printf("  release      %s", state.Current)
	if state.Previous != "" {
		fmt.Printf(" (previous %s)", state.Previous)
	}
	fmt.Printf("\n  environment  %s\n", state.Environment)
	if !state.UpdatedAt.IsZero() {
		fmt.Printf("  deployed     %s\n", state.UpdatedAt.Format("2006-01-02 15:04:05 MST"))
	}

	stateful, stateless := SplitServices(resolved.Services)

	fmt.Printf("\n  release stack, %s\n", ProjectName(resolved.ID, state.Current))
	printServiceStatuses(runner, ProjectName(resolved.ID, state.Current), stateless)

	if len(stateful) > 0 {
		fmt.Printf("\n  shared stack, %s\n", SharedProjectName(resolved.ID))
		printServiceStatuses(runner, SharedProjectName(resolved.ID), stateful)
	}

	if hosted := HostedServices(resolved.Services); len(hosted) > 0 {
		fmt.Printf("\n  exposed\n")
		for _, name := range hosted {
			host := resolved.Services[name].Host
			fmt.Printf("    %-12s %s -> %s:%d\n", name, host.Domain, name, host.Port)
		}
		// saying so beats leaving someone to infer it from a tunnel that never moves
		fmt.Printf("    (the tunnel cutover is not implemented yet, so these are wired by hand)\n")
	}

	return exitOK, nil
}

func printServiceStatuses(runner Runner, projectName string, services map[string]Service) {
	if len(services) == 0 {
		fmt.Printf("    (none)\n")
		return
	}

	statuses, err := containerStatuses(runner, projectName)
	if err != nil {
		fmt.Printf("    (could not read status: %v)\n", err)
		return
	}

	byService := map[string]ContainerStatus{}
	for _, status := range statuses {
		byService[status.Service] = status
	}

	for _, name := range ServiceNames(services) {
		status, running := byService[name]
		if !running {
			fmt.Printf("    %-12s not running\n", name)
			continue
		}
		fmt.Printf("    %-12s %s\n", name, status.Status)
	}
}

func RunLogs(options DeployOptions, serviceName string, follow bool) (int, error) {
	resolved, layout, runner, closeRunner, _, err := openProject(options)
	if err != nil {
		return exitPreconditionNotMet, err
	}
	defer closeRunner()

	state, err := ReadState(runner, layout)
	if err != nil {
		return exitPreconditionNotMet, err
	}

	location, err := LocateService(resolved, layout, state, serviceName)
	if err != nil {
		return exitPreconditionNotMet, err
	}

	command := []string{
		"docker", "compose",
		"--file", location.ComposeFile,
		"--project-name", location.ProjectName,
		"logs", serviceName,
	}
	if follow {
		command = append(command, "--follow")
	}

	// interactive rather than streamed, because following logs is something you
	// stop with ctrl-c and that needs the terminal
	if err := runner.Interactive(command); err != nil {
		return exitDeployFailed, fmt.Errorf("reading logs for %s: %w", serviceName, err)
	}

	return exitOK, nil
}

// RunShell is the one that turns this from a deploy script into something
// livable. It works out which compose project holds the service so nobody has to
// remember, and tries bash before sh because half these images are alpine.
func RunShell(options DeployOptions, serviceName string) (int, error) {
	return runInsideContainer(options, serviceName, nil)
}

func RunExec(options DeployOptions, serviceName string, command []string) (int, error) {
	if len(command) == 0 {
		return exitPreconditionNotMet, fmt.Errorf("nothing to run, try deploy exec %s -- <command>", serviceName)
	}

	return runInsideContainer(options, serviceName, command)
}

func runInsideContainer(options DeployOptions, serviceName string, command []string) (int, error) {
	resolved, layout, runner, closeRunner, _, err := openProject(options)
	if err != nil {
		return exitPreconditionNotMet, err
	}
	defer closeRunner()

	state, err := ReadState(runner, layout)
	if err != nil {
		return exitPreconditionNotMet, err
	}

	location, err := LocateService(resolved, layout, state, serviceName)
	if err != nil {
		return exitPreconditionNotMet, err
	}

	inside := command
	if len(inside) == 0 {
		// bash where it exists, sh where it does not, decided inside the container
		// rather than by guessing from the image name
		inside = []string{"sh", "-c", "command -v bash >/dev/null && exec bash || exec sh"}
	}

	full := append([]string{
		"docker", "compose",
		"--file", location.ComposeFile,
		"--project-name", location.ProjectName,
		"exec", serviceName,
	}, inside...)

	if err := runner.Interactive(full); err != nil {
		return exitDeployFailed, fmt.Errorf("running in %s: %w", serviceName, err)
	}

	return exitOK, nil
}

// RunList answers "what is on this box", which no other command can, because
// every other one starts from a config in a repository. Deploy ids are opaque
// hex, so without this a busy destination is a directory of hashes.
func RunList(options DeployOptions) (int, error) {
	resolved, _, runner, closeRunner, destination, err := openProject(options)
	if err != nil {
		return exitPreconditionNotMet, err
	}
	defer closeRunner()

	projectIDs, err := runner.ListDirectory(destination.Path)
	if err != nil {
		return exitPreconditionNotMet, err
	}

	type entry struct {
		id    string
		state State
	}

	var found []entry
	for _, projectID := range slices.Sorted(slices.Values(projectIDs)) {
		if !projectIDPattern.MatchString(projectID) {
			continue
		}
		state, err := ReadState(runner, NewLayout(destination.Path, projectID))
		if err != nil || state.Current == "" {
			continue
		}
		found = append(found, entry{id: projectID, state: state})
	}

	if len(found) == 0 {
		fmt.Printf("nothing deployed to %s yet\n", destination)
		return exitOK, nil
	}

	fmt.Printf("%-14s %-10s %-12s %-9s %s\n", "PROJECT", "ID", "ENVIRONMENT", "RELEASE", "DEPLOYED")
	for _, project := range found {
		name := project.state.Name
		if name == "" {
			name = "(unnamed)"
		}
		here := ""
		if project.id == resolved.ID {
			here = "  <- this one"
		}

		deployed := "unknown"
		if !project.state.UpdatedAt.IsZero() {
			deployed = project.state.UpdatedAt.Format("2006-01-02 15:04")
		}

		fmt.Printf("%-14s %-10s %-12s %-9s %s%s\n",
			name, project.id, project.state.Environment, project.state.Current, deployed, here)
	}

	return exitOK, nil
}
