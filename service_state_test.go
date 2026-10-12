package main

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func ptr[T any](value T) *T { return &value }

func recordedServiceState() ServiceState {
	return ServiceState{
		Project: "1234abcd", Name: "example", Environment: "production", Destination: "data", Naming: legacyNaming,
		Services: map[string]ServiceRecord{
			"db":  {Stateful: true, Outcome: outcomeLive, Current: ptr(sharedRelease("1234abcd", "aaaaaaa", ""))},
			"old": {Outcome: outcomeLive, Current: ptr(serviceRelease("1234abcd", "old", "bbbbbbb", strings.Repeat("c", 64))), Previous: ptr(legacyRelease("1234abcd", "aaaaaaa")), Releases: []string{"bbbbbbb", "aaaaaaa"}},
		},
		Tasks: map[string][]TaskRecord{
			"migrate": {{Operation: "0123456789abcdef", Commit: "aaaaaaa", ConfigDigest: strings.Repeat("d", 64), Outcome: outcomeCompleted}, {Operation: "fedcba9876543210", Commit: "bbbbbbb", ConfigDigest: strings.Repeat("d", 64), Outcome: outcomeIndeterminate}},
		},
	}
}

func TestServiceStateRoundTripsAndCountsGenerations(t *testing.T) {
	layout := NewLayout(t.TempDir(), "1234abcd")
	if err := os.MkdirAll(layout.Root, 0o700); err != nil {
		t.Fatal(err)
	}
	written, err := WriteServiceState(LocalRunner{}, layout, recordedServiceState())
	if err != nil {
		t.Fatal(err)
	}
	if written.Generation != 1 || written.Version != serviceStateVersion {
		t.Fatalf("first write: %+v", written)
	}
	again, err := WriteServiceState(LocalRunner{}, layout, written)
	if err != nil {
		t.Fatal(err)
	}
	read, found, err := ReadServiceState(LocalRunner{}, layout)
	if err != nil || !found {
		t.Fatalf("found=%v %v", found, err)
	}
	if read.Generation != 2 || again.Generation != 2 || read.Services["old"].Previous.Commit != "aaaaaaa" || read.Tasks["migrate"][1].Outcome != outcomeIndeterminate {
		t.Fatalf("state did not round trip: %+v", read)
	}
	info, err := os.Stat(layout.ServiceStateFile())
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("state mode %v", info.Mode().Perm())
	}
}

func TestServiceStateRefusesWhatItCannotTrust(t *testing.T) {
	invalid := map[string]func(*ServiceState){
		"unknown naming": func(state *ServiceState) { state.Naming = "service" },
		"bad outcome":    func(state *ServiceState) { state.Services["db"] = ServiceRecord{Outcome: "fine"} },
		"bad commit": func(state *ServiceState) {
			state.Services["db"] = ServiceRecord{Outcome: outcomeLive, Current: &ServiceRelease{Commit: "../x"}}
		},
		"previous alone": func(state *ServiceState) {
			state.Services["db"] = ServiceRecord{Outcome: outcomeLive, Previous: &ServiceRelease{Commit: "aaaaaaa"}}
		},
		"short digest": func(state *ServiceState) {
			state.Services["db"] = ServiceRecord{Outcome: outcomeLive, Current: &ServiceRelease{Commit: "aaaaaaa", ConfigDigest: "abc"}}
		},
		"bad service name":     func(state *ServiceState) { state.Services["../db"] = ServiceRecord{Outcome: outcomeLive} },
		"task without digest":  func(state *ServiceState) { state.Tasks["migrate"][0].ConfigDigest = "" },
		"task outcome":         func(state *ServiceState) { state.Tasks["migrate"][0].Outcome = outcomeLive },
		"task operation":       func(state *ServiceState) { state.Tasks["migrate"][0].Operation = "not-an-id" },
		"runtime id":           func(state *ServiceState) { state.Project = "example" },
		"destination slug":     func(state *ServiceState) { state.Destination = "Data Host" },
		"missing services map": func(state *ServiceState) { state.Services = nil },
	}
	for name, change := range invalid {
		layout := NewLayout(t.TempDir(), "1234abcd")
		if err := os.MkdirAll(layout.Root, 0o700); err != nil {
			t.Fatal(err)
		}
		state := recordedServiceState()
		change(&state)
		if _, err := WriteServiceState(LocalRunner{}, layout, state); err == nil {
			t.Fatalf("%s was written", name)
		}
		if _, err := os.Stat(layout.ServiceStateFile()); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("%s left a file behind", name)
		}
	}

	layout := NewLayout(t.TempDir(), "1234abcd")
	for _, contents := range []string{
		"{not json",
		`{"version":1,"project":"1234abcd"}`,
		`{"version":2,"project":"1234abcd","environment":"production","destination":"data","naming":"legacy","generation":1,"services":{},"unknown":true}`,
		`{"version":2,"project":"1234abcd","environment":"production","destination":"data","naming":"legacy","generation":0,"services":{}}`,
	} {
		writeFile(t, layout.Root, serviceStateFileName, contents)
		if _, found, err := ReadServiceState(LocalRunner{}, layout); err == nil || !found {
			t.Fatalf("%q was accepted: found=%v", contents, found)
		}
	}
}

