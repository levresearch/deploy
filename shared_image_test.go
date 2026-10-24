package main

import (
	"os/exec"
	"strings"
	"testing"
)

// a rebuild that changed nothing must name the same image, and one that did
// must not, whatever the image id did in between
func TestASharedImageIsNamedByWhatIsInIt(t *testing.T) {
	stateful := map[string]Service{
		"db":    {Build: []byte(`"db/Dockerfile"`)},
		"cache": {Image: "redis:7"},
	}
	tagFor := func(inspected string) (map[string]string, *scriptedRunner) {
		runner := newScriptedRunner()
		runner.responses["image inspect"] = inspected
		images, err := tagSharedImages(runner, "a3f19c02", stateful, "aaaaaaa")
		if err != nil {
			t.Fatal(err)
		}
		return images, runner
	}

	first, runner := tagFor(`{"Cmd":["postgres"]} {"Layers":["sha256:1"]} linux/arm64` + "\n")
	again, _ := tagFor(`{"Cmd":["postgres"]} {"Layers":["sha256:1"]} linux/arm64`)
	changed, _ := tagFor(`{"Cmd":["postgres"]} {"Layers":["sha256:2"]} linux/arm64`)

	if _, published := first["cache"]; published || !strings.HasPrefix(first["db"], "deploy-a3f19c02/db:content-") {
		t.Fatalf("named %v", first)
	}
	if again["db"] != first["db"] || changed["db"] == first["db"] {
		t.Fatalf("the same image named %s and %s, a changed one %s", first["db"], again["db"], changed["db"])
	}
	if tagged := "docker tag deploy-a3f19c02/db:aaaaaaa " + first["db"]; !strings.Contains(strings.Join(runner.ran, "\n"), tagged) {
		t.Fatalf("never ran %q, ran %v", tagged, runner.ran)
	}

	// moving an existing content tag onto the new build would change the image
	// id compose compares with the running container, and recreate it anyway
	kept := newScriptedRunner()
	kept.responses["image inspect"] = `{"Cmd":["postgres"]} {"Layers":["sha256:1"]} linux/arm64`
	kept.responses["image ls --quiet"] = "sha256:0ld\n"
	if images, err := tagSharedImages(kept, "a3f19c02", stateful, "aaaaaaa"); err != nil || images["db"] != first["db"] {
		t.Fatalf("named %v: %v", images, err)
	}
	if ran := strings.Join(kept.ran, "\n"); strings.Contains(ran, "docker tag") {
		t.Fatalf("an existing content tag was moved: %s", ran)
	}

	missing := newScriptedRunner()
	missing.failCommands = []string{"image inspect"}
	if _, err := tagSharedImages(missing, "a3f19c02", stateful, "aaaaaaa"); err == nil {
		t.Fatal("a built image that is not there was named anyway")
	}
}

func TestOnlyTheStatefulServicesThatChangedAreRecreated(t *testing.T) {
	previous := []byte(`{"name": "s", "services": {
	  "db": {"image": "deploy-x/db:content-1"},
	  "cache": {"image": "redis:7"},
	  "cloudflared-api": {"image": "cloudflare/cloudflared:1"}}}`)
	names := []string{"cache", "db"}

	cases := map[string]struct {
		rendered string
		want     string
	}{
		"a rebuilt database":          {`{"name": "s", "services": {"db": {"image": "deploy-x/db:content-2"}, "cache": {"image": "redis:7"}, "cloudflared-api": {"image": "cloudflare/cloudflared:1"}}}`, "db"},
		"only a tunnel":               {`{"name": "s", "services": {"db": {"image": "deploy-x/db:content-1"}, "cache": {"image": "redis:7"}, "cloudflared-api": {"image": "cloudflare/cloudflared:2"}}}`, ""},
		"formatting is not a change":  {`{"name":"s","services":{"db":{"image":"deploy-x/db:content-1"},"cache":{"image":"redis:7"}}}`, ""},
		"an unreadable previous file": {`not json`, "cache,db"},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			if got := strings.Join(changedServices(previous, []byte(testCase.rendered), names), ","); got != testCase.want {
				t.Fatalf("recreating %q, want %q", got, testCase.want)
			}
		})
	}
}

// the database is built, and deploys that change only the app leave it
// running; changing the database itself still recreates it. it is built on the
// destination, from releases/<commit>, because buildkit's provenance records
// that path and so gives every commit's build of it a new image id
func TestABuiltDatabaseOutlivesDeploysThatDoNotChangeIt(t *testing.T) {
	dockerAvailable(t)
	t.Parallel()
	const id = "fb0000d1"
	removeDeployment(t, id)
	repository := newRepository(t)
	destination := t.TempDir()

	writeFile(t, repository, "db/Dockerfile", "FROM busybox:latest\nCOPY db/version /version\nCMD [\"sh\", \"-c\", \"sleep 600\"]\n")
	writeFile(t, repository, "db/version", "one")
	writeFile(t, repository, "app/Dockerfile", "FROM busybox:latest\nCOPY app/version /version\n")
	writeFile(t, repository, "app/version", "one")
	commitFile(t, repository, configFileName, `{"version": 2, "id": "`+id+`", "name": "built database", "buildOnDestination": true,
	  "destinations": {"main": "`+destination+`"},
	  "services": {
	    "db": {"destination": "main", "build": "db/Dockerfile", "stateful": true, "volumes": ["data:/data"],
	      "healthcheck": {"command": ["CMD", "true"], "interval": "1s", "retries": 5}},
	    "app": {"destination": "main", "build": "app/Dockerfile", "command": ["sh", "-c", "sleep 600"], "dependsOn": {"db": "healthy"},
	      "healthcheck": {"command": ["CMD", "true"], "interval": "1s", "retries": 5}}
	  }}`)
	options := DeployOptions{Context: repository}
	database := func() string {
		t.Helper()
		output, err := exec.Command("docker", "compose", "--project-name", SharedProjectName(id), "ps", "-q", "db").Output()
		if err != nil || strings.TrimSpace(string(output)) == "" {
			t.Fatalf("finding the database: %v", err)
		}
		return strings.TrimSpace(string(output))
	}
	contentTags := func() []string {
		t.Helper()
		output, _ := exec.Command("docker", "images", "--filter", "reference="+ContentImageTag(id, "db", "*"), "--format", "{{.Tag}}").Output()
		return strings.Fields(string(output))
	}

	if code, err := RunDeploy(options); code != exitOK || err != nil {
		t.Fatalf("first deploy: %d %v", code, err)
	}
	first := database()

	commitFile(t, repository, "app/version", "two")
	if code, err := RunDeploy(options); code != exitOK || err != nil {
		t.Fatalf("app deploy: %d %v", code, err)
	}
	if got := database(); got != first {
		t.Fatalf("a deploy that only changed the app recreated the database: %s, then %s", first, got)
	}

	commitFile(t, repository, "db/version", "two")
	if code, err := RunDeploy(options); code != exitOK || err != nil {
		t.Fatalf("database deploy: %d %v", code, err)
	}
	if got := database(); got == first {
		t.Fatal("a changed database kept running the old image")
	}
	if tags := contentTags(); len(tags) != 1 {
		t.Fatalf("the content tags the shared stack no longer runs were kept: %v", tags)
	}
}
