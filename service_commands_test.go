package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReleasesRecordWhatTheyNeedFromTheNetwork(t *testing.T) {
	resolved := topologyResolved(t)
	if got := strings.Join(releasePaths(resolved, "api"), ","); got != "connect 10.77.0.2:5432" {
		t.Fatalf("api paths %q", got)
	}
	if got := strings.Join(releasePaths(resolved.on("data"), "db"), ","); got != "publish 10.77.0.2:5432" {
		t.Fatalf("db paths %q", got)
	}
	if revoked := revokedPaths(resolved, "api", []string{"connect 10.77.0.2:5432"}); len(revoked) != 0 {
		t.Fatalf("a path the network still provides was revoked: %v", revoked)
	}
	moved := topologyResolved(t)
	db := moved.Services["db"]
	db.Endpoints = map[string]Endpoint{"sql": {Port: 5432, HostPort: 5433, Protocol: "tcp"}}
	moved.Services["db"] = db
	if revoked := revokedPaths(moved, "api", []string{"connect 10.77.0.2:5432"}); strings.Join(revoked, ",") != "connect 10.77.0.2:5432" {
		t.Fatalf("a path the network dropped was kept: %v", revoked)
	}
}

func TestRollbackRefusesATargetWhoseNetworkAccessWasRevoked(t *testing.T) {
	resolved := topologyResolved(t)
	layout := NewLayout(t.TempDir(), resolved.RuntimeID())
	live := serviceRelease(resolved.RuntimeID(), "api", "bbbbbbb", "")
	target := serviceRelease(resolved.RuntimeID(), "api", "aaaaaaa", "")
	for _, release := range []ServiceRelease{live, target} {
		file := filepath.Join(layout.Root, release.ComposeFile)
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	state := ServiceState{Services: map[string]ServiceRecord{"api": {
		Current: &live, Previous: &target, Releases: []string{"bbbbbbb", "aaaaaaa"}, Outcome: outcomeLive,
		Paths: map[string][]string{"aaaaaaa": {"connect 10.77.0.9:5432"}, "bbbbbbb": {"connect 10.77.0.2:5432"}},
	}}}
	if _, err := planServiceRollback(LocalRunner{}, resolved.on("app"), layout, state, "", []string{"api"}); err == nil || !strings.Contains(err.Error(), "no longer provides") {
		t.Fatalf("a revoked path was rolled back to: %v", err)
	}
	record := state.Services["api"]
	record.Paths["aaaaaaa"] = []string{"connect 10.77.0.2:5432"}
	steps, err := planServiceRollback(LocalRunner{}, resolved.on("app"), layout, state, "", []string{"api"})
	if err != nil || len(steps) != 1 || steps[0].target.Commit != "aaaaaaa" {
		t.Fatalf("a target the network still serves was refused: %v %v", steps, err)
	}
	delete(record.Paths, "aaaaaaa")
	if _, err := planServiceRollback(LocalRunner{}, resolved.on("app"), layout, state, "", []string{"api"}); err != nil {
		t.Fatalf("a release that needed nothing from the network was refused: %v", err)
	}
}

func TestRecordedNetworkPathsAreValidated(t *testing.T) {
	release := serviceRelease("1234abcd", "api", "aaaaaaa", "")
	record := ServiceRecord{Current: &release, Releases: []string{"aaaaaaa"}, Outcome: outcomeLive}
	for _, paths := range []map[string][]string{
		{"aaaaaaa": {"connect 10.77.0.2:5432; reboot"}},
		{"aaaaaaa": {"open 10.77.0.2:5432"}},
		{"not-a-commit": {"connect 10.77.0.2:5432"}},
	} {
		record.Paths = paths
		if err := record.validate("1234abcd", "api"); err == nil {
			t.Fatalf("%v was accepted", paths)
		}
	}
	record.Paths = map[string][]string{"aaaaaaa": {"connect 10.77.0.2:5432", "publish 10.77.0.1:8080"}}
	if err := record.validate("1234abcd", "api"); err != nil {
		t.Fatal(err)
	}
}

func TestEnvFilesGoOnlyWhereTheyAreListed(t *testing.T) {
	resolved := topologyResolved(t)
	api := resolved.Services["api"]
	api.Env = []string{"api.env"}
	resolved.Services["api"] = api
	resolved.Release = map[string]Service{"migrate": {Destination: "data", Image: "example/db", Env: []string{"api.env", "db.env"}}}
	if got := strings.Join(envFileDestinations(resolved, "api.env"), ","); got != "app,data" {
		t.Fatalf("api.env goes to %q", got)
	}
	if got := strings.Join(envFileDestinations(resolved, "db.env"), ","); got != "data" {
		t.Fatalf("db.env goes to %q", got)
	}
	if got := envFileDestinations(resolved, "unused.env"); len(got) != 0 {
		t.Fatalf("an unlisted file goes to %v", got)
	}
}

func TestASelectedDeployNeedsWhatItWaitsOnToBeThere(t *testing.T) {
	resolved := topologyResolved(t)
	resolved.Services["web"] = Service{Destination: "app", Image: "example/web", DependsOn: map[string]string{"api": "started"}}
	if _, err := selectNodes(resolved, []string{"nope"}); err == nil || !strings.Contains(err.Error(), "does not have") {
		t.Fatalf("an unknown name was selected: %v", err)
	}
	selected, err := selectNodes(resolved, []string{"web"})
	if err != nil {
		t.Fatal(err)
	}
	empty := []*destinationRun{{name: "app", state: ServiceState{Services: map[string]ServiceRecord{}}}}
	if err := selectionIsReady(resolved, selected, empty); err == nil || !strings.Contains(err.Error(), "web waits on api") {
		t.Fatalf("a selection missing its dependency was accepted: %v", err)
	}
	release := serviceRelease(resolved.RuntimeID(), "api", "aaaaaaa", "")
	live := []*destinationRun{{name: "app", state: ServiceState{Services: map[string]ServiceRecord{"api": {Current: &release, Outcome: outcomeLive}}}}}
	if err := selectionIsReady(resolved, selected, live); err != nil {
		t.Fatalf("a selection whose dependency is live was refused: %v", err)
	}
	both, _ := selectNodes(resolved, []string{"web", "api"})
	if err := selectionIsReady(resolved, both, empty); err != nil {
		t.Fatalf("api depends only on stateful db, which always comes up: %v", err)
	}
	if all, err := selectNodes(resolved, nil); all != nil || err != nil {
		t.Fatalf("no selection is not everything: %v %v", all, err)
	}
}
