package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// fakeHelper answers observe like the real helper would, from per-host state.
type fakeHelper struct {
	observed map[string]string
	calls    []string
}

func (helper *fakeHelper) call(_ context.Context, destination Destination, verb, input string) (string, error) {
	helper.calls = append(helper.calls, destination.Host+" "+verb)
	if verb != "observe" {
		return "", fmt.Errorf("unexpected %s", verb)
	}
	if !strings.HasPrefix(input, "runtime 1234abcd\nenvironment production\n") {
		return "", fmt.Errorf("unexpected input %q", input)
	}
	return helper.observed[destination.Host], nil
}

func ownedObservation(key string, applied string, peerKey string, route string) string {
	return networkHelperIdentity() + "\ninterface " + interfaceName("1234abcd") + "\nowner present\npublicKey " + key +
		"\nwireguard enabled active\nboot enabled active\nlisten 51820\npeer " + peerKey + " " + route + "/32\nroute " + route +
		"\nfirewall " + strings.Repeat("a", 64) + "\nfirewall matches\njump INPUT present\njump DOCKER-USER present\njump OUTPUT present\napplied " + applied + "\n"
}

func unownedObservation() string {
	return networkHelperIdentity() + "\ninterface " + interfaceName("1234abcd") + "\nowner absent\n"
}