func TestLegacyAdoptionKeepsEveryLiveRelease(t *testing.T) {
	resolved := topologyResolved(t)
	legacy := State{Version: 1, Current: "bbbbbbb", Previous: "aaaaaaa", Environment: "production", Releases: []string{"bbbbbbb", "aaaaaaa"}}

	data, err := adoptLegacyState(legacy, resolved, "data")
	if err != nil {
		t.Fatal(err)
	}
	db := data.Services["db"]
	if len(data.Services) != 1 || !db.Stateful || db.Current.Commit != "bbbbbbb" || db.Previous != nil || db.Releases != nil || db.Outcome != outcomeLive {
		t.Fatalf("a stateful service was adopted with a rollback target: %+v", data.Services)
	}
	if db.Current.Project != "deploy-1234abcd-shared" || db.Current.ComposeFile != "shared/docker-compose.yml" {
		t.Fatalf("a stateful service was not pointed at the shared stack: %+v", db.Current)
	}
	if data.Naming != legacyNaming || data.Project != "1234abcd" || data.Destination != "data" {
		t.Fatalf("adopted identity: %+v", data)
	}

	app, err := adoptLegacyState(legacy, resolved, "app")
	if err != nil {
		t.Fatal(err)
	}
	api := app.Services["api"]
	if len(app.Services) != 1 || api.Current.Commit != "bbbbbbb" || api.Previous.Commit != "aaaaaaa" || !slices.Equal(api.Releases, legacy.Releases) {
		t.Fatalf("a stateless service lost its releases: %+v", app.Services)
	}
	if api.Current.Project != "deploy-1234abcd-bbbbbbb" || api.Previous.ComposeFile != "releases/aaaaaaa/docker-compose.yml" {
		t.Fatalf("adopted releases do not point at their commit stacks: %+v %+v", api.Current, api.Previous)
	}
	app.Generation = 1
	if err := app.validate(); err != nil {
		t.Fatalf("adopted state is not writable: %v", err)
	}

	never, err := adoptLegacyState(State{}, resolved, "app")
	if err != nil || len(never.Services) != 0 {
		t.Fatalf("a destination never deployed to gained services: %+v %v", never, err)
	}
	if _, err := adoptLegacyState(State{Current: "bbbbbbb", Environment: "staging"}, resolved, "app"); err == nil {
		t.Fatal("staging state was adopted as production")
	}
}

func TestVersionOneCommandsStopAtServiceState(t *testing.T) {
	layout := NewLayout(t.TempDir(), "1234abcd")
	writeFile(t, layout.Root, serviceStateFileName, "{}")
	if lock, err := AcquireLock(LocalRunner{}, layout, "deploy", "aaaaaaa", true); err == nil {
		lock.Release()
		t.Fatal("a version 1 deploy ran over version 2 state")
	} else if !strings.Contains(err.Error(), "version 2 state") {
		t.Fatal(err)
	}
	if _, err := os.Stat(operationFile(layout)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("the refusal recorded an operation")
	}
}

