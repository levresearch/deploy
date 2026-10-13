package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestADestinationViewHoldsItsOwnServicesButResolvesConnectionsAcrossAll(t *testing.T) {
	resolved := topologyResolved(t)
	app, data := resolved.on("app"), resolved.on("data")
	if _, found := app.Services["db"]; found || len(app.Services) != 1 {
		t.Fatalf("app view: %v", app.Services)
	}
	if _, found := data.Services["db"]; !found || len(data.Services) != 1 {
		t.Fatalf("data view: %v", data.Services)
	}
	if len(app.topology().Services) != 2 || len(app.on("data").Services) != 1 {
		t.Fatal("a view lost the whole project")
	}

	layout := NewLayout("/srv/example", resolved.RuntimeID())
	rendered, err := RenderServiceRelease(app, layout, "api", "abcdef1")
	api := renderedNode(t, app, rendered, err, "api")
	if compact(t, api["environment"]) != `{"DB_HOST":"10.77.0.2","DB_PORT":"5432"}` {
		t.Fatalf("a view resolved the connection without its provider: %s", api["environment"])
	}
	rendered, err = RenderShared(data, layout, "abcdef1")
	db := renderedNode(t, data, rendered, err, "db")
	if compact(t, db["ports"]) != `["10.77.0.2:5432:5432/tcp"]` {
		t.Fatalf("a view lost the consumers on other hosts: %s", db["ports"])
	}
	if shared, err := RenderShared(app, layout, "abcdef1"); err == nil && strings.Contains(string(shared), `"db"`) {
		t.Fatal("the app view rendered the data destination's stateful service")
	}
}

func fakeObservation(host string) DestinationObservation {
	return DestinationObservation{HostIdentity: host, CanonicalPath: "/home/deploy/Projects/example", machineID: host, bootID: "boot-" + host, daemonID: "daemon-" + host}
}

func TestDeployRefusesTwoDestinationsOnOneHost(t *testing.T) {
	resolved := topologyResolved(t)
	destinations := map[string]Destination{"app": {Host: "app-host", Path: "Projects/example"}, "data": {Host: "data-host", Path: "Projects/example"}}
	separate := func(_ context.Context, destination Destination) (DestinationObservation, error) {
		return fakeObservation(destination.Host), nil
	}
	observations, err := observeTopology(context.Background(), &resolved, destinations, separate)
	if err != nil || len(observations) != 2 || resolved.destinationHost("data") != "data-host" {
		t.Fatalf("separate hosts: %v %v", observations, err)
	}

	resolved = topologyResolved(t)
	together := func(context.Context, Destination) (DestinationObservation, error) {
		return fakeObservation("one-host"), nil
	}
	if _, err := observeTopology(context.Background(), &resolved, destinations, together); err == nil || !strings.Contains(err.Error(), "same host") {
		t.Fatalf("two destinations on one host were accepted: %v", err)
	}
}

func TestStatefulServicesWaitOnlyOnTheirOwnSharedStack(t *testing.T) {
	repository := t.TempDir()
	raw := strings.Replace(topologyFixture, `"db":{"destination":"data"`, `"cache":{"destination":"app","image":"example/cache","stateful":true,"dependsOn":{"db":"healthy"}},
  "db":{"destination":"data"`, 1)
	if err := os.WriteFile(filepath.Join(repository, configFileName), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadTopology(repository, "production"); err == nil || !strings.Contains(err.Error(), "not a stateful service on the same destination") {
		t.Fatalf("a stateful dependency across destinations was accepted: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repository, configFileName), []byte(topologyFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	resolved, destinations, err := loadTopology(repository, "production")
	if err != nil || len(destinations) != 2 || destinations["data"].Host != "data-host" || resolved.RuntimeID() != "1234abcd" {
		t.Fatalf("%v %v", destinations, err)
	}
	if _, _, _, err := loadServiceConfig(repository, "production"); err == nil || !strings.Contains(err.Error(), "one destination for now") {
		t.Fatalf("a single-destination command took two: %v", err)
	}
}

func TestIndentedOutputPrefixesEveryLine(t *testing.T) {
	var buffer bytes.Buffer
	writer := indented{&buffer}
	if count, err := writer.Write([]byte("a (vm-a): enrolled\nb (vm-b): unchanged\n")); err != nil || count != 39 {
		t.Fatalf("%d %v", count, err)
	}
	if buffer.String() != "    a (vm-a): enrolled\n    b (vm-b): unchanged\n" {
		t.Fatalf("%q", buffer.String())
	}
}

func TestAProjectThatBuildsNothingNeedsNoLocalDocker(t *testing.T) {
	runner := newScriptedRunner()
	runner.failCommands = []string{"docker"}
	if err := CheckLocalRequirements(runner, Destination{Host: "app-host"}, false); err != nil {
		t.Fatal(err)
	}
	for _, command := range runner.ran {
		if strings.HasPrefix(command, "docker") {
			t.Fatalf("checked %s for a project that builds nothing", command)
		}
	}
	if err := CheckLocalRequirements(runner, Destination{Host: "app-host"}, true); err == nil || !strings.Contains(err.Error(), "docker") {
		t.Fatalf("a project that builds was let through without docker: %v", err)
	}
}
