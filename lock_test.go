package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestLockProcessHelper(t *testing.T) {
	root := os.Getenv("DEPLOY_LOCK_HELPER_ROOT")
	if root == "" {
		return
	}
	lock, err := AcquireLock(LocalRunner{}, NewLayout(root, "abcd1234"), "deploy", "aaaaaaa", false)
	if err != nil {
		os.Exit(2)
	}
	lock.ReleaseOnSignal()
	if os.Getenv("DEPLOY_LOCK_HELPER_LOSE") == "1" {
		lock.process.Process.Kill()
		time.Sleep(5 * time.Second)
		os.Exit(2)
	}
	fmt.Println("ready")
	io.Copy(io.Discard, os.Stdin)
	lock.Release()
	os.Exit(0)
}

func TestLockSurvivesProjectRemovalAndProcessExit(t *testing.T) {
	root := t.TempDir()
	command := exec.Command(os.Args[0], "-test.run=^TestLockProcessHelper$")
	command.Env = append(os.Environ(), "DEPLOY_LOCK_HELPER_ROOT="+root)
	input, _ := command.StdinPipe()
	output, _ := command.StdoutPipe()
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { input.Close(); command.Process.Kill() })
	if line, err := bufio.NewReader(output).ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatalf("child did not acquire lock: %q %v", line, err)
	}
	layout := NewLayout(root, "abcd1234")
	if err := os.RemoveAll(layout.Root); err != nil {
		t.Fatal(err)
	}
	if lock, err := AcquireLock(LocalRunner{}, layout, "destroy", "", true); err == nil {
		lock.Release()
		t.Fatal("project removal or force-unlock bypassed active lock")
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	command.Wait()
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, err := AcquireLock(LocalRunner{}, layout, "deploy", "bbbbbbb", false)
		if err != nil && strings.Contains(err.Error(), "did not finish") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("a killed owner should leave the lock free and its operation unresolved: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	operation, found, err := readOperation(LocalRunner{}, operationFile(layout))
	if err != nil || !found {
		t.Fatalf("the killed operation was not recorded: %v", err)
	}
	if err := ResolveOperation(LocalRunner{}, layout, operation.ID); err != nil {
		t.Fatal(err)
	}
	lock, err := AcquireLock(LocalRunner{}, layout, "deploy", "bbbbbbb", false)
	if err != nil {
		t.Fatalf("a resolved operation still blocks: %v", err)
	}
	lock.Release()
}

func TestLostLockStopsTheOperation(t *testing.T) {
	command := exec.Command(os.Args[0], "-test.run=^TestLockProcessHelper$")
	command.Env = append(os.Environ(), "DEPLOY_LOCK_HELPER_ROOT="+t.TempDir(), "DEPLOY_LOCK_HELPER_LOSE=1")
	err := command.Run()
	exit, ok := err.(*exec.ExitError)
	if !ok || exit.ExitCode() != exitDeployFailed {
		t.Fatalf("lost lock did not stop operation: %v", err)
	}
}

type changingConfirmation struct {
	io.Reader
	change func()
}

func (reader *changingConfirmation) Read(body []byte) (int, error) {
	if reader.change != nil {
		reader.change()
		reader.change = nil
	}
	return reader.Reader.Read(body)
}

func TestMutatingCommandsRespectLocksAndConfirmation(t *testing.T) {
	repository := newRepository(t)
	writeFile(t, repository, configFileName, `{"version":1,"id":"abcd1234","name":"locked","services":{"app":{"image":"example"}}}`)
	options := DeployOptions{Context: repository, Destination: t.TempDir(), Environment: "production", ForceUnlock: true}
	layout := NewLayout(options.Destination, "abcd1234")
	lock, err := AcquireLock(LocalRunner{}, layout, "deploy", "aaaaaaa", false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(lock.Release)
	p := loadAndResolve(t, `{"version":1,"id":"abcd1234","name":"locked","services":{"app":{"image":"example"}}}`, "production")
	if _, err := claimEnvironment(LocalRunner{}, p, layout); err != nil {
		t.Fatal(err)
	}
	secret := writeConfig(t, "test-only")
	if _, err := RunEnvPush(options, secret); err == nil {
		t.Fatal("env push bypassed active lock")
	}
	if _, err := RunRollback(options, ""); err == nil || !strings.Contains(err.Error(), "active lock") {
		t.Fatalf("rollback did not respect active lock: %v", err)
	}
	if _, err := RunDestroy(options, true, strings.NewReader("locked\n")); err == nil || !strings.Contains(err.Error(), "active lock") {
		t.Fatalf("destroy bypassed active lock: %v", err)
	}
	finished := exitOK
	lock.Finish(&finished)
	confirmation := &changingConfirmation{Reader: strings.NewReader("locked\n"), change: func() {
		if err := WriteState(LocalRunner{}, layout, State{Environment: "production", Current: "aaaaaaa", Releases: []string{"aaaaaaa"}}); err != nil {
			t.Fatal(err)
		}
	}}
	if _, err := RunDestroy(options, true, confirmation); err == nil || !strings.Contains(err.Error(), "changed during confirmation") {
		t.Fatalf("destroy accepted a stale confirmation: %v", err)
	}
	state, err := ReadState(LocalRunner{}, layout)
	if err != nil || state.Current != "aaaaaaa" {
		t.Fatal("refused destroy changed the deployment")
	}
}
