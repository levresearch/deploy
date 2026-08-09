package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const twoTierConfig = `{
  "version": 1,
  "id": "a3f19c02",
  "name": "lectern",
  "services": {
    "web": {"image": "nginx", "stateful": false},
    "worker": {"image": "busybox", "stateful": false},
    "pg": {"image": "postgres:17", "stateful": true}
  },
  "release": {"migrate": {"image": "busybox", "command": "true"}}
}`

func TestServiceLocationPicksTheRightComposeProject(t *testing.T) {
	resolved := loadAndResolve(t, twoTierConfig, defaultEnvironmentName)
	layout := NewLayout("/srv/projects", "a3f19c02")
	state := State{Current: "9f4be0a", Releases: []string{"9f4be0a"}}

	cases := []struct {
		service      string
		wantProject  string
		wantCompose  string
		wantStateful bool
	}{
		{
			service:      "pg",
			wantProject:  SharedProjectName("a3f19c02"),
			wantCompose:  layout.SharedComposeFile(),
			wantStateful: true,
		},
		{
			service:     "web",
			wantProject: ProjectName("a3f19c02", "9f4be0a"),
			wantCompose: filepath.Join(layout.Release("9f4be0a"), composeFileName),
		},
		{
			service:     "worker",
			wantProject: ProjectName("a3f19c02", "9f4be0a"),
			wantCompose: filepath.Join(layout.Release("9f4be0a"), composeFileName),
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.service, func(t *testing.T) {
			location, err := LocateService(resolved, layout, state, testCase.service)
			if err != nil {
				t.Fatalf("LocateService: %v", err)
			}
			if location.ProjectName != testCase.wantProject {
				t.Errorf("project = %q, want %q", location.ProjectName, testCase.wantProject)
			}
			if location.ComposeFile != testCase.wantCompose {
				t.Errorf("compose file = %q, want %q", location.ComposeFile, testCase.wantCompose)
			}
			if location.Stateful != testCase.wantStateful {
				t.Errorf("stateful = %v, want %v", location.Stateful, testCase.wantStateful)
			}
		})
	}
}

func TestServiceLocationRefusalsAreUseful(t *testing.T) {
	resolved := loadAndResolve(t, twoTierConfig, defaultEnvironmentName)
	layout := NewLayout("/srv/projects", "a3f19c02")
	deployed := State{Current: "9f4be0a", Releases: []string{"9f4be0a"}}

	// an unknown name should list the real ones rather than just saying no
	_, err := LocateService(resolved, layout, deployed, "wbe")
	if err == nil {
		t.Fatal("an unknown service should be refused")
	}
	for _, want := range []string{"web", "worker", "pg"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal should list %q, got: %v", want, err)
		}
	}

	// a release task is not something to attach to, and saying so beats a
	// confusing compose error about a service that is not there
	_, err = LocateService(resolved, layout, deployed, "migrate")
	if err == nil || !strings.Contains(err.Error(), "release task") {
		t.Errorf("a release task should be explained rather than looked up, got: %v", err)
	}

	// a stateless service in a project never deployed has no release to look in
	_, err = LocateService(resolved, layout, State{}, "web")
	if err == nil || !strings.Contains(err.Error(), "never been deployed") {
		t.Errorf("got: %v", err)
	}

	// but a stateful one still resolves, since the shared stack outlives releases
	if _, err := LocateService(resolved, layout, State{}, "pg"); err != nil {
		t.Errorf("a stateful service lives in the shared stack whatever the release history: %v", err)
	}
}

func TestContainerStatusesReadThroughTheComposeLabel(t *testing.T) {
	runner := newScriptedRunner()
	runner.responses["docker ps"] = strings.Join([]string{
		"web\tdeploy-a3f19c02-9f4be0a-web-1\tUp 3 minutes (healthy)",
		"worker\tdeploy-a3f19c02-9f4be0a-worker-1\tUp 3 minutes",
	}, "\n")

	statuses, err := containerStatuses(runner, ProjectName("a3f19c02", "9f4be0a"))
	if err != nil {
		t.Fatalf("containerStatuses: %v", err)
	}
	if len(statuses) != 2 {
		t.Fatalf("expected two containers, got %v", statuses)
	}
	if statuses[0].Service != "web" || !strings.Contains(statuses[0].Status, "healthy") {
		t.Errorf("first status = %+v", statuses[0])
	}

	// the filter has to be the compose project label, since that works without
	// finding a compose file first
	var asked bool
	for _, command := range runner.ran {
		if strings.Contains(command, "label=com.docker.compose.project="+ProjectName("a3f19c02", "9f4be0a")) {
			asked = true
		}
	}
	if !asked {
		t.Errorf("status should be filtered by the compose project label, ran: %v", runner.ran)
	}
}

