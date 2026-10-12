package main

import (
	"bytes"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func safetyDocker(t *testing.T, arguments ...string) string {
	t.Helper()
	command := exec.Command("docker", arguments...)
	var failure bytes.Buffer
	command.Stderr = &failure
	output, err := command.Output()
	if err != nil {
		t.Fatalf("docker %v: %v\n%s\n%s", arguments, err, output, failure.String())
	}
	return strings.TrimSpace(string(output))
}

func safetyDeploy(t *testing.T, options DeployOptions, id string) string {
	t.Helper()
	commit, err := ResolveCommit(options.Context, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		exec.Command("docker", "compose", "--project-name", ProjectName(id, commit), "down").Run()
	})
	if code, err := RunDeploy(options); err != nil || code != exitOK {
		t.Fatalf("deploy: %d %v", code, err)
	}
	return commit
}

func TestEnvironmentIsolationAndStatefulUpgrade(t *testing.T) {
	dockerAvailable(t)
	const id = "dd000031"
	repository := newRepository(t)
	destination := t.TempDir()
	config := fmt.Sprintf(`{
 "version":1,"id":%q,"name":"safety",
 "services":{
  "store":{"build":"Dockerfile","stateful":true,"env":[".env.test"],"volumes":["data:/data"],"healthcheck":{"command":["CMD","true"],"interval":"1s"}},
  "app":{"image":%q,"command":["sleep","300"],"healthcheck":{"command":["CMD","true"],"interval":"1s"}}
 }} `, id, networkProbeImage)
	writeFile(t, repository, configFileName, config)
	writeFile(t, repository, "Dockerfile", "FROM "+networkProbeImage+"\nCOPY version.txt /version\nCMD [\"sleep\",\"300\"]\n")
	first := commitFile(t, repository, "version.txt", "one")
	production := DeployOptions{Context: repository, Destination: destination, Environment: "production"}
	staging := production
	staging.Environment = "staging"
	for _, environment := range []string{"production", "staging"} {
		runtimeID := id
		if environment != "production" {
			runtimeID += "-" + environment
		}
		t.Cleanup(func() { removeProjectImages(LocalRunner{}, runtimeID) })
		cleanUpProject(t, runtimeID)
		options := production
		options.Environment = environment
		secret := filepath.Join(t.TempDir(), ".env.test")
		writeFile(t, filepath.Dir(secret), ".env.test", "TEST_ENV="+environment+"\n")
		if _, err := RunEnvPush(options, secret); err != nil {
			t.Fatal(err)
		}
		safetyDeploy(t, options, runtimeID)
		safetyDocker(t, "compose", "--project-name", SharedProjectName(runtimeID), "exec", "-T", "store", "sh", "-c", "printenv TEST_ENV > /data/marker")
	}
	read := func(runtimeID, file string) string {
		return safetyDocker(t, "compose", "--project-name", SharedProjectName(runtimeID), "exec", "-T", "store", "cat", file)
	}
	if read(id, "/data/marker") != "production" || read(id+"-staging", "/data/marker") != "staging" {
		t.Fatal("environments shared volume data or secrets")
	}
	second := commitFile(t, repository, "version.txt", "two")
	safetyDeploy(t, staging, id+"-staging")
	if read(id+"-staging", "/version") != "two" || read(id+"-staging", "/data/marker") != "staging" || read(id, "/version") != "one" {
		t.Fatal("stateful upgrade lost data or changed production")
	}
	if _, err := RunRollback(staging, ""); err != nil {
		t.Fatal(err)
	}
	stageState, _ := ReadState(LocalRunner{}, NewLayout(destination, id+"-staging"))
	prodState, _ := ReadState(LocalRunner{}, NewLayout(destination, id))
	if stageState.Current != ShortCommit(first) || stageState.Previous != ShortCommit(second) || prodState.Current != ShortCommit(first) {
		t.Fatal("rollback crossed environment state")
	}
	listed := captureStdout(t, func() {
		if _, err := RunList(production); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(listed, id+"-staging") {
		t.Fatal("list omitted staging")
	}
	if _, err := RunDestroy(staging, true, strings.NewReader("safety\n")); err != nil {
		t.Fatal(err)
	}
	if read(id, "/data/marker") != "production" {
		t.Fatal("destroying staging affected production")
	}
}

func TestReusedImagesSurvivePruningAndRollback(t *testing.T) {
	dockerAvailable(t)
	const id = "dd000032"
	repository := newAffectedRepository(t, id)
	t.Cleanup(func() { removeProjectImages(LocalRunner{}, id) })
	writeFile(t, repository, configFileName, strings.Replace(affectedConfig(id), `"name": "affected",`, `"name": "affected", "retention": 2,`, 1))
	for _, service := range []string{"app", "worker"} {
		writeFile(t, repository, "services/"+service+"/Dockerfile", strings.Replace(affectedDockerfile, "busybox:latest", networkProbeImage, 1))
	}
	destination := t.TempDir()
	options := DeployOptions{Context: repository, Destination: destination, Environment: "production", Affected: true}
	var first, previous, current string
	for round := range 4 {
		commitFile(t, repository, "services/app/version.txt", fmt.Sprint(round))
		previous = current
		current = safetyDeploy(t, options, id)
		if round == 0 {
			first = current
			safetyDocker(t, "tag", ImageTag(id, "worker", first), ImageTag(id+"-staging", "worker", first))
			t.Cleanup(func() { removeProjectImages(LocalRunner{}, id+"-staging") })
		}
	}
	for _, tag := range []string{ImageTag(id, "worker", current), ImageTag(id, "worker", previous), ImageTag(id+"-staging", "worker", first)} {
		imageID(t, tag)
	}
	if exec.Command("docker", "image", "inspect", ImageTag(id, "worker", first)).Run() == nil {
		t.Fatal("retired tag survived pruning")
	}
	if _, err := RunRollback(options, ""); err != nil {
		t.Fatal(err)
	}
	state, err := ReadState(LocalRunner{}, NewLayout(destination, id))
	if err != nil || state.Current != ShortCommit(previous) {
		t.Fatalf("rollback after pruning failed: %+v %v", state, err)
	}
	if _, err := RunDestroy(options, true, strings.NewReader("affected\n")); err != nil {
		t.Fatal(err)
	}
	imageID(t, ImageTag(id+"-staging", "worker", first))
}
