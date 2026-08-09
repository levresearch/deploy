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

func decodeCompose(t *testing.T, rendered []byte) ComposeProject {
	t.Helper()

	var project ComposeProject
	if err := json.Unmarshal(rendered, &project); err != nil {
		t.Fatalf("rendered compose is not valid json: %v", err)
	}

	return project
}

const splitConfig = `{
  "version": 1,
  "id": "a3f19c02",
  "name": "lectern",
  "services": {
    "web": {
      "build": {"dockerfile": "Dockerfile"},
      "stateful": false,
      "dependsOn": {"pg": "healthy", "worker": "started"}
    },
    "worker": {"build": {"dockerfile": "Dockerfile.worker"}, "stateful": false},
    "pg": {
      "image": "postgres:17",
      "stateful": true,
      "volumes": ["pgdata:/var/lib/postgresql/data", "./conf:/etc/conf"]
    }
  }
}`

func TestStatefulGoesToSharedAndStatelessToTheRelease(t *testing.T) {
	resolved := loadAndResolve(t, splitConfig, defaultEnvironmentName)

	sharedRaw, err := RenderShared(resolved)
	if err != nil {
		t.Fatalf("RenderShared: %v", err)
	}
	releaseRaw, err := RenderRelease(resolved, "9f4be0affff")
	if err != nil {
		t.Fatalf("RenderRelease: %v", err)
	}

	shared, release := decodeCompose(t, sharedRaw), decodeCompose(t, releaseRaw)

	if _, found := shared.Services["pg"]; !found {
		t.Error("a stateful service belongs in the shared stack")
	}
	for _, stateless := range []string{"web", "worker"} {
		if _, found := shared.Services[stateless]; found {
			t.Errorf("%s is stateless and must not be in the shared stack", stateless)
		}
		if _, found := release.Services[stateless]; !found {
			t.Errorf("%s is stateless and belongs in the release stack", stateless)
		}
	}
	if _, found := release.Services["pg"]; found {
		t.Error("a stateful service must never be duplicated into a release stack")
	}

	// the shared project name is what keeps volumes attached, so it must never
	// carry a commit
	if strings.Contains(shared.Name, "9f4be0a") {
		t.Errorf("the shared project name %q must not contain a commit", shared.Name)
	}
}

func TestOnlyTheSharedStackDeclaresVolumesAndTheyAreExternal(t *testing.T) {
	resolved := loadAndResolve(t, splitConfig, defaultEnvironmentName)

	sharedRaw, _ := RenderShared(resolved)
	releaseRaw, _ := RenderRelease(resolved, "9f4be0affff")
	shared, release := decodeCompose(t, sharedRaw), decodeCompose(t, releaseRaw)

	if len(release.Volumes) != 0 {
		t.Errorf("a release stack declares no volumes of its own, got %v", release.Volumes)
	}

	qualified := VolumeName("a3f19c02", "pgdata")
	declaration, found := shared.Volumes[qualified]
	if !found {
		t.Fatalf("expected the shared stack to declare %s, got %v", qualified, shared.Volumes)
	}

	// external is the whole point. without it compose prefixes the volume with
	// the project name, and a project name that changed would silently start an
	// empty database
	asMap, ok := declaration.(map[string]any)
	if !ok || asMap["external"] != true {
		t.Errorf("%s must be declared external, got %v", qualified, declaration)
	}
}

func TestNamedVolumesAreQualifiedAndBindMountsAreNot(t *testing.T) {
	resolved := loadAndResolve(t, splitConfig, defaultEnvironmentName)

	sharedRaw, _ := RenderShared(resolved)
	shared := decodeCompose(t, sharedRaw)

	var service struct {
		Volumes []string `json:"volumes"`
	}
	if err := json.Unmarshal(shared.Services["pg"], &service); err != nil {
		t.Fatalf("reading pg: %v", err)
	}

	want := []string{
		VolumeName("a3f19c02", "pgdata") + ":/var/lib/postgresql/data",
		"./conf:/etc/conf",
	}
	if !slices.Equal(service.Volumes, want) {
		t.Errorf("volumes = %v, want %v", service.Volumes, want)
	}

	if got := VolumesFor(resolved); !slices.Equal(got, []string{VolumeName("a3f19c02", "pgdata")}) {
		t.Errorf("VolumesFor = %v, want just the named volume", got)
	}
}

func TestBothStacksShareOneExternalNetwork(t *testing.T) {
	resolved := loadAndResolve(t, splitConfig, defaultEnvironmentName)

	sharedRaw, _ := RenderShared(resolved)
	releaseRaw, _ := RenderRelease(resolved, "9f4be0affff")

	for label, rendered := range map[string][]byte{"shared": sharedRaw, "release": releaseRaw} {
		project := decodeCompose(t, rendered)

		network, ok := project.Networks["default"].(map[string]any)
		if !ok {
			t.Fatalf("%s stack declares no default network", label)
		}
		if network["name"] != NetworkName("a3f19c02") {
			t.Errorf("%s network name = %v, want %s", label, network["name"], NetworkName("a3f19c02"))
		}
		if network["external"] != true {
			t.Errorf("%s network must be external so neither stack owns it", label)
		}
	}
}