func TestStatusOnAProjectNeverDeployedSaysSo(t *testing.T) {
	repository := newRepository(t)
	writeFile(t, repository, configFileName, twoTierConfig)
	commitFile(t, repository, "one.txt", "x")

	exitCode, err := RunStatus(DeployOptions{
		Context: repository, Destination: t.TempDir(), Environment: defaultEnvironmentName,
	})
	if err != nil {
		t.Fatalf("status on a fresh project should not be an error: %v", err)
	}
	if exitCode != exitOK {
		t.Errorf("exit code = %d, want %d", exitCode, exitOK)
	}
}

func TestListReadsEveryProjectOnTheDestination(t *testing.T) {
	destination := t.TempDir()
	runner := LocalRunner{}

	projects := []struct {
		id          string
		name        string
		current     string
		environment string
	}{
		{"aaaa1111", "lectern", "9f4be0a", "production"},
		{"bbbb2222", "smp", "1234567", "production"},
		{"cccc3333", "portfolio", "abcdef0", "development"},
	}

	for _, project := range projects {
		layout := NewLayout(destination, project.id)
		if err := runner.MkdirAll(layout.Root); err != nil {
			t.Fatalf("creating %s: %v", layout.Root, err)
		}
		state := State{
			Name:        project.name,
			Current:     project.current,
			Environment: project.environment,
			Releases:    []string{project.current},
			UpdatedAt:   time.Now().UTC(),
		}
		if err := WriteState(runner, layout, state); err != nil {
			t.Fatalf("writing state: %v", err)
		}
	}

	// a directory that is shaped like a deploy id but has never been deployed
	if err := os.MkdirAll(filepath.Join(destination, "dddd4444"), 0o755); err != nil {
		t.Fatalf("creating decoy: %v", err)
	}

	// and something with a perfectly good state.json that is not a deploy id at
	// all. only the id check can exclude this one, so it is what makes that check
	// mean something rather than duplicate the state check
	decoy := NewLayout(destination, "backups-2026")
	if err := runner.MkdirAll(decoy.Root); err != nil {
		t.Fatalf("creating decoy: %v", err)
	}
	if err := WriteState(runner, decoy, State{
		Name: "not-a-deploy-project", Current: "9999999", Environment: "production",
	}); err != nil {
		t.Fatalf("writing decoy state: %v", err)
	}

	repository := newRepository(t)
	writeFile(t, repository, configFileName, strings.Replace(twoTierConfig, "a3f19c02", "aaaa1111", 1))
	commitFile(t, repository, "one.txt", "x")

	output := captureStdout(t, func() {
		if _, err := RunList(DeployOptions{
			Context: repository, Destination: destination, Environment: defaultEnvironmentName,
		}); err != nil {
			t.Fatalf("RunList: %v", err)
		}
	})

	for _, project := range projects {
		if !strings.Contains(output, project.name) {
			t.Errorf("list should name %q, got:\n%s", project.name, output)
		}
		if !strings.Contains(output, project.id) {
			t.Errorf("list should show id %q, got:\n%s", project.id, output)
		}
	}
	// an id shaped directory with no state has never been deployed, so it is not
	// a project to report
	if strings.Contains(output, "dddd4444") {
		t.Errorf("a directory with no state.json should be skipped, got:\n%s", output)
	}
	// and a directory with state but no deploy id was never ours to report on
	for _, decoy := range []string{"backups-2026", "not-a-deploy-project"} {
		if strings.Contains(output, decoy) {
			t.Errorf("%q is not a deploy id and should be ignored, got:\n%s", decoy, output)
		}
	}
	// and the project you are standing in is marked, since that is the question
	// you actually have
	if !strings.Contains(output, "this one") {
		t.Errorf("list should mark the current project, got:\n%s", output)
	}
}

