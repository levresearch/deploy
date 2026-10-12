package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func stateStorageFixture(t *testing.T) (Layout, []byte) {
	t.Helper()
	layout := NewLayout(filepath.Join(t.TempDir(), "it's a project"), "abcd1234")
	if err := os.MkdirAll(layout.Root, 0700); err != nil {
		t.Fatal(err)
	}
	before := []byte(`{"current":"aaaaaaa","environment":"production","releases":["aaaaaaa"]}`)
	if err := os.WriteFile(layout.StateFile(), before, 0600); err != nil {
		t.Fatal(err)
	}
	return layout, before
}

func assertStateStorage(t *testing.T, layout Layout, expected []byte) {
	t.Helper()
	after, err := os.ReadFile(layout.StateFile())
	if err != nil || !bytes.Equal(after, expected) {
		t.Fatalf("state changed: %q %v", after, err)
	}
	leftovers, err := filepath.Glob(filepath.Join(layout.Root, ".deploy-state.*"))
	if err != nil || len(leftovers) != 0 {
		t.Fatalf("temporary state leaked: %v %v", leftovers, err)
	}
}

func TestStateReplacementPreservesOldInodeAndPrivatePermissions(t *testing.T) {
	layout, before := stateStorageFixture(t)
	state, err := ReadState(LocalRunner{}, layout)
	if err != nil || state.Version != 0 || state.Current != "aaaaaaa" {
		t.Fatalf("legacy state was not preserved: %+v %v", state, err)
	}
	old := filepath.Join(layout.Root, "old-inode")
	if err := os.Link(layout.StateFile(), old); err != nil {
		t.Fatal(err)
	}
	if err := WriteState(LocalRunner{}, layout, State{Current: "bbbbbbb", Environment: "production"}); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(old)
	if err != nil || !bytes.Equal(contents, before) {
		t.Fatal("write truncated the previous inode", err)
	}
	state, err = ReadState(LocalRunner{}, layout)
	if err != nil || state.Current != "bbbbbbb" || state.Version != 1 {
		t.Fatalf("replacement state: %+v %v", state, err)
	}
	info, err := os.Stat(layout.StateFile())
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("state permissions are not private", err)
	}
}

type truncatedStateRunner struct{ Runner }

func (runner truncatedStateRunner) Pipe(command []string, input io.Reader) error {
	return runner.Runner.Pipe(command, io.LimitReader(input, 8))
}

func TestIncompleteStateTransferPreservesPreviousState(t *testing.T) {
	layout, before := stateStorageFixture(t)
	if err := WriteState(truncatedStateRunner{LocalRunner{}}, layout, State{Current: "bbbbbbb"}); err == nil {
		t.Fatal("accepted incomplete transfer")
	}
	assertStateStorage(t, layout, before)
}

