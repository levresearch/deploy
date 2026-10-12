package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// removeDeployment clears every container, network and volume a test project
// left behind, whichever stack created them.
func removeDeployment(t *testing.T, id string) {
	t.Helper()
	t.Cleanup(func() {
		if containers, _ := exec.Command("docker", "ps", "-aq", "--filter", "name=deploy-"+id).Output(); len(strings.Fields(string(containers))) > 0 {
			exec.Command("docker", append([]string{"rm", "-f"}, strings.Fields(string(containers))...)...).Run()
		}
		exec.Command("docker", "network", "rm", NetworkName(id)).Run()
		if volumes, _ := exec.Command("docker", "volume", "ls", "-q", "--filter", "name=deploy-"+id).Output(); len(strings.Fields(string(volumes))) > 0 {
			exec.Command("docker", append([]string{"volume", "rm", "-f"}, strings.Fields(string(volumes))...)...).Run()
		}
		if images, _ := exec.Command("docker", "images", "-q", "--filter", "reference=deploy-"+id+"/*").Output(); len(strings.Fields(string(images))) > 0 {
			exec.Command("docker", append([]string{"image", "rm", "-f"}, strings.Fields(string(images))...)...).Run()
		}
	})
}

func runningServices(t *testing.T, project string) []string {
	t.Helper()
	output, err := exec.Command("docker", "compose", "--project-name", project, "ps", "--status", "running", "--format", "{{.Service}}").Output()
	if err != nil {
		t.Fatalf("listing %s: %v", project, err)
	}
	services := strings.Fields(string(output))
	slices.Sort(services)
	return services
}

func recordedState(t *testing.T, destination, id string) ServiceState {
	t.Helper()
	state, found, err := ReadServiceState(LocalRunner{}, NewLayout(destination, id))
	if err != nil || !found {
		t.Fatalf("no per-service state: found=%v %v", found, err)
	}
	return state
}

const sleeper = `"image": "busybox:latest", "command": ["sh", "-c", "sleep 600"], "healthcheck": {"command": ["CMD", "true"], "interval": "1s", "retries": 5}`

