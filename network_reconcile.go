package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	networkRecordFileName = "network.json"
	networkRecordVersion  = 1
)

var errNetworkHelperMissing = errors.New("the network helper is missing, outdated or not allowed by sudo; run deploy network bootstrap")

// helperCall runs one helper verb on a destination with input on stdin.
type helperCall func(ctx context.Context, destination Destination, verb, input string) (string, error)

// callNetworkHelper runs the helper through sudo -n with the strict probe SSH
// options. A refusal comes back as the helper's own reason.
func callNetworkHelper(ctx context.Context, destination Destination, verb, input string) (string, error) {
	if !destination.IsRemote() {
		return "", errors.New("the managed network is set up over ssh; local destinations are not supported")
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	command := ShellCommand([]string{"sudo", "-n", networkHelperPath, verb})
	process := exec.CommandContext(ctx, "ssh", append(observationSSHOptions(), destination.Host, command)...)
	process.WaitDelay = time.Second
	process.Stdin = strings.NewReader(input)
	output, failure := boundedOutput{limit: 65536}, boundedOutput{limit: 4096}
	process.Stdout, process.Stderr = &output, &failure
	err := process.Run()
	if ctx.Err() != nil {
		return "", fmt.Errorf("network helper %s timed out on %s", verb, destination.Host)
	}
	if err == nil {
		return output.String(), nil
	}
	return "", helperFailure(destination.Host, verb, strings.TrimSpace(failure.String()))
}

// helperFailure turns the helper's stderr into one error: a refusal, a helper
// that is missing or not allowed, or the first line of what failed together
// with the helper's own account of whether it put the host back.
func helperFailure(host, verb, reason string) error {
	outcome := ""
	for _, line := range strings.Split(reason, "\n") {
		if refused, found := strings.CutPrefix(line, "refused: "); found {
			return fmt.Errorf("%s refused %s: %s", host, verb, refused)
		}
		if failed, found := strings.CutPrefix(line, "failed: "); found {
			outcome = failed
		}
	}
	for _, missing := range []string{"password is required", "a terminal is required", "not allowed", "command not found", "No such file"} {
		if strings.Contains(reason, missing) {
			return fmt.Errorf("%s: %w", host, errNetworkHelperMissing)
		}
	}
	cause := firstLine([]byte(reason))
	if outcome != "" && !strings.HasPrefix(cause, "failed: ") {
		return fmt.Errorf("network helper %s failed on %s: %s; %s", verb, host, cause, outcome)
	}
	if outcome != "" {
		return fmt.Errorf("network helper %s failed on %s: %s", verb, host, outcome)
	}
	return fmt.Errorf("network helper %s failed on %s: %s", verb, host, cause)
}

// HostObservation is the helper's non-secret report on one host.
type HostObservation struct {
	Interface string            `json:"interface"`
	Owner     bool              `json:"owner"`
	PublicKey string            `json:"publicKey,omitempty"`
	WireGuard string            `json:"wireguard,omitempty"`
	Boot      string            `json:"boot,omitempty"`
	Link      bool              `json:"link"`
	Listen    int               `json:"listen,omitempty"`
	Peers     map[string]string `json:"peers,omitempty"`
	Routes    []string          `json:"routes,omitempty"`
	Firewall  string            `json:"firewall,omitempty"`
	Jumps     map[string]bool   `json:"jumps,omitempty"`
	Applied   string            `json:"applied,omitempty"`
}

var (
	observationTokenPattern = regexp.MustCompile(`^[!-~]{1,128}$`)
	digestPattern           = regexp.MustCompile(`^[0-9a-f]{64}$`)
	enrolledKeyPattern      = regexp.MustCompile(`(?m)^publicKey \S+$`)
)

// parseHostObservation accepts only what this release's helper prints, so a
// different helper is caught rather than half understood.
func parseHostObservation(raw string) (HostObservation, error) {
	lines := strings.Split(strings.TrimSuffix(raw, "\n"), "\n")
	if len(lines) == 0 || lines[0] != networkHelperIdentity() {
		return HostObservation{}, errNetworkHelperMissing
	}
	invalid := errors.New("invalid network helper observation")
	observed := HostObservation{Peers: map[string]string{}, Jumps: map[string]bool{}}
	for _, line := range lines[1:] {
		// systemctl prints nothing for a unit that does not exist, which leaves
		// an empty field, so fields rather than an exact split
		fields := strings.Fields(line)
		if len(fields) == 0 {
			return HostObservation{}, invalid
		}
		for _, field := range fields {
			if !observationTokenPattern.MatchString(field) {
				return HostObservation{}, invalid
			}
		}
		line = strings.Join(fields, " ")
		switch {
		case len(fields) == 2 && fields[0] == "interface":
			observed.Interface = fields[1]
		case line == "owner absent":
		case line == "owner present":
			observed.Owner = true
		case line == "publicKey absent":
		case len(fields) == 2 && fields[0] == "publicKey" && validWireGuardKey(fields[1]):
			observed.PublicKey = fields[1]
		case len(fields) <= 3 && fields[0] == "wireguard":
			observed.WireGuard = strings.Join(fields[1:], " ")
		case len(fields) <= 3 && fields[0] == "boot":
			observed.Boot = strings.Join(fields[1:], " ")
		case line == "link absent":
		case len(fields) == 2 && fields[0] == "listen":
			port, err := strconv.Atoi(fields[1])
			if err != nil || !validPort(port) {
				return HostObservation{}, invalid
			}
			observed.Listen, observed.Link = port, true
		case len(fields) == 1 && fields[0] == "listen":
			observed.Link = true
		case len(fields) >= 3 && fields[0] == "peer" && validWireGuardKey(fields[1]):
			observed.Peers[fields[1]] = strings.Join(fields[2:], " ")
		case len(fields) == 2 && fields[0] == "route":
			observed.Routes = append(observed.Routes, fields[1])
		case len(fields) == 2 && fields[0] == "firewall" && digestPattern.MatchString(fields[1]):
		case line == "firewall matches" || line == "firewall drifted":
			observed.Firewall = fields[1]
		case len(fields) == 3 && fields[0] == "jump" && (fields[2] == "present" || fields[2] == "absent"):
			observed.Jumps[fields[1]] = fields[2] == "present"
		case line == "applied none":
		case len(fields) == 2 && fields[0] == "applied" && digestPattern.MatchString(fields[1]):
			observed.Applied = fields[1]
		default:
			return HostObservation{}, invalid
		}
	}
	if !strings.HasPrefix(observed.Interface, "dw") {
		return HostObservation{}, invalid
	}
	return observed, nil
}

func helperInputDigest(input string) string {
	digest := sha256.Sum256([]byte(input))
	return hex.EncodeToString(digest[:])
}

func identityInput(resolved ResolvedProject) string {
	return fmt.Sprintf("runtime %s\nenvironment %s\n", resolved.RuntimeID(), resolved.Environment)
}

func interfaceInput(network HostNetwork) string {
	return fmt.Sprintf("runtime %s\nenvironment %s\ncidr %s\naddress %s\nlisten %d\n",
		network.RuntimeID, network.Environment, network.CIDR, network.Address, network.ListenPort)
}

// networkChanges lists why a host differs from its desired network. An empty
// list means apply would change nothing there.
func networkChanges(network HostNetwork, observed HostObservation, input string) []string {
	if !observed.Owner {
		return []string{"enroll"}
	}
	var changes []string
	if input == "" {
		changes = append(changes, "waiting for a peer to enroll")
	} else if observed.Applied != helperInputDigest(input) {
		changes = append(changes, "desired network differs from the applied one")
	}
	if observed.WireGuard != "enabled active" {
		changes = append(changes, "wireguard is "+observed.WireGuard)
	}
	if observed.Boot != "enabled active" {
		changes = append(changes, "boot unit is "+observed.Boot)
	}
	if !observed.Link {
		changes = append(changes, "interface is down")
	} else if observed.Listen != network.ListenPort {
		changes = append(changes, fmt.Sprintf("listening on %d", observed.Listen))
	}
	if observed.Firewall != "matches" {
		changes = append(changes, "firewall chains drifted")
	}
	for _, builtin := range []string{"INPUT", "DOCKER-USER", "OUTPUT"} {
		if !observed.Jumps[builtin] {
			changes = append(changes, "jump from "+builtin+" is missing")
		}
	}
	for _, peer := range network.Peers {
		if !slices.Contains(observed.Routes, peer.Address) && !slices.Contains(observed.Routes, peer.Address+"/32") {
			changes = append(changes, "route to "+peer.Address+" is missing")
		}
	}
	if observed.Link && len(observed.Peers) != len(network.Peers) {
		changes = append(changes, fmt.Sprintf("%d of %d peers configured", len(observed.Peers), len(network.Peers)))
	}
	return changes
}

// HostReport is one host's observation and what apply would do there.
type HostReport struct {
	Host         string          `json:"host"`
	Target       string          `json:"target"`
	Destinations []string        `json:"destinations"`
	Address      string          `json:"address"`
	ListenPort   int             `json:"listenPort"`
	Observed     HostObservation `json:"observed"`
	Changes      []string        `json:"changes"`
	input        string
}

// inspectNetwork observes every host and works out each one's helper input.
// The input needs every peer's public key, so it stays empty until all of
// them have enrolled.
func inspectNetwork(ctx context.Context, resolved ResolvedProject, hosts []networkHost, call helperCall) ([]HostReport, error) {
	reports := make([]HostReport, 0, len(hosts))
	keys := map[string]string{}
	for _, host := range hosts {
		raw, err := call(ctx, Destination{Host: host.Target}, "observe", identityInput(resolved))
		if err != nil {
			return nil, err
		}
		observed, err := parseHostObservation(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", host.Target, err)
		}
		if observed.Interface != host.Network.Interface {
			return nil, fmt.Errorf("%s reports interface %s, expected %s", host.Target, observed.Interface, host.Network.Interface)
		}
		if observed.Owner && observed.PublicKey == "" {
			return nil, fmt.Errorf("%s has lost the private key of %s; it needs a new identity, which deploy never creates silently", host.Target, observed.Interface)
		}
		if observed.Owner {
			keys[host.Name] = observed.PublicKey
		}
		reports = append(reports, HostReport{
			Host: host.Name, Target: host.Target, Destinations: host.Destinations,
			Address: host.Network.Address, ListenPort: host.Network.ListenPort, Observed: observed,
		})
	}
	for index, host := range hosts {
		if input, err := host.Network.HelperInput(keys); err == nil {
			reports[index].input = input
		}
		reports[index].Changes = networkChanges(host.Network, reports[index].Observed, reports[index].input)
	}
	return reports, nil
}

func (report HostReport) label() string {
	return fmt.Sprintf("%s (%s)", strings.Join(report.Destinations, ","), report.Target)
}

// networkContext observes every destination and resolves the environment with
// the observed host identities, so two aliases of one host count as one.
type networkContext struct {
	resolved     ResolvedProject
	hosts        []networkHost
	observations map[string]DestinationObservation
}

func loadNetworkContext(ctx context.Context, directory, environment string) (networkContext, error) {
	project, err := loadPlanningProject(filepath.Join(directory, configFileName))
	if err != nil {
		return networkContext{}, err
	}
	if project.Version != 2 {
		return networkContext{}, errors.New("a managed network needs a version 2 config")
	}
	observations, err := observePlanningProject(ctx, project, observeDestination)
	if err != nil {
		return networkContext{}, err
	}
	hosts := observedHostIDs(observations)
	if err := validateObservedProject(project, hosts); err != nil {
		return networkContext{}, err
	}
	resolved, err := project.ResolveEnvironment(environment)
	if err != nil {
		return networkContext{}, err
	}
	resolved.observedHosts = hosts
	networks, err := networkHosts(resolved)
	if err != nil {
		return networkContext{}, err
	}
	return networkContext{resolved: resolved, hosts: networks, observations: observations}, nil
}

// RunNetworkInspect is status and plan. Both only read: status prints what
// each host reports, plan only what apply would do.
func RunNetworkInspect(directory, environment, command string, out io.Writer) (int, error) {
	ctx := context.Background()
	network, err := loadNetworkContext(ctx, directory, environment)
	if err != nil {
		return exitPreconditionNotMet, err
	}
	if len(network.hosts) == 0 {
		fmt.Fprintf(out, "%s needs no managed network\n", network.resolved.Environment)
		return exitOK, nil
	}
	reports, err := inspectNetwork(ctx, network.resolved, network.hosts, callNetworkHelper)
	if err != nil {
		return exitPreconditionNotMet, err
	}
	fmt.Fprintf(out, "network %s for %s %s, %s\n", network.hosts[0].Network.Interface, network.resolved.Name, network.resolved.Environment, network.resolved.Network.CIDR)
	pending := 0
	for _, report := range reports {
		if len(report.Changes) > 0 {
			pending++
		}
		if command == "plan" {
			if len(report.Changes) == 0 {
				fmt.Fprintf(out, "  %s: unchanged\n", report.label())
			} else {
				fmt.Fprintf(out, "  %s: %s\n", report.label(), strings.Join(report.Changes, "; "))
			}
			continue
		}
		observed := report.Observed
		fmt.Fprintf(out, "\n%s %s, listen %d\n", report.label(), report.Address, report.ListenPort)
		if !observed.Owner {
			fmt.Fprintf(out, "  owner      none; apply enrolls it\n")
			continue
		}
		fmt.Fprintf(out, "  publicKey  %s\n", observed.PublicKey)
		fmt.Fprintf(out, "  wireguard  %s\n", observed.WireGuard)
		fmt.Fprintf(out, "  boot unit  %s\n", observed.Boot)
		fmt.Fprintf(out, "  peers      %d, routes %d\n", len(observed.Peers), len(observed.Routes))
		fmt.Fprintf(out, "  firewall   %s\n", observed.Firewall)
		if len(report.Changes) == 0 {
			fmt.Fprintf(out, "  state      matches the desired network\n")
		} else {
			fmt.Fprintf(out, "  state      %s\n", strings.Join(report.Changes, "; "))
		}
	}
	if command == "plan" {
		if pending == 0 {
			fmt.Fprintln(out, "nothing to change")
		} else {
			fmt.Fprintf(out, "deploy network apply would change %d of %d hosts\n", pending, len(reports))
		}
	}
	return exitOK, nil
}

// NetworkRecord is what a destination remembers of its host's network. The
// generation goes up on every apply that changed something there.
type NetworkRecord struct {
	Version     int       `json:"version"`
	Project     string    `json:"project"`
	Environment string    `json:"environment"`
	Destination string    `json:"destination"`
	Interface   string    `json:"interface"`
	Generation  int       `json:"generation"`
	Applied     string    `json:"applied"`
	Operation   string    `json:"operation"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

func (layout Layout) NetworkRecordFile() string {
	return path.Join(layout.Root, networkRecordFileName)
}

func (record NetworkRecord) validate() error {
	if record.Version != networkRecordVersion || !validRuntimeID(record.Project) || !validEnvironment(record.Environment) ||
		!validEnvironment(record.Destination) || record.Interface != interfaceName(record.Project) ||
		record.Generation < 1 || !digestPattern.MatchString(record.Applied) || record.Operation == "" {
		return errors.New("invalid network record")
	}
	return nil
}

func readNetworkRecord(reader stateReader, layout Layout) (NetworkRecord, bool, error) {
	raw, err := reader.ReadFile(layout.NetworkRecordFile())
	if errors.Is(err, fs.ErrNotExist) {
		return NetworkRecord{}, false, nil
	}
	if err != nil {
		return NetworkRecord{}, false, err
	}
	var record NetworkRecord
	if err := decodeOwned(raw, &record); err != nil {
		return NetworkRecord{}, true, fmt.Errorf("%s is unreadable: %w", layout.NetworkRecordFile(), err)
	}
	if err := record.validate(); err != nil {
		return NetworkRecord{}, true, fmt.Errorf("%s: %w", layout.NetworkRecordFile(), err)
	}
	return record, true, nil
}

func writeNetworkRecord(runner Runner, layout Layout, record NetworkRecord) error {
	if err := record.validate(); err != nil {
		return fmt.Errorf("refusing to write: %w", err)
	}
	encoded, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	if err := runner.MkdirAll(layout.Root); err != nil {
		return err
	}
	return writeStateFile(runner, layout.NetworkRecordFile(), append(encoded, '\n'))
}

// RunNetworkApply converges every host of the environment's network under the
// lock set of all its destinations. Hosts are applied one at a time in order,
// and a failure stops before the next host; apply again once it is fixed,
// since every step is safe to repeat.
func RunNetworkApply(directory, environment string, out io.Writer) (exitCode int, err error) {
	ctx := context.Background()
	network, err := loadNetworkContext(ctx, directory, environment)
	if err != nil {
		return exitPreconditionNotMet, err
	}
	resolved := network.resolved
	if len(network.hosts) == 0 {
		fmt.Fprintf(out, "%s needs no managed network\n", resolved.Environment)
		return exitOK, nil
	}
	// fail on a missing helper before taking any lock
	if _, err := inspectNetwork(ctx, resolved, network.hosts, callNetworkHelper); err != nil {
		return exitPreconditionNotMet, err
	}

	destinations := map[string]Destination{}
	generations := map[string]int{}
	for _, host := range network.hosts {
		for _, name := range host.Destinations {
			destination, err := topologyDestination(resolved.Destinations[name])
			if err != nil {
				return exitPreconditionNotMet, err
			}
			destinations[name] = destination
			state, _, err := ReadServiceState(observedReader{ctx: ctx, destination: destination}, NewLayout(network.observations[name].CanonicalPath, resolved.RuntimeID()))
			if err != nil {
				return exitPreconditionNotMet, fmt.Errorf("destination %s: %w", name, err)
			}
			generations[name] = state.Generation
		}
	}
	targets, err := lockOrder(destinations, network.observations)
	if err != nil {
		return exitPreconditionNotMet, err
	}
	set, err := acquireLockSet(ctx, targets, lockSetOptions{
		runtimeID: resolved.RuntimeID(), generations: generations, open: OpenRunner, observe: observeDestination,
	})
	if err != nil {
		return exitPreconditionNotMet, err
	}
	defer set.Finish(&exitCode)
	set.ReleaseOnSignal()

	runners := map[string]Runner{}
	for index, target := range targets {
		layout := NewLayout(target.Observed.CanonicalPath, resolved.RuntimeID())
		if _, err := checkEnvironment(set.Runner(index), resolved, layout); err != nil {
			return exitPreconditionNotMet, fmt.Errorf("destination %s: %w", target.Name, err)
		}
		runners[target.Name] = set.Runner(index)
	}
	operation, err := set.Begin("network apply", resolved.Environment)
	if err != nil {
		return exitPreconditionNotMet, err
	}

	// observe again under the locks; what was seen before them may be stale
	reports, err := inspectNetwork(ctx, resolved, network.hosts, callNetworkHelper)
	if err != nil {
		return exitDeployFailed, err
	}
	enrolled := false
	for _, report := range reports {
		if report.Observed.Owner {
			continue
		}
		host := network.hosts[slices.IndexFunc(network.hosts, func(host networkHost) bool { return host.Name == report.Host })]
		output, err := callNetworkHelper(ctx, Destination{Host: host.Target}, "enroll", interfaceInput(host.Network))
		if err != nil {
			return exitDeployFailed, err
		}
		if !enrolledKeyPattern.MatchString(output) {
			return exitDeployFailed, fmt.Errorf("%s: enroll returned no public key", host.Target)
		}
		fmt.Fprintf(out, "%s: enrolled %s\n", report.label(), host.Network.Interface)
		enrolled = true
	}
	if enrolled {
		if reports, err = inspectNetwork(ctx, resolved, network.hosts, callNetworkHelper); err != nil {
			return exitDeployFailed, err
		}
	}

	changedHosts := 0
	for index, report := range reports {
		host := network.hosts[index]
		if report.input == "" {
			return exitDeployFailed, fmt.Errorf("%s: a peer has no public key after enrolling", report.Target)
		}
		output, err := callNetworkHelper(ctx, Destination{Host: host.Target}, "apply", report.input)
		if err != nil {
			if index > 0 {
				err = fmt.Errorf("%w; hosts before it were applied, apply again once it is fixed", err)
			}
			return exitDeployFailed, err
		}
		changed := strings.TrimSpace(output) != "unchanged"
		if changed {
			changedHosts++
			fmt.Fprintf(out, "%s: %s\n", report.label(), strings.ReplaceAll(strings.TrimSpace(output), "\n", ", "))
		} else {
			fmt.Fprintf(out, "%s: unchanged\n", report.label())
		}

		digest := helperInputDigest(report.input)
		for _, name := range host.Destinations {
			layout := NewLayout(network.observations[name].CanonicalPath, resolved.RuntimeID())
			record, _, err := readNetworkRecord(runners[name], layout)
			if err != nil {
				return exitDeployFailed, fmt.Errorf("destination %s: %w", name, err)
			}
			if !changed && record.Applied == digest {
				continue
			}
			record = NetworkRecord{
				Version: networkRecordVersion, Project: resolved.RuntimeID(), Environment: resolved.Environment,
				Destination: name, Interface: host.Network.Interface, Generation: record.Generation + 1,
				Applied: digest, Operation: operation.ID, UpdatedAt: time.Now().UTC().Truncate(time.Second),
			}
			if err := writeNetworkRecord(runners[name], layout, record); err != nil {
				return exitDeployFailed, fmt.Errorf("destination %s: %w", name, err)
			}
		}
	}
	if changedHosts == 0 {
		fmt.Fprintln(out, "nothing changed")
	}
	return exitOK, nil
}