func TestFailedStateWritesPreservePreviousState(t *testing.T) {
	for _, program := range []string{"cat", "sync", "mv", "mktemp"} {
		t.Run(program, func(t *testing.T) {
			layout, before := stateStorageFixture(t)
			bin := t.TempDir()
			if err := os.WriteFile(filepath.Join(bin, program), []byte("#!/bin/sh\necho sentinel-private-error >&2\nexit 1\n"), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			err := WriteState(LocalRunner{}, layout, State{Current: "bbbbbbb"})
			if err == nil || strings.Contains(err.Error(), "sentinel-private-error") {
				t.Fatal("write failure was lost or disclosed stderr", err)
			}
			assertStateStorage(t, layout, before)
		})
	}
}

func TestStateDirectorySyncFailureReportsUncertainOutcome(t *testing.T) {
	layout, _ := stateStorageFixture(t)
	bin := t.TempDir()
	writeFile(t, bin, "sync", "#!/bin/sh\nif test -d \"$2\"; then exit 1; fi\nexec /usr/bin/sync \"$@\"\n")
	if err := os.Chmod(filepath.Join(bin, "sync"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	err := WriteState(LocalRunner{}, layout, State{Current: "bbbbbbb"})
	if err == nil || !strings.Contains(err.Error(), "state may already have changed") {
		t.Fatal("failed directory sync was reported as success", err)
	}
	state, err := ReadState(LocalRunner{}, layout)
	if err != nil || state.Current != "bbbbbbb" {
		t.Fatal("post-rename failure corrupted state", err)
	}
}

func TestStateRefusesUnsupportedVersionsAndUnsafeTargets(t *testing.T) {
	for _, filename := range []string{"", "state\x00.json", "parent\n/state.json", "parent\r/state.json"} {
		if err := writeStateFile(nil, filename, nil); err == nil {
			t.Fatal("accepted a path with ambiguous shell framing")
		}
	}
	for _, version := range []int{-1, 2, 99} {
		layout, _ := stateStorageFixture(t)
		raw := []byte(fmt.Sprintf(`{"version":%d,"current":"aaaaaaa"}`, version))
		if err := os.WriteFile(layout.StateFile(), raw, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadState(LocalRunner{}, layout); err == nil {
			t.Fatal("accepted unknown state version")
		}
		if err := WriteState(LocalRunner{}, layout, State{Version: version}); err == nil {
			t.Fatal("wrote unknown state version")
		}
		assertStateStorage(t, layout, raw)
	}
	for _, kind := range []string{"symlink", "directory"} {
		t.Run(kind, func(t *testing.T) {
			layout, before := stateStorageFixture(t)
			outside := filepath.Join(layout.Root, "outside")
			if err := os.Rename(layout.StateFile(), outside); err != nil {
				t.Fatal(err)
			}
			var err error
			if kind == "symlink" {
				err = os.Symlink(outside, layout.StateFile())
			} else {
				err = os.Mkdir(layout.StateFile(), 0700)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := WriteState(LocalRunner{}, layout, State{Current: "bbbbbbb"}); err == nil {
				t.Fatal("accepted unsafe target")
			}
			contents, _ := os.ReadFile(outside)
			if !bytes.Equal(contents, before) {
				t.Fatal("changed unrelated target")
			}
		})
	}
}

func TestReadersNeverSeePartialState(t *testing.T) {
	layout, _ := stateStorageFixture(t)
	done := make(chan error, 1)
	go func() {
		for range 20 {
			if err := WriteState(LocalRunner{}, layout, State{Name: strings.Repeat("x", 32768), Current: "bbbbbbb"}); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	for {
		if _, err := ReadState(LocalRunner{}, layout); err != nil {
			<-done
			t.Fatal("reader saw partial state", err)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
			return
		default:
		}
	}
}

func TestRollbackChecksStateToolsBeforeChangingContainers(t *testing.T) {
	repository := newRepository(t)
	writeFile(t, repository, configFileName, `{"version":1,"id":"abcd1234","name":"state-check","services":{"app":{"image":"example"}}}`)
	layout := NewLayout(t.TempDir(), "abcd1234")
	if err := os.MkdirAll(layout.Root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := WriteState(LocalRunner{}, layout, State{Current: "bbbbbbb", Previous: "aaaaaaa", Environment: "production"}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(layout.StateFile())
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	marker := filepath.Join(bin, "docker-called")
	for name, script := range map[string]string{
		"sync":   "#!/bin/sh\nexit 1\n",
		"docker": "#!/bin/sh\ntouch " + ShellQuote(marker) + "\nexit 99\n",
	} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	code, err := RunRollback(DeployOptions{Context: repository, Destination: filepath.Dir(layout.Root), Environment: "production"}, "")
	if code != exitPreconditionNotMet || err == nil || !strings.Contains(err.Error(), "GNU coreutils (sync)") {
		t.Fatalf("missing storage tools were not refused: %d %v", code, err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("rollback invoked docker despite missing state tools")
	}
	assertStateStorage(t, layout, before)
}

func TestDockerFixtureSeparatesProgressFromContainerID(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte("#!/bin/sh\necho pull-progress >&2\necho container-id\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if got := safetyDocker(t, "run"); got != "container-id" {
		t.Fatal("image pull progress contaminated container identity", got)
	}
}
