package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// useSSHAliases points alias-one and alias-two at the loopback sshd through an
// ssh wrapper first on PATH, so every ssh deploy runs resolves them.
func useSSHAliases(t *testing.T) (*SSHRunner, string) {
	t.Helper()
	runner, directory, port := newSSHTestRunner(t)
	ssh, err := exec.LookPath("ssh")
	if err != nil {
		t.Fatal(err)
	}
	user, _, _ := strings.Cut(runner.host, "@")
	config := fmt.Sprintf("Host alias-one alias-two\n HostName 127.0.0.1\n Port %d\n User %s\n IdentityFile %s\n UserKnownHostsFile %s\n", port, user, filepath.Join(directory, "client"), filepath.Join(directory, "known_hosts"))
	writeFile(t, directory, "ssh_config", config)
	bin := filepath.Join(directory, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	wrapper := "#!/bin/sh\nexec " + ShellQuote(ssh) + " -F " + ShellQuote(filepath.Join(directory, "ssh_config")) + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte(wrapper), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return runner, directory
}

func TestObservationOverRealSSHLeavesMissingPathsUntouched(t *testing.T) {
	_, directory := useSSHAliases(t)
	knownBefore, err := os.ReadFile(filepath.Join(directory, "known_hosts"))
	if err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(directory, "real")
	if err := os.Mkdir(parent, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "alias")
	if err := os.Symlink(parent, link); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(link, "it's missing", "nested")
	project := topologyProject(t, topologyFixture)
	project.Destinations = map[string]string{"app": "alias-one:" + missing, "data": "alias-two:" + missing}
	project.Network = nil
	observations, err := observePlanningProject(context.Background(), project, observeDestination)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planObservedTopology(project, "production", "unverified", observations)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Network.Mode != "docker-only" || observations["app"].HostIdentity != observations["data"].HostIdentity {
		t.Fatal("real aliases were not grouped")
	}
	if observations["app"].CanonicalPath != filepath.Join(parent, "it's missing", "nested") {
		t.Fatal(observations["app"].CanonicalPath)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(home, missing)
	if err != nil {
		t.Fatal(err)
	}
	remote, err := observeDestination(context.Background(), Destination{Host: "alias-one", Path: relative})
	if err != nil || remote.CanonicalPath != observations["app"].CanonicalPath {
		t.Fatalf("home-relative observation: %+v %v", remote, err)
	}
	repo := newRepository(t)
	configuration, err := json.Marshal(project)
	if err != nil {
		t.Fatal(err)
	}
	commit := commitFile(t, repo, configFileName, string(configuration))
	output := captureStdout(t, func() {
		if code := run([]string{"plan", "--observe", "-C", repo}); code != exitOK {
			t.Fatalf("observed plan exit: %d", code)
		}
	})
	var cliPlan TopologyPlan
	if err := json.Unmarshal([]byte(output), &cliPlan); err != nil {
		t.Fatal(err)
	}
	if cliPlan.Commit != commit || cliPlan.Network.Mode != "docker-only" || cliPlan.Observations["app"].CanonicalPath != observations["app"].CanonicalPath {
		t.Fatal("CLI did not use destination observations")
	}
	if _, err := os.Stat(filepath.Join(parent, "it's missing")); !os.IsNotExist(err) {
		t.Fatal("probe created missing path")
	}
	if cliPlan.Destinations[0].RecordedState != "none" || cliPlan.Services[0].PreviousRelease != "none" {
		t.Fatalf("a missing destination reported releases: %+v", cliPlan)
	}

	deployed := filepath.Join(link, "deployed")
	writeFile(t, filepath.Join(parent, "deployed", "1234abcd"), stateFileName, `{"version":1,"current":"bbbbbbb","previous":"aaaaaaa","environment":"production","releases":["bbbbbbb","aaaaaaa"]}`)
	project.Destinations = map[string]string{"app": "alias-one:" + deployed, "data": "alias-two:" + missing}
	configuration, err = json.Marshal(project)
	if err != nil {
		t.Fatal(err)
	}
	commitFile(t, repo, configFileName, string(configuration))
	output = captureStdout(t, func() {
		if code := run([]string{"plan", "--observe", "-C", repo}); code != exitOK {
			t.Fatalf("observed plan exit: %d", code)
		}
	})
	var recordedPlan TopologyPlan
	if err := json.Unmarshal([]byte(output), &recordedPlan); err != nil {
		t.Fatal(err)
	}
	if recordedPlan.Destinations[0].RecordedState != "version 1, adopted in memory" || recordedPlan.Services[0].Name != "api" || recordedPlan.Services[0].PreviousRelease != "bbbbbbb" {
		t.Fatalf("the live release was not read over ssh: %+v", recordedPlan)
	}
	if entries, err := os.ReadDir(filepath.Join(parent, "deployed", "1234abcd")); err != nil || len(entries) != 1 {
		t.Fatalf("planning changed the deployment: %v %v", entries, err)
	}
	knownAfter, err := os.ReadFile(filepath.Join(directory, "known_hosts"))
	if err != nil {
		t.Fatal(err)
	}
	if string(knownBefore) != string(knownAfter) {
		t.Fatal("probe modified known hosts")
	}
	t.Setenv("DOCKER_HOST", "tcp://127.0.0.1:1")
	t.Setenv("DOCKER_CONTEXT", "must-not-use-this-context")
	local, err := observeDestination(context.Background(), Destination{Path: missing})
	if err != nil {
		t.Fatal(err)
	}
	if local.HostIdentity != observations["app"].HostIdentity {
		t.Fatal("local and ssh host differed")
	}
	writeFile(t, parent, "file", "not a directory")
	if _, err := observeDestination(context.Background(), Destination{Path: filepath.Join(parent, "file", "nested")}); err == nil {
		t.Fatal("accepted a non-directory ancestor")
	}
	if err := os.WriteFile(filepath.Join(directory, "known_hosts"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := observeDestination(context.Background(), Destination{Host: "alias-one", Path: missing}); err == nil {
		t.Fatal("accepted unknown SSH host key")
	}
	contents, _ := os.ReadFile(filepath.Join(directory, "known_hosts"))
	if len(contents) != 0 {
		t.Fatal("unknown key was added")
	}
}
