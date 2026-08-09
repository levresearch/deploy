package main

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"strings"
)

// ShellQuote makes one argument survive being re-parsed by the remote shell.
// Everything goes in single quotes, and an embedded single quote is closed,
// escaped, and reopened, which is the only sequence bash treats literally.
func ShellQuote(argument string) string {
	return "'" + strings.ReplaceAll(argument, "'", `'\''`) + "'"
}

func ShellCommand(command []string) string {
	quoted := make([]string, len(command))
	for index, argument := range command {
		quoted[index] = ShellQuote(argument)
	}

	return strings.Join(quoted, " ")
}

// SSHRunner runs everything over one multiplexed connection. Opening it is also
// what proves the destination is reachable, so a bad host or a refused key fails
// before a release has been placed rather than halfway through one.
type SSHRunner struct {
	host        string
	controlPath string
}

func NewSSHRunner(host string) (*SSHRunner, error) {
	// the socket lives in a temp dir because a unix socket path is limited to
	// about 104 characters and a home directory can already be most of that
	socketDirectory, err := os.MkdirTemp("", "deploy-ssh-")
	if err != nil {
		return nil, err
	}

	runner := &SSHRunner{host: host, controlPath: path.Join(socketDirectory, "control")}

	open := exec.Command("ssh",
		"-o", "ControlMaster=yes",
		"-o", "ControlPath="+runner.controlPath,
		"-o", "ControlPersist=60s",
		"-o", "BatchMode=yes",
		"-N", "-f", host,
	)

	var failure bytes.Buffer
	open.Stderr = &failure

	if err := open.Run(); err != nil {
		os.RemoveAll(socketDirectory)

		return nil, fmt.Errorf(
			"cannot reach %s over ssh: %s", host, strings.TrimSpace(failure.String()),
		)
	}

	return runner, nil
}

func (runner *SSHRunner) Close() {
	exec.Command("ssh", "-o", "ControlPath="+runner.controlPath, "-O", "exit", runner.host).Run()
	os.RemoveAll(path.Dir(runner.controlPath))
}

func (runner *SSHRunner) Describe() string {
	return runner.host
}

// ssh builds an invocation that rides the connection already open, so no later
// step pays for another handshake.
func (runner *SSHRunner) ssh(remoteCommand string) *exec.Cmd {
	return exec.Command("ssh",
		"-o", "ControlPath="+runner.controlPath,
		"-o", "BatchMode=yes",
		runner.host, remoteCommand,
	)
}

func (runner *SSHRunner) Run(command []string) ([]byte, error) {
	return runner.ssh(ShellCommand(command)).CombinedOutput()
}

func (runner *SSHRunner) Stream(command []string, output io.Writer) error {
	process := runner.ssh(ShellCommand(command))
	process.Stdout = output
	process.Stderr = output

	return process.Run()
}

func (runner *SSHRunner) MkdirAll(directory string) error {
	if output, err := runner.Run([]string{"mkdir", "-p", directory}); err != nil {
		return fmt.Errorf("creating %s on %s: %s", directory, runner.host, firstLine(output))
	}

	return nil
}

func (runner *SSHRunner) WriteFile(remotePath string, contents []byte) error {
	process := runner.ssh("cat > " + ShellQuote(remotePath))
	process.Stdin = bytes.NewReader(contents)

	var failure bytes.Buffer
	process.Stderr = &failure

	if err := process.Run(); err != nil {
		return fmt.Errorf("writing %s on %s: %s", remotePath, runner.host, strings.TrimSpace(failure.String()))
	}

	return nil
}

// ReadFile reports a missing file as fs.ErrNotExist, because callers like
// ReadState tell "never deployed" from "something is wrong" that way, and over
// ssh both would otherwise arrive as exit status 1.
func (runner *SSHRunner) ReadFile(remotePath string) ([]byte, error) {
	process := runner.ssh("cat " + ShellQuote(remotePath))

	var contents, failure bytes.Buffer
	process.Stdout = &contents
	process.Stderr = &failure

	if err := process.Run(); err != nil {
		if strings.Contains(failure.String(), "No such file") {
			return nil, fs.ErrNotExist
		}

		return nil, fmt.Errorf("reading %s on %s: %s", remotePath, runner.host, strings.TrimSpace(failure.String()))
	}

	return contents.Bytes(), nil
}

func (runner *SSHRunner) ListDirectory(directory string) ([]string, error) {
	process := runner.ssh("ls -1 " + ShellQuote(directory))

	var listing, failure bytes.Buffer
	process.Stdout = &listing
	process.Stderr = &failure

	if err := process.Run(); err != nil {
		// a directory that is not there yet lists as nothing, the same as local
		if strings.Contains(failure.String(), "No such file") {
			return nil, nil
		}

		return nil, fmt.Errorf("listing %s on %s: %s", directory, runner.host, strings.TrimSpace(failure.String()))
	}

	return strings.Fields(listing.String()), nil
}

func (runner *SSHRunner) RemoveAll(target string) error {
	if output, err := runner.Run([]string{"rm", "-rf", target}); err != nil {
		return fmt.Errorf("removing %s on %s: %s", target, runner.host, firstLine(output))
	}

	return nil
}

func (runner *SSHRunner) Pipe(command []string, input io.Reader) error {
	process := runner.ssh(ShellCommand(command))
	process.Stdin = input

	var failure bytes.Buffer
	process.Stderr = &failure

	if err := process.Run(); err != nil {
		return fmt.Errorf(
			"%s on %s: %s", strings.Join(command, " "), runner.host, strings.TrimSpace(failure.String()),
		)
	}

	return nil
}
