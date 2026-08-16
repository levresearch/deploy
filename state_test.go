package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestRecordRelease(t *testing.T) {
	state := State{}

	state = state.RecordRelease("aaaaaaa1111", defaultEnvironmentName)
	if state.Current != "aaaaaaa" || state.Previous != "" {
		t.Fatalf("first deploy: current=%q previous=%q", state.Current, state.Previous)
	}

	state = state.RecordRelease("bbbbbbb2222", defaultEnvironmentName)
	if state.Current != "bbbbbbb" || state.Previous != "aaaaaaa" {
		t.Fatalf("second deploy: current=%q previous=%q", state.Current, state.Previous)
	}
	if want := []string{"bbbbbbb", "aaaaaaa"}; !slices.Equal(state.Releases, want) {
		t.Errorf("releases = %v, want newest first %v", state.Releases, want)
	}

	// redeploying what is already current must not make it its own rollback
	// target, or there is nothing left to roll back to
	state = state.RecordRelease("bbbbbbb2222", defaultEnvironmentName)
	if state.Previous != "aaaaaaa" {
		t.Errorf("redeploying current changed previous to %q", state.Previous)
	}
	if count := len(state.Releases); count != 2 {
		t.Errorf("redeploying current duplicated a release, got %v", state.Releases)
	}
}

func TestReleasesToPrune(t *testing.T) {
	cases := []struct {
		name      string
		state     State
		retention int
		want      []string
	}{
		{
			name:      "first deploy has nothing to prune",
			state:     State{Current: "aaaaaaa", Releases: []string{"aaaaaaa"}},
			retention: 3,
			want:      nil,
		},
		{
			name:      "three releases at retention three are all kept",
			state:     State{Current: "ccccccc", Previous: "bbbbbbb", Releases: []string{"ccccccc", "bbbbbbb", "aaaaaaa"}},
			retention: 3,
			want:      nil,
		},
		{
			name:      "a fourth deploy prunes the oldest",
			state:     State{Current: "ddddddd", Previous: "ccccccc", Releases: []string{"ddddddd", "ccccccc", "bbbbbbb", "aaaaaaa"}},
			retention: 3,
			want:      []string{"aaaaaaa"},
		},
		{
			name:      "retention of one still keeps current and previous",
			state:     State{Current: "ccccccc", Previous: "bbbbbbb", Releases: []string{"ccccccc", "bbbbbbb", "aaaaaaa"}},
			retention: 1,
			want:      []string{"aaaaaaa"},
		},
		{
			name:      "a nonsense retention is treated as one rather than pruning everything",
			state:     State{Current: "ccccccc", Previous: "bbbbbbb", Releases: []string{"ccccccc", "bbbbbbb"}},
			retention: 0,
			want:      nil,
		},
		{
			name:      "several old releases all go",
			state:     State{Current: "eeeeeee", Previous: "ddddddd", Releases: []string{"eeeeeee", "ddddddd", "ccccccc", "bbbbbbb", "aaaaaaa"}},
			retention: 2,
			want:      []string{"ccccccc", "bbbbbbb", "aaaaaaa"},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := testCase.state.ReleasesToPrune(testCase.retention)

			if !slices.Equal(got, testCase.want) {
				t.Errorf("ReleasesToPrune(%d) = %v, want %v", testCase.retention, got, testCase.want)
			}
			for _, pruned := range got {
				if pruned == testCase.state.Current {
					t.Error("current must never be pruned, it is serving")
				}
				if pruned == testCase.state.Previous {
					t.Error("previous must never be pruned, it is the rollback target")
				}
			}
		})
	}
}

func TestPruneFenceRefusesAnythingOutsideReleases(t *testing.T) {
	const releasesRoot = "/srv/projects/a3f19c02/releases"

	refused := []string{
		"..",
		"../..",
		"../volumes",
		"/etc",
		"/",
		"",
		".",
		"a3f19c02/../../etc",
		"9f4be0a/../../../volumes",
		"not-a-commit",
		"9F4BE0A",
		"9f4be0",
		"; rm -rf /",
		"9f4be0a; rm -rf /",
		"9f4be0a/../../env",
	}

	for _, commit := range refused {
		t.Run(commit, func(t *testing.T) {
			if resolved, err := releasePathWithin(releasesRoot, commit); err == nil {
				t.Errorf("the fence let %q through as %q", commit, resolved)
			}
		})
	}

	allowed, err := releasePathWithin(releasesRoot, "9f4be0a")
	if err != nil {
		t.Fatalf("a real commit should be allowed: %v", err)
	}
	if allowed != releasesRoot+"/9f4be0a" {
		t.Errorf("resolved to %q", allowed)
	}
}

