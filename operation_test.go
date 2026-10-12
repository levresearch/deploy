package main

import (
	"os"
	"strings"
	"testing"
)

func pendingOperation(t *testing.T, layout Layout) Operation {
	t.Helper()
	operation, found, err := readOperation(LocalRunner{}, operationFile(layout))
	if err != nil || !found {
		t.Fatalf("expected an unresolved operation: found=%v %v", found, err)
	}
	return operation
}

func TestAnObservedOutcomeClearsTheOperation(t *testing.T) {
	for _, exitCode := range []int{exitOK, exitDeployFailed, exitPreconditionNotMet} {
		layout := NewLayout(t.TempDir(), "abcd1234")
		lock, err := AcquireLock(LocalRunner{}, layout, "deploy", "aaaaaaa", false)
		if err != nil {
			t.Fatal(err)
		}
		operation := pendingOperation(t, layout)
		if operation.Command != "deploy" || operation.Target != "aaaaaaa" || operation.PID != os.Getpid() {
			t.Fatalf("operation does not describe the deploy: %+v", operation)
		}

		finished := exitCode
		lock.Finish(&finished)
		if _, err := os.Stat(operationFile(layout)); !os.IsNotExist(err) {
			t.Fatalf("exit %d left the operation recorded: %v", exitCode, err)
		}
		next, err := AcquireLock(LocalRunner{}, layout, "deploy", "bbbbbbb", false)
		if err != nil {
			t.Fatalf("exit %d blocked the next deploy: %v", exitCode, err)
		}
		next.Release()
	}
}

func TestAnUnrecordedLiveReleaseNeedsResolving(t *testing.T) {
	layout := NewLayout(t.TempDir(), "abcd1234")
	lock, err := AcquireLock(LocalRunner{}, layout, "rollback", "aaaaaaa", false)
	if err != nil {
		t.Fatal(err)
	}
	finished := exitLiveButNeedsAHuman
	lock.Finish(&finished)
	operation := pendingOperation(t, layout)

	_, err = AcquireLock(LocalRunner{}, layout, "deploy", "bbbbbbb", true)
	if err == nil || !strings.Contains(err.Error(), "deploy resolve "+operation.ID) || !strings.Contains(err.Error(), "rollback aaaaaaa") {
		t.Fatalf("an unresolved operation should block even --force-unlock and name the way out: %v", err)
	}

	if err := ResolveOperation(LocalRunner{}, layout, "0123456789abcdef"); err == nil || !strings.Contains(err.Error(), operation.ID) {
		t.Fatalf("resolving the wrong id should name the recorded one: %v", err)
	}
	if err := ResolveOperation(LocalRunner{}, layout, "not-an-id"); err == nil {
		t.Fatal("a malformed id was accepted")
	}
	if err := ResolveOperation(LocalRunner{}, layout, operation.ID); err != nil {
		t.Fatal(err)
	}
	if err := ResolveOperation(LocalRunner{}, layout, operation.ID); err == nil || !strings.Contains(err.Error(), "nothing to resolve") {
		t.Fatalf("resolving twice should say there is nothing left: %v", err)
	}
	next, err := AcquireLock(LocalRunner{}, layout, "deploy", "bbbbbbb", false)
	if err != nil {
		t.Fatal(err)
	}
	next.Release()
}

func TestAPanicKeepsTheOperation(t *testing.T) {
	layout := NewLayout(t.TempDir(), "abcd1234")
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("the panic was swallowed")
			}
		}()
		exitCode := exitOK
		lock, err := AcquireLock(LocalRunner{}, layout, "deploy", "aaaaaaa", false)
		if err != nil {
			t.Fatal(err)
		}
		defer lock.Finish(&exitCode)
		panic("interrupted mid release")
	}()
	pendingOperation(t, layout)
}

func TestResolveWaitsForAnActiveOperation(t *testing.T) {
	layout := NewLayout(t.TempDir(), "abcd1234")
	lock, err := AcquireLock(LocalRunner{}, layout, "deploy", "aaaaaaa", false)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	operation := pendingOperation(t, layout)
	if err := ResolveOperation(LocalRunner{}, layout, operation.ID); err == nil || !strings.Contains(err.Error(), "another operation holds it") {
		t.Fatalf("resolve cleared a running operation: %v", err)
	}
	pendingOperation(t, layout)
}

func TestTheOperationOutlivesTheProjectDirectory(t *testing.T) {
	layout := NewLayout(t.TempDir(), "abcd1234")
	lock, err := AcquireLock(LocalRunner{}, layout, "destroy", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(layout.Root); err != nil {
		t.Fatal(err)
	}
	lock.Release()
	pendingOperation(t, layout)
}

func TestAnUnreadableOperationBlocksChanges(t *testing.T) {
	layout := NewLayout(t.TempDir(), "abcd1234")
	if err := os.MkdirAll(lockDirectory(layout), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, contents := range []string{"{not json", `{"version":9,"id":"0123456789abcdef"}`} {
		if err := os.WriteFile(operationFile(layout), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
		if lock, err := AcquireLock(LocalRunner{}, layout, "deploy", "aaaaaaa", false); err == nil {
			lock.Release()
			t.Fatalf("%q did not block a deploy", contents)
		}
	}
}

func TestAnOperationIsRecordedPrivately(t *testing.T) {
	layout := NewLayout(t.TempDir(), "abcd1234")
	lock, err := AcquireLock(LocalRunner{}, layout, "env push", "app.env", false)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	info, err := os.Stat(operationFile(layout))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("operation record mode %v", info.Mode().Perm())
	}
}

func TestResolveCommandClearsTheRecordedOperation(t *testing.T) {
	repository := newRepository(t)
	writeFile(t, repository, configFileName, `{"version":1,"id":"abcd1234","name":"resolving","services":{"app":{"image":"example"}}}`)
	destination := t.TempDir()
	layout := NewLayout(destination, "abcd1234")
	lock, err := AcquireLock(LocalRunner{}, layout, "deploy", "aaaaaaa", false)
	if err != nil {
		t.Fatal(err)
	}
	finished := exitLiveButNeedsAHuman
	lock.Finish(&finished)
	operation := pendingOperation(t, layout)

	if code := run([]string{"resolve", "0123456789abcdef", "-C", repository, "-D", destination}); code != exitPreconditionNotMet {
		t.Fatalf("resolving another id exited %d", code)
	}
	pendingOperation(t, layout)
	if code := run([]string{"resolve", operation.ID, "-C", repository, "-D", destination}); code != exitOK {
		t.Fatalf("resolve exited %d", code)
	}
	if _, err := os.Stat(operationFile(layout)); !os.IsNotExist(err) {
		t.Fatalf("resolve left the operation recorded: %v", err)
	}
}
