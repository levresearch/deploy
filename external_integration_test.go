package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// one external, run for real: it records what it printed, goes back with the
// rollback command, keeps its release when the command fails, rolls itself
// back when verify fails, and is skipped by --affected when nothing of it
// changed.
func TestAnExternalReleasesRecordsRollsBackAndVerifies(t *testing.T) {
	dockerAvailable(t)
	t.Parallel()
	const id = "fb0000c1"
	removeDeployment(t, id)
	repository := newRepository(t)
	destination := t.TempDir()
	rollbacks := filepath.Join(destination, "rollbacks")

	var status atomic.Int32
	status.Store(http.StatusOK)
	// the release stays broken until the second rollback has put the first
	// one back, the way a bad deploy recovers once the old one serves again
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if written, _ := os.ReadFile(rollbacks); strings.Count(string(written), "\n") >= 2 {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(int(status.Load()))
	}))
	defer server.Close()

	writeFile(t, repository, "site/Dockerfile", "FROM busybox:latest\nCOPY site/version /version\n")
	writeFile(t, repository, "site/version", "one")
	writeFile(t, repository, "api/Dockerfile", "FROM busybox:latest\nCOPY api/version /version\n")
	writeFile(t, repository, "api/version", "one")
	config := func(run string) string {
		return `{"version": 2, "id": "` + id + `", "name": "externals",
		  "destinations": {"main": "` + destination + `"},
		  "services": {
		    "api": {"destination": "main", "build": "api/Dockerfile", "command": ["sh", "-c", "sleep 600"],
		      "healthcheck": {"command": ["CMD", "true"], "interval": "1s", "retries": 5}}
		  },
		  "external": {
		    "site": {"destination": "main", "build": "site/Dockerfile", "volumes": ["` + destination + `:/out"],
		      "run": ["sh", "-c", ` + fmt.Sprintf("%q", run) + `],
		      "reference": "^deployed (\\S+)$",
		      "verify": {"url": "` + server.URL + `/health", "timeout": "3s"},
		      "rollback": ["sh", "-c", "echo rolled to {reference} >> /out/rollbacks"]}
		  }}`
	}
	options := DeployOptions{Context: repository}

	first := commitFile(t, repository, configFileName, config("echo building; echo deployed https://one.example; echo done"))
	if code, err := RunDeploy(options); code != exitOK || err != nil {
		t.Fatalf("first deploy: %d %v", code, err)
	}
	state := recordedState(t, destination, id)
	site := state.Services["site"]
	if !site.External || site.Current.Commit != ShortCommit(first) || site.References[ShortCommit(first)] != "https://one.example" || site.Outcome != outcomeLive {
		t.Fatalf("first release recorded as %+v", site)
	}
	if runs := state.Tasks["site"]; len(runs) != 1 || runs[0].Outcome != outcomeCompleted {
		t.Fatalf("site runs %+v", runs)
	}
	if got := runningServices(t, ServiceReleaseProject(id, "site", first)); len(got) != 0 {
		t.Fatalf("an external left a container running: %v", got)
	}

	second := commitFile(t, repository, configFileName, config("echo deployed https://two.example"))
	if code, err := RunDeploy(options); code != exitOK || err != nil {
		t.Fatalf("second deploy: %d %v", code, err)
	}
	site = recordedState(t, destination, id).Services["site"]
	if site.Current.Commit != ShortCommit(second) || site.Previous.Commit != ShortCommit(first) || site.References[ShortCommit(second)] != "https://two.example" {
		t.Fatalf("second release recorded as %+v", site)
	}

	// --affected skips an external whose image and config did not change
	commitFile(t, repository, "api/version", "two")
	affected := options
	affected.Affected = true
	if code, err := RunDeploy(affected); code != exitOK || err != nil {
		t.Fatalf("--affected deploy: %d %v", code, err)
	}
	state = recordedState(t, destination, id)
	if len(state.Tasks["site"]) != 2 || state.Services["site"].Current.Commit != ShortCommit(second) {
		t.Fatalf("--affected ran an unchanged external: %+v %+v", state.Tasks["site"], state.Services["site"])
	}

	if code, err := RunRollback(options, "", "site"); code != exitOK || err != nil {
		t.Fatalf("rolling back the site: %d %v", code, err)
	}
	site = recordedState(t, destination, id).Services["site"]
	if site.Current.Commit != ShortCommit(first) || site.Previous.Commit != ShortCommit(second) {
		t.Fatalf("rollback recorded as %+v", site)
	}
	if written, _ := os.ReadFile(rollbacks); strings.TrimSpace(string(written)) != "rolled to https://one.example" {
		t.Fatalf("the rollback command ran as %q", written)
	}

	commitFile(t, repository, configFileName, config("echo broken; exit 3"))
	if code, err := RunDeploy(options); code != exitDeployFailed || err == nil || !strings.Contains(err.Error(), "stays on its previous release") {
		t.Fatalf("a failing external did not fail the deploy: %d %v", code, err)
	}
	state = recordedState(t, destination, id)
	if runs := state.Tasks["site"]; runs[len(runs)-1].Outcome != outcomeFailed || state.Services["site"].Current.Commit != ShortCommit(first) {
		t.Fatalf("a failed run moved the site: %+v %+v", runs, state.Services["site"])
	}

	status.Store(http.StatusServiceUnavailable)
	commitFile(t, repository, configFileName, config("echo deployed https://three.example"))
	code, err := RunDeploy(options)
	if code != exitDeployFailed || err == nil || !strings.Contains(err.Error(), "failed verification and was rolled back to "+ShortCommit(first)) {
		t.Fatalf("an unverified release did not roll back: %d %v", code, err)
	}
	if written, _ := os.ReadFile(rollbacks); strings.Count(string(written), "rolled to https://one.example") != 2 {
		t.Fatalf("rollbacks ran as %q", written)
	}
	if current := recordedState(t, destination, id).Services["site"].Current.Commit; current != ShortCommit(first) {
		t.Fatalf("an unverified release was recorded as current: %s", current)
	}
	if _, err := os.Stat(operationFile(NewLayout(destination, id))); !os.IsNotExist(err) {
		t.Fatal("a deploy left its operation recorded")
	}
	if code, err := RunStatus(options); code != exitOK || err != nil {
		t.Fatalf("status: %d %v", code, err)
	}
	if _, err := RunShell(options, "site"); err == nil || !strings.Contains(err.Error(), "is external") {
		t.Fatalf("a shell into an external was allowed: %v", err)
	}
}