func TestDependsOnKeepsSiblingsAndDropsCrossProject(t *testing.T) {
	resolved := loadAndResolve(t, splitConfig, defaultEnvironmentName)

	releaseRaw, _ := RenderRelease(resolved, "9f4be0affff")
	release := decodeCompose(t, releaseRaw)

	var web struct {
		DependsOn map[string]struct {
			Condition string `json:"condition"`
		} `json:"depends_on"`
	}
	if err := json.Unmarshal(release.Services["web"], &web); err != nil {
		t.Fatalf("reading web: %v", err)
	}

	if condition := web.DependsOn["worker"].Condition; condition != "service_started" {
		t.Errorf("a sibling dependency should survive, got %q", condition)
	}
	// compose cannot wait on a service another project owns, and the ordering is
	// already guaranteed by the shared stack coming up first
	if _, kept := web.DependsOn["pg"]; kept {
		t.Error("a dependency on a service in the shared stack must be dropped rather than rendered")
	}
}

func TestAllThreeDependsOnConditionsTranslate(t *testing.T) {
	const contents = `{
      "version": 1, "id": "a3f19c02", "name": "x",
      "services": {
        "a": {"image": "busybox"},
        "b": {"image": "busybox"},
        "c": {"image": "busybox"},
        "web": {"image": "nginx", "dependsOn": {"a": "healthy", "b": "completed", "c": "started"}}
      }
    }`

	releaseRaw, _ := RenderRelease(loadAndResolve(t, contents, defaultEnvironmentName), "9f4be0a")
	release := decodeCompose(t, releaseRaw)

	var web struct {
		DependsOn map[string]struct {
			Condition string `json:"condition"`
		} `json:"depends_on"`
	}
	if err := json.Unmarshal(release.Services["web"], &web); err != nil {
		t.Fatalf("reading web: %v", err)
	}

	want := map[string]string{
		"a": "service_healthy",
		"b": "service_completed_successfully",
		"c": "service_started",
	}
	for service, condition := range want {
		if got := web.DependsOn[service].Condition; got != condition {
			t.Errorf("depends_on %s = %q, want %q", service, got, condition)
		}
	}
}

func TestSharedStackRendersIdenticallyTwiceSoTheDiffIsANoOp(t *testing.T) {
	resolved := loadAndResolve(t, splitConfig, defaultEnvironmentName)

	first, err := RenderShared(resolved)
	if err != nil {
		t.Fatalf("RenderShared: %v", err)
	}
	for range 5 {
		again, err := RenderShared(resolved)
		if err != nil {
			t.Fatalf("RenderShared: %v", err)
		}
		if string(again) != string(first) {
			t.Fatal("rendering the same config twice must produce identical bytes, or every deploy would look like a change and recreate the database")
		}
	}
}

func TestAChangedSharedStackRendersDifferently(t *testing.T) {
	before := loadAndResolve(t, splitConfig, defaultEnvironmentName)
	after := loadAndResolve(t, strings.Replace(splitConfig, "postgres:17", "postgres:18", 1), defaultEnvironmentName)

	beforeRaw, _ := RenderShared(before)
	afterRaw, _ := RenderShared(after)

	if string(beforeRaw) == string(afterRaw) {
		t.Fatal("changing a stateful service's image must show up as a change, or it would silently never take effect")
	}
}