func TestPruneFenceRefusesASymlinkEscapingTheRoot(t *testing.T) {
	root := t.TempDir()
	releases := filepath.Join(root, "releases")
	volumes := filepath.Join(root, "volumes")

	for _, directory := range []string{releases, volumes} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatalf("creating %s: %v", directory, err)
		}
	}
	if err := os.WriteFile(filepath.Join(volumes, "database"), []byte("precious"), 0o644); err != nil {
		t.Fatalf("writing volume data: %v", err)
	}

	// a release directory replaced by a link to the volumes it must never touch
	escaping := filepath.Join(releases, "9f4be0a")
	if err := os.Symlink(volumes, escaping); err != nil {
		t.Fatalf("creating symlink: %v", err)
	}

	if err := resolvedWithinRoot(releases, escaping); err == nil {
		t.Fatal("the fence should refuse a release that resolves outside releases/")
	}

	// and the real thing still passes
	real := filepath.Join(releases, "aaaaaaa")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatalf("creating release: %v", err)
	}
	if err := resolvedWithinRoot(releases, real); err != nil {
		t.Errorf("a genuine release directory should be allowed: %v", err)
	}
}

func TestPruneRemovesOldReleasesAndLeavesVolumesAlone(t *testing.T) {
	destination := t.TempDir()
	layout := NewLayout(destination, "a3f19c02")
	runner := LocalRunner{}

	volumes := filepath.Join(layout.Root, "volumes")
	if err := os.MkdirAll(volumes, 0o755); err != nil {
		t.Fatalf("creating volumes: %v", err)
	}
	if err := os.WriteFile(filepath.Join(volumes, "database"), []byte("precious"), 0o644); err != nil {
		t.Fatalf("writing volume data: %v", err)
	}

	releases := []string{"ddddddd", "ccccccc", "bbbbbbb", "aaaaaaa"}
	for _, release := range releases {
		if err := os.MkdirAll(filepath.Join(layout.Releases(), release), 0o755); err != nil {
			t.Fatalf("creating release %s: %v", release, err)
		}
	}

	state := State{Current: "ddddddd", Previous: "ccccccc", Releases: releases}
	state, err := PruneReleases(runner, layout, "a3f19c02", state, 3)
	if err != nil {
		t.Fatalf("PruneReleases: %v", err)
	}

	if want := []string{"ddddddd", "ccccccc", "bbbbbbb"}; !slices.Equal(state.Releases, want) {
		t.Errorf("releases = %v, want %v", state.Releases, want)
	}
	if _, err := os.Stat(filepath.Join(layout.Releases(), "aaaaaaa")); err == nil {
		t.Error("the oldest release should have been removed from disk")
	}
	for _, kept := range []string{"ddddddd", "ccccccc", "bbbbbbb"} {
		if _, err := os.Stat(filepath.Join(layout.Releases(), kept)); err != nil {
			t.Errorf("release %s should still be on disk: %v", kept, err)
		}
	}

	precious, err := os.ReadFile(filepath.Join(volumes, "database"))
	if err != nil || string(precious) != "precious" {
		t.Error("pruning must never be able to reach volumes/")
	}
}

func TestStateRoundTrips(t *testing.T) {
	layout := NewLayout(t.TempDir(), "a3f19c02")
	runner := LocalRunner{}

	empty, err := ReadState(runner, layout)
	if err != nil {
		t.Fatalf("a project with no state yet is not an error: %v", err)
	}
	if empty.Current != "" {
		t.Errorf("expected empty state, got %+v", empty)
	}

	if err := runner.MkdirAll(layout.Root); err != nil {
		t.Fatalf("creating project root: %v", err)
	}

	written := State{Current: "bbbbbbb", Previous: "aaaaaaa", Environment: "production", Releases: []string{"bbbbbbb", "aaaaaaa"}}
	if err := WriteState(runner, layout, written); err != nil {
		t.Fatalf("WriteState: %v", err)
	}

	read, err := ReadState(runner, layout)
	if err != nil {
		t.Fatalf("ReadState: %v", err)
	}
	if read.Current != written.Current || read.Previous != written.Previous {
		t.Errorf("read back %+v, want %+v", read, written)
	}
	if !slices.Equal(read.Releases, written.Releases) {
		t.Errorf("releases = %v, want %v", read.Releases, written.Releases)
	}
	if read.UpdatedAt.IsZero() {
		t.Error("writing state should stamp updatedAt")
	}
}