func captureStdout(t *testing.T, run func()) string {
	t.Helper()

	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("creating pipe: %v", err)
	}

	original := os.Stdout
	os.Stdout = writer
	defer func() { os.Stdout = original }()

	done := make(chan string, 1)
	go func() {
		var collected strings.Builder
		buffer := make([]byte, 4096)
		for {
			read, err := reader.Read(buffer)
			if read > 0 {
				collected.Write(buffer[:read])
			}
			if err != nil {
				break
			}
		}
		done <- collected.String()
	}()

	run()
	writer.Close()

	return <-done
}

func TestStatusAndShellAgainstARunningDeploy(t *testing.T) {
	dockerAvailable(t)

	repository := newRepository(t)
	writeFile(t, repository, configFileName, `{
      "version": 1,
      "id": "dd000012",
      "name": "livable",
      "services": {
        "store": {
          "image": "busybox:latest",
          "stateful": true,
          "command": ["sh", "-c", "sleep 300"],
          "volumes": ["data:/data"],
          "healthcheck": {"command": ["CMD", "true"], "interval": "1s", "retries": 5}
        },
        "app": {
          "image": "busybox:latest",
          "stateful": false,
          "command": ["sh", "-c", "echo hello from the app; sleep 300"],
          "healthcheck": {"command": ["CMD", "true"], "interval": "1s", "retries": 5}
        }
      }
    }`)
	commit := commitFile(t, repository, "one.txt", "x")

	destination := t.TempDir()
	t.Cleanup(func() {
		exec.Command("docker", "compose", "--project-name", ProjectName("dd000012", commit), "down").Run()
		exec.Command("docker", "compose", "--project-name", SharedProjectName("dd000012"), "down").Run()
		exec.Command("docker", "volume", "rm", "-f", VolumeName("dd000012", "data")).Run()
		exec.Command("docker", "network", "rm", NetworkName("dd000012")).Run()
	})

	options := DeployOptions{
		Context: repository, Destination: destination, Environment: defaultEnvironmentName,
	}
	if _, err := RunDeploy(options); err != nil {
		t.Fatalf("RunDeploy: %v", err)
	}

	status := captureStdout(t, func() {
		if _, err := RunStatus(options); err != nil {
			t.Fatalf("RunStatus: %v", err)
		}
	})

	for _, want := range []string{"livable", ShortCommit(commit), "app", "store", "healthy"} {
		if !strings.Contains(status, want) {
			t.Errorf("status should mention %q, got:\n%s", want, status)
		}
	}
	// both stacks are reported, because "which one is it in" is exactly what
	// nobody should have to remember
	if !strings.Contains(status, "shared stack") || !strings.Contains(status, "release stack") {
		t.Errorf("status should report both stacks, got:\n%s", status)
	}

	// exec reaches the stateless service in the per-commit project
	if _, err := RunExec(options, "app", []string{"echo", "reached the app"}); err != nil {
		t.Errorf("exec into a stateless service: %v", err)
	}
	// and the stateful one in the shared project, without being told which
	if _, err := RunExec(options, "store", []string{"sh", "-c", "echo reached > /data/proof.txt"}); err != nil {
		t.Errorf("exec into a stateful service: %v", err)
	}

	written, err := exec.Command(
		"docker", "compose", "--project-name", SharedProjectName("dd000012"),
		"exec", "-T", "store", "cat", "/data/proof.txt",
	).Output()
	if err != nil || !strings.Contains(string(written), "reached") {
		t.Errorf("exec should have run inside the shared service, got %q: %v", written, err)
	}
}

func TestStateCarriesTheProjectNameForList(t *testing.T) {
	layout := NewLayout(t.TempDir(), "a3f19c02")
	runner := LocalRunner{}

	if err := runner.MkdirAll(layout.Root); err != nil {
		t.Fatalf("creating root: %v", err)
	}
	if err := WriteState(runner, layout, State{Name: "lectern", Current: "9f4be0a"}); err != nil {
		t.Fatalf("WriteState: %v", err)
	}

	raw, err := runner.ReadFile(layout.StateFile())
	if err != nil {
		t.Fatalf("reading state: %v", err)
	}

	var stored map[string]any
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatalf("parsing state: %v", err)
	}
	// list has no config to read, so the name has to be in state or a destination
	// is a directory of hex
	if stored["name"] != "lectern" {
		t.Errorf("state should carry the project name, got %v", stored)
	}
}