// This is the one that catches the volume namespacing bug. Two deploys produce
// two differently named compose projects, and if the volume went with the
// project name the second deploy would come up against an empty database.
func TestDataSurvivesASecondDeploy(t *testing.T) {
	dockerAvailable(t)

	repository := newRepository(t)
	writeFile(t, repository, configFileName, `{
      "version": 1,
      "id": "dd000005",
      "name": "survives",
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
      }
    }`)

	destination := t.TempDir()
	sharedProject := SharedProjectName("dd000005")

	t.Cleanup(func() {
		exec.Command("docker", "compose", "--project-name", sharedProject, "down").Run()
		exec.Command("docker", "volume", "rm", "-f", VolumeName("dd000005", "data")).Run()
		exec.Command("docker", "network", "rm", NetworkName("dd000005")).Run()
	})

	deployOnce := func(round int) string {
		commit := commitFile(t, repository, "round.txt", string(rune('a'+round)))
		t.Cleanup(func() {
			exec.Command("docker", "compose", "--project-name", ProjectName("dd000005", commit), "down").Run()
		})

		if _, err := RunDeploy(DeployOptions{
			Context:     repository,
			Destination: destination,
			Environment: defaultEnvironmentName,
		}); err != nil {
			t.Fatalf("deploy %d: %v", round+1, err)
		}

		return commit
	}

	deployOnce(0)

	// write a row into the volume, the way a real database would
	write := exec.Command(
		"docker", "compose", "--project-name", sharedProject,
		"exec", "-T", "store", "sh", "-c", "echo 'the precious row' > /data/rows.txt",
	)
	if output, err := write.CombinedOutput(); err != nil {
		t.Fatalf("writing into the volume: %v\n%s", err, output)
	}

	secondCommit := deployOnce(1)

	read := exec.Command(
		"docker", "compose", "--project-name", sharedProject,
		"exec", "-T", "store", "cat", "/data/rows.txt",
	)
	stored, err := read.CombinedOutput()
	if err != nil {
		t.Fatalf("reading back from the volume after a second deploy: %v\n%s", err, stored)
	}
	if !strings.Contains(string(stored), "the precious row") {
		t.Fatalf("the volume lost its contents across a deploy, got %q", stored)
	}

	// and the stateful service was not duplicated into the release stack
	containers, err := exec.Command(
		"docker", "compose", "--project-name", ProjectName("dd000005", secondCommit),
		"ps", "--format", "{{.Service}}",
	).Output()
	if err != nil {
		t.Fatalf("listing release containers: %v", err)
	}
	if strings.Contains(string(containers), "store") {
		t.Error("the stateful service must not run in the per-commit stack")
	}

	// the shared compose file lives outside releases/, so pruning cannot reach it
	layout := NewLayout(destination, "dd000005")
	if _, err := os.Stat(layout.SharedComposeFile()); err != nil {
		t.Errorf("the shared compose file should be on disk: %v", err)
	}
	if strings.HasPrefix(layout.SharedComposeFile(), layout.Releases()) {
		t.Error("the shared stack must not live under releases/, which is what gets pruned")
	}
}

// A pruned release must not leave containers running. Removing only the
// directory would strand them, and the compose file naming them goes with it.
func TestPruningStopsTheContainersOfThePrunedRelease(t *testing.T) {
	dockerAvailable(t)

	repository := newRepository(t)
	writeFile(t, repository, configFileName, `{
      "version": 1,
      "id": "dd000006",
      "name": "pruned",
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

	var commits []string
	for round := range 3 {
		commit := commitFile(t, repository, "round.txt", string(rune('a'+round)))
		commits = append(commits, commit)
		t.Cleanup(func() {
			exec.Command("docker", "compose", "--project-name", ProjectName("dd000006", commit), "down").Run()
		})
		exec.Command("docker", "network", "rm", NetworkName("dd000006")).Run()

		if _, err := RunDeploy(DeployOptions{
			Context:     repository,
			Destination: destination,
			Environment: defaultEnvironmentName,
		}); err != nil {
			t.Fatalf("deploy %d: %v", round+1, err)
		}
	}

	// retention 1 keeps current and previous, so the first release was pruned
	pruned := ProjectName("dd000006", commits[0])
	running, err := exec.Command("docker", "ps", "--format", "{{.Names}}").Output()
	if err != nil {
		t.Fatalf("listing containers: %v", err)
	}
	if strings.Contains(string(running), pruned) {
		t.Errorf("containers from the pruned release %s are still running", pruned)
	}

	// and the two retained releases are untouched
	for _, kept := range commits[1:] {
		if !strings.Contains(string(running), ProjectName("dd000006", kept)) {
			t.Errorf("release %s should still be running", ShortCommit(kept))
		}
	}
}

func TestSharedStackIsSkippedWhenNothingIsStateful(t *testing.T) {
	const contents = `{
      "version": 1, "id": "a3f19c02", "name": "x",
      "services": {"web": {"image": "nginx", "stateful": false}}
    }`

	resolved := loadAndResolve(t, contents, defaultEnvironmentName)
	stateful, stateless := SplitServices(resolved.Services)

	if len(stateful) != 0 {
		t.Errorf("nothing here is stateful, got %v", stateful)
	}
	if len(stateless) != 1 {
		t.Errorf("web is stateless, got %v", stateless)
	}
}

func TestSharedComposeFileSitsOutsideReleases(t *testing.T) {
	layout := NewLayout("/srv/projects", "a3f19c02")

	if strings.HasPrefix(layout.SharedComposeFile(), layout.Releases()+"/") {
		t.Errorf("SharedComposeFile() = %q, which pruning would eventually delete", layout.SharedComposeFile())
	}
	if want := filepath.Join(layout.Root, "shared", composeFileName); layout.SharedComposeFile() != want {
		t.Errorf("SharedComposeFile() = %q, want %q", layout.SharedComposeFile(), want)
	}
}
