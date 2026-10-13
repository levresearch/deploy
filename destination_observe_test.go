package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const observationFixture = "/srv/projects/app\n11111111111111111111111111111111\n11111111-2222-3333-4444-555555555555\n1000\n" + `{"id":"daemon-one","os":"linux","architecture":"x86_64","version":"29.1.3","security":["name=seccomp"]}` + "\n"

func testObservation(t *testing.T) DestinationObservation {
	t.Helper()
	observation, err := parseDestinationObservation(observationFixture)
	if err != nil {
		t.Fatal(err)
	}
	return observation
}

func TestDestinationObservationRejectsInvalidFacts(t *testing.T) {
	for _, tc := range []struct{ old, new string }{
		{"/srv/projects/app", "relative/path"},
		{"/srv/projects/app", "/"},
		{"/srv/projects/app", "/srv/../app"},
		{strings.Repeat("1", 32), strings.Repeat("0", 32)},
		{"11111111-2222-3333-4444-555555555555", "bad-boot"},
		{"1000", "not-a-uid"},
		{`"id":"daemon-one"`, `"id":""`},
		{`"linux"`, `"windows"`},
		{`"name=seccomp"`, `"name=rootless"`},
	} {
		if _, err := parseDestinationObservation(strings.Replace(observationFixture, tc.old, tc.new, 1)); err == nil {
			t.Fatalf("accepted %s", tc.new)
		}
	}
	if _, err := parseDestinationObservation("banner\n" + observationFixture); err == nil {
		t.Fatal("accepted contaminated output")
	}
	encoded, err := json.Marshal(testObservation(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{strings.Repeat("1", 32), "11111111-2222-3333-4444-555555555555", "daemon-one"} {
		if strings.Contains(string(encoded), private) {
			t.Fatal("raw identity leaked into output")
		}
	}
}

func TestObservedAliasesChangeTransportAndCatchConflicts(t *testing.T) {
	project := topologyProject(t, topologyFixture)
	project.Network = nil
	observations := map[string]DestinationObservation{"app": testObservation(t), "data": testObservation(t)}
	plan, err := planObservedTopology(project, "production", "unverified", observations)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Network.Mode != "docker-only" || plan.Connections[0].Host != "db" {
		t.Fatal("verified aliases still required WireGuard")
	}
	if plan.Destinations[0].CanonicalPath != "/srv/projects/app" || !plan.ExecutionAvailable {
		t.Fatal(plan)
	}
	api := project.Services["api"]
	api.Endpoints = map[string]Endpoint{"http": {Port: 8080, HostPort: 5432, Protocol: "tcp"}}
	project.Services["api"] = api
	if _, err := planObservedTopology(project, "production", "unverified", observations); err == nil || !strings.Contains(err.Error(), "port collision") {
		t.Fatal(err)
	}
	project = topologyProject(t, topologyFixture)
	if _, err := planObservedTopology(project, "production", "unverified", observations); err == nil || !strings.Contains(err.Error(), "identical peer") {
		t.Fatal("conflicting same-host peers accepted", err)
	}
}

func TestDestinationIdentityContradictionsBlockPlanning(t *testing.T) {
	base := testObservation(t)
	for _, field := range []string{"boot", "daemon", "machine"} {
		other := base
		switch field {
		case "boot":
			other.bootID = "another-boot"
		case "daemon":
			other.daemonID = "another-daemon"
		case "machine":
			other.machineID = "another-machine"
		}
		if err := checkObservedIdentity("other", other, map[string]DestinationObservation{"first": base}); err == nil {
			t.Fatalf("accepted contradictory %s", field)
		}
	}
	project := topologyProject(t, topologyFixture)
	called := false
	project.Destinations["unsafe"] = "-flag:Projects/app"
	_, err := observePlanningProject(context.Background(), project, func(context.Context, Destination) (DestinationObservation, error) { called = true; return base, nil })
	if err == nil || called {
		t.Fatal("contacted a host before validating all destinations")
	}
	delete(project.Destinations, "unsafe")
	_, err = observePlanningProject(context.Background(), project, func(context.Context, Destination) (DestinationObservation, error) {
		return DestinationObservation{}, errors.New("unreachable")
	})
	if err == nil || !strings.Contains(err.Error(), "unreachable") {
		t.Fatal("unreachable treated as absent", err)
	}
	if _, err := planObservedTopology(project, "production", "unverified", map[string]DestinationObservation{}); err == nil {
		t.Fatal("accepted incomplete observations")
	}
}

func TestObservationTransportIsBoundedAndSanitized(t *testing.T) {
	directory := t.TempDir()
	writeFile(t, directory, "reply", observationFixture)
	ssh := filepath.Join(directory, "ssh")
	if err := os.WriteFile(ssh, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > "+ShellQuote(filepath.Join(directory, "args"))+"\ncat "+ShellQuote(filepath.Join(directory, "reply"))+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
	if _, err := observeDestination(context.Background(), Destination{Host: "example", Path: "Projects/it's a test"}); err != nil {
		t.Fatal(err)
	}
	args, err := os.ReadFile(filepath.Join(directory, "args"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"StrictHostKeyChecking=yes", "UpdateHostKeys=no", "ControlPath=none", "ClearAllForwardings=yes", "ForwardAgent=no", "PermitLocalCommand=no", ShellQuote("Projects/it's a test")} {
		if !strings.Contains(string(args), want) {
			t.Fatalf("missing %s", want)
		}
	}
	if err := os.WriteFile(ssh, []byte("#!/bin/sh\necho sentinel-private-error >&2\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := observeDestination(context.Background(), Destination{Host: "example", Path: "Projects/app"}); err == nil || strings.Contains(err.Error(), "sentinel-private-error") {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := observeDestination(ctx, Destination{Host: "example", Path: "Projects/app"}); err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatal(err)
	}
	var output boundedOutput
	if _, err := output.Write(make([]byte, 16385)); err == nil {
		t.Fatal("unbounded output")
	}
	if err := os.WriteFile(ssh, []byte("#!/bin/sh\nhead -c 20000 /dev/zero\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := observeDestination(context.Background(), Destination{Host: "example", Path: "Projects/app"}); err == nil {
		t.Fatal("accepted oversized subprocess output")
	}
}
