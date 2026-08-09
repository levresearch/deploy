package main

import (
	"fmt"
	"maps"
	"path"
	"slices"
	"strconv"
	"strings"
)

// freeSpaceFloorMB is what the destination has to have left before a deploy is
// allowed to start. Running out halfway leaves a half extracted release and a
// held lock, and one df is much cheaper than that cleanup.
const freeSpaceFloorMB = 2048

// DestinationFacts is what preflight learned about the machine that will run the
// containers, and what later steps need in order to target it.
type DestinationFacts struct {
	Architecture string
	DataRoot     string
	FreeSpaceMB  int
}

// CheckLocalRequirements covers the machine deploy was invoked on. Docker is
// here rather than only on the destination because builds happen locally.
func CheckLocalRequirements(runner Runner, destination Destination) error {
	required := []struct {
		packageName string
		probe       []string
	}{
		{"git", []string{"git", "--version"}},
		{"tar", []string{"tar", "--version"}},
		{"docker", []string{"docker", "--version"}},
	}
	if destination.IsRemote() {
		required = append(required, struct {
			packageName string
			probe       []string
		}{"ssh", []string{"ssh", "-V"}})
	}

	var missing []string
	for _, requirement := range required {
		if _, err := runner.Run(requirement.probe); err != nil {
			missing = append(missing, requirement.packageName)
		}
	}
	if len(missing) > 0 {
		return missingPackages("this machine", missing)
	}

	if output, err := runner.Run([]string{"docker", "info"}); err != nil {
		return fmt.Errorf(
			"docker is installed on this machine but its daemon is not reachable, and builds happen here: %s",
			firstLine(output),
		)
	}

	return nil
}

// CheckDestination runs on whichever machine will actually run the containers,
// which is this one for a local path and the remote one over the connection just
// opened. Everything it reports names that host, since the first question anyone
// asks is which machine is missing something.
func CheckDestination(runner Runner, resolved ResolvedProject, layout Layout) (DestinationFacts, error) {
	host := runner.Describe()

	needed := [][2]string{
		{"docker", "docker --version"},
		{"docker compose plugin", "docker compose version"},
	}
	if len(HostedServices(resolved.Services)) > 0 {
		// only required when something is actually exposed, so a project with no
		// host block is never blocked on it
		needed = append(needed, [2]string{"cloudflared", "cloudflared --version"})
	}

	missing, err := missingOn(runner, needed)
	if err != nil {
		return DestinationFacts{}, err
	}
	if len(missing) > 0 {
		return DestinationFacts{}, missingPackages(host, missing)
	}

	if output, err := runner.Run([]string{"docker", "info"}); err != nil {
		return DestinationFacts{}, fmt.Errorf(
			"docker is installed on %s but its daemon is not reachable, which is usually a stopped daemon or a user outside the docker group: %s",
			host, firstLine(output),
		)
	}

	facts, err := readDestinationFacts(runner)
	if err != nil {
		return DestinationFacts{}, err
	}
	if facts.FreeSpaceMB > 0 && facts.FreeSpaceMB < freeSpaceFloorMB {
		return facts, fmt.Errorf(
			"%s has only %d MB free on %s, which holds docker's images, and deploy wants at least %d MB. running out mid deploy leaves a half placed release behind",
			host, facts.FreeSpaceMB, facts.DataRoot, freeSpaceFloorMB,
		)
	}

	if err := checkEnvFiles(runner, resolved, layout); err != nil {
		return facts, err
	}

	return facts, nil
}

// missingOn asks once for everything rather than paying a round trip per binary,
// which over ssh is the difference between one hop and five.
func missingOn(runner Runner, needed [][2]string) ([]string, error) {
	var script strings.Builder
	for _, requirement := range needed {
		fmt.Fprintf(&script, "%s >/dev/null 2>&1 || echo %s\n", requirement[1], ShellQuote(requirement[0]))
	}

	output, err := runner.Run([]string{"sh", "-c", script.String()})
	if err != nil {
		return nil, fmt.Errorf("checking requirements on %s: %s", runner.Describe(), firstLine(output))
	}

	var missing []string
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			missing = append(missing, line)
		}
	}

	return missing, nil
}