func plannedAt(t *testing.T, resolved ResolvedProject) (TopologyPlan, map[string]string) {
	t.Helper()
	plan, err := planTopology(resolved, "ccccccc")
	if err != nil {
		t.Fatal(err)
	}
	roots := map[string]string{}
	for index := range plan.Destinations {
		root := t.TempDir()
		plan.Destinations[index].CanonicalPath = root
		roots[plan.Destinations[index].Name] = root
	}
	return plan, roots
}

func localReaders(plan TopologyPlan) map[string]stateReader {
	readers := map[string]stateReader{}
	for _, destination := range plan.Destinations {
		readers[destination.Name] = observedReader{ctx: context.Background(), destination: Destination{Path: destination.CanonicalPath}}
	}
	return readers
}

func plannedService(t *testing.T, plan TopologyPlan, name string) PlannedService {
	t.Helper()
	for _, service := range plan.Services {
		if service.Name == name {
			return service
		}
	}
	t.Fatalf("no planned service %s", name)
	return PlannedService{}
}

func TestPlanReportsVersionOneReleasesWithoutWriting(t *testing.T) {
	resolved := topologyResolved(t)
	resolved.Release = map[string]Service{"migrate": {Destination: "data", Image: "example/migrate"}}
	plan, roots := plannedAt(t, resolved)
	layout := NewLayout(roots["app"], "1234abcd")
	writeFile(t, layout.Root, stateFileName, `{"version":1,"current":"bbbbbbb","previous":"aaaaaaa","environment":"production","releases":["bbbbbbb","aaaaaaa"]}`)

	if err := applyRecordedState(&plan, resolved, localReaders(plan)); err != nil {
		t.Fatal(err)
	}
	if got := plannedService(t, plan, "api").PreviousRelease; got != "bbbbbbb" {
		t.Fatalf("api previous release %q", got)
	}
	if got := plannedService(t, plan, "db").PreviousRelease; got != "none" {
		t.Fatalf("db previous release %q", got)
	}
	if got := plannedService(t, plan, "migrate").PreviousRelease; got != "none" {
		t.Fatalf("migrate previous release %q", got)
	}
	if plan.Destinations[0].RecordedState != "version 1, adopted in memory" || plan.Destinations[1].RecordedState != "none" {
		t.Fatalf("recorded state sources: %+v", plan.Destinations)
	}
	if slices.Contains(plan.Unverified, "live and retained releases") {
		t.Fatal("live releases are still reported unverified")
	}
	if _, err := os.Stat(layout.ServiceStateFile()); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("planning wrote version 2 state")
	}
	if entries, _ := os.ReadDir(roots["data"]); len(entries) != 0 {
		t.Fatal("planning created files at an empty destination")
	}
}