func TestAdoptionKeepsDataAndTheFirstServiceDeployMovesEachService(t *testing.T) {
	dockerAvailable(t)
	t.Parallel()
	const id = "fb000001"
	removeDeployment(t, id)
	repository := newRepository(t)
	destination := t.TempDir()

	commitFile(t, repository, configFileName, `{"version": 1, "id": "`+id+`", "name": "adopting", "retention": 2,
	  "services": {
	    "store": {`+sleeper+`, "stateful": true, "volumes": ["data:/data"]},
	    "app": {`+sleeper+`},
	    "worker": {`+sleeper+`}
	  }}`)
	if _, err := RunDeploy(DeployOptions{Context: repository, Destination: destination, Environment: defaultEnvironmentName}); err != nil {
		t.Fatal(err)
	}
	legacyCommit := commitFile(t, repository, "round.txt", "two")
	if _, err := RunDeploy(DeployOptions{Context: repository, Destination: destination, Environment: defaultEnvironmentName}); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("docker", "compose", "--project-name", SharedProjectName(id), "exec", "-T", "store", "sh", "-c", "echo 'the precious row' > /data/rows.txt").CombinedOutput(); err != nil {
		t.Fatalf("writing into the volume: %v %s", err, output)
	}

	v2 := `{"version": 2, "id": "` + id + `", "name": "adopting", "retention": 2,
	  "destinations": {"main": "` + destination + `"},
	  "services": {
	    "store": {` + sleeper + `, "destination": "main", "stateful": true, "volumes": ["data:/data"]},
	    "app": {` + sleeper + `, "destination": "main", "dependsOn": {"store": "healthy"}}
	  }}`
	commitFile(t, repository, configFileName, v2)
	if _, err := RunDeploy(DeployOptions{Context: repository}); err == nil || !strings.Contains(err.Error(), "deploy adopt") {
		t.Fatalf("a version 1 deployment was deployed over without adoption: %v", err)
	}

	if code, err := RunAdopt(DeployOptions{Context: repository}, false); code != exitOK || err != nil {
		t.Fatalf("adopt preview: %d %v", code, err)
	}
	layout := NewLayout(destination, id)
	if _, err := os.Stat(layout.ServiceStateFile()); !os.IsNotExist(err) {
		t.Fatal("the adoption preview wrote state")
	}
	if code, err := RunAdopt(DeployOptions{Context: repository}, true); code != exitOK || err != nil {
		t.Fatalf("adopt: %d %v", code, err)
	}
	adopted := recordedState(t, destination, id)
	if adopted.Services["worker"].Current == nil || adopted.Services["app"].Current.Project != ProjectName(id, legacyCommit) || adopted.Services["app"].Previous == nil {
		t.Fatalf("adoption missed the live stack: %+v", adopted.Services)
	}
	if _, err := os.Stat(operationFile(layout)); !os.IsNotExist(err) {
		t.Fatal("adoption left its operation recorded")
	}

	first, err := ResolveCommit(repository, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if code, err := RunDeploy(DeployOptions{Context: repository}); code != exitOK || err != nil {
		t.Fatalf("first version 2 deploy: %d %v", code, err)
	}
	state := recordedState(t, destination, id)
	app := state.Services["app"]
	if app.Current.Project != ServiceReleaseProject(id, "app", first) || app.Previous.Project != ProjectName(id, legacyCommit) || app.Current.ConfigDigest == "" {
		t.Fatalf("app was not moved to its own release: %+v", app)
	}
	if got := runningServices(t, ServiceReleaseProject(id, "app", first)); !slices.Equal(got, []string{"app"}) {
		t.Fatalf("app release runs %v", got)
	}
	if got := runningServices(t, ProjectName(id, legacyCommit)); !slices.Equal(got, []string{"worker"}) {
		t.Fatalf("the legacy stack should keep only the unconfigured worker running, got %v", got)
	}
	if _, kept := state.Services["worker"]; !kept {
		t.Fatal("a service dropped from the config was forgotten")
	}
	stored, err := exec.Command("docker", "compose", "--project-name", SharedProjectName(id), "exec", "-T", "store", "cat", "/data/rows.txt").CombinedOutput()
	if err != nil || !strings.Contains(string(stored), "the precious row") {
		t.Fatalf("the volume lost its contents: %v %s", err, stored)
	}

	second := commitFile(t, repository, "round.txt", "three")
	if code, err := RunDeploy(DeployOptions{Context: repository}); code != exitOK || err != nil {
		t.Fatalf("second version 2 deploy: %d %v", code, err)
	}
	state = recordedState(t, destination, id)
	app = state.Services["app"]
	if app.Current.Commit != ShortCommit(second) || app.Previous.Project != ServiceReleaseProject(id, "app", first) {
		t.Fatalf("app history: %+v", app)
	}
	if got := runningServices(t, ServiceReleaseProject(id, "app", first)); len(got) != 0 {
		t.Fatalf("the replaced release still runs %v", got)
	}
	if _, err := RunDeploy(DeployOptions{Context: repository, Destination: destination, Environment: defaultEnvironmentName}); err == nil {
		t.Fatal("a destination flag was accepted for version 2")
	}
}

func TestReleaseTasksRunInOrderAndAreRecorded(t *testing.T) {
	dockerAvailable(t)
	t.Parallel()
	const id = "fb000002"
	removeDeployment(t, id)
	repository := newRepository(t)
	destination := t.TempDir()
	marker := filepath.Join(destination, "task-ran")
	config := func(task string) string {
		return `{"version": 2, "id": "` + id + `", "name": "tasks",
		  "destinations": {"main": "` + destination + `"},
		  "services": {
		    "store": {` + sleeper + `, "destination": "main", "stateful": true},
		    "app": {` + sleeper + `, "destination": "main", "dependsOn": {"migrate": "completed"}}
		  },
		  "release": {
		    "migrate": {"destination": "main", "image": "busybox:latest", "dependsOn": {"store": "healthy"},
		      "volumes": ["` + destination + `:/out"], "command": ["sh", "-c", ` + fmt.Sprintf("%q", task) + `]}
		  }}`
	}

	commitFile(t, repository, configFileName, config("touch /out/task-ran"))
	if code, err := RunDeploy(DeployOptions{Context: repository}); code != exitOK || err != nil {
		t.Fatalf("deploy: %d %v", code, err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("the release task did not run")
	}
	state := recordedState(t, destination, id)
	runs := state.Tasks["migrate"]
	if len(runs) != 1 || runs[0].Outcome != outcomeCompleted || runs[0].ConfigDigest == "" {
		t.Fatalf("task record: %+v", runs)
	}
	live := state.Services["app"].Current.Project

	failing := commitFile(t, repository, configFileName, config("exit 3"))
	if code, err := RunDeploy(DeployOptions{Context: repository}); code != exitDeployFailed || err == nil {
		t.Fatalf("a failing release task did not fail the deploy: %d %v", code, err)
	}
	state = recordedState(t, destination, id)
	runs = state.Tasks["migrate"]
	if len(runs) != 2 || runs[1].Outcome != outcomeFailed || runs[1].Commit != ShortCommit(failing) {
		t.Fatalf("failed task record: %+v", runs)
	}
	if state.Services["app"].Current.Project != live || !slices.Equal(runningServices(t, live), []string{"app"}) {
		t.Fatal("a failed release task moved or stopped the live app")
	}
	if got := runningServices(t, ServiceReleaseProject(id, "app", failing)); len(got) != 0 {
		t.Fatalf("a candidate started after its task failed: %v", got)
	}

	// a crash between recording the run and recording its result
	layout := NewLayout(destination, id)
	state.Tasks["migrate"] = append(state.Tasks["migrate"], TaskRecord{Operation: "0123456789abcdef", Commit: ShortCommit(failing), ConfigDigest: runs[1].ConfigDigest, Outcome: outcomeIndeterminate})
	if _, err := WriteServiceState(LocalRunner{}, layout, state); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(lockDirectory(layout), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, lockDirectory(layout), id+".operation.json", `{"version":1,"id":"0123456789abcdef","command":"deploy","target":"`+failing+`","host":"crashed","pid":1,"startedAt":"2026-10-12T00:00:00Z"}`)
	commitFile(t, repository, configFileName, config("touch /out/task-ran"))
	if _, err := RunDeploy(DeployOptions{Context: repository}); err == nil || !strings.Contains(err.Error(), "deploy resolve 0123456789abcdef") {
		t.Fatalf("a crashed operation did not block: %v", err)
	}
	if code := run([]string{"resolve", "0123456789abcdef", "-C", repository}); code != exitOK {
		t.Fatalf("resolve exited %d", code)
	}
	state = recordedState(t, destination, id)
	if last := state.Tasks["migrate"][2]; last.Outcome != outcomeResolved {
		t.Fatalf("resolve did not settle the task: %+v", last)
	}
	if code, err := RunDeploy(DeployOptions{Context: repository}); code != exitOK || err != nil {
		t.Fatalf("deploy after resolving: %d %v", code, err)
	}
}

func TestAFailedCandidateLeavesEveryServiceOnItsRelease(t *testing.T) {
	dockerAvailable(t)
	t.Parallel()
	const id = "fb000003"
	removeDeployment(t, id)
	repository := newRepository(t)
	destination := t.TempDir()
	config := func(workerCheck string) string {
		return `{"version": 2, "id": "` + id + `", "name": "candidates",
		  "destinations": {"main": "` + destination + `"},
		  "services": {
		    "api": {` + sleeper + `, "destination": "main"},
		    "worker": {"destination": "main", "image": "busybox:latest", "command": ["sh", "-c", "sleep 600"],
		      "healthcheck": {"command": ["CMD", "` + workerCheck + `"], "interval": "1s", "retries": 2}}
		  }}`
	}
	first := commitFile(t, repository, configFileName, config("true"))
	if code, err := RunDeploy(DeployOptions{Context: repository}); code != exitOK || err != nil {
		t.Fatalf("deploy: %d %v", code, err)
	}

	broken := commitFile(t, repository, configFileName, config("false"))
	if code, err := RunDeploy(DeployOptions{Context: repository}); code != exitDeployFailed || err == nil {
		t.Fatalf("an unhealthy candidate did not fail the deploy: %d %v", code, err)
	}
	for _, service := range []string{"api", "worker"} {
		if got := runningServices(t, ServiceReleaseProject(id, service, first)); !slices.Equal(got, []string{service}) {
			t.Fatalf("%s stopped serving: %v", service, got)
		}
		if got := runningServices(t, ServiceReleaseProject(id, service, broken)); len(got) != 0 {
			t.Fatalf("the %s candidate was left running: %v", service, got)
		}
	}
	state := recordedState(t, destination, id)
	if state.Services["api"].Current.Commit != ShortCommit(first) || state.Services["worker"].Current.Commit != ShortCommit(first) {
		t.Fatalf("state moved on a failed deploy: %+v", state.Services)
	}
	if _, err := os.Stat(operationFile(NewLayout(destination, id))); !os.IsNotExist(err) {
		t.Fatal("an observed failure left the operation recorded")
	}

	// a redeploy of the live commit must never discard what is serving
	if _, err := runGit(repository, "reset", "--hard", first); err != nil {
		t.Fatal(err)
	}
	if code, err := RunDeploy(DeployOptions{Context: repository}); code != exitOK || err != nil {
		t.Fatalf("redeploying the live commit: %d %v", code, err)
	}
	if got := runningServices(t, ServiceReleaseProject(id, "api", first)); !slices.Equal(got, []string{"api"}) {
		t.Fatalf("a redeploy stopped the live api: %v", got)
	}
}

func TestTheEverydayCommandsFollowEachServicesRelease(t *testing.T) {
	dockerAvailable(t)
	const id = "fb000004"
	removeDeployment(t, id)
	repository := newRepository(t)
	destination := t.TempDir()
	config := `{"version": 2, "id": "` + id + `", "name": "everyday",
	  "destinations": {"main": "` + destination + `"},
	  "services": {
	    "store": {` + sleeper + `, "destination": "main", "stateful": true, "volumes": ["data:/data"]},
	    "app": {` + sleeper + `, "destination": "main", "env": ["app.env"]}
	  }}`
	commit := commitFile(t, repository, configFileName, config)
	options := DeployOptions{Context: repository}

	if code, err := RunDeploy(options); code != exitPreconditionNotMet || err == nil || !strings.Contains(err.Error(), "app.env") {
		t.Fatalf("a missing env file did not stop the deploy: %d %v", code, err)
	}
	secret := filepath.Join(t.TempDir(), "app.env")
	writeFile(t, filepath.Dir(secret), "app.env", "GREETING=hello from the env file\n")
	if code, err := RunEnvPush(options, secret); code != exitOK || err != nil {
		t.Fatalf("env push: %d %v", code, err)
	}
	if state := recordedState(t, destination, id); state.Generation != 1 || len(state.Services) != 0 {
		t.Fatalf("env push did not claim the destination per service: %+v", state)
	}
	if _, err := os.Stat(operationFile(NewLayout(destination, id))); !os.IsNotExist(err) {
		t.Fatal("env push left its operation recorded")
	}
	if _, err := RunEnvPush(DeployOptions{Context: repository, Destination: destination}, secret); err == nil {
		t.Fatal("a destination flag was accepted for version 2 env push")
	}

	if code, err := RunDeploy(options); code != exitOK || err != nil {
		t.Fatalf("deploy: %d %v", code, err)
	}

	status := captureStdout(t, func() {
		if _, err := RunStatus(options); err != nil {
			t.Fatalf("status: %v", err)
		}
	})
	for _, want := range []string{"everyday", "app", ShortCommit(commit), "store", "shared", "healthy"} {
		if !strings.Contains(status, want) {
			t.Errorf("status should mention %q:\n%s", want, status)
		}
	}
	releases := captureStdout(t, func() {
		if _, err := RunReleases(options); err != nil {
			t.Fatalf("releases: %v", err)
		}
	})
	if !strings.Contains(releases, "* "+ShortCommit(commit)) || !strings.Contains(releases, "keeping 3 per service") {
		t.Errorf("releases:\n%s", releases)
	}
	listed := captureStdout(t, func() {
		if _, err := RunList(options); err != nil {
			t.Fatalf("list: %v", err)
		}
	})
	if !strings.Contains(listed, "everyday") || !strings.Contains(listed, ShortCommit(commit)) || !strings.Contains(listed, "<- this one") {
		t.Errorf("list:\n%s", listed)
	}

	if _, err := RunExec(options, "app", []string{"sh", "-c", "test \"$GREETING\" = 'hello from the env file'"}); err != nil {
		t.Errorf("exec into the app's own release did not see its env file: %v", err)
	}
	if _, err := RunExec(options, "store", []string{"sh", "-c", "echo reached > /data/proof.txt"}); err != nil {
		t.Errorf("exec into the shared stack: %v", err)
	}
	if _, err := RunLogs(options, "app", false); err != nil {
		t.Errorf("logs: %v", err)
	}
	if _, err := RunExec(options, "missing", []string{"true"}); err == nil || !strings.Contains(err.Error(), "app, store") {
		t.Errorf("an unknown service did not list the real ones: %v", err)
	}
	if _, err := RunRollback(options, ""); err == nil || !strings.Contains(err.Error(), "not available for version 2 yet") {
		t.Errorf("rollback should say it is not available yet: %v", err)
	}
	if _, err := RunDestroy(options, true, strings.NewReader("everyday\n")); err == nil || !strings.Contains(err.Error(), "not available for version 2 yet") {
		t.Errorf("destroy should say it is not available yet: %v", err)
	}
	if got := runningServices(t, ServiceReleaseProject(id, "app", commit)); !slices.Equal(got, []string{"app"}) {
		t.Fatalf("a refused destroy touched the app: %v", got)
	}
}