func readDestinationFacts(runner Runner) (DestinationFacts, error) {
	architecture, err := runner.Run([]string{"docker", "version", "--format", "{{.Server.Arch}}"})
	if err != nil {
		return DestinationFacts{}, fmt.Errorf(
			"cannot read the architecture of %s: %s", runner.Describe(), firstLine(architecture),
		)
	}

	facts := DestinationFacts{Architecture: strings.TrimSpace(string(architecture))}

	// the data root rather than the project directory, since that is where images
	// and container layers land and where a Pi actually runs out of room
	dataRoot, err := runner.Run([]string{"docker", "info", "--format", "{{.DockerRootDir}}"})
	if err != nil {
		return facts, nil
	}
	facts.DataRoot = strings.TrimSpace(string(dataRoot))
	facts.FreeSpaceMB = freeSpaceMB(runner, facts.DataRoot)

	return facts, nil
}

// freeSpaceMB reports 0 when it cannot tell, so an unreadable df is not treated
// as a full disk.
func freeSpaceMB(runner Runner, target string) int {
	if target == "" {
		return 0
	}

	output, err := runner.Run([]string{"df", "-Pk", target})
	if err != nil {
		return 0
	}

	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) < 2 {
		return 0
	}

	columns := strings.Fields(lines[len(lines)-1])
	if len(columns) < 4 {
		return 0
	}

	availableKB, err := strconv.Atoi(columns[3])
	if err != nil {
		return 0
	}

	return availableKB / 1024
}

// checkEnvFiles fails here rather than at container start, where the error would
// be compose complaining about a path with no explanation of where the file was
// meant to come from.
func checkEnvFiles(runner Runner, resolved ResolvedProject, layout Layout) error {
	wanted := map[string]bool{}
	for _, service := range resolved.Services {
		collectProjectEnvFiles(service, wanted)
	}
	for _, task := range resolved.Release {
		collectProjectEnvFiles(task, wanted)
	}
	if len(wanted) == 0 {
		return nil
	}

	present, err := runner.ListDirectory(layout.EnvDirectory())
	if err != nil {
		return err
	}

	var absent []string
	for _, name := range slices.Sorted(maps.Keys(wanted)) {
		if !slices.Contains(present, name) {
			absent = append(absent, name)
		}
	}
	if len(absent) > 0 {
		return fmt.Errorf(
			"%s is missing %s under %s. put them there with deploy env push, since a gitignored env file never travels with the code",
			runner.Describe(), strings.Join(absent, ", "), layout.EnvDirectory(),
		)
	}

	return nil
}

func collectProjectEnvFiles(service Service, wanted map[string]bool) {
	for _, entry := range service.Env {
		// a bare name is one deploy placed in the env directory, and anything
		// written as a path came with the code and is not ours to check
		if !strings.ContainsRune(entry, '/') {
			wanted[entry] = true
		}
	}
}

func missingPackages(host string, missing []string) error {
	return fmt.Errorf(
		"Deploy needs you to install the following packages on %s in order to operate: %s",
		host, strings.Join(missing, ", "),
	)
}

// HostedServices names everything exposed through a tunnel, which is what decides
// whether cloudflared is required at all.
func HostedServices(services map[string]Service) []string {
	var hosted []string
	for _, name := range ServiceNames(services) {
		if services[name].Host != nil {
			hosted = append(hosted, name)
		}
	}

	return hosted
}

// PushEnvFile puts one env file where the rendered compose expects it, with
// permissions that match what it holds.
func PushEnvFile(runner Runner, layout Layout, name string, contents []byte) error {
	if err := runner.MkdirAll(layout.EnvDirectory()); err != nil {
		return err
	}

	remotePath := path.Join(layout.EnvDirectory(), name)
	if err := runner.WriteFile(remotePath, contents); err != nil {
		return err
	}
	if _, err := runner.Run([]string{"chmod", "600", remotePath}); err != nil {
		return fmt.Errorf("tightening permissions on %s: %w", remotePath, err)
	}

	return nil
}
