package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

const topologyFixture = `{
 "version":2,"id":"1234abcd","name":"example",
 "destinations":{"app":"app-host:Projects/example","data":"data-host:Projects/example"},
 "network":{"cidr":"10.77.0.0/24","peers":{
   "app":{"address":"10.77.0.1","listenPort":51820,"keepalive":25},
   "data":{"address":"10.77.0.2","listenPort":51820,"endpoint":"data.example.com:51820"}}},
 "services":{
  "api":{"destination":"app","image":"example/api","dependsOn":{"db":"healthy"},
   "connections":{"database":{"service":"db","endpoint":"sql","hostEnv":"DB_HOST","portEnv":"DB_PORT"}}},
  "db":{"destination":"data","image":"example/db","stateful":true,
   "healthcheck":{"command":["CMD","check"]},
   "endpoints":{"sql":{"port":5432,"hostPort":5432,"protocol":"tcp"}}}
 }
}`

func topologyProject(t *testing.T, raw string) Project {
	t.Helper()
	project, err := loadPlanningProject(writeConfig(t, raw))
	if err != nil {
		t.Fatal(err)
	}
	return project
}

func topologyResolved(t *testing.T) ResolvedProject {
	t.Helper()
	resolved, err := topologyProject(t, topologyFixture).ResolveEnvironment("production")
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func TestTopologyPlanPathsAndDeterminism(t *testing.T) {
	resolved := topologyResolved(t)
	plan, err := planTopology(resolved, strings.Repeat("a", 40))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plan.Waves, [][]string{{"db"}, {"api"}}) {
		t.Fatal(plan.Waves)
	}
	if plan.ExecutionAvailable || plan.Network.Mode != "wireguard-required" || plan.Connections[0].Host != "10.77.0.2" || plan.Connections[0].Port != 5432 {
		t.Fatalf("unexpected plan: %+v", plan)
	}
	first, _ := json.Marshal(plan)
	for range 10 {
		again, err := planTopology(resolved, strings.Repeat("a", 40))
		if err != nil {
			t.Fatal(err)
		}
		encoded, _ := json.Marshal(again)
		if string(first) != string(encoded) {
			t.Fatal("plan is not deterministic")
		}
	}
	resolved.Destinations["data"] = "other-user@app-host:Projects/another-path"
	resolved.Network = nil
	local, err := planTopology(resolved, "unverified")
	if err != nil {
		t.Fatal(err)
	}
	if local.Network.Mode != "docker-only" || local.Connections[0].Host != "db" || len(local.Network.Peers) != 0 {
		t.Fatal(local)
	}
	resolved.Destinations["data"] = "different-alias:Projects/example"
	if _, err := planTopology(resolved, "unverified"); err == nil {
		t.Fatal("unverified aliases must not be assumed to share a host")
	}
}

func TestTopologyCommunicationDoesNotCreateReadinessCycle(t *testing.T) {
	resolved := topologyResolved(t)
	api, db := resolved.Services["api"], resolved.Services["db"]
	api.DependsOn = nil
	api.Endpoints = map[string]Endpoint{"http": {Port: 8080, HostPort: 8080, Protocol: "tcp"}}
	db.Connections = map[string]Connection{"api": {Service: "api", Endpoint: "http", HostEnv: "API_HOST", PortEnv: "API_PORT"}}
	resolved.Services["api"], resolved.Services["db"] = api, db
	plan, err := planTopology(resolved, "unverified")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plan.Waves, [][]string{{"api", "db"}}) {
		t.Fatal(plan.Waves)
	}
}