func TestObservationParsingAcceptsOnlyThisHelper(t *testing.T) {
	observed, err := parseHostObservation(ownedObservation(testKey(1), strings.Repeat("b", 64), testKey(2), "10.77.0.2"))
	if err != nil {
		t.Fatal(err)
	}
	if !observed.Owner || observed.PublicKey != testKey(1) || observed.WireGuard != "enabled active" || observed.Listen != 51820 ||
		observed.Peers[testKey(2)] != "10.77.0.2/32" || observed.Routes[0] != "10.77.0.2" || observed.Firewall != "matches" ||
		!observed.Jumps["DOCKER-USER"] || observed.Applied != strings.Repeat("b", 64) {
		t.Fatalf("observed %+v", observed)
	}
	// a unit that does not exist prints an empty enabled state
	missingUnit := strings.Replace(unownedObservation(), "owner absent", "owner present\npublicKey absent\nwireguard  inactive\nboot  inactive\nlink absent\napplied none", 1)
	observed, err = parseHostObservation(missingUnit)
	if err != nil || observed.WireGuard != "inactive" || observed.Link {
		t.Fatalf("missing units: %+v %v", observed, err)
	}
	if _, err := parseHostObservation(strings.Replace(unownedObservation(), "deploy-network 1", "deploy-network 0", 1)); !errors.Is(err, errNetworkHelperMissing) {
		t.Fatalf("another helper version was accepted: %v", err)
	}
	for name, raw := range map[string]string{
		"unknown line":    unownedObservation() + "privateKey something\n",
		"control bytes":   unownedObservation() + "route 10.77.0.2\x1b[2J\n",
		"bad listen port": unownedObservation() + "listen 70000\n",
		"bad digest":      unownedObservation() + "applied nothex\n",
		"no interface":    networkHelperIdentity() + "\nowner absent\n",
		"blank line":      unownedObservation() + "\n\n",
	} {
		if _, err := parseHostObservation(raw); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
}

func TestInspectionWaitsForEveryKeyAndThenComparesTheAppliedInput(t *testing.T) {
	resolved := topologyResolved(t)
	hosts, err := networkHosts(resolved)
	if err != nil {
		t.Fatal(err)
	}
	helper := &fakeHelper{observed: map[string]string{"app-host": unownedObservation(), "data-host": unownedObservation()}}
	reports, err := inspectNetwork(context.Background(), resolved, hosts, helper.call)
	if err != nil {
		t.Fatal(err)
	}
	for _, report := range reports {
		if strings.Join(report.Changes, ",") != "enroll" || report.input != "" {
			t.Fatalf("an unenrolled host: %+v", report)
		}
	}

	keys := map[string]string{"app-host": testKey(1), "data-host": testKey(2)}
	app, _ := hosts[0].Network.HelperInput(keys)
	data, _ := hosts[1].Network.HelperInput(keys)
	helper.observed = map[string]string{
		"app-host":  ownedObservation(testKey(1), helperInputDigest(app), testKey(2), "10.77.0.2"),
		"data-host": ownedObservation(testKey(2), helperInputDigest(data), testKey(1), "10.77.0.1"),
	}
	reports, err = inspectNetwork(context.Background(), resolved, hosts, helper.call)
	if err != nil {
		t.Fatal(err)
	}
	for _, report := range reports {
		if len(report.Changes) != 0 {
			t.Fatalf("%s should be unchanged: %v", report.Host, report.Changes)
		}
	}

	helper.observed["data-host"] = strings.NewReplacer("firewall matches", "firewall drifted", "jump DOCKER-USER present", "jump DOCKER-USER absent",
		"route 10.77.0.1\n", "", "applied "+helperInputDigest(data), "applied "+strings.Repeat("c", 64)).Replace(helper.observed["data-host"])
	reports, err = inspectNetwork(context.Background(), resolved, hosts, helper.call)
	if err != nil {
		t.Fatal(err)
	}
	want := "desired network differs from the applied one; firewall chains drifted; jump from DOCKER-USER is missing; route to 10.77.0.1 is missing"
	if got := strings.Join(reports[1].Changes, "; "); got != want {
		t.Fatalf("changes\n%s\nwant\n%s", got, want)
	}
	if len(reports[0].Changes) != 0 {
		t.Fatalf("the healthy host changed: %v", reports[0].Changes)
	}
}

func TestInspectionRefusesALostKeyAndAForeignInterface(t *testing.T) {
	resolved := topologyResolved(t)
	hosts, _ := networkHosts(resolved)
	lost := strings.Replace(unownedObservation(), "owner absent", "owner present\npublicKey absent", 1)
	helper := &fakeHelper{observed: map[string]string{"app-host": lost, "data-host": unownedObservation()}}
	if _, err := inspectNetwork(context.Background(), resolved, hosts, helper.call); err == nil || !strings.Contains(err.Error(), "lost the private key") {
		t.Fatalf("a lost key was accepted: %v", err)
	}
	helper.observed["app-host"] = strings.Replace(unownedObservation(), interfaceName("1234abcd"), interfaceName("1234abcd-staging"), 1)
	if _, err := inspectNetwork(context.Background(), resolved, hosts, helper.call); err == nil || !strings.Contains(err.Error(), "expected") {
		t.Fatalf("another interface was accepted: %v", err)
	}
}

func TestNetworkRecordsRoundTripAndRefuseNonsense(t *testing.T) {
	layout := NewLayout(t.TempDir(), "1234abcd")
	if _, found, err := readNetworkRecord(LocalRunner{}, layout); found || err != nil {
		t.Fatalf("an empty root has a record: %v %v", found, err)
	}
	record := NetworkRecord{
		Version: networkRecordVersion, Project: "1234abcd", Environment: "production", Destination: "app",
		Interface: interfaceName("1234abcd"), Generation: 1, Applied: strings.Repeat("d", 64), Operation: "0123456789abcdef",
		UpdatedAt: time.Now().UTC().Truncate(time.Second),
	}
	if err := writeNetworkRecord(LocalRunner{}, layout, record); err != nil {
		t.Fatal(err)
	}
	read, found, err := readNetworkRecord(LocalRunner{}, layout)
	if err != nil || !found || read != record {
		t.Fatalf("read back %+v %v %v", read, found, err)
	}
	for name, broken := range map[string]func(*NetworkRecord){
		"foreign interface": func(record *NetworkRecord) { record.Interface = interfaceName("1234abcd-staging") },
		"zero generation":   func(record *NetworkRecord) { record.Generation = 0 },
		"bad digest":        func(record *NetworkRecord) { record.Applied = "x" },
		"no operation":      func(record *NetworkRecord) { record.Operation = "" },
	} {
		invalid := record
		broken(&invalid)
		if err := writeNetworkRecord(LocalRunner{}, layout, invalid); err == nil {
			t.Fatalf("%s was written", name)
		}
	}
}

func TestNetworkCommandsNeedAKnownVerb(t *testing.T) {
	for _, args := range [][]string{{}, {"destroy"}, {"apply", "extra"}} {
		if code := runNetwork(args); code != exitPreconditionNotMet {
			t.Fatalf("%v gave %d", args, code)
		}
	}
}
