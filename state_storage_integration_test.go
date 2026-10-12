package main

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type stateContainerRunner struct {
	Runner
	container string
	errors    bytes.Buffer
}

func (runner stateContainerRunner) Describe() string { return "disposable state container" }

func (runner *stateContainerRunner) Pipe(command []string, input io.Reader) error {
	process := exec.Command("docker", append([]string{"exec", "-i", runner.container}, command...)...)
	process.Stdin = input
	process.Stderr = &runner.errors
	return process.Run()
}

func TestStateWriteOnFullFilesystem(t *testing.T) {
	dockerAvailable(t)
	const image = "debian:bookworm-slim@sha256:7c7b2c966bc9ee8cedfeef67e0e279108992c77681fa595db4a9d65c06ccc587"
	container := safetyDocker(t, "run", "--detach", "--rm", "--network", "none", "--read-only", "--cap-drop", "ALL", "--tmpfs", "/state:rw,size=1m,mode=0700", image, "sleep", "300")
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", container).Run() })
	runner := &stateContainerRunner{container: container}
	layout := Layout{Root: "/state"}
	if err := WriteState(runner, layout, State{Current: "aaaaaaa"}); err != nil {
		t.Fatal(err, runner.errors.String())
	}
	before := safetyDocker(t, "exec", container, "cat", layout.StateFile())
	output, err := exec.Command("docker", "exec", container, "dd", "if=/dev/zero", "of=/state/fill", "bs=4096").CombinedOutput()
	if err == nil || !strings.Contains(string(output), "No space left on device") {
		t.Fatalf("fixture did not exhaust its isolated filesystem: %s %v", output, err)
	}
	if err := WriteState(runner, layout, State{Current: "bbbbbbb"}); err == nil {
		t.Fatal("full filesystem accepted replacement")
	}
	after := safetyDocker(t, "exec", container, "cat", layout.StateFile())
	if before != after {
		t.Fatal("full filesystem corrupted previous state")
	}
	if files := safetyDocker(t, "exec", container, "find", "/state", "-name", ".deploy-state.*"); files != "" {
		t.Fatal("failed write left temporary files", files)
	}
	safetyDocker(t, "exec", container, "rm", "/state/fill")
	if err := WriteState(runner, layout, State{Current: "ccccccc"}); err != nil {
		t.Fatal("state write did not recover after freeing space", err)
	}
}

func TestStateWriteOverSSHAndInterruptedTransport(t *testing.T) {
	runner, _, _ := newSSHTestRunner(t)
	layout, _ := stateStorageFixture(t)
	if err := WriteState(runner, layout, State{Current: "bbbbbbb", Environment: "production"}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(layout.StateFile())
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteState(truncatedStateRunner{runner}, layout, State{Current: "ccccccc"}); err == nil {
		t.Fatal("remote accepted incomplete transfer")
	}
	assertStateStorage(t, layout, before)
	process := runner.ssh(ShellCommand([]string{"sh", "-c", atomicStateWrite, "deploy-state", layout.StateFile(), "1000000"}))
	input, err := process.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { input.Close(); process.Process.Kill() })
	if _, err := input.Write([]byte(`{"current":`)); err != nil {
		t.Fatal(err)
	}
	waitForStateTemporary(t, layout, true)
	if err := process.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	process.Wait()
	input.Close()
	waitForStateTemporary(t, layout, false)
	assertStateStorage(t, layout, before)
	if err := WriteState(runner, layout, State{Current: "ddddddd"}); err != nil {
		t.Fatal("subsequent write failed", err)
	}
	after, _ := os.ReadFile(layout.StateFile())
	if bytes.Equal(before, after) {
		t.Fatal("subsequent write was lost")
	}
}

func waitForStateTemporary(t *testing.T, layout Layout, present bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		files, err := filepath.Glob(filepath.Join(layout.Root, ".deploy-state.*"))
		if err != nil {
			t.Fatal(err)
		}
		if (len(files) > 0) == present {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("temporary state presence did not become %v", present)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
