package main

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestDestroyFenceRefusesAnythingOutsideTheDestination(t *testing.T) {
	const destination = "/srv/projects"

	refused := []string{
		"..",
		"../..",
		"../../etc",
		"/etc",
		"/",
		"",
		".",
		"a3f19c02/..",
		"not-a-deploy-id",
		"A3F19C02",
		"a3f19c0",
		"a3f19c022",
		"; rm -rf /",
		"a3f19c02; rm -rf /",
		"*",
	}

	for _, projectID := range refused {
		t.Run(projectID, func(t *testing.T) {
			if resolved, err := projectPathWithin(destination, projectID); err == nil {
				t.Errorf("the fence let %q through as %q", projectID, resolved)
			}
		})
	}

	allowed, err := projectPathWithin(destination, "a3f19c02")
	if err != nil {
		t.Fatalf("a real deploy id should be allowed: %v", err)
	}
	if allowed != destination+"/a3f19c02" {
		t.Errorf("resolved to %q", allowed)
	}
}

func TestConfirmationRequiresTheProjectNameTypedBack(t *testing.T) {
	plan := DestroyPlan{ProjectName: "lectern", ProjectID: "a3f19c02"}

	cases := []struct {
		name    string
		typed   string
		allowed bool
	}{
		{"the name exactly", "lectern\n", true},
		{"the name with whitespace around it", "  lectern  \n", true},
		{"the name with no newline", "lectern", true},
		// a yes or no gets answered reflexively, which is the whole reason this
		// asks for something that has to be read first
		{"yes", "yes\n", false},
		{"y", "y\n", false},
		{"empty", "\n", false},
		{"nothing at all", "", false},
		{"the project id instead of the name", "a3f19c02\n", false},
		{"nearly the name", "lecturn\n", false},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			err := ConfirmDestroy(plan, strings.NewReader(testCase.typed), io.Discard)

			if testCase.allowed && err != nil {
				t.Errorf("typing %q should have confirmed, got: %v", testCase.typed, err)
			}
			if !testCase.allowed {
				if err == nil {
					t.Errorf("typing %q should not have confirmed", testCase.typed)
				} else if !strings.Contains(err.Error(), "nothing was destroyed") {
					t.Errorf("the refusal should say nothing happened, got: %v", err)
				}
			}
		})
	}
}

// A destination holds many projects, and the one thing destroy must never do is
// reach a sibling.
func TestDestroyRemovesOneProjectAndNothingBesideIt(t *testing.T) {
	destination := t.TempDir()
	runner := LocalRunner{}

	// two projects, one of which is not being destroyed
	for _, projectID := range []string{"aaaa1111", "bbbb2222"} {
		layout := NewLayout(destination, projectID)
		for _, directory := range []string{
			layout.Releases() + "/9f4be0a",
			layout.EnvDirectory(),
			filepath.Dir(layout.SharedComposeFile()),
		} {
			if err := os.MkdirAll(directory, 0o755); err != nil {
				t.Fatalf("creating %s: %v", directory, err)
			}
		}
		writeFile(t, layout.EnvDirectory(), ".env.production", "SECRET=keep me")
		writeFile(t, layout.Releases()+"/9f4be0a", "marker.txt", projectID)
		if err := WriteState(runner, layout, State{Name: projectID, Current: "9f4be0a"}); err != nil {
			t.Fatalf("writing state: %v", err)
		}
	}

	// something else entirely, sitting beside them
	unrelated := filepath.Join(destination, "AnglophoneEast")
	if err := os.MkdirAll(unrelated, 0o755); err != nil {
		t.Fatalf("creating unrelated: %v", err)
	}
	writeFile(t, unrelated, "important.txt", "not a deploy project at all")

	target := NewLayout(destination, "aaaa1111")
	plan := DestroyPlan{
		ProjectName:   "aaaa1111",
		ProjectID:     "aaaa1111",
		Root:          target.Root,
		Releases:      []string{"9f4be0a"},
		SharedProject: SharedProjectName("aaaa1111"),
		Network:       NetworkName("aaaa1111"),
	}

	if err := ExecuteDestroy(runner, target, plan); err != nil {
		t.Fatalf("ExecuteDestroy: %v", err)
	}

	// the target lost its releases, its shared stack, and its state
	for _, gone := range []string{target.Releases(), filepath.Dir(target.SharedComposeFile()), target.StateFile()} {
		if _, err := os.Stat(gone); err == nil {
			t.Errorf("%s should have been removed", gone)
		}
	}

	// the sibling is completely untouched
	sibling := NewLayout(destination, "bbbb2222")
	for _, kept := range []string{
		sibling.Releases() + "/9f4be0a/marker.txt",
		sibling.EnvDirectory() + "/.env.production",
		sibling.StateFile(),
	} {
		if _, err := os.Stat(kept); err != nil {
			t.Errorf("a sibling project lost %s: %v", kept, err)
		}
	}

	// and so is everything that was never a deploy project
	if contents, err := os.ReadFile(filepath.Join(unrelated, "important.txt")); err != nil {
		t.Errorf("destroy reached outside the project: %v", err)
	} else if !strings.Contains(string(contents), "not a deploy project") {
		t.Errorf("unrelated file was changed, got %q", contents)
	}
}

