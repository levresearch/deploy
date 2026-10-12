package main

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
)

func observedAt(host, root string) DestinationObservation {
	return DestinationObservation{HostIdentity: host, CanonicalPath: root, bootID: "boot-" + host, daemonID: "daemon-" + host}
}

// localLockSet locks real temporary roots with the local runner, and observes
// whatever observations says each destination looks like right now.
func localLockSet(t *testing.T, observations map[string]DestinationObservation, generations map[string]int, now map[string]DestinationObservation) (*LockSet, error) {
	t.Helper()
	destinations := map[string]Destination{}
	byPath := map[string]DestinationObservation{}
	for name, observed := range observations {
		destinations[name] = Destination{Path: observed.CanonicalPath}
		byPath[observed.CanonicalPath] = observed
		if current, changed := now[name]; changed {
			byPath[observed.CanonicalPath] = current
		}
	}
	targets, err := lockOrder(destinations, observations)
	if err != nil {
		return nil, err
	}
	return acquireLockSet(context.Background(), targets, lockSetOptions{
		runtimeID:   "1234abcd",
		generations: generations,
		open:        func(Destination) (Runner, func(), error) { return LocalRunner{}, func() {}, nil },
		observe: func(_ context.Context, destination Destination) (DestinationObservation, error) {
			return byPath[destination.Path], nil
		},
	})
}

func lockedElsewhere(t *testing.T, root string) bool {
	t.Helper()
	lock, err := holdLock(LocalRunner{}, NewLayout(root, "1234abcd"), false)
	if err != nil {
		return true
	}
	lock.Release()
	return false
}

func TestLockOrderIsTheSameWhateverTheNames(t *testing.T) {
	observations := map[string]DestinationObservation{
		"zeta":  observedAt("aaaa", "/srv/one"),
		"alpha": observedAt("bbbb", "/srv/one"),
		"mid":   observedAt("aaaa", "/srv/two"),
	}
	destinations := map[string]Destination{"zeta": {Path: "/srv/one"}, "alpha": {Host: "b", Path: "/srv/one"}, "mid": {Path: "/srv/two"}}
	for range 20 {
		targets, err := lockOrder(destinations, observations)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, target := range targets {
			names = append(names, target.Name)
		}
		if !slices.Equal(names, []string{"zeta", "mid", "alpha"}) {
			t.Fatalf("locks are not taken by host and path: %v", names)
		}
	}

	observations["alias"] = observedAt("aaaa", "/srv/two")
	destinations["alias"] = Destination{Host: "other-alias", Path: "two"}
	if _, err := lockOrder(destinations, observations); err == nil || !strings.Contains(err.Error(), "alias and mid") {
		t.Fatalf("two names for one root were accepted: %v", err)
	}
	delete(observations, "alias")
	if _, err := lockOrder(destinations, observations); err == nil || !strings.Contains(err.Error(), "not been observed") {
		t.Fatalf("an unobserved destination was locked: %v", err)
	}
	if _, err := lockOrder(nil, nil); err == nil {
		t.Fatal("an empty lock set was accepted")
	}
}