func TestLockBlocksASecondDeployAndForceUnlockBreaksIt(t *testing.T) {
	layout := NewLayout(t.TempDir(), "a3f19c02")
	runner := LocalRunner{}

	first, err := AcquireLock(runner, layout, "aaaaaaa", false)
	if err != nil {
		t.Fatalf("taking the lock: %v", err)
	}

	_, err = AcquireLock(runner, layout, "bbbbbbb", false)
	if err == nil {
		t.Fatal("a second deploy must not be able to take a held lock")
	}
	if !strings.Contains(err.Error(), "--force-unlock") {
		t.Errorf("the refusal should name the way out, got: %v", err)
	}

	forced, err := AcquireLock(runner, layout, "bbbbbbb", true)
	if err != nil {
		t.Fatalf("--force-unlock should break a stale lock: %v", err)
	}
	forced.Release()

	if _, err := os.Stat(layout.LockFile()); err == nil {
		t.Error("releasing the lock should remove the file")
	}

	// and the project is deployable again afterwards
	again, err := AcquireLock(runner, layout, "ccccccc", false)
	if err != nil {
		t.Fatalf("the lock should be free again: %v", err)
	}
	again.Release()

	first.Release()
}

// A lock left by a deploy that is definitely gone is not a lock, it is litter.
// Making someone type --force-unlock after a ctrl-c teaches them to reach for
// that flag by reflex, which is the last thing you want on a flag that exists to
// override a safety check.
func TestAStaleLockFromThisMachineIsClearedAutomatically(t *testing.T) {
	layout := NewLayout(t.TempDir(), "a3f19c02")
	runner := LocalRunner{}

	// a real process that has already exited, so its pid is genuinely dead
	finished := exec.Command("true")
	if err := finished.Run(); err != nil {
		t.Fatalf("running a throwaway process: %v", err)
	}
	deadPID := finished.Process.Pid

	hostname, err := os.Hostname()
	if err != nil {
		t.Fatalf("reading hostname: %v", err)
	}

	if err := runner.MkdirAll(layout.Root); err != nil {
		t.Fatalf("creating project root: %v", err)
	}
	writeLockFile(t, runner, layout, Lock{
		PID: deadPID, Host: hostname, Commit: "aaaaaaa",
		TakenAt: time.Now().Add(-5 * time.Minute).UTC(),
	})

	lock, err := AcquireLock(runner, layout, "bbbbbbb", false)
	if err != nil {
		t.Fatalf("a lock held by a dead process should be cleared without --force-unlock: %v", err)
	}
	lock.Release()
}

func TestALiveLockIsNeverClearedAutomatically(t *testing.T) {
	layout := NewLayout(t.TempDir(), "a3f19c02")
	runner := LocalRunner{}

	hostname, err := os.Hostname()
	if err != nil {
		t.Fatalf("reading hostname: %v", err)
	}
	if err := runner.MkdirAll(layout.Root); err != nil {
		t.Fatalf("creating project root: %v", err)
	}

	// this test's own pid is unarguably alive
	writeLockFile(t, runner, layout, Lock{
		PID: os.Getpid(), Host: hostname, Commit: "aaaaaaa", TakenAt: time.Now().UTC(),
	})

	if _, err := AcquireLock(runner, layout, "bbbbbbb", false); err == nil {
		t.Fatal("a lock held by a running deploy must not be cleared")
	}
}

// A pid on another machine means nothing here, and guessing would let two
// deploys run at once against the same project.
func TestALockFromAnotherMachineIsLeftAlone(t *testing.T) {
	layout := NewLayout(t.TempDir(), "a3f19c02")
	runner := LocalRunner{}

	if err := runner.MkdirAll(layout.Root); err != nil {
		t.Fatalf("creating project root: %v", err)
	}
	writeLockFile(t, runner, layout, Lock{
		PID: 999999, Host: "some-other-laptop", Commit: "aaaaaaa",
		TakenAt: time.Now().Add(-time.Hour).UTC(),
	})

	_, err := AcquireLock(runner, layout, "bbbbbbb", false)
	if err == nil {
		t.Fatal("a lock from another machine must not be cleared, even an old one")
	}
	if !strings.Contains(err.Error(), "--force-unlock") {
		t.Errorf("the refusal should still name the way out, got: %v", err)
	}

	// and forcing still works, because that is what the flag is for
	forced, err := AcquireLock(runner, layout, "bbbbbbb", true)
	if err != nil {
		t.Fatalf("--force-unlock should break any lock: %v", err)
	}
	forced.Release()
}

func writeLockFile(t *testing.T, runner Runner, layout Layout, lock Lock) {
	t.Helper()

	encoded, err := json.Marshal(lock)
	if err != nil {
		t.Fatalf("encoding lock: %v", err)
	}
	if err := runner.WriteFile(layout.LockFile(), append(encoded, '\n')); err != nil {
		t.Fatalf("writing lock: %v", err)
	}
}