func TestPlanReportsVersionTwoHistoryAndUnfinishedWork(t *testing.T) {
	resolved := topologyResolved(t)
	resolved.Release = map[string]Service{"migrate": {Destination: "data", Image: "example/migrate"}}
	plan, roots := plannedAt(t, resolved)
	layout := NewLayout(roots["data"], "1234abcd")
	if err := os.MkdirAll(layout.Root, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteServiceState(LocalRunner{}, layout, recordedServiceState()); err != nil {
		t.Fatal(err)
	}
	lock, err := AcquireLock(LocalRunner{}, NewLayout(roots["data"], "1234abcd"), "deploy", "bbbbbbb", false)
	if err == nil {
		lock.Release()
		t.Fatal("the version 2 state did not block a version 1 deploy")
	}
	if err := os.MkdirAll(lockDirectory(layout), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, lockDirectory(layout), "1234abcd.operation.json", `{"version":1,"id":"0123456789abcdef","command":"deploy","target":"bbbbbbb","host":"laptop","pid":7,"startedAt":"2026-10-12T00:00:00Z"}`)

	if err := applyRecordedState(&plan, resolved, localReaders(plan)); err != nil {
		t.Fatal(err)
	}
	if got := plannedService(t, plan, "db").PreviousRelease; got != "aaaaaaa" {
		t.Fatalf("db previous release %q", got)
	}
	if got := plannedService(t, plan, "migrate").PreviousRelease; got != "bbbbbbb (indeterminate)" {
		t.Fatalf("migrate history %q", got)
	}
	if !slices.Equal(plan.RecordedOnly, []string{"data/old"}) {
		t.Fatalf("a removed service vanished from the plan: %v", plan.RecordedOnly)
	}
	data := plan.Destinations[1]
	if data.RecordedState != "version 2" || data.StateGeneration != 1 || !strings.Contains(data.UnresolvedOperation, "deploy resolve 0123456789abcdef") {
		t.Fatalf("data destination: %+v", data)
	}
}

func TestPlanRefusesStateRecordedForAnotherDestination(t *testing.T) {
	resolved := topologyResolved(t)
	plan, roots := plannedAt(t, resolved)
	layout := NewLayout(roots["app"], "1234abcd")
	if err := os.MkdirAll(layout.Root, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteServiceState(LocalRunner{}, layout, recordedServiceState()); err != nil {
		t.Fatal(err)
	}
	if err := applyRecordedState(&plan, resolved, localReaders(plan)); err == nil || !strings.Contains(err.Error(), "destination data") {
		t.Fatalf("state for data was read as app: %v", err)
	}
}

func TestObservedReaderSeparatesMissingFromUnreadable(t *testing.T) {
	directory := t.TempDir()
	reader := observedReader{ctx: context.Background(), destination: Destination{Path: directory}}
	if _, err := reader.ReadFile(filepath.Join(directory, "missing")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing file: %v", err)
	}
	if listing, err := reader.ListDirectory(filepath.Join(directory, "missing")); err != nil || listing != nil {
		t.Fatalf("missing directory: %v %v", listing, err)
	}
	if _, err := reader.ReadFile(directory); err == nil {
		t.Fatal("a directory was read as a file")
	}
	if err := os.Symlink(filepath.Join(directory, "elsewhere"), filepath.Join(directory, "dangling")); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.ReadFile(filepath.Join(directory, "dangling")); err == nil || errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a dangling link was treated as missing: %v", err)
	}
	writeFile(t, directory, ".hidden", "x")
	if listing, err := reader.ListDirectory(directory); err != nil || !slices.Contains(listing, ".hidden") {
		t.Fatalf("listing: %v %v", listing, err)
	}
	if err := os.WriteFile(filepath.Join(directory, "large"), make([]byte, observedStateLimit+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.ReadFile(filepath.Join(directory, "large")); err == nil {
		t.Fatal("unbounded state was read")
	}
}

func TestObservedReaderUsesTheStrictProbeConnection(t *testing.T) {
	directory := t.TempDir()
	ssh := filepath.Join(directory, "ssh")
	if err := os.WriteFile(ssh, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > "+ShellQuote(filepath.Join(directory, "args"))+"\nexit 7\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
	reader := observedReader{ctx: context.Background(), destination: Destination{Host: "example", Path: "Projects/app"}}
	if _, err := reader.ReadFile("/home/deploy/Projects/app/1234abcd/services.json"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing remote file: %v", err)
	}
	args, err := os.ReadFile(filepath.Join(directory, "args"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ControlPath=none", "StrictHostKeyChecking=yes", "UpdateHostKeys=no"} {
		if !strings.Contains(string(args), want) {
			t.Fatalf("missing %s", want)
		}
	}
	if err := os.WriteFile(ssh, []byte("#!/bin/sh\nexit 255\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.ReadFile("/home/deploy/Projects/app/1234abcd/services.json"); err == nil || errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a broken connection was treated as a missing file: %v", err)
	}
}
