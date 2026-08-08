package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestParseDestination(t *testing.T) {
	cases := []struct {
		raw      string
		host     string
		wantPath string
	}{
		{"/srv/projects", "", "/srv/projects"},
		{"~/Projects", "", "~/Projects"},
		{"./builds", "", "./builds"},
		{"builds", "", "builds"},
		{"git:Projects", "git", "Projects"},
		{"ethan@192.168.2.13:Projects", "ethan@192.168.2.13", "Projects"},
		{"192.168.2.13:Projects", "192.168.2.13", "Projects"},
		{"git:/srv/projects", "git", "/srv/projects"},
		// a slash before the colon means the colon is part of a path, which is
		// the rule scp and rsync use
		{"./weird:name", "", "./weird:name"},
		{"/srv/a:b", "", "/srv/a:b"},
	}

	for _, testCase := range cases {
		t.Run(testCase.raw, func(t *testing.T) {
			destination, err := ParseDestination(testCase.raw)
			if err != nil {
				t.Fatalf("ParseDestination(%q): %v", testCase.raw, err)
			}
			if destination.Host != testCase.host {
				t.Errorf("host = %q, want %q", destination.Host, testCase.host)
			}
			if destination.Path != testCase.wantPath {
				t.Errorf("path = %q, want %q", destination.Path, testCase.wantPath)
			}
			if destination.IsRemote() != (testCase.host != "") {
				t.Errorf("IsRemote() = %v", destination.IsRemote())
			}
		})
	}
}

func TestParseDestinationRejects(t *testing.T) {
	for _, raw := range []string{"", "git:", "ssh://git/srv"} {
		if _, err := ParseDestination(raw); err == nil {
			t.Errorf("ParseDestination(%q) should have been refused", raw)
		}
	}
}

// newRepository builds a real git repo in a temp dir, because the git helpers
// shell out to git and mocking that would only prove the mock works.
func newRepository(t *testing.T) string {
	t.Helper()

	root := t.TempDir()
	for _, command := range [][]string{
		{"init", "--initial-branch=main"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "test"},
	} {
		if _, err := runGit(root, command...); err != nil {
			t.Fatalf("preparing repository: %v", err)
		}
	}

	return root
}

func commitFile(t *testing.T, repositoryPath, name, contents string) string {
	t.Helper()

	full := filepath.Join(repositoryPath, name)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("creating %s: %v", filepath.Dir(full), err)
	}
	if err := os.WriteFile(full, []byte(contents), 0o644); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}
	if _, err := runGit(repositoryPath, "add", "-A"); err != nil {
		t.Fatalf("staging %s: %v", name, err)
	}
	if _, err := runGit(repositoryPath, "commit", "-m", "add "+name); err != nil {
		t.Fatalf("committing %s: %v", name, err)
	}

	commit, err := ResolveCommit(repositoryPath, "HEAD")
	if err != nil {
		t.Fatalf("resolving HEAD: %v", err)
	}

	return commit
}

func TestFindRepositoryFromASubdirectory(t *testing.T) {
	root := newRepository(t)
	commitFile(t, root, "nested/deep/file.txt", "hello")

	found, err := FindRepository(filepath.Join(root, "nested", "deep"))
	if err != nil {
		t.Fatalf("FindRepository: %v", err)
	}

	// macos temp dirs are symlinked, so compare what git itself reports
	expected, err := FindRepository(root)
	if err != nil {
		t.Fatalf("FindRepository(root): %v", err)
	}
	if found != expected {
		t.Errorf("found %q, want %q", found, expected)
	}
}

func TestFindRepositoryRefusesSomewhereWithoutOne(t *testing.T) {
	if _, err := FindRepository(t.TempDir()); err == nil {
		t.Fatal("expected a directory outside any repository to be refused")
	}
}