func TestWithoutVolumesTheDataAndSecretsSurvive(t *testing.T) {
	destination := t.TempDir()
	runner := LocalRunner{}
	layout := NewLayout(destination, "aaaa1111")

	for _, directory := range []string{
		layout.Releases() + "/9f4be0a",
		layout.EnvDirectory(),
		filepath.Join(layout.Root, "volumes"),
	} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatalf("creating %s: %v", directory, err)
		}
	}
	writeFile(t, layout.EnvDirectory(), ".env.production", "SECRET=keep me")
	writeFile(t, filepath.Join(layout.Root, "volumes"), "database", "precious")

	plan := DestroyPlan{
		ProjectName: "aaaa1111", ProjectID: "aaaa1111", Root: layout.Root,
		Releases: []string{"9f4be0a"}, SharedProject: SharedProjectName("aaaa1111"),
		RemoveVolumes: false,
	}
	if err := ExecuteDestroy(runner, layout, plan); err != nil {
		t.Fatalf("ExecuteDestroy: %v", err)
	}

	// the only things that cannot be rebuilt from the repository are the ones
	// that stay
	for _, kept := range []string{
		layout.EnvDirectory() + "/.env.production",
		filepath.Join(layout.Root, "volumes", "database"),
	} {
		if _, err := os.Stat(kept); err != nil {
			t.Errorf("%s should survive a destroy without --volumes: %v", kept, err)
		}
	}
	if _, err := os.Stat(layout.Releases()); err == nil {
		t.Error("releases are rebuildable and should have gone")
	}
}

func TestWithVolumesEverythingGoes(t *testing.T) {
	destination := t.TempDir()
	runner := LocalRunner{}
	layout := NewLayout(destination, "aaaa1111")

	for _, directory := range []string{layout.Releases() + "/9f4be0a", layout.EnvDirectory()} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatalf("creating %s: %v", directory, err)
		}
	}
	writeFile(t, layout.EnvDirectory(), ".env.production", "SECRET=goes too")

	sibling := NewLayout(destination, "bbbb2222")
	if err := os.MkdirAll(sibling.EnvDirectory(), 0o755); err != nil {
		t.Fatalf("creating sibling: %v", err)
	}
	writeFile(t, sibling.EnvDirectory(), ".env.production", "SECRET=still here")

	plan := DestroyPlan{
		ProjectName: "aaaa1111", ProjectID: "aaaa1111", Root: layout.Root,
		Releases: []string{"9f4be0a"}, SharedProject: SharedProjectName("aaaa1111"),
		RemoveVolumes: true,
	}
	if err := ExecuteDestroy(runner, layout, plan); err != nil {
		t.Fatalf("ExecuteDestroy: %v", err)
	}

	if _, err := os.Stat(layout.Root); err == nil {
		t.Error("--volumes removes the whole project tree")
	}
	// even then, only this project's tree
	if _, err := os.Stat(sibling.EnvDirectory() + "/.env.production"); err != nil {
		t.Errorf("--volumes must still not reach a sibling: %v", err)
	}
}

func TestPlanCoversReleasesFromStateAndFromDisk(t *testing.T) {
	resolved := loadAndResolve(t, `{
      "version": 1, "id": "a3f19c02", "name": "lectern",
      "services": {"pg": {"image": "postgres:17", "stateful": true, "volumes": ["pgdata:/data"]}}
    }`, defaultEnvironmentName)

	layout := NewLayout("/srv/projects", "a3f19c02")
	state := State{Current: "ccccccc", Releases: []string{"ccccccc", "bbbbbbb"}}

	// a release on disk that state has forgotten, which is what a half tidied
	// project looks like and exactly what would otherwise be left running
	onDisk := []string{"ccccccc", "bbbbbbb", "aaaaaaa"}

	plan, err := PlanDestroy(resolved, layout, "/srv/projects", state, onDisk, false)
	if err != nil {
		t.Fatalf("PlanDestroy: %v", err)
	}

	for _, release := range []string{"aaaaaaa", "bbbbbbb", "ccccccc"} {
		if !slices.Contains(plan.Releases, release) {
			t.Errorf("plan should stop release %q, got %v", release, plan.Releases)
		}
	}
	if want := VolumeName("a3f19c02", "pgdata"); !slices.Contains(plan.Volumes, want) {
		t.Errorf("plan should know about volume %q, got %v", want, plan.Volumes)
	}
	if plan.Root != "/srv/projects/a3f19c02" {
		t.Errorf("root = %q", plan.Root)
	}
}

