package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestEnvironmentRuntimeIdentity(t *testing.T) {
	production := loadAndResolve(t, splitConfig, "production")
	staging := loadAndResolve(t, splitConfig, "staging")
	if production.RuntimeID() != production.ID {
		t.Fatal("production identity must remain compatible")
	}
	if staging.RuntimeID() == production.RuntimeID() {
		t.Fatal("staging must have its own identity")
	}
	for _, project := range []ResolvedProject{production, staging} {
		layout := NewLayout("/srv/projects", project.RuntimeID())
		shared, err := RenderShared(project, layout, "aaaaaaa")
		if err != nil {
			t.Fatal(err)
		}
		if decodeCompose(t, shared).Name != SharedProjectName(project.RuntimeID()) {
			t.Fatal("renderer must use the environment identity")
		}
	}
}

func TestBuiltStatefulImageUsesTheCommit(t *testing.T) {
	p := loadAndResolve(t, `{"version":1,"id":"abcd1234","name":"built","services":{"db":{"build":"Dockerfile","stateful":true}}}`, "production")
	raw, err := RenderShared(p, NewLayout("/srv/projects", p.ID), "aaaaaaa")
	if err != nil {
		t.Fatal(err)
	}
	var fields struct{ Image string }
	if err := json.Unmarshal(decodeCompose(t, raw).Services["db"], &fields); err != nil {
		t.Fatal(err)
	}
	if fields.Image != ImageTag(p.ID, "db", "aaaaaaa") {
		t.Fatalf("wrong stateful image %s", fields.Image)
	}
}

func TestConcurrentLockHasOneWinner(t *testing.T) {
	layout := NewLayout(t.TempDir(), "abcd1234")
	start := make(chan struct{})
	locks := make(chan *Lock, 2)
	var contenders sync.WaitGroup
	contenders.Add(2)
	for range 2 {
		go func() {
			defer contenders.Done()
			<-start
			lock, _ := AcquireLock(LocalRunner{}, layout, "aaaaaaa", false)
			locks <- lock
		}()
	}
	close(start)
	contenders.Wait()
	close(locks)
	winners := 0
	for lock := range locks {
		if lock != nil {
			winners++
			defer lock.Release()
		}
	}
	if winners != 1 {
		t.Fatalf("got %d lock owners, want one", winners)
	}
}

func TestEnvironmentValidationAndOwnership(t *testing.T) {
	for _, name := range []string{"../production", "Production", "a/b", "", strings.Repeat("a", 33)} {
		p := loadAndResolve(t, splitConfig, name)
		if p.Validate() == nil {
			t.Errorf("accepted invalid environment %q", name)
		}
	}
	for _, name := range []string{"production", "staging", "dev-2"} {
		p := loadAndResolve(t, splitConfig, name)
		if err := p.Validate(); err != nil {
			t.Errorf("rejected %s: %v", name, err)
		}
	}
	p := loadAndResolve(t, splitConfig, "production")
	root := t.TempDir()
	layout := NewLayout(root, p.ID)
	if err := os.MkdirAll(layout.Root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteState(LocalRunner{}, layout, State{Environment: "staging"}); err != nil {
		t.Fatal(err)
	}
	if _, err := claimEnvironment(LocalRunner{}, p, layout); err == nil {
		t.Fatal("production adopted legacy staging")
	}
	p.Environment = "staging"
	if _, err := checkEnvironment(LocalRunner{}, p, NewLayout(root, p.RuntimeID())); err == nil {
		t.Fatal("legacy staging was silently relocated")
	}
	if err := os.Remove(layout.StateFile()); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(layout.EnvDirectory(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(layout.EnvDirectory(), "secrets"), []byte("test"), 0o600); err != nil {
		t.Fatal(err)
	}
	p.Environment = "production"
	if _, err := claimEnvironment(LocalRunner{}, p, layout); err == nil {
		t.Fatal("unknown existing data was silently adopted")
	}
}

func TestPruningUsesExactTags(t *testing.T) {
	runner := newScriptedRunner()
	runner.responses["docker images"] = "deploy-abcd1234/app:aaaaaaa\n"
	pruneImages(runner, "abcd1234", "aaaaaaa")
	commands := strings.Join(runner.ran, "\n")
	if !strings.Contains(commands, "docker image rm deploy-abcd1234/app:aaaaaaa") || strings.Contains(commands, "--force") || strings.Contains(commands, "--quiet") {
		t.Fatalf("unsafe image cleanup: %s", commands)
	}
}

func TestEnvironmentDeleteFence(t *testing.T) {
	for _, identity := range []string{"abcd1234-staging", "abcd1234-dev-2", "abcd1234"} {
		if _, err := projectPathWithin("/srv/projects", identity); err != nil {
			t.Errorf("refused valid identity %s: %v", identity, err)
		}
	}
	for _, identity := range []string{"abcd1234-production", "abcd1234-../production", "abcd1234-Production", "abcd1234-", "../abcd1234"} {
		if _, err := projectPathWithin("/srv/projects", identity); err == nil {
			t.Errorf("accepted invalid identity %s", identity)
		}
	}
}
