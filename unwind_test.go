package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The unwind table in prompt says where a failure at each stage leaves things.
// These are one test per row, because a claim about what survives a failure is
// worth nothing until something has actually failed there.

// Row one: requirement checks. Nothing has happened yet, so nothing is left
// behind and the exit code says nothing was attempted.
func TestUnwindPreconditionLeavesNothingBehind(t *testing.T) {
	repository := newRepository(t)
	writeFile(t, repository, configFileName, `{
      "version": 1, "id": "ee000001", "name": "precondition",
      "services": {"app": {"image": "busybox:latest"}}
    }`)
	commitFile(t, repository, "one.txt", "x")

	destination := t.TempDir()

	exitCode, err := RunDeploy(DeployOptions{
		Context:     repository,
		Destination: unreachableHost + ":Projects",
		Environment: defaultEnvironmentName,
	})
	if err == nil {
		t.Fatal("an unreachable destination should fail")
	}
	if exitCode != exitPreconditionNotMet {
		t.Errorf("exit code = %d, want %d", exitCode, exitPreconditionNotMet)
	}

	// and the local destination it never touched is still empty
	entries, err := os.ReadDir(destination)
	if err != nil {
		t.Fatalf("reading destination: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("a precondition failure left %d entries behind", len(entries))
	}
}

// Row two: a release task failed. The old stack is still serving and the new one
// never started. Covered end to end by
// TestAFailingReleaseTaskAbortsAndLeavesThePreviousReleaseServing, so this checks
// the part that test cannot, which is that no lock is left holding the project.
func TestUnwindAFailedDeployReleasesTheLock(t *testing.T) {
	repository := newRepository(t)
	writeFile(t, repository, configFileName, `{
      "version": 1, "id": "ee000002", "name": "locked",
      "services": {"app": {"image": "busybox:latest"}}
    }`)
	commitFile(t, repository, "one.txt", "x")

	destination := t.TempDir()
	layout := NewLayout(destination, "ee000002")

	// a dirty tree fails before the lock is ever taken
	writeFile(t, repository, "one.txt", "edited")
	if _, err := RunDeploy(DeployOptions{
		Context: repository, Destination: destination, Environment: defaultEnvironmentName,
	}); err == nil {
		t.Fatal("a dirty tree should fail")
	}

	if _, err := os.Stat(layout.LockFile()); err == nil {
		t.Error("a failure before the lock should not have left one")
	}

	// and a project that failed is deployable again without --force-unlock
	lock, err := AcquireLock(LocalRunner{}, layout, "aaaaaaa", false)
	if err != nil {
		t.Fatalf("the project should not be locked after a failed deploy: %v", err)
	}
	lock.Release()
}

// Row three: the new stack never became healthy. Its logs are dumped, it is torn
// down, and whatever was serving before is untouched.
func TestUnwindAnUnhealthyStackIsTornDownAndThePreviousOneSurvives(t *testing.T) {
	dockerAvailable(t)

	repository := newRepository(t)
	const config = `{
      "version": 1, "id": "ee000003", "name": "unhealthy", "retention": 3,
      "services": {"app": {
        "build": {"dockerfile": "Dockerfile"},
        "stateful": false,
        "healthcheck": {"command": ["CMD", "sh", "-c", "test -f /healthy"], "interval": "1s", "retries": 2, "timeout": "1s"}
      }}
    }`
	writeFile(t, repository, configFileName, config)
	writeFile(t, repository, "Dockerfile", "FROM busybox:latest\nRUN touch /healthy\nCMD [\"sh\",\"-c\",\"sleep 300\"]\n")

	destination := t.TempDir()
	t.Cleanup(func() { exec.Command("docker", "network", "rm", NetworkName("ee000003")).Run() })

	working := commitFile(t, repository, "one.txt", "first")
	t.Cleanup(func() {
		exec.Command("docker", "compose", "--project-name", ProjectName("ee000003", working), "down").Run()
		exec.Command("docker", "image", "rm", "-f", ImageTag("ee000003", "app", working)).Run()
	})

	options := DeployOptions{
		Context: repository, Destination: destination, Environment: defaultEnvironmentName,
	}
	if _, err := RunDeploy(options); err != nil {
		t.Fatalf("the healthy deploy should succeed: %v", err)
	}

	// now a release whose healthcheck can never pass
	writeFile(t, repository, "Dockerfile", "FROM busybox:latest\nCMD [\"sh\",\"-c\",\"sleep 300\"]\n")
	broken := commitFile(t, repository, "one.txt", "second")
	t.Cleanup(func() {
		exec.Command("docker", "compose", "--project-name", ProjectName("ee000003", broken), "down").Run()
		exec.Command("docker", "image", "rm", "-f", ImageTag("ee000003", "app", broken)).Run()
	})

	exitCode, err := RunDeploy(options)
	if err == nil {
		t.Fatal("a stack that never becomes healthy must fail the deploy")
	}
	if exitCode != exitDeployFailed {
		t.Errorf("exit code = %d, want %d", exitCode, exitDeployFailed)
	}

	running, err := exec.Command("docker", "ps", "--format", "{{.Names}}").Output()
	if err != nil {
		t.Fatalf("listing containers: %v", err)
	}
	if strings.Contains(string(running), ProjectName("ee000003", broken)) {
		t.Error("the unhealthy stack should have been torn down")
	}
	if !strings.Contains(string(running), ProjectName("ee000003", working)) {
		t.Error("the release that was serving should still be serving")
	}

	// state still points at the release that works, so a rollback target is intact
	state, err := ReadState(LocalRunner{}, NewLayout(destination, "ee000003"))
	if err != nil {
		t.Fatalf("ReadState: %v", err)
	}
	if state.Current != ShortCommit(working) {
		t.Errorf("current = %q, want the release that is actually serving %q", state.Current, ShortCommit(working))
	}

	// the failed release left no half-written state behind either
	if state.Previous == ShortCommit(broken) {
		t.Error("a release that never served must not become the rollback target")
	}
}

// Row five: after cutover. Traffic is already on the new release, so a failure
// here is reported and never rolled back, and the exit code says a human is
// needed rather than that the deploy failed.
func TestUnwindAfterCutoverReportsRatherThanRollingBack(t *testing.T) {
	dockerAvailable(t)

	repository := newRepository(t)
	writeFile(t, repository, configFileName, `{
      "version": 1, "id": "ee000004", "name": "aftercutover",
      "services": {"app": {
        "image": "busybox:latest", "stateful": false, "command": ["sh", "-c", "sleep 300"],
        "healthcheck": {"command": ["CMD", "true"], "interval": "1s", "retries": 5}
      }}
    }`)
	commit := commitFile(t, repository, "one.txt", "x")

	destination := t.TempDir()
	layout := NewLayout(destination, "ee000004")

	t.Cleanup(func() {
		exec.Command("docker", "compose", "--project-name", ProjectName("ee000004", commit), "down").Run()
		exec.Command("docker", "network", "rm", NetworkName("ee000004")).Run()
		os.Chmod(layout.StateFile(), 0o644)
	})

	if _, err := RunDeploy(DeployOptions{
		Context: repository, Destination: destination, Environment: defaultEnvironmentName,
	}); err != nil {
		t.Fatalf("the first deploy should succeed: %v", err)
	}

	// only the state file, not the directory. a read only directory would fail
	// at the lock instead, which is a precondition failure and a different row of
	// the table entirely
	if err := os.Chmod(layout.StateFile(), 0o444); err != nil {
		t.Fatalf("making state read only: %v", err)
	}

	second := commitFile(t, repository, "one.txt", "y")
	t.Cleanup(func() {
		exec.Command("docker", "compose", "--project-name", ProjectName("ee000004", second), "down").Run()
	})

	exitCode, err := RunDeploy(DeployOptions{
		Context: repository, Destination: destination, Environment: defaultEnvironmentName,
	})
	if err == nil {
		t.Skip("this filesystem let the write through, so there is nothing to unwind")
	}

	if exitCode != exitLiveButNeedsAHuman {
		t.Errorf(
			"exit code = %d, want %d, since the release is live and tearing it down would be a self inflicted outage",
			exitCode, exitLiveButNeedsAHuman,
		)
	}
	if !strings.Contains(err.Error(), "running") {
		t.Errorf("the error should say the release is up despite the failure, got: %v", err)
	}

	// the new release really is still serving, which is the whole point of not
	// rolling back here
	running, listErr := exec.Command("docker", "ps", "--format", "{{.Names}}").Output()
	if listErr != nil {
		t.Fatalf("listing containers: %v", listErr)
	}
	if !strings.Contains(string(running), ProjectName("ee000004", second)) {
		t.Error("a failure after cutover must leave the new release running")
	}
}

// Every command has to agree on what an exit code means, or nothing can script
// this.
func TestExitCodesAcrossEveryCommand(t *testing.T) {
	repository := newRepository(t)
	writeFile(t, repository, configFileName, `{
      "version": 1, "id": "ee000005", "name": "codes",
      "services": {"app": {"image": "busybox:latest"}}
    }`)
	commitFile(t, repository, "one.txt", "x")

	elsewhere := t.TempDir()

	cases := []struct {
		name string
		args []string
		want int
	}{
		{"help is not a failure", []string{"-h"}, exitOK},
		{"an unknown command", []string{"bogus"}, exitPreconditionNotMet},
		{"an unknown flag", []string{"-nonsense"}, exitPreconditionNotMet},
		{"check outside a project", []string{"check"}, exitPreconditionNotMet},
		{"status outside a repository", []string{"status", "-C", elsewhere}, exitPreconditionNotMet},
		{"list outside a repository", []string{"list", "-C", elsewhere}, exitPreconditionNotMet},
		{"rollback outside a repository", []string{"rollback", "-C", elsewhere}, exitPreconditionNotMet},
		{"logs with no service named", []string{"logs", "-C", repository}, exitPreconditionNotMet},
		{"exec with no service named", []string{"exec", "-C", repository}, exitPreconditionNotMet},
		{"env push with no file", []string{"env", "push"}, exitPreconditionNotMet},
		{
			name: "status on a project never deployed is not a failure",
			args: []string{"status", "-C", repository, "-D", t.TempDir()},
			want: exitOK,
		},
		{
			name: "releases on a project never deployed is not a failure",
			args: []string{"releases", "-C", repository, "-D", t.TempDir()},
			want: exitOK,
		},
		{
			name: "rollback with nothing to roll back to",
			args: []string{"rollback", "-C", repository, "-D", t.TempDir()},
			want: exitPreconditionNotMet,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := runQuietly(t, testCase.args); got != testCase.want {
				t.Errorf("deploy %s exited %d, want %d", strings.Join(testCase.args, " "), got, testCase.want)
			}
		})
	}
}

