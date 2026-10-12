package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestALockSetOverRealSSHExcludesEveryAlias(t *testing.T) {
	_, directory := useSSHAliases(t)
	real := filepath.Join(directory, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(directory, "other")
	destinations := map[string]Destination{
		"app":  {Host: "alias-one", Path: filepath.Join(real, "app")},
		"data": {Host: "alias-two", Path: other},
	}
	observations := map[string]DestinationObservation{}
	for name, destination := range destinations {
		observed, err := observeDestination(context.Background(), destination)
		if err != nil {
			t.Fatal(err)
		}
		observations[name] = observed
	}
	targets, err := lockOrder(destinations, observations)
	if err != nil {
		t.Fatal(err)
	}
	set, err := acquireLockSet(context.Background(), targets, lockSetOptions{runtimeID: "1234abcd", open: OpenRunner, observe: observeDestination})
	if err != nil {
		t.Fatal(err)
	}

	// another alias and a symlinked spelling of the same root reach the same lock
	runner, closeRunner, err := OpenRunner(Destination{Host: "alias-two"})
	if err != nil {
		t.Fatal(err)
	}
	defer closeRunner()
	if lock, err := AcquireLock(runner, NewLayout(filepath.Join(link, "app"), "1234abcd"), "deploy", "aaaaaaa", true); err == nil {
		lock.Release()
		t.Fatal("a deploy through another alias ran while the set held the destination")
	} else if !strings.Contains(err.Error(), "another operation holds it") {
		t.Fatal(err)
	}

	operation, err := set.Begin("deploy", "ccccccc")
	if err != nil {
		t.Fatal(err)
	}
	finished := exitOK
	set.Finish(&finished)
	for _, root := range []string{filepath.Join(real, "app"), other} {
		if _, err := os.Stat(operationFile(NewLayout(root, "1234abcd"))); !os.IsNotExist(err) {
			t.Fatalf("operation %s was not cleared over ssh: %v", operation.ID, err)
		}
	}
	lock, err := AcquireLock(runner, NewLayout(filepath.Join(link, "app"), "1234abcd"), "deploy", "aaaaaaa", false)
	if err != nil {
		t.Fatalf("the set did not release over ssh: %v", err)
	}
	lock.Release()
}
