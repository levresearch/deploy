package main

import (
	"os/exec"
	"strings"
	"testing"
)

func TestReleaseTasksRenderIntoTheirOwnProjectOnTheSharedNetwork(t *testing.T) {
	const contents = `{
      "version": 1, "id": "a3f19c02", "name": "lectern",
      "services": {"web": {"image": "nginx", "stateful": false}},
      "release": {
        "migrate": {
          "build": {"dockerfile": "Dockerfile"},
          "command": "db:migrate",
          "env": [".env"],
          "dependsOn": {"pg": "healthy"}
        }
      }
    }`

	resolved := loadAndResolve(t, contents, defaultEnvironmentName)
	rendered, err := RenderReleaseTasks(resolved, NewLayout("/srv/projects", resolved.ID), "9f4be0affff")
	if err != nil {
		t.Fatalf("RenderReleaseTasks: %v", err)
	}
	tasks := decodeCompose(t, rendered)

	if _, found := tasks.Services["migrate"]; !found {
		t.Fatalf("expected a migrate task, got %v", tasks.Services)
	}
	if _, found := tasks.Services["web"]; found {
		t.Error("a release task project holds only tasks, not services")
	}

	network, ok := tasks.Networks["default"].(map[string]any)
	if !ok || network["name"] != NetworkName("a3f19c02") {
		t.Errorf("release tasks must join the shared network, got %v", tasks.Networks)
	}
	if network["external"] != true {
		t.Error("the shared network is external to a release task project too")
	}

	// its own project, so compose run never collides with the release stack
	if tasks.Name == ProjectName("a3f19c02", "9f4be0affff") {
		t.Error("release tasks need a project name of their own")
	}

	var task struct {
		Image     string   `json:"image"`
		Command   string   `json:"command"`
		EnvFile   []string `json:"env_file"`
		DependsOn any      `json:"depends_on"`
	}
	decodeService(t, tasks.Services["migrate"], &task)

	if want := ImageTag("a3f19c02", "migrate", "9f4be0affff"); task.Image != want {
		t.Errorf("image = %q, want %q", task.Image, want)
	}
	if task.Command != "db:migrate" {
		t.Errorf("command = %q, want it passed through", task.Command)
	}
	// a bare name points at the project level env directory, since a gitignored
	// env file is never in the release tree git archive placed
	if want := "/srv/projects/a3f19c02/env/.env"; len(task.EnvFile) != 1 || task.EnvFile[0] != want {
		t.Errorf("env_file = %v, want [%s]", task.EnvFile, want)
	}
	// pg lives in the shared stack, so compose cannot wait on it from here
	if task.DependsOn != nil {
		t.Errorf("a cross-project dependency should be dropped, got %v", task.DependsOn)
	}
}

func TestReleaseTasksAreValidatedLikeServices(t *testing.T) {
	const contents = `{
      "version": 1, "id": "a3f19c02", "name": "x",
      "services": {"web": {"image": "nginx"}},
      "release": {"migrate": {"dependsOn": {"web": "healthy"}}}
    }`

	err := loadAndResolve(t, contents, defaultEnvironmentName).Validate()
	if err == nil {
		t.Fatal("a release task with neither image nor build should be rejected")
	}
	if !strings.Contains(err.Error(), "neither image nor build") {
		t.Errorf("got: %v", err)
	}
}

func TestASucceedingReleaseTaskLetsTheDeployContinue(t *testing.T) {
	dockerAvailable(t)

	repository := newRepository(t)
	writeFile(t, repository, configFileName, `{
      "version": 1,
      "id": "dd000007",
      "name": "withtask",
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
          "command": ["sh", "-c", "sleep 300"],
          "healthcheck": {"command": ["CMD", "true"], "interval": "1s", "retries": 5}
        }
      },
      "release": {
        "migrate": {
          "image": "busybox:latest",
          "command": ["sh", "-c", "echo 'migration ran' > /data/migrated.txt"],
          "volumes": ["data:/data"]
        }
      }
    }`)

	destination := t.TempDir()
	cleanUpProject(t, "dd000007")

	commit := commitFile(t, repository, "round.txt", "a")
	t.Cleanup(func() {
		exec.Command("docker", "compose", "--project-name", ProjectName("dd000007", commit), "down").Run()
	})

	exitCode, err := RunDeploy(DeployOptions{
		Context:     repository,
		Destination: destination,
		Environment: defaultEnvironmentName,
	})
	if err != nil {
		t.Fatalf("RunDeploy: %v", err)
	}
	if exitCode != exitOK {
		t.Fatalf("exit code = %d, want %d", exitCode, exitOK)
	}

	// the task wrote into the shared volume, which proves it reached the shared
	// stack rather than running in isolation
	written, err := exec.Command(
		"docker", "compose", "--project-name", SharedProjectName("dd000007"),
		"exec", "-T", "store", "cat", "/data/migrated.txt",
	).CombinedOutput()
	if err != nil {
		t.Fatalf("reading what the task wrote: %v\n%s", err, written)
	}
	if !strings.Contains(string(written), "migration ran") {
		t.Errorf("the release task did not write to the shared volume, got %q", written)
	}
}

