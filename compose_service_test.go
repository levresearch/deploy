package main

import (
	"encoding/json"
	"strings"
	"testing"
)

const serviceReleaseFixture = `{
 "version":2,"id":"1234abcd","name":"example",
 "destinations":{"main":"host:Projects/example"},
 "services":{
  "api":{"destination":"main","build":".","env":["api.env"],"dependsOn":{"db":"healthy","cache":"started"},
   "environment":{"MODE":"production"},
   "connections":{"database":{"service":"db","endpoint":"sql","hostEnv":"DB_HOST","portEnv":"DB_PORT"}}},
  "worker":{"destination":"main","image":"example/worker","environment":["QUEUE=jobs"],
   "connections":{"database":{"service":"db","endpoint":"sql","hostEnv":"DB_HOST","portEnv":"DB_PORT"}}},
  "cache":{"destination":"main","image":"example/cache"},
  "db":{"destination":"main","image":"example/db","stateful":true,
   "healthcheck":{"command":["CMD","check"]},
   "endpoints":{"sql":{"port":5432,"protocol":"tcp"}}}
 }
}`

func renderedService(t *testing.T, resolved ResolvedProject, name string) (ComposeProject, map[string]json.RawMessage) {
	t.Helper()
	rendered, err := RenderServiceRelease(resolved, NewLayout("/srv/example", resolved.RuntimeID()), name, "abcdef1234567")
	if err != nil {
		t.Fatal(err)
	}
	var project ComposeProject
	if err := json.Unmarshal(rendered, &project); err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(project.Services[name], &fields); err != nil {
		t.Fatal(err)
	}
	return project, fields
}

func serviceReleaseResolved(t *testing.T, raw string) ResolvedProject {
	t.Helper()
	resolved, err := topologyProject(t, raw).ResolveEnvironment("production")
	if err != nil {
		t.Fatal(err)
	}
	if err := resolved.Validate(); err != nil {
		t.Fatal(err)
	}
	return resolved
}

func TestAServiceReleaseHoldsOnlyThatService(t *testing.T) {
	resolved := serviceReleaseResolved(t, serviceReleaseFixture)
	project, fields := renderedService(t, resolved, "api")
	if project.Name != "deploy-1234abcd_api_abcdef1" || len(project.Services) != 1 {
		t.Fatalf("api release: %s %v", project.Name, len(project.Services))
	}
	if string(fields["image"]) != `"deploy-1234abcd/api:abcdef1"` {
		t.Fatalf("image %s", fields["image"])
	}
	if _, found := fields["depends_on"]; found {
		t.Fatal("a dependency on another project was handed to compose")
	}
	network, _ := json.Marshal(project.Networks)
	if !strings.Contains(string(network), `"name":"deploy-1234abcd-net"`) || !strings.Contains(string(network), `"external":true`) {
		t.Fatalf("the release is not on the existing project network: %s", network)
	}
	if !strings.Contains(string(fields["env_file"]), "/srv/example/1234abcd/env/api.env") {
		t.Fatalf("env files: %s", fields["env_file"])
	}
	var environment map[string]string
	if err := json.Unmarshal(fields["environment"], &environment); err != nil {
		t.Fatal(err)
	}
	if environment["MODE"] != "production" || environment["DB_HOST"] != "db" || environment["DB_PORT"] != "5432" {
		t.Fatalf("connection environment: %v", environment)
	}

	_, fields = renderedService(t, resolved, "worker")
	var list []string
	if err := json.Unmarshal(fields["environment"], &list); err != nil {
		t.Fatal(err)
	}
	if strings.Join(list, ",") != "QUEUE=jobs,DB_HOST=db,DB_PORT=5432" {
		t.Fatalf("list environment: %v", list)
	}
}

