package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGenerateProjectIDIsEightHexCharacters(t *testing.T) {
	seen := map[string]bool{}

	for range 200 {
		projectID, err := GenerateProjectID()
		if err != nil {
			t.Fatalf("GenerateProjectID: %v", err)
		}
		if !projectIDPattern.MatchString(projectID) {
			t.Fatalf("id %q is not 8 hex characters", projectID)
		}
		seen[projectID] = true
	}

	// 200 draws from 32 bits should not repeat, and a generator that did would
	// mean two projects sharing a directory on the destination
	if len(seen) != 200 {
		t.Errorf("got %d distinct ids from 200 draws", len(seen))
	}
}

func TestDeployWritesAConfigForAProjectThatHasNone(t *testing.T) {
	repository := newRepository(t)
	commitFile(t, repository, "one.txt", "x")

	created, err := EnsureProjectConfig(repository, "/srv/projects")
	if err != nil {
		t.Fatalf("EnsureProjectConfig: %v", err)
	}
	if !created {
		t.Fatal("a project with no config should get one")
	}

	project, err := LoadProject(filepath.Join(repository, configFileName))
	if err != nil {
		t.Fatalf("the config it wrote should parse: %v", err)
	}

	if !projectIDPattern.MatchString(project.ID) {
		t.Errorf("id = %q", project.ID)
	}
	// the name comes from the directory, since that is the one thing deploy can
	// tell without asking
	if project.Name != filepath.Base(repository) {
		t.Errorf("name = %q, want %q", project.Name, filepath.Base(repository))
	}
	if project.Destination != "/srv/projects" {
		t.Errorf("destination = %q, want the one passed in", project.Destination)
	}
	if project.Version != supportedConfigVersion {
		t.Errorf("version = %d", project.Version)
	}

	// machine-local state does not belong in anybody's commits
	ignore, err := os.ReadFile(filepath.Join(repository, ".gitignore"))
	if err != nil {
		t.Fatalf("reading .gitignore: %v", err)
	}
	if !strings.Contains(string(ignore), ".deploy/") {
		t.Errorf(".gitignore should exclude .deploy/, got:\n%s", ignore)
	}
}

// Overwriting a config somebody wrote would lose their services and their id,
// and a new id means a second directory on the destination.
func TestAnExistingConfigIsNeverOverwritten(t *testing.T) {
	repository := newRepository(t)
	const original = `{
      "version": 1, "id": "a3f19c02", "name": "lectern",
      "services": {"web": {"image": "nginx"}}
    }`
	writeFile(t, repository, configFileName, original)
	commitFile(t, repository, "one.txt", "x")

	created, err := EnsureProjectConfig(repository, "/somewhere/else")
	if err != nil {
		t.Fatalf("EnsureProjectConfig: %v", err)
	}
	if created {
		t.Fatal("a project that already has a config should be left alone")
	}

	contents, err := os.ReadFile(filepath.Join(repository, configFileName))
	if err != nil {
		t.Fatalf("reading config: %v", err)
	}
	if string(contents) != original {
		t.Errorf("the config was rewritten.\n--- got ---\n%s\n--- want ---\n%s", contents, original)
	}
}

func TestGitignoreIsNotDuplicatedOrClobbered(t *testing.T) {
	repository := newRepository(t)
	writeFile(t, repository, ".gitignore", "node_modules\n*.log\n")
	commitFile(t, repository, "one.txt", "x")

	for range 3 {
		if _, err := ignoreDeployDirectory(repository); err != nil {
			t.Fatalf("ignoreDeployDirectory: %v", err)
		}
	}

	contents, err := os.ReadFile(filepath.Join(repository, ".gitignore"))
	if err != nil {
		t.Fatalf("reading .gitignore: %v", err)
	}

	// what was already there stays there
	for _, existing := range []string{"node_modules", "*.log"} {
		if !strings.Contains(string(contents), existing) {
			t.Errorf("%q was lost from .gitignore, got:\n%s", existing, contents)
		}
	}
	if count := strings.Count(string(contents), ".deploy/"); count != 1 {
		t.Errorf(".deploy/ appears %d times after three runs, want 1:\n%s", count, contents)
	}
}