func TestPlanRefusesAProjectIdThatIsNotOne(t *testing.T) {
	resolved := ResolvedProject{ID: "../../etc", Name: "sneaky"}

	if _, err := PlanDestroy(resolved, Layout{}, "/srv/projects", State{}, nil, true); err == nil {
		t.Fatal("planning a destroy for a bogus id should be refused before anything runs")
	}
}

func TestDestroyDescribesBeforeItAsks(t *testing.T) {
	plan := DestroyPlan{
		ProjectName: "lectern", ProjectID: "a3f19c02", Root: "/srv/projects/a3f19c02",
		Releases: []string{"9f4be0a"}, SharedProject: "deploy-a3f19c02-shared",
		Network: "deploy-a3f19c02-net", Volumes: []string{"deploy-a3f19c02-pgdata"},
		KeptDirectories: []string{"/srv/projects/a3f19c02/env"},
	}

	var kept strings.Builder
	plan.Describe(&kept)

	if !strings.Contains(kept.String(), "keeping") || !strings.Contains(kept.String(), "--volumes") {
		t.Errorf("the safe form should say what survives and how to change that, got:\n%s", kept.String())
	}

	plan.RemoveVolumes = true
	var everything strings.Builder
	plan.Describe(&everything)

	if !strings.Contains(everything.String(), "DELETING") {
		t.Errorf("the destructive form should be unmistakable, got:\n%s", everything.String())
	}
	if !strings.Contains(everything.String(), "none of it comes back") {
		t.Errorf("the destructive form should say it is final, got:\n%s", everything.String())
	}
}

func TestDestroyAgainstARealDeploy(t *testing.T) {
	dockerAvailable(t)

	repository := newRepository(t)
	writeFile(t, repository, configFileName, `{
      "version": 1,
      "id": "dd000013",
      "name": "doomed",
      "services": {
        "store": {
          "image": "busybox:latest",
          "stateful": true,
          "command": ["sh", "-c", "sleep 300"],
          "volumes": ["data:/data"],
          "healthcheck": {"command": ["CMD", "true"], "interval": "1s", "retries": 5}
        },
        "app": {
          "build": {"from": "busybox:latest", "start": "sleep 300"},
          "stateful": false,
          "healthcheck": {"command": ["CMD", "true"], "interval": "1s", "retries": 5}
        }
      }
    }`)
	commit := commitFile(t, repository, "one.txt", "x")

	destination := t.TempDir()
	t.Cleanup(func() {
		exec.Command("docker", "compose", "--project-name", ProjectName("dd000013", commit), "down").Run()
		exec.Command("docker", "compose", "--project-name", SharedProjectName("dd000013"), "down").Run()
		exec.Command("docker", "volume", "rm", "-f", VolumeName("dd000013", "data")).Run()
		exec.Command("docker", "network", "rm", NetworkName("dd000013")).Run()
		exec.Command("docker", "image", "rm", "-f", ImageTag("dd000013", "app", commit)).Run()
	})

	options := DeployOptions{
		Context: repository, Destination: destination, Environment: defaultEnvironmentName,
	}
	if _, err := RunDeploy(options); err != nil {
		t.Fatalf("RunDeploy: %v", err)
	}

	// a wrong confirmation must leave everything exactly as it was
	if _, err := RunDestroy(options, false, strings.NewReader("yes\n")); err == nil {
		t.Fatal("a wrong confirmation should abort")
	}
	running, _ := exec.Command("docker", "ps", "--format", "{{.Names}}").Output()
	if !strings.Contains(string(running), ProjectName("dd000013", commit)) {
		t.Fatal("an aborted destroy stopped containers anyway")
	}

	if _, err := RunDestroy(options, false, strings.NewReader("doomed\n")); err != nil {
		t.Fatalf("RunDestroy: %v", err)
	}

	running, err := exec.Command("docker", "ps", "--all", "--format", "{{.Names}}").Output()
	if err != nil {
		t.Fatalf("listing containers: %v", err)
	}
	for _, project := range []string{ProjectName("dd000013", commit), SharedProjectName("dd000013")} {
		if strings.Contains(string(running), project) {
			t.Errorf("containers from %s survived the destroy", project)
		}
	}

	images, _ := exec.Command("docker", "images", "--format", "{{.Repository}}").Output()
	if strings.Contains(string(images), "deploy-dd000013/") {
		t.Error("the project's images should have been removed")
	}

	// without --volumes the data is still there, which is the point of the flag
	volumes, _ := exec.Command("docker", "volume", "ls", "--format", "{{.Name}}").Output()
	if !strings.Contains(string(volumes), VolumeName("dd000013", "data")) {
		t.Error("the volume should survive a destroy without --volumes")
	}
}
