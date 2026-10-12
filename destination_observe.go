package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path"
	"regexp"
	"strings"
	"time"
)

type DestinationObservation struct {
	HostIdentity  string `json:"hostIdentity"`
	CanonicalPath string `json:"canonicalPath"`
	UserID        string `json:"userId"`
	OS            string `json:"os"`
	Architecture  string `json:"architecture"`
	DockerVersion string `json:"dockerVersion"`
	machineID     string
	bootID        string
	daemonID      string
}

// no directories, keys, sockets or known-host entries are created by this probe.
const destinationProbe = `set -eu
test "$(uname -s)" = Linux
if [ "$2" = remote ]; then cd "$HOME"; fi
target=$(realpath -m -- "$1")
ancestor=$target
while ! test -d "$ancestor"; do
  test ! -e "$ancestor"
  test ! -L "$ancestor"
  ancestor=$(dirname -- "$ancestor")
done
test -x "$ancestor"
printf '%s\n' "$target"
if test -r /etc/machine-id; then cat /etc/machine-id; else cat /var/lib/dbus/machine-id; fi
cat /proc/sys/kernel/random/boot_id
id -u
unset DOCKER_CONTEXT DOCKER_HOST DOCKER_TLS_VERIFY DOCKER_CERT_PATH DOCKER_API_VERSION
docker --host unix:///var/run/docker.sock info --format '{"id":{{json .ID}},"os":{{json .OSType}},"architecture":{{json .Architecture}},"version":{{json .ServerVersion}},"security":{{json .SecurityOptions}}}'
`

// boundedOutput caps what a destination can send back; zero means 16 KiB.
type boundedOutput struct {
	buffer bytes.Buffer
	limit  int
}

func (output *boundedOutput) Write(contents []byte) (int, error) {
	limit := output.limit
	if limit == 0 {
		limit = 16384
	}
	if len(contents) > limit-output.buffer.Len() {
		return 0, errors.New("observation exceeded output limit")
	}
	return output.buffer.Write(contents)
}

func (output *boundedOutput) String() string { return output.buffer.String() }

func observeDestination(ctx context.Context, destination Destination) (DestinationObservation, error) {
	if _, err := topologyDestination(destination.String()); err != nil {
		return DestinationObservation{}, err
	}
	command := []string{"sh", "-c", destinationProbe, "deploy-observe", destination.Path, "local"}
	if destination.IsRemote() {
		command[len(command)-1] = "remote"
	}
	output := boundedOutput{}
	if _, err := runObservation(ctx, destination, command, &output); err != nil {
		if errors.Is(err, errObservationCancelled) {
			return DestinationObservation{}, err
		}
		return DestinationObservation{}, errors.New("cannot observe destination; require trusted SSH access, Linux identity files, realpath, directory access and the local Docker socket")
	}
	return parseDestinationObservation(output.String())
}

var errObservationCancelled = errors.New("destination observation timed out or was cancelled")

// runObservation runs one read-only command with the strict probe options. It
// returns the exit status so callers can tell an expected refusal from a
// broken connection.
func runObservation(ctx context.Context, destination Destination, command []string, output *boundedOutput) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var process *exec.Cmd
	if destination.IsRemote() {
		process = exec.CommandContext(ctx, "ssh", append(observationSSHOptions(), destination.Host, ShellCommand(command))...)
	} else {
		process = exec.CommandContext(ctx, command[0], command[1:]...)
	}
	process.WaitDelay = time.Second
	process.Stdout, process.Stderr = output, io.Discard
	err := process.Run()
	if ctx.Err() != nil {
		return 0, errObservationCancelled
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), err
	}
	return 0, err
}

func observationSSHOptions() []string {
	return []string{"-T", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o", "UpdateHostKeys=no", "-o", "ControlMaster=no", "-o", "ControlPath=none", "-o", "ConnectTimeout=5", "-o", "ClearAllForwardings=yes", "-o", "ForwardAgent=no", "-o", "ForwardX11=no", "-o", "PermitLocalCommand=no"}
}

var machineIdentityPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)
var bootIdentityPattern = regexp.MustCompile(`^[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}$`)
var numericUserPattern = regexp.MustCompile(`^[0-9]+$`)

func parseDestinationObservation(raw string) (DestinationObservation, error) {
	lines := strings.Split(strings.TrimSuffix(raw, "\n"), "\n")
	if len(lines) != 5 {
		return DestinationObservation{}, errors.New("invalid destination observation framing")
	}
	var docker struct {
		ID           string   `json:"id"`
		OS           string   `json:"os"`
		Architecture string   `json:"architecture"`
		Version      string   `json:"version"`
		Security     []string `json:"security"`
	}
	if err := json.Unmarshal([]byte(lines[4]), &docker); err != nil {
		return DestinationObservation{}, errors.New("invalid Docker observation")
	}
	if !path.IsAbs(lines[0]) || path.Clean(lines[0]) != lines[0] || lines[0] == "/" || strings.ContainsAny(lines[0], "\x00\r") || !machineIdentityPattern.MatchString(lines[1]) || lines[1] == strings.Repeat("0", 32) || !bootIdentityPattern.MatchString(lines[2]) || !numericUserPattern.MatchString(lines[3]) {
		return DestinationObservation{}, errors.New("invalid destination path or host identity")
	}
	if docker.ID == "" || len(docker.ID) > 128 || strings.ContainsAny(docker.ID, "\x00\r\n") || docker.OS != "linux" || docker.Architecture == "" || docker.Version == "" {
		return DestinationObservation{}, errors.New("destination needs a local Linux Docker daemon with an identity")
	}
	for _, option := range docker.Security {
		if strings.Contains(option, "rootless") {
			return DestinationObservation{}, errors.New("rootless Docker is not supported by destination observation")
		}
	}
	digest := sha256.Sum256([]byte(lines[1]))
	return DestinationObservation{HostIdentity: hex.EncodeToString(digest[:]), CanonicalPath: lines[0], UserID: lines[3], OS: docker.OS, Architecture: docker.Architecture, DockerVersion: docker.Version, machineID: lines[1], bootID: lines[2], daemonID: docker.ID}, nil
}

func checkObservedIdentity(name string, current DestinationObservation, previous map[string]DestinationObservation) error {
	for otherName, other := range previous {
		if current.machineID == other.machineID && (current.bootID != other.bootID || current.daemonID != other.daemonID) {
			return fmt.Errorf("destinations %s and %s share a machine identity but disagree on boot or Docker identity; resolve cloned hosts or retry after restart", otherName, name)
		}
		if current.machineID != other.machineID && current.daemonID == other.daemonID {
			return fmt.Errorf("destinations %s and %s share a Docker identity across different hosts; resolve cloned or forwarded daemons", otherName, name)
		}
	}
	return nil
}
