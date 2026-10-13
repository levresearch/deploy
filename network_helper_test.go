package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runHelper runs the embedded helper as the current user, which is enough for
// the verbs that need no root.
func runHelper(t *testing.T, input string, args ...string) (string, string, error) {
	t.Helper()
	script := filepath.Join(t.TempDir(), "deploy-network")
	if err := os.WriteFile(script, networkHelper, 0o755); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	command := exec.Command("bash", append([]string{script}, args...)...)
	command.Stdin = strings.NewReader(input)
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	return stdout.String(), stderr.String(), err
}

func helperNetworks(t *testing.T) map[string]HostNetwork {
	t.Helper()
	networks, err := desiredNetworks(topologyResolved(t))
	if err != nil {
		t.Fatal(err)
	}
	return networks
}

func TestTheHelperRendersExactlyWhatDeployRenders(t *testing.T) {
	keys := map[string]string{"app-host": testKey(1), "data-host": testKey(2)}
	for host, network := range helperNetworks(t) {
		input, err := network.HelperInput(keys)
		if err != nil {
			t.Fatal(err)
		}
		output, failure, err := runHelper(t, input, "check")
		if err != nil {
			t.Fatalf("%s: %v %s", host, err, failure)
		}
		config, err := network.WireGuardConfig(keys)
		if err != nil {
			t.Fatal(err)
		}
		if want := config + "%%\n" + network.FirewallRules(); output != want {
			t.Fatalf("%s: helper rendered\n%s\ndeploy rendered\n%s", host, output, want)
		}
	}
}

func TestTheHelperRefusesAnythingOutsideItsGrammar(t *testing.T) {
	data := helperNetworks(t)["data-host"]
	valid, err := data.HelperInput(map[string]string{"app-host": testKey(1)})
	if err != nil {
		t.Fatal(err)
	}
	if _, failure, err := runHelper(t, valid, "check"); err != nil {
		t.Fatalf("the valid input was refused: %s", failure)
	}
	key := testKey(1)
	replace := func(old, replacement string) string { return strings.Replace(valid, old, replacement, 1) }
	cases := map[string]string{
		"unknown field":           valid + "command id\n",
		"field given twice":       valid + "listen 51821\n",
		"double space":            replace("listen 51820", "listen  51820"),
		"trailing space":          replace("listen 51820", "listen 51820 "),
		"tab":                     replace("listen 51820", "listen\t51820"),
		"shell in an endpoint":    replace("peer "+key+" 10.77.0.1 -", "peer "+key+" 10.77.0.1 $(id):1"),
		"bad endpoint port":       replace("peer "+key+" 10.77.0.1 -", "peer "+key+" 10.77.0.1 host:99999"),
		"short key":               replace(key, key[:40]+"=="),
		"key with bad padding":    replace(key, key[:42]+"B="),
		"peer outside the cidr":   replace("10.77.0.1 -", "10.78.0.1 -"),
		"peer is the host itself": replace("10.77.0.1 -", "10.77.0.2 -"),
		"broadcast peer":          replace("10.77.0.1 -", "10.77.0.255 -"),
		"public cidr":             strings.ReplaceAll(valid, "10.77.0.", "11.77.0."),
		"non canonical cidr":      replace("cidr 10.77.0.0/24", "cidr 10.77.0.1/24"),
		"leading zero bits":       replace("cidr 10.77.0.0/24", "cidr 10.77.0.0/024"),
		"leading zero octet":      replace("address 10.77.0.2", "address 10.77.0.02"),
		"wide cidr":               replace("cidr 10.77.0.0/24", "cidr 10.77.0.0/31"),
		"zero listen port":        replace("listen 51820", "listen 0"),
		"access from a stranger":  replace("access 10.77.0.1 5432", "access 10.77.0.9 5432"),
		"access with extra field": replace("access 10.77.0.1 5432", "access 10.77.0.1 5432 x"),
		"runtime of another env":  replace("runtime 1234abcd", "runtime 1234abcd-staging"),
		"invalid runtime":         replace("runtime 1234abcd", "runtime ../etc"),
		"production suffix":       replace("runtime 1234abcd", "runtime 1234abcd-production"),
		"missing environment":     replace("environment production\n", ""),
		"too much input":          valid + strings.Repeat("access 10.77.0.1 5432\n", 3200),
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			output, failure, err := runHelper(t, input, "check")
			if err == nil || !strings.HasPrefix(failure, "refused: ") || output != "" {
				t.Fatalf("accepted or rendered: err=%v stderr=%q stdout=%q", err, failure, output)
			}
		})
	}
}

func TestTheHelperNeedsRootAndAKnownVerb(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root")
	}
	for _, args := range [][]string{{"apply"}, {"enroll"}, {"observe"}, {"remove"}, {"restore", "dw0123456789ab"}} {
		if _, failure, err := runHelper(t, "", args...); err == nil || !strings.Contains(failure, "needs root") {
			t.Fatalf("%v ran without root: %s", args, failure)
		}
	}
	for _, args := range [][]string{{}, {"install"}, {"version", "extra"}, {"apply", "now"}} {
		if _, failure, err := runHelper(t, "", args...); err == nil || !strings.Contains(failure, "usage") {
			t.Fatalf("%v was accepted: %s", args, failure)
		}
	}
}

func TestTheHelperReportsTheChecksumDeployExpects(t *testing.T) {
	output, failure, err := runHelper(t, "", "version")
	if err != nil {
		t.Fatal(failure)
	}
	if strings.TrimSpace(output) != networkHelperIdentity() {
		t.Fatalf("helper says %q, deploy expects %q", output, networkHelperIdentity())
	}
}