func TestALockSetHoldsEveryDestinationUntilReleased(t *testing.T) {
	app, data := t.TempDir(), t.TempDir()
	observations := map[string]DestinationObservation{"app": observedAt("aaaa", app), "data": observedAt("bbbb", data)}
	set, err := localLockSet(t, observations, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !lockedElsewhere(t, app) || !lockedElsewhere(t, data) {
		t.Fatal("a destination was left unlocked")
	}
	set.Release()
	set.Release()
	if lockedElsewhere(t, app) || lockedElsewhere(t, data) {
		t.Fatal("a lock outlived its release")
	}
}

func TestAPartialLockSetReleasesWhatItTook(t *testing.T) {
	app, data := t.TempDir(), t.TempDir()
	held, err := holdLock(LocalRunner{}, NewLayout(data, "1234abcd"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()

	observations := map[string]DestinationObservation{"app": observedAt("aaaa", app), "data": observedAt("bbbb", data)}
	if _, err := localLockSet(t, observations, nil, nil); err == nil || !strings.Contains(err.Error(), "destination data") || !strings.Contains(err.Error(), "no destination was changed") {
		t.Fatalf("a busy destination did not stop the set: %v", err)
	}
	if lockedElsewhere(t, app) {
		t.Fatal("the destination locked before the failure stayed locked")
	}
}

func TestALockSetRechecksDestinationsUnderTheLocks(t *testing.T) {
	app, data := t.TempDir(), t.TempDir()
	observations := map[string]DestinationObservation{"app": observedAt("aaaa", app), "data": observedAt("bbbb", data)}

	rebooted := observedAt("bbbb", data)
	rebooted.bootID = "after-reboot"
	moved := observedAt("bbbb", data+"-elsewhere")
	replaced := observedAt("bbbb", data)
	replaced.daemonID = "another-daemon"
	for name, now := range map[string]DestinationObservation{"reboot": rebooted, "path": moved, "daemon": replaced} {
		if _, err := localLockSet(t, observations, nil, map[string]DestinationObservation{"data": now}); err == nil || !strings.Contains(err.Error(), "changed since it was planned") {
			t.Fatalf("%s change was not noticed: %v", name, err)
		}
		if lockedElsewhere(t, app) || lockedElsewhere(t, data) {
			t.Fatalf("%s change left a lock held", name)
		}
	}
}

func TestALockSetRefusesStateThatMovedSincePlanning(t *testing.T) {
	app, data := t.TempDir(), t.TempDir()
	observations := map[string]DestinationObservation{"app": observedAt("aaaa", app), "data": observedAt("bbbb", data)}
	layout := NewLayout(data, "1234abcd")
	if err := os.MkdirAll(layout.Root, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteServiceState(LocalRunner{}, layout, recordedServiceState()); err != nil {
		t.Fatal(err)
	}

	if _, err := localLockSet(t, observations, nil, nil); err == nil || !strings.Contains(err.Error(), "generation 0 to 1") {
		t.Fatalf("state written after planning was ignored: %v", err)
	}
	set, err := localLockSet(t, observations, map[string]int{"data": 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	set.Release()

	wrong := recordedServiceState()
	wrong.Destination = "app"
	appLayout := NewLayout(app, "1234abcd")
	if err := os.MkdirAll(appLayout.Root, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteServiceState(LocalRunner{}, appLayout, wrong); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(appLayout.ServiceStateFile()); err != nil {
		t.Fatal(err)
	}
	if _, err := localLockSet(t, map[string]DestinationObservation{"data": observedAt("aaaa", app)}, map[string]int{"data": 1}, nil); err == nil || !strings.Contains(err.Error(), "refusing to change it") {
		t.Fatalf("state owned by another destination was accepted: %v", err)
	}
}

func TestOneOperationCoversEveryDestination(t *testing.T) {
	app, data := t.TempDir(), t.TempDir()
	observations := map[string]DestinationObservation{"app": observedAt("aaaa", app), "data": observedAt("bbbb", data)}
	set, err := localLockSet(t, observations, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	operation, err := set.Begin("deploy", "ccccccc")
	if err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{app, data} {
		if recorded := pendingOperation(t, NewLayout(root, "1234abcd")); recorded.ID != operation.ID {
			t.Fatalf("destinations disagree on the operation: %s %s", recorded.ID, operation.ID)
		}
	}
	finished := exitLiveButNeedsAHuman
	set.Finish(&finished)
	for _, root := range []string{app, data} {
		pendingOperation(t, NewLayout(root, "1234abcd"))
	}

	if _, err := localLockSet(t, observations, nil, nil); err == nil || !strings.Contains(err.Error(), "deploy resolve "+operation.ID) {
		t.Fatalf("an unfinished operation did not block the next set: %v", err)
	}
	for _, root := range []string{app, data} {
		if err := ResolveOperation(LocalRunner{}, NewLayout(root, "1234abcd"), operation.ID); err != nil {
			t.Fatal(err)
		}
	}

	set, err = localLockSet(t, observations, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := set.Begin("deploy", "ddddddd"); err != nil {
		t.Fatal(err)
	}
	finished = exitDeployFailed
	set.Finish(&finished)
	for _, root := range []string{app, data} {
		if _, err := os.Stat(operationFile(NewLayout(root, "1234abcd"))); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("an observed failure left the operation behind: %v", err)
		}
	}
}

func TestAPanicKeepsTheOperationOnEveryDestination(t *testing.T) {
	app, data := t.TempDir(), t.TempDir()
	observations := map[string]DestinationObservation{"app": observedAt("aaaa", app), "data": observedAt("bbbb", data)}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("the panic was swallowed")
			}
		}()
		set, err := localLockSet(t, observations, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		exitCode := exitOK
		defer set.Finish(&exitCode)
		if _, err := set.Begin("deploy", "ccccccc"); err != nil {
			t.Fatal(err)
		}
		panic("interrupted between destinations")
	}()
	for _, root := range []string{app, data} {
		pendingOperation(t, NewLayout(root, "1234abcd"))
	}
	if lockedElsewhere(t, app) || lockedElsewhere(t, data) {
		t.Fatal("a panic left a lock held")
	}
}

func TestAFailedRecordClearsTheOthers(t *testing.T) {
	app, data := t.TempDir(), t.TempDir()
	observations := map[string]DestinationObservation{"app": observedAt("aaaa", app), "data": observedAt("bbbb", data)}
	set, err := localLockSet(t, observations, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer set.Release()
	// a directory where the record goes makes the second write fail
	if err := os.Mkdir(operationFile(NewLayout(data, "1234abcd")), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := set.Begin("deploy", "ccccccc"); err == nil {
		t.Fatal("a failed record was ignored")
	}
	if _, err := os.Stat(operationFile(NewLayout(app, "1234abcd"))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the first destination kept a record for an operation that never started: %v", err)
	}
}