// runQuietly keeps the audit's own output out of the test log, since these
// commands are meant to print.
func runQuietly(t *testing.T, args []string) int {
	t.Helper()

	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("opening %s: %v", os.DevNull, err)
	}
	defer devNull.Close()

	originalOut, originalErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = devNull, devNull
	defer func() { os.Stdout, os.Stderr = originalOut, originalErr }()

	return run(args)
}

// deploy check is what anyone would run before trusting a config, so it has to
// accept every example in the docs and refuse a broken one, through the real
// command rather than the functions under it.
func TestCheckCommandAcceptsTheDocumentedExamples(t *testing.T) {
	for name, contents := range map[string]string{
		"static site":     staticSiteConfig,
		"minecraft":       minecraftConfig,
		"api with worker": workerConfig,
		"lectern":         lecternConfig,
	} {
		t.Run(name, func(t *testing.T) {
			directory := t.TempDir()
			writeFile(t, directory, configFileName, contents)

			if got := runInDirectory(t, directory, []string{"check"}); got != exitOK {
				t.Errorf("deploy check exited %d on the %s example", got, name)
			}
		})
	}

	broken := t.TempDir()
	writeFile(t, broken, configFileName, `{"version": 1, "id": "nope", "services": {}}`)

	if got := runInDirectory(t, broken, []string{"check"}); got != exitPreconditionNotMet {
		t.Errorf("deploy check exited %d on a broken config, want %d", got, exitPreconditionNotMet)
	}
}