func TestPlaceReleaseShipsTheCommitAndNotTheWorkingTree(t *testing.T) {
	repository := newRepository(t)
	commit := commitFile(t, repository, "tracked.txt", "committed contents")

	// an untracked file and an uncommitted edit, neither of which may arrive
	if err := os.WriteFile(filepath.Join(repository, "untracked.txt"), []byte("nope"), 0o644); err != nil {
		t.Fatalf("writing untracked file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repository, "tracked.txt"), []byte("edited"), 0o644); err != nil {
		t.Fatalf("editing tracked file: %v", err)
	}

	release := filepath.Join(t.TempDir(), "release")
	if err := PlaceRelease(LocalRunner{}, repository, commit, release); err != nil {
		t.Fatalf("PlaceRelease: %v", err)
	}

	placed, err := os.ReadFile(filepath.Join(release, "tracked.txt"))
	if err != nil {
		t.Fatalf("reading placed file: %v", err)
	}
	if string(placed) != "committed contents" {
		t.Errorf("placed %q, want the committed contents rather than the working tree", placed)
	}
	if _, err := os.Stat(filepath.Join(release, "untracked.txt")); err == nil {
		t.Error("an untracked file must never reach the release")
	}
	if _, err := os.Stat(filepath.Join(release, ".git")); err == nil {
		t.Error("the .git directory must never reach the release")
	}
}

// Placing an older commit has to yield that commit rather than HEAD, which is
// what rollback will depend on and what deploying HEAD alone cannot prove.
func TestPlaceReleaseShipsTheNamedCommitRatherThanHead(t *testing.T) {
	repository := newRepository(t)
	older := commitFile(t, repository, "version.txt", "first")
	commitFile(t, repository, "version.txt", "second")

	release := filepath.Join(t.TempDir(), "release")
	if err := PlaceRelease(LocalRunner{}, repository, older, release); err != nil {
		t.Fatalf("PlaceRelease: %v", err)
	}

	placed, err := os.ReadFile(filepath.Join(release, "version.txt"))
	if err != nil {
		t.Fatalf("reading placed file: %v", err)
	}
	if string(placed) != "first" {
		t.Errorf("placed %q, want the older commit's contents rather than HEAD's", placed)
	}
}

func TestWorkingTreeDirtiness(t *testing.T) {
	repository := newRepository(t)
	commitFile(t, repository, "tracked.txt", "clean")

	dirty, err := IsWorkingTreeDirty(repository)
	if err != nil {
		t.Fatalf("IsWorkingTreeDirty: %v", err)
	}
	if dirty {
		t.Error("a freshly committed tree is clean")
	}

	if err := os.WriteFile(filepath.Join(repository, "tracked.txt"), []byte("edited"), 0o644); err != nil {
		t.Fatalf("editing file: %v", err)
	}

	dirty, err = IsWorkingTreeDirty(repository)
	if err != nil {
		t.Fatalf("IsWorkingTreeDirty: %v", err)
	}
	if !dirty {
		t.Error("an edited tree is dirty")
	}
}

func TestCommitExists(t *testing.T) {
	repository := newRepository(t)
	commit := commitFile(t, repository, "a.txt", "a")

	if !CommitExists(repository, commit) {
		t.Error("a commit that was just made should exist")
	}
	if CommitExists(repository, "0000000000000000000000000000000000000000") {
		t.Error("a commit that was never made should not exist")
	}
}

func TestRenderComposeGoldenOutput(t *testing.T) {
	const contents = `{
      "version": 1,
      "id": "a3f19c02",
      "name": "lectern",
      "services": {
        "web": {
          "build": { "dockerfile": "Dockerfile" },
          "env": [".env.production"],
          "restart": "unless-stopped",
          "dependsOn": { "pg": "healthy" },
          "healthcheck": {
            "command": ["CMD", "curl", "-f", "http://localhost:3000/health"],
            "interval": "5s",
            "startPeriod": "10s"
          }
        },
        "pg": { "image": "postgres:17", "stateful": true }
      }
    }`

	resolved := loadAndResolve(t, contents, defaultEnvironmentName)
	rendered, err := RenderCompose(resolved, "9f4be0affffffffffffffffffffffffffffffffff")
	if err != nil {
		t.Fatalf("RenderCompose: %v", err)
	}

	const want = `{
  "name": "deploy-a3f19c02-9f4be0a",
  "services": {
    "pg": {
      "image": "postgres:17"
    },
    "web": {
      "depends_on": {
        "pg": {
          "condition": "service_healthy"
        }
      },
      "env_file": [
        ".env.production"
      ],
      "healthcheck": {
        "interval": "5s",
        "start_period": "10s",
        "test": [
          "CMD",
          "curl",
          "-f",
          "http://localhost:3000/health"
        ]
      },
      "image": "deploy-a3f19c02/web:9f4be0a",
      "restart": "unless-stopped"
    }
  }
}
`

	if string(rendered) != want {
		t.Errorf("rendered compose does not match.\n--- got ---\n%s\n--- want ---\n%s", rendered, want)
	}
}

func TestRenderComposeNeverEmitsABuildKey(t *testing.T) {
	const contents = `{
      "version": 1, "id": "a3f19c02", "name": "x",
      "services": {"web": {"build": {"dockerfile": "Dockerfile"}}}
    }`

	rendered, err := RenderCompose(loadAndResolve(t, contents, defaultEnvironmentName), "abcdef1234")
	if err != nil {
		t.Fatalf("RenderCompose: %v", err)
	}
	if strings.Contains(string(rendered), `"build"`) {
		t.Errorf("compose must only ever see image, got:\n%s", rendered)
	}

	var document struct {
		Services map[string]struct {
			Image string `json:"image"`
		} `json:"services"`
	}
	if err := json.Unmarshal(rendered, &document); err != nil {
		t.Fatalf("rendered compose is not valid json: %v", err)
	}
	if got, want := document.Services["web"].Image, "deploy-a3f19c02/web:abcdef1"; got != want {
		t.Errorf("image = %q, want %q", got, want)
	}
}

func TestDockerfilePathReadsBothForms(t *testing.T) {
	cases := []struct {
		name  string
		build string
		want  string
	}{
		{"object form", `{"dockerfile": "Dockerfile.worker"}`, "Dockerfile.worker"},
		{"string form", `"Dockerfile"`, "Dockerfile"},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got, err := dockerfilePath("web", Service{Build: json.RawMessage(testCase.build)})
			if err != nil {
				t.Fatalf("dockerfilePath: %v", err)
			}
			if got != testCase.want {
				t.Errorf("got %q, want %q", got, testCase.want)
			}
		})
	}

	// the inline build description is task 11, and half handling it would be worse
	// than refusing it
	if _, err := dockerfilePath("web", Service{Build: json.RawMessage(`{"from": "node:24"}`)}); err == nil {
		t.Error("an inline build block should be refused until it is implemented")
	}
}

