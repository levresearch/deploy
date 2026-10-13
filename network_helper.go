package main

import (
	"bufio"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// networkHelper is the root helper deploy installs on each host of a managed
// network. It is embedded so the binary and the helper it expects can never
// come from different releases.
//
//go:embed network_helper.sh
var networkHelper []byte

const (
	networkHelperPath    = "/usr/local/libexec/deploy-network"
	networkHelperSudoers = "/etc/sudoers.d/deploy-network"
	networkHelperVersion = 1
)

// networkHelperVerbs are the only commands the sudoers rule allows. check and
// restore are left out: the first needs no root and the second is for systemd.
var networkHelperVerbs = []string{"version", "observe", "enroll", "apply", "remove"}

func networkHelperChecksum() string {
	digest := sha256.Sum256(networkHelper)
	return hex.EncodeToString(digest[:])
}

func networkHelperIdentity() string {
	return fmt.Sprintf("deploy-network %d %s", networkHelperVersion, networkHelperChecksum())
}

// HelperInput is what the helper reads on stdin for apply. It carries fields,
// not rendered files, so the helper renders everything it writes as root.
func (network HostNetwork) HelperInput(publicKeys map[string]string) (string, error) {
	if err := network.validate(); err != nil {
		return "", err
	}
	var input strings.Builder
	fmt.Fprintf(&input, "runtime %s\nenvironment %s\ncidr %s\naddress %s\nlisten %d\n",
		network.RuntimeID, network.Environment, network.CIDR, network.Address, network.ListenPort)
	for _, peer := range network.Peers {
		key := publicKeys[peer.Host]
		if !validWireGuardKey(key) {
			return "", fmt.Errorf("no valid public key for peer %s", peer.Host)
		}
		endpoint := peer.Endpoint
		if endpoint == "" {
			endpoint = "-"
		}
		fmt.Fprintf(&input, "peer %s %s %s %d\n", key, peer.Address, endpoint, peer.Keepalive)
	}
	for _, rule := range network.Access {
		fmt.Fprintf(&input, "access %s %d\n", rule.Source, rule.Port)
	}
	return input.String(), nil
}

// networkHelperInstall runs under interactive sudo on the host. It verifies the
// installed copy rather than the uploaded one, since the upload sits in a file
// the deploy user can still change.
const networkHelperInstall = `set -eu
source=$1 checksum=$2 helper=$3 sudoers=$4 verbs=$5
user=${SUDO_USER:-}
case $user in '' | root) echo 'run bootstrap as the deploy user, not as root' >&2; exit 2 ;; esac
printf '%s\n' "$user" | grep -Eqx '[a-z_][a-z0-9_-]{0,31}' || { echo "unsupported user name $user" >&2; exit 2; }
if ! command -v wg >/dev/null 2>&1; then
  DEBIAN_FRONTEND=noninteractive apt-get update -q
  DEBIAN_FRONTEND=noninteractive apt-get install -y -q wireguard-tools
fi
install -d -o root -g root -m 0755 "$(dirname "$helper")"
install -o root -g root -m 0755 "$source" "$helper.new"
if ! printf '%s  %s\n' "$checksum" "$helper.new" | sha256sum -c --status -; then
  rm -f "$helper.new"
  echo 'the uploaded helper does not match its checksum; nothing was installed' >&2
  exit 1
fi
mv -f "$helper.new" "$helper"
rules=$(mktemp)
allowed=''
for verb in $verbs; do allowed="$allowed${allowed:+, }$helper $verb"; done
printf '%s ALL=(root) NOPASSWD: %s\n' "$user" "$allowed" > "$rules"
visudo -cqf "$rules"
install -o root -g root -m 0440 "$rules" "$sudoers"
rm -f "$rules"
`

// networkHost is one host of the managed network, the destinations on it and
// the one used to reach it.
type networkHost struct {
	Name         string
	Target       string
	Destinations []string
	Network      HostNetwork
}

func networkHosts(resolved ResolvedProject) ([]networkHost, error) {
	networks, err := desiredNetworks(resolved)
	if err != nil {
		return nil, err
	}
	var hosts []networkHost
	for _, host := range sortedHosts(networks) {
		entry := networkHost{Name: host, Network: networks[host]}
		for _, name := range slices.Sorted(maps.Keys(resolved.Destinations)) {
			if _, peer := resolved.Network.Peers[name]; !peer || resolved.destinationHost(name) != host {
				continue
			}
			destination, err := ParseDestination(resolved.Destinations[name])
			if err != nil {
				return nil, err
			}
			if !destination.IsRemote() {
				return nil, fmt.Errorf("destination %s is local; the managed network is set up over ssh", name)
			}
			if entry.Target == "" {
				entry.Target = destination.Host
			}
			entry.Destinations = append(entry.Destinations, name)
		}
		hosts = append(hosts, entry)
	}
	return hosts, nil
}

func runNetwork(args []string) int {
	if len(args) < 1 || !slices.Contains([]string{"bootstrap", "status", "plan", "apply"}, args[0]) {
		fatal("usage: deploy network bootstrap|status|plan|apply")
		return exitPreconditionNotMet
	}
	flags := newFlagSet("network " + args[0])
	var context, environment string
	stringFlag(flags, &context, "context", "C", ".", "directory containing .deploy.json")
	stringFlag(flags, &environment, "environment", "e", defaultEnvironmentName, "environment whose network to use")
	if proceed, code := parseCommandFlags(flags, args[1:]); !proceed {
		return code
	}
	var exitCode int
	var err error
	switch args[0] {
	case "bootstrap":
		exitCode, err = RunNetworkBootstrap(context, environment, os.Stdin, os.Stdout)
	case "status", "plan":
		exitCode, err = RunNetworkInspect(context, environment, args[0], os.Stdout)
	case "apply":
		exitCode, err = RunNetworkApply(context, environment, os.Stdout)
	}
	if err != nil {
		fatal("%v", err)
	}
	return exitCode
}

// RunNetworkBootstrap installs or upgrades the helper and its sudoers rule on
// every host of the environment's managed network. It is the one step that
// needs the operator's own sudo, and it shows exactly what it installs first.
func RunNetworkBootstrap(context, environment string, in io.Reader, out io.Writer) (int, error) {
	project, err := loadPlanningProject(filepath.Join(context, configFileName))
	if err != nil {
		return exitPreconditionNotMet, err
	}
	if project.Version != 2 {
		return exitPreconditionNotMet, errors.New("a managed network needs a version 2 config")
	}
	if err := validatePlanningProject(project); err != nil {
		return exitPreconditionNotMet, err
	}
	resolved, err := project.ResolveEnvironment(environment)
	if err != nil {
		return exitPreconditionNotMet, err
	}
	hosts, err := networkHosts(resolved)
	if err != nil {
		return exitPreconditionNotMet, err
	}
	if len(hosts) == 0 {
		fmt.Fprintf(out, "%s needs no managed network, so there is nothing to bootstrap\n", resolved.Environment)
		return exitOK, nil
	}

	pending := []networkHost{}
	runners := map[string]*SSHRunner{}
	defer func() {
		for _, runner := range runners {
			runner.Close()
		}
	}()
	for _, host := range hosts {
		runner, err := NewSSHRunner(host.Target)
		if err != nil {
			return exitPreconditionNotMet, err
		}
		runners[host.Name] = runner
		if networkHelperInstalled(runner) {
			fmt.Fprintf(out, "%s: helper %s already installed\n", host.Name, networkHelperChecksum()[:12])
			continue
		}
		pending = append(pending, host)
	}
	if len(pending) == 0 {
		return exitOK, nil
	}

	for _, host := range pending {
		fmt.Fprintf(out, "\n%s (%s):\n", host.Name, host.Target)
		fmt.Fprintf(out, "  install  %s, version %d, sha256 %s\n", networkHelperPath, networkHelperVersion, networkHelperChecksum())
		fmt.Fprintf(out, "  write    %s, letting this ssh user run only: %s\n", networkHelperSudoers, strings.Join(networkHelperVerbs, ", "))
		fmt.Fprintf(out, "  install  wireguard-tools with apt, if wg is missing\n")
	}
	fmt.Fprintf(out, "\nsudo will ask for your password on each host. type bootstrap to continue: ")
	typed, err := bufio.NewReader(in).ReadString('\n')
	if strings.TrimSpace(typed) != "bootstrap" {
		if err != nil && typed == "" {
			return exitPreconditionNotMet, errors.New("nothing typed, so nothing was installed")
		}
		return exitPreconditionNotMet, errors.New("not confirmed, so nothing was installed")
	}

	for _, host := range pending {
		runner := runners[host.Name]
		if err := installNetworkHelper(runner); err != nil {
			return exitDeployFailed, fmt.Errorf("%s: %w", host.Name, err)
		}
		if !networkHelperInstalled(runner) {
			return exitDeployFailed, fmt.Errorf("%s: the helper was installed but sudo -n cannot run it; check %s", host.Name, networkHelperSudoers)
		}
		fmt.Fprintf(out, "%s: helper %s installed\n", host.Name, networkHelperChecksum()[:12])
	}
	return exitOK, nil
}

// networkHelperInstalled holds only when the installed helper is this release's
// and sudo runs it without a password.
func networkHelperInstalled(runner Runner) bool {
	output, err := runner.Run([]string{"sudo", "-n", networkHelperPath, "version"})
	return err == nil && strings.TrimSpace(string(output)) == networkHelperIdentity()
}

func installNetworkHelper(runner *SSHRunner) error {
	output, err := runner.Run([]string{"mktemp"})
	upload := strings.TrimSpace(string(output))
	if err != nil || !strings.HasPrefix(upload, "/") || strings.ContainsAny(upload, "\n'") {
		return fmt.Errorf("cannot create a temporary file: %s", firstLine(output))
	}
	defer runner.Run([]string{"rm", "-f", upload})
	if err := runner.WriteFile(upload, networkHelper); err != nil {
		return err
	}
	return runner.Interactive([]string{"sudo", "sh", "-c", networkHelperInstall, "deploy-network-bootstrap",
		upload, networkHelperChecksum(), networkHelperPath, networkHelperSudoers, strings.Join(networkHelperVerbs, " ")})
}