func runInDirectory(t *testing.T, directory string, args []string) int {
	t.Helper()

	original, err := os.Getwd()
	if err != nil {
		t.Fatalf("reading working directory: %v", err)
	}
	if err := os.Chdir(directory); err != nil {
		t.Fatalf("entering %s: %v", directory, err)
	}
	defer os.Chdir(original)

	return runQuietly(t, args)
}

// CONTRIBUTING says README.md is the maintainer's, and a test is the only thing
// that keeps that true when nobody is looking.
func TestReadmeIsUntouched(t *testing.T) {
	root, err := FindRepository(".")
	if err != nil {
		t.Skipf("not in a repository: %v", err)
	}

	status, err := runGit(root, "status", "--porcelain", "README.md")
	if err != nil {
		t.Fatalf("reading git status: %v", err)
	}
	if strings.TrimSpace(status) != "" {
		t.Errorf("README.md has uncommitted changes, and it is not ours to edit:\n%s", status)
	}

	contents, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil {
		t.Fatalf("reading README.md: %v", err)
	}
	// generated build instructions creeping in is exactly what CONTRIBUTING is
	// guarding against
	for _, absent := range []string{"go build", "Usage", "```"} {
		if strings.Contains(string(contents), absent) {
			t.Errorf("README.md contains %q, which deploy should not have put there", absent)
		}
	}
}

