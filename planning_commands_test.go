package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestPlanningCommandsAreOfflineAndDoNotDiscloseEnvironment(t *testing.T) {
	repo := newRepository(t)
	raw := strings.Replace(topologyFixture, `"image":"example/api"`, `"image":"example/api","environment":{"PRIVATE_VALUE":"sentinel-secret"},"env":[".env.missing"]`, 1)
	commit := commitFile(t, repo, configFileName, raw)
	before, err := os.ReadFile(filepath.Join(repo, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	marker := filepath.Join(bin, "called")
	for _, name := range []string{"ssh", "docker", "wg", "sudo"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\ntouch "+ShellQuote(marker)+"\nexit 97\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, command := range []string{"check", "plan"} {
		output := captureStdout(t, func() {
			if code := run([]string{command, "-C", repo}); code != exitOK {
				t.Fatalf("%s: %d", command, code)
			}
		})
		if strings.Contains(output, "sentinel-secret") {
			t.Fatal("printed environment contents")
		}
		var plan TopologyPlan
		if err := json.Unmarshal([]byte(output), &plan); err != nil {
			t.Fatal(err)
		}
		if command == "plan" && plan.Commit != commit {
			t.Fatalf("commit = %s", plan.Commit)
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("external tool invoked: %v", err)
	}
	for _, path := range []string{".deploy", ".gitignore"} {
		if _, err := os.Stat(filepath.Join(repo, path)); !os.IsNotExist(err) {
			t.Fatalf("created %s", path)
		}
	}
	after, err := os.ReadFile(filepath.Join(repo, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("planning modified git index")
	}
	code, err := RunDeploy(DeployOptions{Context: repo, Environment: "production", Destination: filepath.Join(repo, "must-not-exist")})
	if code != exitPreconditionNotMet || err == nil || !strings.Contains(err.Error(), "one destination for now") {
		t.Fatalf("%d %v", code, err)
	}
	if _, err := os.Stat(filepath.Join(repo, "must-not-exist")); !os.IsNotExist(err) {
		t.Fatal("version 2 execution touched destination")
	}
}

func TestMigrationPreservesConfigurationAndNeverWrites(t *testing.T) {
	raw := `{"version":1,"id":"1234abcd","name":"example","destination":"host:Projects/example","notify":{"discordWebhookFrom":"NOTICE"},"retention":7,"custom":{"preserve":true},"services":{"api":{"image":"example/api","restart":"unless-stopped","labels":{"Key":"A","key":"B"}}},"release":{"migrate":{"image":"example/migrate"}},"environments":{"production":{},"staging":{"services":{"api":{"env":[".env.staging"]},"worker":{"image":"example/worker"}},"release":{"migrate":{"command":"migrate"}}}}}`
	filename := writeConfig(t, raw)
	preview, err := previewMigration(filename, "")
	if err != nil {
		t.Fatal(err)
	}
	var converted map[string]json.RawMessage
	if err := json.Unmarshal(preview, &converted); err != nil {
		t.Fatal(err)
	}
	if _, exists := converted["destination"]; exists {
		t.Fatal("root destination retained")
	}
	var old map[string]json.RawMessage
	json.Unmarshal([]byte(raw), &old)
	for _, key := range []string{"notify", "retention", "custom"} {
		var a, b any
		json.Unmarshal(old[key], &a)
		json.Unmarshal(converted[key], &b)
		if !reflect.DeepEqual(a, b) {
			t.Fatalf("lost %s", key)
		}
	}
	var project Project
	if err := json.Unmarshal(preview, &project); err != nil {
		t.Fatal(err)
	}
	for _, environment := range []string{"production", "staging"} {
		r, err := project.ResolveEnvironment(environment)
		if err != nil {
			t.Fatal(err)
		}
		for name, service := range topologyNodes(r) {
			if service.Destination != "default" {
				t.Fatalf("missing destination for %s", name)
			}
		}
	}
	after, _ := os.ReadFile(filename)
	if string(after) != raw {
		t.Fatal("migration wrote the source")
	}
	delete(converted, "custom")
	valid, _ := json.Marshal(converted)
	if err := validatePlanningProject(topologyProject(t, string(valid))); err != nil {
		t.Fatal(err)
	}
	if _, err := previewMigration(writeConfig(t, staticSiteConfig), ""); err == nil {
		t.Fatal("migration guessed a destination")
	}
	if _, err := previewMigration(writeConfig(t, staticSiteConfig), "host:Projects/site"); err != nil {
		t.Fatal(err)
	}
}

func TestReleaseTaskReadinessAndNoTunnelForDisconnectedHosts(t *testing.T) {
	r := topologyResolved(t)
	api := r.Services["api"]
	api.DependsOn = map[string]string{"migrate": "completed"}
	r.Services["api"] = api
	r.Release = map[string]Service{"migrate": {Image: "example/migrate", Destination: "app", DependsOn: map[string]string{"db": "healthy"}, Connections: api.Connections}}
	plan, err := planTopology(r, "unverified")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plan.Waves, [][]string{{"db"}, {"migrate"}, {"api"}}) {
		t.Fatal(plan.Waves)
	}
	task := r.Release["migrate"]
	task.DependsOn = nil
	r.Release["migrate"] = task
	if err := r.Validate(); err == nil || !strings.Contains(err.Error(), "must depend") {
		t.Fatal(err)
	}
	r.Release = nil
	api.DependsOn = nil
	api.Connections = nil
	r.Services["api"] = api
	r.Network = nil
	plan, err = planTopology(r, "unverified")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Network.Mode != "docker-only" {
		t.Fatal("unconnected hosts created a tunnel")
	}
}

func TestLegacyEmptyDependencyOverrideStillInherits(t *testing.T) {
	base := Service{DependsOn: map[string]string{"db": "started"}}
	override := Service{DependsOn: map[string]string{}}
	if len(mergeService(base, override, 1).DependsOn) != 1 {
		t.Fatal("changed version 1 inheritance")
	}
	if len(mergeService(base, override, 2).DependsOn) != 0 {
		t.Fatal("version 2 did not clear dependencies")
	}
}

func TestTopologyRefusesOptionAndRootDestinations(t *testing.T) {
	for _, raw := range []string{"-user@host:Projects/app", "host:-option", "host:/tmp/..", "host:Projects/..", "host:~/Projects/app"} {
		if _, err := topologyDestination(raw); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
}

func TestTopologyRequiresDeclaredNonProductionEnvironment(t *testing.T) {
	project := topologyProject(t, topologyFixture)
	if _, err := project.ResolveEnvironment("staging"); err == nil {
		t.Fatal("silently inherited production network for undeclared environment")
	}
}