// A failing migration must stop the deploy before anything new starts, leaving
// whatever was already serving exactly as it was.
func TestAFailingReleaseTaskAbortsAndLeavesThePreviousReleaseServing(t *testing.T) {
	dockerAvailable(t)

	repository := newRepository(t)
	const config = `{
      "version": 1,
      "id": "dd000008",
      "name": "failtask",
      "services": {
        "app": {
          "image": "busybox:latest",
          "stateful": false,
          "command": ["sh", "-c", "sleep 300"],
          "healthcheck": {"command": ["CMD", "true"], "interval": "1s", "retries": 5}
        }
      }%s
    }`

	writeFile(t, repository, configFileName, strings.Replace(config, "%s", "", 1))
	cleanUpProject(t, "dd000008")

	firstCommit := commitFile(t, repository, "round.txt", "a")
	t.Cleanup(func() {
		exec.Command("docker", "compose", "--project-name", ProjectName("dd000008", firstCommit), "down").Run()
	})

	if _, err := RunDeploy(DeployOptions{
		Context: repository, Destination: t.TempDir(), Environment: defaultEnvironmentName,
	}); err != nil {
		t.Fatalf("the first deploy should succeed: %v", err)
	}

	// deploy that first release into the destination we will actually reuse
	destination := t.TempDir()
	if _, err := RunDeploy(DeployOptions{
		Context: repository, Destination: destination, Environment: defaultEnvironmentName,
	}); err != nil {
		t.Fatalf("seeding the destination: %v", err)
	}

	// now add a release task that always fails
	failing := strings.Replace(config, "%s", `,
      "release": {
        "migrate": {
          "image": "busybox:latest",
          "command": ["sh", "-c", "echo 'schema is wrong' >&2; exit 1"]
        }
      }`, 1)
	writeFile(t, repository, configFileName, failing)

	secondCommit := commitFile(t, repository, "round.txt", "b")
	t.Cleanup(func() {
		exec.Command("docker", "compose", "--project-name", ProjectName("dd000008", secondCommit), "down").Run()
	})

	exitCode, err := RunDeploy(DeployOptions{
		Context: repository, Destination: destination, Environment: defaultEnvironmentName,
	})
	if err == nil {
		t.Fatal("a failing release task must fail the deploy")
	}
	if exitCode != exitDeployFailed {
		t.Errorf("exit code = %d, want %d", exitCode, exitDeployFailed)
	}
	if !strings.Contains(err.Error(), "migrate") {
		t.Errorf("the error should name the task that failed, got: %v", err)
	}

	// the new stack never started
	running, err := exec.Command("docker", "ps", "--format", "{{.Names}}").Output()
	if err != nil {
		t.Fatalf("listing containers: %v", err)
	}
	if strings.Contains(string(running), ProjectName("dd000008", secondCommit)) {
		t.Error("the new stack must not start when a release task failed")
	}

	// and the release that was already serving is untouched
	if !strings.Contains(string(running), ProjectName("dd000008", firstCommit)) {
		t.Error("the previously running release should still be serving")
	}

	// state still points at the release that actually works
	state, err := ReadState(LocalRunner{}, NewLayout(destination, "dd000008"))
	if err != nil {
		t.Fatalf("ReadState: %v", err)
	}
	if state.Current != ShortCommit(firstCommit) {
		t.Errorf("current = %q, want the release that is actually serving %q", state.Current, ShortCommit(firstCommit))
	}
}

func cleanUpProject(t *testing.T, projectID string) {
	t.Helper()

	t.Cleanup(func() {
		exec.Command("docker", "compose", "--project-name", SharedProjectName(projectID), "down").Run()
		exec.Command("docker", "volume", "rm", "-f", VolumeName(projectID, "data")).Run()
		exec.Command("docker", "network", "rm", NetworkName(projectID)).Run()
	})
}