func TestGitignoreWithoutATrailingNewlineStillGetsALineOfItsOwn(t *testing.T) {
	repository := newRepository(t)
	// a file whose last line has no newline is a real thing an editor leaves
	if err := os.WriteFile(filepath.Join(repository, ".gitignore"), []byte("node_modules"), 0o644); err != nil {
		t.Fatalf("writing .gitignore: %v", err)
	}

	if _, err := ignoreDeployDirectory(repository); err != nil {
		t.Fatalf("ignoreDeployDirectory: %v", err)
	}

	contents, err := os.ReadFile(filepath.Join(repository, ".gitignore"))
	if err != nil {
		t.Fatalf("reading .gitignore: %v", err)
	}
	if strings.Contains(string(contents), "node_modules.deploy/") {
		t.Errorf("the entry was appended onto the previous line:\n%s", contents)
	}
	if !strings.Contains(string(contents), "\n.deploy/") {
		t.Errorf(".deploy/ should be on its own line, got:\n%s", contents)
	}
}

// Running deploy in a fresh project should not require running something else
// first. It writes what it can, then stops on the one thing it cannot invent.
func TestDeployInAFreshProjectInitialisesThenStopsOnServices(t *testing.T) {
	repository := newRepository(t)
	commitFile(t, repository, "one.txt", "x")

	if _, err := os.Stat(filepath.Join(repository, configFileName)); err == nil {
		t.Fatal("this project should start without a config")
	}

	exitCode, err := RunDeploy(DeployOptions{
		Context: repository, Destination: t.TempDir(), Environment: defaultEnvironmentName,
	})

	if err == nil {
		t.Fatal("a project with no services cannot be deployed")
	}
	if exitCode != exitPreconditionNotMet {
		t.Errorf("exit code = %d, want %d", exitCode, exitPreconditionNotMet)
	}
	// and the reason is the one thing deploy could not work out for itself
	if !strings.Contains(err.Error(), "no services defined") {
		t.Errorf("the error should name what is missing, got: %v", err)
	}

	// the file exists now, so the next run has something to read
	project, err := LoadProject(filepath.Join(repository, configFileName))
	if err != nil {
		t.Fatalf("deploy should have left a readable config: %v", err)
	}
	if !projectIDPattern.MatchString(project.ID) {
		t.Errorf("id = %q", project.ID)
	}
}

// The whole point of doing this inside deploy is that filling in services and
// running the same command again works.
func TestASecondRunAfterFillingInServicesDeploys(t *testing.T) {
	dockerAvailable(t)

	repository := newRepository(t)
	commitFile(t, repository, "one.txt", "x")

	destination := t.TempDir()
	options := DeployOptions{
		Context: repository, Destination: destination, Environment: defaultEnvironmentName,
	}

	if _, err := RunDeploy(options); err == nil {
		t.Fatal("the first run has no services to deploy")
	}

	// fill in the one thing it asked for, keeping the id it generated
	project, err := LoadProject(filepath.Join(repository, configFileName))
	if err != nil {
		t.Fatalf("loading the generated config: %v", err)
	}

	writeFile(t, repository, configFileName, `{
      "version": 1,
      "id": "`+project.ID+`",
      "name": "`+project.Name+`",
      "services": {
        "app": {
          "image": "busybox:latest",
          "stateful": false,
          "command": ["sh", "-c", "sleep 300"],
          "healthcheck": {"command": ["CMD", "true"], "interval": "1s", "retries": 5}
        }
      }
    }`)
	commit := commitFile(t, repository, "two.txt", "y")

	t.Cleanup(func() {
		exec.Command("docker", "compose", "--project-name", ProjectName(project.ID, commit), "down").Run()
		exec.Command("docker", "network", "rm", NetworkName(project.ID)).Run()
	})

	if _, err := RunDeploy(options); err != nil {
		t.Fatalf("the second run should deploy: %v", err)
	}

	state, err := ReadState(LocalRunner{}, NewLayout(destination, project.ID))
	if err != nil {
		t.Fatalf("ReadState: %v", err)
	}
	if state.Current != ShortCommit(commit) {
		t.Errorf("current = %q, want %q", state.Current, ShortCommit(commit))
	}
	// and the id it generated is the one it deployed under, so a second run does
	// not become a second project
	if state.Name != project.Name {
		t.Errorf("state name = %q, want %q", state.Name, project.Name)
	}
}

// The layer cache must not land in the repository, because a cache inside the
// checkout makes the tree dirty and the next deploy refuses over a directory
// deploy created itself.
func TestTheBuildCacheStaysOutOfTheRepository(t *testing.T) {
	repository := newRepository(t)

	cache := buildCacheDirectory("a3f19c02")

	if strings.HasPrefix(cache, repository) {
		t.Errorf("the build cache is inside the repository at %s", cache)
	}
	if !strings.Contains(cache, "a3f19c02") {
		t.Errorf("the cache should be per project, got %s", cache)
	}
}