func TestTheHelperInputRefusesWhatTheHelperWould(t *testing.T) {
	networks := helperNetworks(t)
	if _, err := networks["app-host"].HelperInput(map[string]string{}); err == nil {
		t.Fatal("a peer without a key was sent")
	}
	broken := networks["data-host"]
	broken.Address = "10.99.0.2"
	if _, err := broken.HelperInput(map[string]string{"app-host": testKey(1)}); err == nil {
		t.Fatal("an address outside the cidr was sent")
	}
}

func TestBootstrapNeedsAVersionTwoNetwork(t *testing.T) {
	directory := t.TempDir()
	config := func(raw string) {
		if err := os.WriteFile(filepath.Join(directory, configFileName), []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	config(`{"id":"1234abcd","name":"example","destination":"host:Projects/example","services":{"web":{"image":"nginx"}}}`)
	if code, err := RunNetworkBootstrap(directory, defaultEnvironmentName, strings.NewReader(""), &bytes.Buffer{}); err == nil || code != exitPreconditionNotMet {
		t.Fatalf("version 1 was bootstrapped: %d %v", code, err)
	}
	config(serviceReleaseFixture)
	var out bytes.Buffer
	if code, err := RunNetworkBootstrap(directory, defaultEnvironmentName, strings.NewReader(""), &out); err != nil || code != exitOK || !strings.Contains(out.String(), "needs no managed network") {
		t.Fatalf("a single destination wanted a network: %d %v %s", code, err, out.String())
	}
}

func TestNetworkHostsReachEachHostThroughOneDestination(t *testing.T) {
	hosts, err := networkHosts(topologyResolved(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 2 || hosts[0].Name != "app-host" || hosts[0].Target != "app-host" || strings.Join(hosts[0].Destinations, ",") != "app" ||
		hosts[1].Name != "data-host" || hosts[1].Target != "data-host" || strings.Join(hosts[1].Destinations, ",") != "data" {
		t.Fatalf("hosts %+v", hosts)
	}
	if hosts[0].Network.Address != "10.77.0.1" || hosts[1].Network.Address != "10.77.0.2" {
		t.Fatalf("networks %+v", hosts)
	}
}

func TestWorkloadsAreTheLiveNetworkServicesOfOneHostInReadinessOrder(t *testing.T) {
	resolved := topologyResolved(t)
	cache := Service{Destination: "app", Image: "example/cache"}
	resolved.Services["cache"] = cache
	live := map[string]ServiceRelease{
		"db":    sharedRelease(resolved.RuntimeID(), "abcdef1", ""),
		"api":   serviceRelease(resolved.RuntimeID(), "api", "abcdef1", ""),
		"cache": serviceRelease(resolved.RuntimeID(), "cache", "abcdef1", ""),
	}
	data, err := workloadContainers(resolved, resolved.destinationHost("data"), live)
	if err != nil || strings.Join(data, ",") != "deploy-1234abcd-shared-db-1" {
		t.Fatalf("data host workloads %v %v", data, err)
	}
	app, err := workloadContainers(resolved, resolved.destinationHost("app"), live)
	if err != nil || strings.Join(app, ",") != "deploy-1234abcd_api_abcdef1-api-1" {
		t.Fatalf("app host workloads %v %v", app, err)
	}
	if got := workloadsInput(resolved, app); got != "runtime 1234abcd\nenvironment production\nworkload deploy-1234abcd_api_abcdef1-api-1\n" {
		t.Fatalf("input %q", got)
	}

	live["api"] = legacyRelease(resolved.RuntimeID(), "abcdef1")
	if _, err := workloadContainers(resolved, resolved.destinationHost("app"), live); err == nil || !strings.Contains(err.Error(), "deploy it again") {
		t.Fatalf("a legacy commit stack was handed to the helper: %v", err)
	}
	delete(live, "api")
	if app, err := workloadContainers(resolved, resolved.destinationHost("app"), live); err != nil || len(app) != 0 {
		t.Fatalf("a service that never went live was listed: %v %v", app, err)
	}
}

func TestTheHelperAcceptsOnlyThisRuntimesWorkloads(t *testing.T) {
	identity := "runtime 1234abcd\nenvironment production\n"
	for name, input := range map[string]string{
		"another runtime":   identity + "workload deploy-1234abce-shared-db-1\n",
		"another env":       identity + "workload deploy-1234abcd-staging-shared-db-1\n",
		"no replica number": identity + "workload deploy-1234abcd-shared-db\n",
		"shell":             identity + "workload deploy-1234abcd-shared-db-1;reboot\n",
		"twice":             identity + "workload deploy-1234abcd-shared-db-1\nworkload deploy-1234abcd-shared-db-1\n",
		"network fields":    identity + "cidr 10.77.0.0/24\nworkload deploy-1234abcd-shared-db-1\n",
	} {
		if _, failure, err := runHelper(t, input, "workloads"); err == nil || !strings.Contains(failure, "refused") || strings.Contains(failure, "needs root") {
			t.Fatalf("%s was accepted: %s", name, failure)
		}
	}
	if os.Getuid() != 0 {
		valid := identity + "workload deploy-1234abcd-shared-db-1\nworkload deploy-1234abcd_api_abcdef1-api-1\n"
		if _, failure, err := runHelper(t, valid, "workloads"); err == nil || !strings.Contains(failure, "needs root") {
			t.Fatalf("valid workloads did not reach the root check: %s", failure)
		}
	}
}