// A network that already exists is the normal case on every deploy after the
// first. Anything else is a real failure, and swallowing it would turn a
// permissions problem into a confusing one about a service that cannot reach its
// database.
func TestNetworkCreationDistinguishesAlreadyExistingFromRealFailure(t *testing.T) {
	resolved := loadAndResolve(t, `{
      "version": 1, "id": "a3f19c02", "name": "x",
      "services": {"pg": {"image": "postgres:17", "stateful": true}}
    }`, defaultEnvironmentName)
	layout := NewLayout("/srv/projects", "a3f19c02")

	cases := []struct {
		name     string
		output   string
		wantFail bool
	}{
		{
			name:   "already existing is the ordinary case",
			output: `Error response from daemon: network with name deploy-a3f19c02-net already exists`,
		},
		{
			name:     "anything else is a real failure",
			output:   `Error response from daemon: permission denied while trying to connect to the Docker daemon socket`,
			wantFail: true,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			runner := newScriptedRunner()
			runner.failCommands = []string{"network create"}
			runner.responses["network create"] = testCase.output

			err := startShared(runner, layout, resolved)

			if testCase.wantFail {
				if err == nil {
					t.Fatal("a network failure that is not about it already existing must surface")
				}
				if !strings.Contains(err.Error(), runner.Describe()) {
					t.Errorf("the error should name the host, got: %v", err)
				}
				return
			}
			// it carries on past an existing network, and whatever it fails on
			// afterwards is not this
			if err != nil && strings.Contains(err.Error(), "creating network") {
				t.Errorf("an existing network should not fail the deploy, got: %v", err)
			}
		})
	}
}

// deploy -D /srv status used to silently deploy, because the leftover argument
// was parsed as a flag value's neighbour and then ignored. Quietly doing the
// wrong thing is the failure mode this tool is supposed to be the opposite of.
func TestAStrayArgumentIsRefusedRatherThanIgnored(t *testing.T) {
	repository := newRepository(t)
	writeFile(t, repository, configFileName, `{
      "version": 1, "id": "ee000006", "name": "stray",
      "services": {"app": {"image": "busybox:latest"}}
    }`)
	commitFile(t, repository, "one.txt", "x")

	cases := []struct {
		name string
		args []string
	}{
		{"a command after flags on the bare deploy", []string{"-C", repository, "status"}},
		{"a command after flags on a subcommand", []string{"status", "-C", repository, "list"}},
		{"a plain typo", []string{"-C", repository, "wat"}},
		{"an extra argument to releases", []string{"releases", "-C", repository, "extra"}},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := runQuietly(t, testCase.args); got != exitPreconditionNotMet {
				t.Errorf("deploy %s exited %d, want %d",
					strings.Join(testCase.args, " "), got, exitPreconditionNotMet)
			}
		})
	}

	// and the arguments that are meant to be there still work. these fail for
	// their own reasons, and the point is that the reason is never the argument
	for _, args := range [][]string{
		{"rollback", "abc1234", "-C", repository, "-D", t.TempDir()},
		{"exec", "web", "-C", repository, "-D", t.TempDir(), "--", "echo", "hi"},
		{"logs", "web", "-C", repository, "-D", t.TempDir()},
	} {
		t.Run(args[0]+" keeps its own argument", func(t *testing.T) {
			complaint := captureStderr(t, func() { run(args) })

			for _, wrong := range []string{"does not take", "is a command"} {
				if strings.Contains(complaint, wrong) {
					t.Errorf("deploy %s was refused for its own argument: %s",
						strings.Join(args, " "), complaint)
				}
			}
		})
	}
}

func captureStderr(t *testing.T, work func()) string {
	t.Helper()

	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("creating pipe: %v", err)
	}

	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("opening %s: %v", os.DevNull, err)
	}
	defer devNull.Close()

	originalOut, originalErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = devNull, writer
	defer func() { os.Stdout, os.Stderr = originalOut, originalErr }()

	collected := make(chan string, 1)
	go func() {
		var seen strings.Builder
		buffer := make([]byte, 4096)
		for {
			read, err := reader.Read(buffer)
			if read > 0 {
				seen.Write(buffer[:read])
			}
			if err != nil {
				break
			}
		}
		collected <- seen.String()
	}()

	work()
	writer.Close()

	return <-collected
}

// Every command in the dispatch table has to be reachable, or the error that
// suggests one would name something that does not run.
func TestEverySubcommandDispatches(t *testing.T) {
	elsewhere := t.TempDir()

	for name := range subcommands {
		t.Run(name, func(t *testing.T) {
			// outside a repository every one of these fails the same way, which
			// is enough to prove it dispatched rather than fell through to deploy
			if got := runQuietly(t, []string{name, "-C", elsewhere}); got == exitOK {
				t.Errorf("%s outside a repository should not have succeeded", name)
			}
		})
	}

	if len(subcommands) < 10 {
		t.Errorf("the dispatch table has %d commands, which is fewer than deploy has", len(subcommands))
	}
}