func TestTopologyValidation(t *testing.T) {
	cases := []struct {
		name, want string
		edit       func(*ResolvedProject)
	}{
		{"missing destination", "known destination", func(r *ResolvedProject) { s := r.Services["api"]; s.Destination = ""; r.Services["api"] = s }},
		{"root destination", "root destination", func(r *ResolvedProject) { r.Destination = "host:path" }},
		{"cycle", "cycle", func(r *ResolvedProject) {
			s := r.Services["db"]
			s.DependsOn = map[string]string{"api": "started"}
			r.Services["db"] = s
		}},
		{"self", "itself", func(r *ResolvedProject) { r.Services["api"].DependsOn["api"] = "started" }},
		{"unknown dependency", "does not define", func(r *ResolvedProject) { r.Services["api"].DependsOn["absent"] = "started" }},
		{"completed service", "completed requires", func(r *ResolvedProject) { r.Services["api"].DependsOn["db"] = "completed" }},
		{"disabled healthcheck", "enabled healthcheck", func(r *ResolvedProject) {
			s := r.Services["db"]
			s.Healthcheck = json.RawMessage(`{"command":["NONE"]}`)
			r.Services["db"] = s
		}},
		{"duplicate task", "both a service", func(r *ResolvedProject) { r.Release = map[string]Service{"db": r.Services["db"]} }},
		{"unknown endpoint", "named service endpoint", func(r *ResolvedProject) {
			c := r.Services["api"].Connections["database"]
			c.Endpoint = "missing"
			r.Services["api"].Connections["database"] = c
		}},
		{"environment collision", "colliding environment", func(r *ResolvedProject) {
			s := r.Services["api"]
			s.Extra = map[string]json.RawMessage{"environment": json.RawMessage(`["DB_HOST=secret"]`)}
			r.Services["api"] = s
		}},
		{"same generated variable", "colliding environment", func(r *ResolvedProject) {
			c := r.Services["api"].Connections["database"]
			c.PortEnv = c.HostEnv
			r.Services["api"].Connections["database"] = c
		}},
		{"network missing", "requires network", func(r *ResolvedProject) { r.Network = nil }},
		{"peer missing", "both network peers", func(r *ResolvedProject) { delete(r.Network.Peers, "app") }},
		{"both unreachable", "reachable UDP", func(r *ResolvedProject) { p := r.Network.Peers["data"]; p.Endpoint = ""; r.Network.Peers["data"] = p }},
		{"duplicate address", "duplicates a private address", func(r *ResolvedProject) {
			p := r.Network.Peers["data"]
			p.Address = "10.77.0.1"
			r.Network.Peers["data"] = p
		}},
		{"broadcast", "usable IPv4", func(r *ResolvedProject) {
			p := r.Network.Peers["data"]
			p.Address = "10.77.0.255"
			r.Network.Peers["data"] = p
		}},
		{"public subnet", "private IPv4", func(r *ResolvedProject) { r.Network.CIDR = "8.8.8.0/24" }},
		{"missing host port", "stable provider hostPort", func(r *ResolvedProject) {
			e := r.Services["db"].Endpoints["sql"]
			e.HostPort = 0
			r.Services["db"].Endpoints["sql"] = e
		}},
		{"duplicate port", "port collision", func(r *ResolvedProject) {
			r.Services["db"].Endpoints["other"] = Endpoint{Port: 1234, HostPort: 5432, Protocol: "tcp"}
		}},
		{"raw topology", "raw network_mode", func(r *ResolvedProject) {
			s := r.Services["api"]
			s.Extra = map[string]json.RawMessage{"network_mode": json.RawMessage(`"host"`)}
			r.Services["api"] = s
		}},
		{"raw published port", "raw ports", func(r *ResolvedProject) {
			s := r.Services["db"]
			s.Extra = map[string]json.RawMessage{"ports": json.RawMessage(`["5432:5432"]`)}
			r.Services["db"] = s
		}},
		{"unsafe ssh host", "plain SSH", func(r *ResolvedProject) { r.Destinations["app"] = "-oProxyCommand=touch:Projects/app" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := topologyResolved(t)
			tc.edit(&r)
			err := r.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
		})
	}
}

func TestTopologyStrictDecoding(t *testing.T) {
	for _, replacement := range []struct{ old, new, want string }{
		{`"version":2`, `"version":2,"version":2`, "duplicate"},
		{`"name":"example"`, `"name":"example","unknown":true`, "unknown field"},
		{`"listenPort":51820`, `"listenPort":51820,"privateKey":"never-accepted"`, "unknown field"},
		{`"protocol":"tcp"`, `"protocol":"tcp","protcol":"udp"`, "unknown field"},
		{`"name":"example"`, `"name":"example","destination":null`, "root destination"},
	} {
		_, err := loadPlanningProject(writeConfig(t, strings.Replace(topologyFixture, replacement.old, replacement.new, 1)))
		if err == nil || !strings.Contains(err.Error(), replacement.want) {
			t.Fatalf("want %s, got %v", replacement.want, err)
		}
	}
	if _, err := LoadProject(writeConfig(t, topologyFixture)); err == nil || !strings.Contains(err.Error(), "execution is not available") {
		t.Fatal(err)
	}
}

func TestTopologyEnvironmentReplacementRoundTrip(t *testing.T) {
	project := topologyProject(t, topologyFixture)
	project.Environments = map[string]Environment{"production": {}, "staging": {
		Network: &ManagedNetwork{CIDR: "10.78.0.0/24", Peers: map[string]NetworkPeer{
			"app": {Address: "10.78.0.1", ListenPort: 51821}, "data": {Address: "10.78.0.2", ListenPort: 51821, Endpoint: "data.example.com:51821"},
		}},
		Services: map[string]Service{"api": {Connections: map[string]Connection{}, DependsOn: map[string]string{}}, "db": {Endpoints: map[string]Endpoint{"sql": {Port: 1234, Protocol: "tcp"}}}},
	}}
	encoded, err := json.Marshal(project)
	if err != nil {
		t.Fatal(err)
	}
	reloaded := topologyProject(t, string(encoded))
	if err := validatePlanningProject(reloaded); err != nil {
		t.Fatal(err)
	}
	stage, _ := reloaded.ResolveEnvironment("staging")
	if len(stage.Services["api"].Connections) != 0 || len(stage.Services["api"].DependsOn) != 0 || stage.Services["db"].Endpoints["sql"].HostPort != 0 {
		t.Fatal("overrides leaked base fields")
	}
	prod, _ := reloaded.ResolveEnvironment("production")
	if len(prod.Services["api"].Connections) != 1 || prod.Network.CIDR != "10.77.0.0/24" {
		t.Fatal("base mutated")
	}
	stageEnv := reloaded.Environments["staging"]
	stageEnv.Network = nil
	reloaded.Environments["staging"] = stageEnv
	if err := validatePlanningProject(reloaded); err == nil || !strings.Contains(err.Error(), "separate network") {
		t.Fatal(err)
	}
}