func TestServiceReleasesRefuseWhatOneHostCannotRun(t *testing.T) {
	resolved := serviceReleaseResolved(t, serviceReleaseFixture)
	layout := NewLayout("/srv/example", "1234abcd")
	if _, err := RenderServiceRelease(resolved, layout, "db", "abcdef1"); err == nil {
		t.Fatal("a stateful service was rendered as a release")
	}
	if _, err := RenderServiceRelease(resolved, layout, "missing", "abcdef1"); err == nil {
		t.Fatal("an unknown service was rendered")
	}

	published := resolved
	published.Services = map[string]Service{}
	for name, service := range resolved.Services {
		published.Services[name] = service
	}
	cache := published.Services["cache"]
	cache.Endpoints = map[string]Endpoint{"resp": {Port: 6379, HostPort: 6379, Protocol: "tcp"}}
	published.Services["cache"] = cache
	if _, err := RenderServiceRelease(published, layout, "cache", "abcdef1"); err == nil || !strings.Contains(err.Error(), "managed networking") {
		t.Fatalf("a published host port was rendered: %v", err)
	}

	elsewhere := published
	elsewhere.Services = map[string]Service{}
	for name, service := range resolved.Services {
		elsewhere.Services[name] = service
	}
	db := elsewhere.Services["db"]
	db.Destination = "other"
	elsewhere.Services["db"] = db
	if _, err := RenderServiceRelease(elsewhere, layout, "api", "abcdef1"); err == nil || !strings.Contains(err.Error(), "crosses destinations") {
		t.Fatalf("a cross-destination connection was rendered: %v", err)
	}

	clashing := elsewhere
	clashing.Services = map[string]Service{}
	for name, service := range resolved.Services {
		clashing.Services[name] = service
	}
	api := clashing.Services["api"]
	api.Extra = map[string]json.RawMessage{"environment": json.RawMessage(`{"DB_HOST":"elsewhere"}`)}
	clashing.Services["api"] = api
	if _, err := RenderServiceRelease(clashing, layout, "api", "abcdef1"); err == nil || !strings.Contains(err.Error(), "DB_HOST") {
		t.Fatalf("a colliding variable was overwritten: %v", err)
	}
}

func TestServiceReleaseNamesCannotCollide(t *testing.T) {
	names := map[string]string{}
	for _, candidate := range []struct{ id, service, commit string }{
		{"1234abcd", "staging", "abcdef1"},
		{"1234abcd-staging", "api", "abcdef1"},
		{"1234abcd", "api", "abcdef1"},
		{"1234abcd-s", "api", "abcdef1"},
	} {
		name := ServiceReleaseProject(candidate.id, candidate.service, candidate.commit)
		if previous, taken := names[name]; taken {
			t.Fatalf("%s is both %s and %v", name, previous, candidate)
		}
		names[name] = candidate.id + "/" + candidate.service
		for _, legacy := range []string{ProjectName(candidate.id, candidate.commit), ProjectName("1234abcd-staging", candidate.commit), SharedProjectName(candidate.id)} {
			if legacy == name {
				t.Fatalf("%s collides with a version 1 project", name)
			}
		}
	}
	if !validArtifacts("1234abcd", "api", false, serviceRelease("1234abcd", "api", "abcdef1", "")) || !validArtifacts("1234abcd", "api", false, legacyRelease("1234abcd", "abcdef1")) || !validArtifacts("1234abcd", "db", true, sharedRelease("1234abcd", "abcdef1", "")) {
		t.Fatal("deploy's own artifacts were refused")
	}
}

func TestDestroyFindsOnlyThisEnvironmentsProjects(t *testing.T) {
	production := projectsOf("1234abcd")
	for _, own := range []string{"deploy-1234abcd-shared", "deploy-1234abcd-abcdef1", "deploy-1234abcd-abcdef1-tasks", "deploy-1234abcd_api_abcdef1", "deploy-1234abcd_match-worker_abcdef1"} {
		if !production.MatchString(own) {
			t.Errorf("%s was not found", own)
		}
	}
	for _, other := range []string{"deploy-1234abcd-staging-shared", "deploy-1234abcd-staging-abcdef1", "deploy-1234abcd-staging_api_abcdef1", "deploy-1234abcd-abcdef1-shared", "deploy-1234abcd-abcdef1_api_abcdef1", "deploy-1234abcde-shared", "deploy-1234abcd-net", "other-1234abcd-shared"} {
		if production.MatchString(other) {
			t.Errorf("%s belongs to someone else", other)
		}
	}
	if staging := projectsOf("1234abcd-staging"); !staging.MatchString("deploy-1234abcd-staging_api_abcdef1") || staging.MatchString("deploy-1234abcd_api_abcdef1") {
		t.Error("staging matched the wrong projects")
	}
}
