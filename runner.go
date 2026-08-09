package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
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
	ReadFile(path string) ([]byte, error)
	ListDirectory(path string) ([]string, error)
	RemoveAll(path string) error
	// Pipe feeds a local stream into a command running wherever the runner runs,
	// which is how a release tar and an image tarball both get across.
	Pipe(command []string, input io.Reader) error
	// Interactive hands this terminal to a command on the other side, which a
	// shell is useless without.
	Interactive(command []string) error
}

// AbsoluteDestination expands the destination to a real path on the machine that
// will hold it. Everything deploy writes into a compose file is read back
// relative to that file unless it is absolute, so `git:Projects` resolving
// against a release directory instead of the home directory is the difference
// between an env file being found and a deploy that dies at the last step.
func AbsoluteDestination(runner Runner, destination Destination) (Destination, error) {
	expand := fmt.Sprintf("mkdir -p %s && cd %s && pwd", ShellQuote(destination.Path), ShellQuote(destination.Path))

	output, err := runner.Run([]string{"sh", "-c", expand})
	if err != nil {
		return destination, fmt.Errorf(
			"cannot use %s on %s: %s", destination.Path, runner.Describe(), firstLine(output),
		)
	}

	resolved := strings.TrimSpace(string(output))
	if resolved == "" {
		return destination, fmt.Errorf("%s on %s resolved to nothing", destination.Path, runner.Describe())
	}
	destination.Path = resolved

	return destination, nil
}

// OpenRunner picks where the work happens. The returned close is what tears down
// the ssh connection, and is a no-op locally.
func OpenRunner(destination Destination) (Runner, func(), error) {
	if !destination.IsRemote() {
		return LocalRunner{}, func() {}, nil
	}

	runner, err := NewSSHRunner(destination.Host)
	if err != nil {
		return nil, nil, err
	}

	return runner, runner.Close, nil
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

func (LocalRunner) ReadFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}

func (LocalRunner) ListDirectory(path string) ([]string, error) {
	entries, err := os.ReadDir(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}

	return names, nil
}

func (LocalRunner) RemoveAll(path string) error {
	return os.RemoveAll(path)
}

func (LocalRunner) Interactive(command []string) error {
	process := exec.Command(command[0], command[1:]...)
	process.Stdin = os.Stdin
	process.Stdout = os.Stdout
	process.Stderr = os.Stderr

	return process.Run()
}

func (LocalRunner) Pipe(command []string, input io.Reader) error {
	process := exec.Command(command[0], command[1:]...)
	process.Stdin = input

	var failure bytes.Buffer
	process.Stderr = &failure

	if err := process.Run(); err != nil {
		return fmt.Errorf("%s: %s", strings.Join(command, " "), strings.TrimSpace(failure.String()))
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
