package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// Destination is where a project gets deployed. Local and remote are told apart
// the way scp and rsync do it, which is that a colon before the first slash means
// remote.
type Destination struct {
	Host string
	Path string
}

func (destination Destination) IsRemote() bool {
	return destination.Host != ""
}

func (destination Destination) String() string {
	if destination.IsRemote() {
		return destination.Host + ":" + destination.Path
	}

	return destination.Path
}

func ParseDestination(raw string) (Destination, error) {
	if raw == "" {
		return Destination{}, errors.New("no destination, pass -D or set destination in " + configFileName)
	}
	if strings.HasPrefix(raw, "ssh://") {
		return Destination{}, fmt.Errorf("destination %q uses a url, write it as host:path instead", raw)
	}

	colon := strings.Index(raw, ":")
	slash := strings.Index(raw, "/")
	if colon <= 0 || (slash >= 0 && slash < colon) {
		return Destination{Path: raw}, nil
	}

	host, path := raw[:colon], raw[colon+1:]
	if path == "" {
		return Destination{}, fmt.Errorf("destination %q names a host but no path", raw)
	}

	return Destination{Host: host, Path: path}, nil
}

// Runner is where a command actually runs. Local today, ssh in task 6, and that
// second implementation is what makes this worth being an interface. It is also
// what lets every path above it be tested without a server.
type Runner interface {
	Describe() string
	Run(command []string) ([]byte, error)
	Stream(command []string, output io.Writer) error
	MkdirAll(path string) error
	WriteFile(path string, contents []byte) error
	ExtractTar(directory string, archive io.Reader) error
}

type LocalRunner struct{}

func (LocalRunner) Describe() string {
	return "this machine"
}

func (LocalRunner) Run(command []string) ([]byte, error) {
	return exec.Command(command[0], command[1:]...).CombinedOutput()
}

func (LocalRunner) Stream(command []string, output io.Writer) error {
	process := exec.Command(command[0], command[1:]...)
	process.Stdout = output
	process.Stderr = output

	return process.Run()
}

func (LocalRunner) MkdirAll(path string) error {
	return os.MkdirAll(path, 0o755)
}

func (LocalRunner) WriteFile(path string, contents []byte) error {
	return os.WriteFile(path, contents, 0o644)
}

func (LocalRunner) ExtractTar(directory string, archive io.Reader) error {
	extract := exec.Command("tar", "-x", "-C", directory)
	extract.Stdin = archive

	var failure bytes.Buffer
	extract.Stderr = &failure

	if err := extract.Run(); err != nil {
		return fmt.Errorf("extracting into %s: %s", directory, strings.TrimSpace(failure.String()))
	}

	return nil
}

// requiredCommands are checked as a group so one run reports everything missing
// rather than sending someone back around the loop per package.
var requiredCommands = []struct {
	packageName string
	probe       []string
}{
	{"git", []string{"git", "--version"}},
	{"tar", []string{"tar", "--version"}},
	{"docker", []string{"docker", "--version"}},
	{"docker compose plugin", []string{"docker", "compose", "version"}},
}

func CheckRequirements(runner Runner) error {
	var missing []string
	for _, requirement := range requiredCommands {
		if _, err := runner.Run(requirement.probe); err != nil {
			missing = append(missing, requirement.packageName)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf(
			"Deploy needs you to install the following packages on %s in order to operate: %s",
			runner.Describe(), strings.Join(missing, ", "),
		)
	}

	// a present binary with an unreachable daemon is a different failure, and the
	// more common one, since it catches a stopped daemon and a user who is not in
	// the docker group
	if output, err := runner.Run([]string{"docker", "info"}); err != nil {
		return fmt.Errorf(
			"docker is installed on %s but its daemon is not reachable: %s",
			runner.Describe(), firstLine(output),
		)
	}

	return nil
}

func firstLine(output []byte) string {
	text := strings.TrimSpace(string(output))
	if index := strings.Index(text, "\n"); index >= 0 {
		return text[:index]
	}

	return text
}
