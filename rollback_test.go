package main

import (
	"os/exec"
	"strings"
	"testing"
)

func TestRollbackTargetSelection(t *testing.T) {
	threeReleases := State{
		Current:  "ccccccc",
		Previous: "bbbbbbb",
		Releases: []string{"ccccccc", "bbbbbbb", "aaaaaaa"},
	}
	allOnDisk := []string{"aaaaaaa", "bbbbbbb", "ccccccc"}

	cases := []struct {
		name        string
		state       State
		requested   string
		onDisk      []string
		want        string
		wantRefusal string
	}{
		{
			name:   "no argument means the previous release",
			state:  threeReleases,
			onDisk: allOnDisk,
			want:   "bbbbbbb",
		},
		{
			name:      "any retained release can be named",
			state:     threeReleases,
			requested: "aaaaaaa",
			onDisk:    allOnDisk,
			want:      "aaaaaaa",
		},
		{
			name:      "a full length commit is accepted where a short one is stored",
			state:     threeReleases,
			requested: "aaaaaaa1111111111111111111111111111111",
			onDisk:    allOnDisk,
			want:      "aaaaaaa",
		},
		{
			name:        "a pruned release is refused",
			state:       threeReleases,
			requested:   "9999999",
			onDisk:      allOnDisk,
			wantRefusal: "not available",
		},
		{
			name:        "a release deploy knows but disk does not is refused too",
			state:       threeReleases,
			requested:   "aaaaaaa",
			onDisk:      []string{"bbbbbbb", "ccccccc"},
			wantRefusal: "not available",
		},
		{
			name:        "rolling back to what is already running is refused",
			state:       threeReleases,
			requested:   "ccccccc",
			onDisk:      allOnDisk,
			wantRefusal: "already what is running",
		},
		{
			name:        "a project with one release has nowhere to go",
			state:       State{Current: "aaaaaaa", Releases: []string{"aaaaaaa"}},
			onDisk:      []string{"aaaaaaa"},
			wantRefusal: "nothing to roll back to",
		},
		{
			name:        "a project never deployed",
			state:       State{},
			wantRefusal: "never been deployed",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got, err := RollbackTarget(testCase.state, testCase.requested, testCase.onDisk)

			if testCase.wantRefusal != "" {
				if err == nil {
					t.Fatalf("expected a refusal, got target %q", got)
				}
				if !strings.Contains(err.Error(), testCase.wantRefusal) {
					t.Errorf("refusal should mention %q, got: %v", testCase.wantRefusal, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("RollbackTarget: %v", err)
			}
			if got != testCase.want {
				t.Errorf("target = %q, want %q", got, testCase.want)
			}
		})
	}
}

func TestARefusedRollbackListsWhatIsStillAvailable(t *testing.T) {
	state := State{
		Current:  "ccccccc",
		Previous: "bbbbbbb",
		Releases: []string{"ccccccc", "bbbbbbb", "aaaaaaa"},
	}

	_, err := RollbackTarget(state, "9999999", []string{"bbbbbbb", "ccccccc"})
	if err == nil {
		t.Fatal("expected a refusal")
	}

	// naming what is left is the difference between a dead end and a next step
	if !strings.Contains(err.Error(), "bbbbbbb") {
		t.Errorf("should list the releases still available, got: %v", err)
	}
	// aaaaaaa is in state but gone from disk, so offering it would be a lie
	if strings.Contains(err.Error(), "aaaaaaa") {
		t.Errorf("should not offer a release that is not on disk, got: %v", err)
	}
	// and the running one is not somewhere to roll back to
	if strings.Contains(err.Error(), "ccccccc") {
		t.Errorf("should not offer the release already running, got: %v", err)
	}
}

func TestRecordRollbackSwapsCurrentAndPrevious(t *testing.T) {
	state := State{
		Current:  "ccccccc",
		Previous: "bbbbbbb",
		Releases: []string{"ccccccc", "bbbbbbb", "aaaaaaa"},
	}

	rolledBack := state.RecordRollback("bbbbbbb")

	if rolledBack.Current != "bbbbbbb" {
		t.Errorf("current = %q, want the release rolled back to", rolledBack.Current)
	}
	// the one just left has to become the rollback target, or there is no way
	// back to it after deciding the rollback was wrong
	if rolledBack.Previous != "ccccccc" {
		t.Errorf("previous = %q, want the release just left", rolledBack.Previous)
	}
	// rolling back is not a deploy, so deploy history is unchanged
	if len(rolledBack.Releases) != 3 {
		t.Errorf("releases = %v, should be untouched", rolledBack.Releases)
	}
}

func TestRollingBackTwiceReturnsToWhereItStarted(t *testing.T) {
	state := State{
		Current:  "ccccccc",
		Previous: "bbbbbbb",
		Releases: []string{"ccccccc", "bbbbbbb", "aaaaaaa"},
	}
	onDisk := []string{"aaaaaaa", "bbbbbbb", "ccccccc"}

	first, err := RollbackTarget(state, "", onDisk)
	if err != nil {
		t.Fatalf("first rollback: %v", err)
	}
	state = state.RecordRollback(first)

	second, err := RollbackTarget(state, "", onDisk)
	if err != nil {
		t.Fatalf("second rollback: %v", err)
	}
	state = state.RecordRollback(second)

	if state.Current != "ccccccc" {
		t.Errorf("current = %q, want to be back where we started", state.Current)
	}
	if state.Previous != "bbbbbbb" {
		t.Errorf("previous = %q", state.Previous)
	}
	// current and previous must never be the same, or there is nowhere to go
	if state.Current == state.Previous {
		t.Error("current and previous collapsed onto one release")
	}
}

// Neither current nor previous may ever be pruned, and rollback is what makes
// that matter, so the two have to agree after a rollback as well as after a
// deploy.
func TestPruningStillProtectsBothReleasesAfterARollback(t *testing.T) {
	state := State{
		Current:  "ddddddd",
		Previous: "ccccccc",
		Releases: []string{"ddddddd", "ccccccc", "bbbbbbb", "aaaaaaa"},
	}.RecordRollback("bbbbbbb")

	for _, pruned := range state.ReleasesToPrune(2) {
		if pruned == state.Current {
			t.Error("pruning would remove the release that is running after a rollback")
		}
		if pruned == state.Previous {
			t.Error("pruning would remove the release a further rollback returns to")
		}
	}
}

func TestRollbackRestoresThePreviousReleaseForReal(t *testing.T) {
	dockerAvailable(t)

	repository := newRepository(t)
	writeFile(t, repository, configFileName, `{
      "version": 1,
      "id": "dd00000b",
      "name": "rolled",
      "retention": 3,
      "services": {
        "app": {
          "build": {"dockerfile": "Dockerfile"},
          "stateful": false,
          "command": ["sh", "-c", "cat /version.txt; sleep 300"],
          "healthcheck": {"command": ["CMD", "true"], "interval": "1s", "retries": 5}
        }
      }
    }`)
	writeFile(t, repository, "Dockerfile", "FROM busybox:latest\nCOPY version.txt /version.txt\n")

	destination := t.TempDir()
	layout := NewLayout(destination, "dd00000b")

	deployVersion := func(label string) string {
		commit := commitFile(t, repository, "version.txt", label)
		t.Cleanup(func() {
			exec.Command("docker", "compose", "--project-name", ProjectName("dd00000b", commit), "down").Run()
			exec.Command("docker", "image", "rm", "-f", ImageTag("dd00000b", "app", commit)).Run()
		})

		if _, err := RunDeploy(DeployOptions{
			Context: repository, Destination: destination, Environment: defaultEnvironmentName,
		}); err != nil {
			t.Fatalf("deploying %s: %v", label, err)
		}

		return ShortCommit(commit)
	}

	t.Cleanup(func() { exec.Command("docker", "network", "rm", NetworkName("dd00000b")).Run() })

	first := deployVersion("version-one")
	second := deployVersion("version-two")

	exitCode, err := RunRollback(DeployOptions{
		Context: repository, Destination: destination, Environment: defaultEnvironmentName,
	}, "")
	if err != nil {
		t.Fatalf("RunRollback: %v", err)
	}
	if exitCode != exitOK {
		t.Errorf("exit code = %d, want %d", exitCode, exitOK)
	}

	// the older release is running again
	running, err := exec.Command("docker", "ps", "--format", "{{.Names}}").Output()
	if err != nil {
		t.Fatalf("listing containers: %v", err)
	}
	if !strings.Contains(string(running), ProjectName("dd00000b", first)) {
		t.Error("the release rolled back to should be running")
	}
	if strings.Contains(string(running), ProjectName("dd00000b", second)) {
		t.Error("the release rolled away from should have been stopped")
	}

	// and it really is the older code, not merely an older name. reading the file
	// out of the running container is the only thing that proves the image went
	// back too, rather than an old label on a new build
	container, err := exec.Command(
		"docker", "ps", "--filter", "name="+ProjectName("dd00000b", first), "--format", "{{.Names}}",
	).Output()
	if err != nil || strings.TrimSpace(string(container)) == "" {
		t.Fatalf("could not find the rolled back container: %v", err)
	}

	shipped, err := exec.Command(
		"docker", "exec", strings.TrimSpace(string(container)), "cat", "/version.txt",
	).Output()
	if err != nil {
		t.Fatalf("reading the version out of the container: %v", err)
	}
	if !strings.Contains(string(shipped), "version-one") {
		t.Errorf("the running container should hold the older build, got %q", shipped)
	}

	state, err := ReadState(LocalRunner{}, layout)
	if err != nil {
		t.Fatalf("ReadState: %v", err)
	}
	if state.Current != first {
		t.Errorf("current = %q, want %q", state.Current, first)
	}
	if state.Previous != second {
		t.Errorf("previous = %q, want %q so a further rollback goes forward again", state.Previous, second)
	}
}

func TestRollbackToAPrunedReleaseFailsWithoutTouchingAnything(t *testing.T) {
	dockerAvailable(t)

	repository := newRepository(t)
	writeFile(t, repository, configFileName, `{
      "version": 1,
      "id": "dd00000c",
      "name": "prunedtarget",
      "retention": 1,
      "services": {
        "app": {
          "image": "busybox:latest",
          "stateful": false,
          "command": ["sh", "-c", "sleep 300"],
          "healthcheck": {"command": ["CMD", "true"], "interval": "1s", "retries": 5}
        }
      }
    }`)

	destination := t.TempDir()
	t.Cleanup(func() { exec.Command("docker", "network", "rm", NetworkName("dd00000c")).Run() })

	var commits []string
	for round := range 3 {
		commit := commitFile(t, repository, "round.txt", string(rune('a'+round)))
		commits = append(commits, ShortCommit(commit))
		t.Cleanup(func() {
			exec.Command("docker", "compose", "--project-name", ProjectName("dd00000c", commit), "down").Run()
		})

		if _, err := RunDeploy(DeployOptions{
			Context: repository, Destination: destination, Environment: defaultEnvironmentName,
		}); err != nil {
			t.Fatalf("deploy %d: %v", round+1, err)
		}
	}

	// retention 1 keeps current and previous, so the first release is long gone
	exitCode, err := RunRollback(DeployOptions{
		Context: repository, Destination: destination, Environment: defaultEnvironmentName,
	}, commits[0])

	if err == nil {
		t.Fatal("rolling back to a pruned release must fail")
	}
	if exitCode != exitPreconditionNotMet {
		t.Errorf("exit code = %d, want %d, since nothing was attempted", exitCode, exitPreconditionNotMet)
	}
	if !strings.Contains(err.Error(), commits[1]) {
		t.Errorf("the refusal should list what is still available, got: %v", err)
	}

	// the running release is untouched
	running, _ := exec.Command("docker", "ps", "--format", "{{.Names}}").Output()
	if !strings.Contains(string(running), ProjectName("dd00000c", commits[2])) {
		t.Error("a refused rollback must leave the current release running")
	}
}

// One release stack at a time is the steady state. Without this, every deploy
// leaves its predecessor running and a small box fills up with old stacks.
func TestADeployStopsTheReleaseItReplaces(t *testing.T) {
	dockerAvailable(t)

	repository := newRepository(t)
	writeFile(t, repository, configFileName, `{
      "version": 1,
      "id": "dd00000d",
      "name": "superseded",
      "retention": 3,
      "services": {
        "app": {
          "image": "busybox:latest",
          "stateful": false,
          "command": ["sh", "-c", "sleep 300"],
          "healthcheck": {"command": ["CMD", "true"], "interval": "1s", "retries": 5}
        }
      }
    }`)

	destination := t.TempDir()
	t.Cleanup(func() { exec.Command("docker", "network", "rm", NetworkName("dd00000d")).Run() })

	var commits []string
	for round := range 3 {
		commit := commitFile(t, repository, "round.txt", string(rune('a'+round)))
		commits = append(commits, ShortCommit(commit))
		t.Cleanup(func() {
			exec.Command("docker", "compose", "--project-name", ProjectName("dd00000d", commit), "down").Run()
		})

		if _, err := RunDeploy(DeployOptions{
			Context: repository, Destination: destination, Environment: defaultEnvironmentName,
		}); err != nil {
			t.Fatalf("deploy %d: %v", round+1, err)
		}
	}

	running, err := exec.Command("docker", "ps", "--format", "{{.Names}}").Output()
	if err != nil {
		t.Fatalf("listing containers: %v", err)
	}

	newest := commits[len(commits)-1]
	if !strings.Contains(string(running), ProjectName("dd00000d", newest)) {
		t.Error("the newest release should be running")
	}
	for _, superseded := range commits[:len(commits)-1] {
		if strings.Contains(string(running), ProjectName("dd00000d", superseded)) {
			t.Errorf("release %s was replaced and should have been stopped", superseded)
		}
	}

	// stopped is not removed, so rolling back is still just a compose up
	if _, err := RunRollback(DeployOptions{
		Context: repository, Destination: destination, Environment: defaultEnvironmentName,
	}, ""); err != nil {
		t.Fatalf("a stopped release must still be startable: %v", err)
	}
}

// An exposed project keeps both stacks up, because stopping the old one before
// the tunnel points at the new one is the outage the cutover exists to prevent.
func TestAnExposedProjectKeepsTheOldStackUntilTheTunnelCanBeMoved(t *testing.T) {
	dockerAvailable(t)

	// a host block makes cloudflared a genuine requirement, which is the rule
	// working rather than a problem, so skip where it is not installed
	if err := exec.Command("cloudflared", "--version").Run(); err != nil {
		t.Skip("cloudflared is not installed, and a hosted project requires it")
	}

	repository := newRepository(t)
	writeFile(t, repository, configFileName, `{
      "version": 1,
      "id": "dd00000e",
      "name": "exposed",
      "retention": 3,
      "services": {
        "app": {
          "image": "busybox:latest",
          "stateful": false,
          "command": ["sh", "-c", "sleep 300"],
          "healthcheck": {"command": ["CMD", "true"], "interval": "1s", "retries": 5},
          "host": {"domain": "example.com", "port": 80, "tunnelTokenFrom": "TOKEN"}
        }
      }
    }`)

	destination := t.TempDir()
	t.Cleanup(func() { exec.Command("docker", "network", "rm", NetworkName("dd00000e")).Run() })

	var commits []string
	for round := range 2 {
		commit := commitFile(t, repository, "round.txt", string(rune('a'+round)))
		commits = append(commits, ShortCommit(commit))
		t.Cleanup(func() {
			exec.Command("docker", "compose", "--project-name", ProjectName("dd00000e", commit), "down").Run()
		})

		if _, err := RunDeploy(DeployOptions{
			Context: repository, Destination: destination, Environment: defaultEnvironmentName,
		}); err != nil {
			t.Fatalf("deploy %d: %v", round+1, err)
		}
	}

	running, err := exec.Command("docker", "ps", "--format", "{{.Names}}").Output()
	if err != nil {
		t.Fatalf("listing containers: %v", err)
	}
	for _, commit := range commits {
		if !strings.Contains(string(running), ProjectName("dd00000e", commit)) {
			t.Errorf("release %s should still be running, since nothing may be stopped before the tunnel moves", commit)
		}
	}
}

// Redeploying the commit that is already current must not stop the stack it just
// started. Running deploy twice is an ordinary thing to do after a hiccup.
func TestRedeployingTheSameCommitLeavesItRunning(t *testing.T) {
	dockerAvailable(t)

	repository := newRepository(t)
	writeFile(t, repository, configFileName, `{
      "version": 1,
      "id": "dd00000f",
      "name": "redeployed",
      "services": {
        "app": {
          "image": "busybox:latest",
          "stateful": false,
          "command": ["sh", "-c", "sleep 300"],
          "healthcheck": {"command": ["CMD", "true"], "interval": "1s", "retries": 5}
        }
      }
    }`)

	destination := t.TempDir()
	commit := commitFile(t, repository, "round.txt", "a")

	t.Cleanup(func() {
		exec.Command("docker", "compose", "--project-name", ProjectName("dd00000f", commit), "down").Run()
		exec.Command("docker", "network", "rm", NetworkName("dd00000f")).Run()
	})

	options := DeployOptions{
		Context: repository, Destination: destination, Environment: defaultEnvironmentName,
	}
	for round := range 2 {
		if _, err := RunDeploy(options); err != nil {
			t.Fatalf("deploy %d: %v", round+1, err)
		}
	}

	running, err := exec.Command("docker", "ps", "--format", "{{.Names}}").Output()
	if err != nil {
		t.Fatalf("listing containers: %v", err)
	}
	if !strings.Contains(string(running), ProjectName("dd00000f", commit)) {
		t.Error("redeploying the current commit stopped the stack it had just started")
	}

	// and it is still its own rollback target problem, not a lost release
	state, err := ReadState(LocalRunner{}, NewLayout(destination, "dd00000f"))
	if err != nil {
		t.Fatalf("ReadState: %v", err)
	}
	if state.Current != ShortCommit(commit) {
		t.Errorf("current = %q, want %q", state.Current, ShortCommit(commit))
	}
	if state.Previous == state.Current {
		t.Error("a redeploy made the release its own rollback target")
	}
}