func dockerAvailable(t *testing.T) {
	t.Helper()

	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker is not available on this machine")
	}
}

func TestDeployEndToEnd(t *testing.T) {
	dockerAvailable(t)

	repository := newRepository(t)
	writeFile(t, repository, "Dockerfile", "FROM busybox:latest\nCOPY marker.txt /marker.txt\nCMD [\"sh\", \"-c\", \"sleep 300\"]\n")
	writeFile(t, repository, "marker.txt", "from the commit\n")
	writeFile(t, repository, configFileName, `{
      "version": 1,
      "id": "dd000001",
      "name": "endtoend",
      "services": {
        "app": {
          "build": {"dockerfile": "Dockerfile"},
          "stateful": false,
          "healthcheck": {"command": ["CMD", "true"], "interval": "1s", "retries": 5}
        }
      }
    }`)

	if _, err := runGit(repository, "add", "-A"); err != nil {
		t.Fatalf("staging: %v", err)
	}
	if _, err := runGit(repository, "commit", "-m", "initial"); err != nil {
		t.Fatalf("committing: %v", err)
	}
	commit, err := ResolveCommit(repository, "HEAD")
	if err != nil {
		t.Fatalf("resolving HEAD: %v", err)
	}

	destination := t.TempDir()
	projectName := ProjectName("dd000001", commit)
	release := filepath.Join(destination, "dd000001", "releases", ShortCommit(commit))

	t.Cleanup(func() {
		exec.Command("docker", "compose", "--project-name", projectName, "down", "--volumes").Run()
		exec.Command("docker", "image", "rm", "-f", ImageTag("dd000001", "app", commit)).Run()
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

	if _, err := os.Stat(filepath.Join(release, composeFileName)); err != nil {
		t.Errorf("the rendered compose file should be in the release directory: %v", err)
	}

	running, err := exec.Command(
		"docker", "compose", "--project-name", projectName, "ps", "--format", "{{.State}}",
	).Output()
	if err != nil {
		t.Fatalf("listing containers: %v", err)
	}
	if !strings.Contains(string(running), "running") {
		t.Errorf("the container should be running, got %q", strings.TrimSpace(string(running)))
	}
}

func writeFile(t *testing.T, directory, name, contents string) {
	t.Helper()

	if err := os.WriteFile(filepath.Join(directory, name), []byte(contents), 0o644); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}
}

func TestDeployRefusesADirtyTree(t *testing.T) {
	repository := newRepository(t)
	writeFile(t, repository, configFileName, `{
      "version": 1, "id": "dd000002", "name": "dirty",
      "services": {"app": {"image": "busybox:latest"}}
    }`)
	commitFile(t, repository, "tracked.txt", "clean")
	writeFile(t, repository, "tracked.txt", "edited")

	options := DeployOptions{
		Context:     repository,
		Destination: t.TempDir(),
		Environment: defaultEnvironmentName,
	}

	exitCode, err := RunDeploy(options)
	if err == nil {
		t.Fatal("a dirty working tree should be refused")
	}
	if exitCode != exitPreconditionNotMet {
		t.Errorf("exit code = %d, want %d", exitCode, exitPreconditionNotMet)
	}
	if !strings.Contains(err.Error(), "--allow-dirty") {
		t.Errorf("the refusal should name the flag that overrides it, got: %v", err)
	}

	// --allow-dirty gets past the check, and then fails later for its own reasons
	// on a machine without docker, so only the dirtiness complaint matters here
	options.AllowDirty = true
	if _, err := RunDeploy(options); err != nil && strings.Contains(err.Error(), "--allow-dirty") {
		t.Errorf("--allow-dirty should get past the dirty tree check, got: %v", err)
	}
}

func TestDeployRefusesARemoteDestinationForNow(t *testing.T) {
	repository := newRepository(t)
	writeFile(t, repository, configFileName, `{
      "version": 1, "id": "dd000003", "name": "remote",
      "services": {"app": {"image": "busybox:latest"}}
    }`)
	commitFile(t, repository, "tracked.txt", "clean")

	exitCode, err := RunDeploy(DeployOptions{
		Context:     repository,
		Destination: "git:Projects",
		Environment: defaultEnvironmentName,
	})
	if err == nil {
		t.Fatal("a remote destination should be refused until ssh transport exists")
	}
	if exitCode != exitPreconditionNotMet {
		t.Errorf("exit code = %d, want %d", exitCode, exitPreconditionNotMet)
	}
	if !strings.Contains(err.Error(), "not implemented yet") {
		t.Errorf("the refusal should say so honestly, got: %v", err)
	}
}

// Deploying repeatedly is where retention actually gets exercised, so this walks
// four real deploys rather than asserting on the arithmetic alone.
func TestRepeatedDeploysRetainThreeReleasesAndPruneTheOldest(t *testing.T) {
	dockerAvailable(t)

	repository := newRepository(t)
	writeFile(t, repository, configFileName, `{
      "version": 1,
      "id": "dd000004",
      "name": "retained",
      "services": {
        "app": {
          "image": "busybox:latest",
          "command": ["sh", "-c", "sleep 300"],
          "stateful": false,
          "healthcheck": {"command": ["CMD", "true"], "interval": "1s", "retries": 5}
        }
      }
    }`)

	destination := t.TempDir()
	layout := NewLayout(destination, "dd000004")

	var commits []string
	for round := range 4 {
		commit := commitFile(t, repository, "round.txt", string(rune('a'+round)))
		commits = append(commits, ShortCommit(commit))

		t.Cleanup(func() {
			exec.Command("docker", "compose", "--project-name", ProjectName("dd000004", commit), "down").Run()
		})

		exitCode, err := RunDeploy(DeployOptions{
			Context:     repository,
			Destination: destination,
			Environment: defaultEnvironmentName,
		})
		if err != nil {
			t.Fatalf("deploy %d: %v", round+1, err)
		}
		if exitCode != exitOK {
			t.Fatalf("deploy %d exit code = %d", round+1, exitCode)
		}
	}

	state, err := ReadState(LocalRunner{}, layout)
	if err != nil {
		t.Fatalf("ReadState: %v", err)
	}

	newest, second, third, oldest := commits[3], commits[2], commits[1], commits[0]

	if state.Current != newest {
		t.Errorf("current = %q, want the newest commit %q", state.Current, newest)
	}
	if state.Previous != second {
		t.Errorf("previous = %q, want %q", state.Previous, second)
	}
	if want := []string{newest, second, third}; !slices.Equal(state.Releases, want) {
		t.Errorf("releases = %v, want %v", state.Releases, want)
	}

	onDisk, err := LocalRunner{}.ListDirectory(layout.Releases())
	if err != nil {
		t.Fatalf("listing releases: %v", err)
	}
	if len(onDisk) != 3 {
		t.Errorf("release directories on disk = %v, want three", onDisk)
	}
	if slices.Contains(onDisk, oldest) {
		t.Errorf("the oldest release %q should have been pruned from disk", oldest)
	}
}

func TestLayoutKeepsVolumesOutsideReleases(t *testing.T) {
	layout := NewLayout("/srv/projects", "a3f19c02")

	if got, want := layout.Release("9f4be0affff"), "/srv/projects/a3f19c02/releases/9f4be0a"; got != want {
		t.Errorf("Release() = %q, want %q", got, want)
	}
	if !strings.HasPrefix(layout.Release("9f4be0a"), layout.Releases()) {
		t.Error("every release must live under releases/, which is what the pruner is allowed to touch")
	}
}
