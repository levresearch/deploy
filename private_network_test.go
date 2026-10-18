package main

import (
	"encoding/json"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

const privateConfig = `{
  "version": 1, "id": "a3f19c02", "name": "private",
  "services": {
    "db": {"image": "postgres:18", "stateful": true, "isolated": "auth"},
    "web": {"image": "nginx", "joins": ["auth"], "dependsOn": {"db": "healthy"}},
    "other": {"image": "nginx"}
  },
  "release": {
    "migrate": {"image": "postgres:18", "isolated": "auth"}
  }
}`

func renderedNetworks(t *testing.T, rendered []byte) (map[string]any, map[string][]string) {
	t.Helper()
	var document struct {
		Networks map[string]any `json:"networks"`
		Services map[string]struct {
			Networks []string `json:"networks"`
		} `json:"services"`
	}
	if err := json.Unmarshal(rendered, &document); err != nil {
		t.Fatalf("rendered compose is not json: %v", err)
	}
	perService := map[string][]string{}
	for name, service := range document.Services {
		perService[name] = service.Networks
	}
	return document.Networks, perService
}

func TestIsolatedServicesSitOnlyOnTheirPrivateNetwork(t *testing.T) {
	resolved := loadAndResolve(t, privateConfig, defaultEnvironmentName)
	layout := NewLayout("/srv/projects", resolved.ID)

	shared, err := RenderShared(resolved, layout, "aaaaaaa")
	if err != nil {
		t.Fatal(err)
	}
	networks, services := renderedNetworks(t, shared)
	if !slices.Equal(services["db"], []string{"auth"}) {
		t.Errorf("db networks = %v, want only auth", services["db"])
	}
	auth, _ := networks["auth"].(map[string]any)
	if auth["name"] != "deploy-a3f19c02-auth-private" || auth["external"] != true {
		t.Errorf("auth network = %v", networks["auth"])
	}

	release, err := RenderRelease(resolved, layout, "aaaaaaa")
	if err != nil {
		t.Fatal(err)
	}
	networks, services = renderedNetworks(t, release)
	if !slices.Equal(services["web"], []string{"default", "auth"}) {
		t.Errorf("web networks = %v, want default and auth", services["web"])
	}
	if services["other"] != nil {
		t.Errorf("a service with no private network got %v", services["other"])
	}
	if _, found := networks["auth"]; !found {
		t.Error("the release stack does not declare the network web joins")
	}

	tasks, err := RenderReleaseTasks(resolved, layout, "aaaaaaa")
	if err != nil {
		t.Fatal(err)
	}
	if _, services = renderedNetworks(t, tasks); !slices.Equal(services["migrate"], []string{"auth"}) {
		t.Errorf("migrate networks = %v, want only auth", services["migrate"])
	}
}

func TestAStackWithoutPrivateNetworksRendersAsBefore(t *testing.T) {
	resolved := loadAndResolve(t, `{"version": 1, "id": "a3f19c02", "name": "x", "services": {"web": {"image": "nginx"}}}`, defaultEnvironmentName)
	rendered, err := RenderRelease(resolved, NewLayout("/srv/projects", resolved.ID), "aaaaaaa")
	if err != nil {
		t.Fatal(err)
	}
	networks, services := renderedNetworks(t, rendered)
	if len(networks) != 1 || services["web"] != nil {
		t.Errorf("networks %v, web %v", networks, services["web"])
	}
}

func TestPrivateNetworkMistakesAreRefused(t *testing.T) {
	cases := map[string]struct {
		services string
		want     string
	}{
		"isolated and joining": {`"db": {"image": "x", "isolated": "auth", "joins": ["auth"]}`, "also joins"},
		"bad name":             {`"db": {"image": "x", "isolated": "Auth"}`, "lowercase name"},
		"joining twice":        {`"db": {"image": "x", "isolated": "auth"}, "web": {"image": "x", "joins": ["auth", "auth"]}`, "twice"},
		"hosted": {`"db": {"image": "x", "isolated": "auth", "healthcheck": {"command": ["CMD", "true"]},
		  "host": {"domain": "a.example.com", "port": 80, "tunnelTokenFrom": "T"}}`, "tunnel could never reach it"},
		"published":       {`"db": {"image": "x", "isolated": "auth", "ports": ["5432:5432"]}`, "must not publish ports"},
		"raw networks":    {`"db": {"image": "x", "isolated": "auth", "networks": ["x"]}`, "deploy owns its networks"},
		"waiting outside": {`"db": {"image": "x", "isolated": "auth"}, "web": {"image": "x", "dependsOn": {"db": "healthy"}}`, "add \"auth\" to its joins"},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			resolved := loadAndResolve(t, `{"version": 1, "id": "a3f19c02", "name": "x", "services": {`+testCase.services+`}}`, defaultEnvironmentName)
			err := resolved.Validate()
			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("got %v, want an error containing %q", err, testCase.want)
			}
		})
	}

	if err := loadAndResolve(t, privateConfig, defaultEnvironmentName).Validate(); err != nil {
		t.Fatalf("a correct private network was refused: %v", err)
	}
	joinsFirst := `{"version": 1, "id": "a3f19c02", "name": "x", "services": {"db": {"image": "x", "stateful": true}, "web": {"image": "x", "joins": ["auth"]}}}`
	if err := loadAndResolve(t, joinsFirst, defaultEnvironmentName).Validate(); err != nil {
		t.Fatalf("joining ahead of the isolation, the gapless first step, was refused: %v", err)
	}
}

func TestAPrivateNetworkCannotSpanDestinations(t *testing.T) {
	problems := validatePrivateNetworks(ResolvedProject{
		Services: map[string]Service{
			"db":  {Image: "x", Destination: "one", Isolated: "auth"},
			"web": {Image: "x", Destination: "two", Joins: []string{"auth"}},
		},
	})
	if len(problems) != 1 || !strings.Contains(problems[0].Error(), "lives on one host") {
		t.Fatalf("got %v", problems)
	}
}

func TestPrivateNetworkFieldsSurviveAnEnvironmentAndARoundTrip(t *testing.T) {
	resolved := loadAndResolve(t, `{"version": 1, "id": "a3f19c02", "name": "x",
	  "services": {"db": {"image": "x", "isolated": "auth"}, "web": {"image": "x"}},
	  "environments": {"production": {}, "staging": {"services": {"web": {"joins": ["auth"]}}}}}`, "staging")
	if !slices.Equal(resolved.Services["web"].Joins, []string{"auth"}) || resolved.Services["db"].Isolated != "auth" {
		t.Fatalf("merged services: %+v", resolved.Services)
	}

	encoded, err := json.Marshal(resolved.Services["web"])
	if err != nil {
		t.Fatal(err)
	}
	var decoded Service
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(decoded.Joins, []string{"auth"}) || decoded.Extra["joins"] != nil {
		t.Fatalf("joins did not round trip as an owned field: %s", encoded)
	}
}

func TestPrivateNetworksAreCreatedInternalAndAStrayOneIsRefused(t *testing.T) {
	resolved := loadAndResolve(t, privateConfig, defaultEnvironmentName)

	fresh := &scriptedRunner{host: "local", responses: map[string]string{}}
	if err := ensurePrivateNetworks(fresh, resolved); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(fresh.ran, "docker network create --internal deploy-a3f19c02-auth-private") {
		t.Fatalf("ran %v", fresh.ran)
	}

	existing := func(internal string) *scriptedRunner {
		return &scriptedRunner{
			host:         "local",
			failCommands: []string{"network create"},
			responses: map[string]string{
				"network create":  "Error response from daemon: network with name deploy-a3f19c02-auth-private already exists",
				"network inspect": internal + "\n",
			},
		}
	}
	if err := ensurePrivateNetworks(existing("true"), resolved); err != nil {
		t.Fatalf("an existing internal network was refused: %v", err)
	}
	if err := ensurePrivateNetworks(existing("false"), resolved); err == nil || !strings.Contains(err.Error(), "not internal") {
		t.Fatalf("an existing network with outside access was accepted: %v", err)
	}
}

func TestDestroyRemovesPrivateNetworks(t *testing.T) {
	resolved := loadAndResolve(t, privateConfig, defaultEnvironmentName)
	plan, err := PlanDestroy(resolved, NewLayout("/srv/projects", resolved.ID), "/srv/projects", State{}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(plan.PrivateNetworks, []string{"deploy-a3f19c02-auth-private"}) {
		t.Fatalf("plan removes %v", plan.PrivateNetworks)
	}
}

func TestTunnelsRunWithoutPrivileges(t *testing.T) {
	resolved := loadAndResolve(t, hostedConfig, defaultEnvironmentName)
	tunnels, err := renderTunnels(resolved)
	if err != nil {
		t.Fatal(err)
	}
	for name, raw := range tunnels {
		var tunnel struct {
			ReadOnly    bool     `json:"read_only"`
			CapDrop     []string `json:"cap_drop"`
			SecurityOpt []string `json:"security_opt"`
			PidsLimit   int      `json:"pids_limit"`
		}
		if err := json.Unmarshal(raw, &tunnel); err != nil {
			t.Fatal(err)
		}
		if !tunnel.ReadOnly || !slices.Equal(tunnel.CapDrop, []string{"ALL"}) || !slices.Contains(tunnel.SecurityOpt, "no-new-privileges:true") || tunnel.PidsLimit == 0 {
			t.Errorf("%s is not locked down: %s", name, raw)
		}
	}
}

// Isolation has to hold on a real docker engine, not just in the rendered file:
// a service outside the private network cannot even resolve the isolated one,
// a service that joins it can reach it, and the isolated one cannot reach out.
func TestAnIsolatedServiceIsUnreachableFromOutsideItsNetwork(t *testing.T) {
	dockerAvailable(t)
	t.Parallel()
	const id = "fb0000a1"
	removeDeployment(t, id)
	t.Cleanup(func() { exec.Command("docker", "network", "rm", PrivateNetworkName(id, "vault")).Run() })
	repository := newRepository(t)
	destination := t.TempDir()

	listener := `"image": "busybox:latest", "command": ["sh", "-c", "while true; do echo sealed | nc -l -p 7000; done"], "healthcheck": {"command": ["CMD", "true"], "interval": "1s", "retries": 5}`
	config := func(vault string) string {
		return `{"version": 2, "id": "` + id + `", "name": "isolation", "retention": 2,
		  "destinations": {"main": "` + destination + `"},
		  "services": {
		    "vault": {` + listener + `, "destination": "main", "stateful": true` + vault + `},
		    "member": {` + sleeper + `, "destination": "main", "joins": ["vault"]},
		    "outsider": {` + sleeper + `, "destination": "main"}
		  }}`
	}
	reach := func(service string) (string, error) {
		project := recordedState(t, destination, id).Services[service].Current.Project
		output, err := exec.Command("docker", "compose", "--project-name", project, "exec", "-T", service,
			"sh", "-c", "nc -w 3 vault 7000 </dev/null").CombinedOutput()
		return string(output), err
	}

	// the gapless order: everything that uses the vault joins its network first,
	// and only then is the vault moved onto it
	commitFile(t, repository, configFileName, config(""))
	if code, err := RunDeploy(DeployOptions{Context: repository}); code != exitOK || err != nil {
		t.Fatalf("first deploy: %d %v", code, err)
	}
	if output, err := reach("outsider"); err != nil || !strings.Contains(output, "sealed") {
		t.Fatalf("before isolation everything should reach the vault: %v %s", err, output)
	}

	commitFile(t, repository, configFileName, config(`, "isolated": "vault"`))
	if code, err := RunDeploy(DeployOptions{Context: repository}); code != exitOK || err != nil {
		t.Fatalf("isolating deploy: %d %v", code, err)
	}

	internal, err := exec.Command("docker", "network", "inspect", "--format", "{{.Internal}}", PrivateNetworkName(id, "vault")).Output()
	if err != nil || strings.TrimSpace(string(internal)) != "true" {
		t.Fatalf("the private network is not internal: %q %v", internal, err)
	}
	if output, err := reach("member"); err != nil || !strings.Contains(output, "sealed") {
		t.Fatalf("a service that joins the network could not reach it: %v %s", err, output)
	}
	if output, err := reach("outsider"); err == nil || strings.Contains(output, "sealed") {
		t.Fatalf("a service outside the network reached the isolated one: %s", output)
	}
	if output, err := exec.Command("docker", "compose", "--project-name", SharedProjectName(id), "exec", "-T", "vault",
		"sh", "-c", "nc -w 3 1.1.1.1 53 </dev/null && echo escaped").CombinedOutput(); strings.Contains(string(output), "escaped") {
		t.Fatalf("the isolated service reached the internet: %v %s", err, output)
	}
}

// The compose keys that lock a container down are not deploy's own, so this
// checks they still reach the container and hold there: the root filesystem
// refuses writes, the tmpfs takes them, and the capabilities are gone.
func TestHardeningKeysReachTheContainerAndHold(t *testing.T) {
	dockerAvailable(t)
	t.Parallel()
	const id = "fb0000a2"
	removeDeployment(t, id)
	repository := newRepository(t)
	destination := t.TempDir()

	commitFile(t, repository, configFileName, `{"version": 2, "id": "`+id+`", "name": "hardened", "retention": 2,
	  "destinations": {"main": "`+destination+`"},
	  "services": {
	    "locked": {`+sleeper+`, "destination": "main", "read_only": true, "tmpfs": ["/tmp"],
	      "cap_drop": ["ALL"], "security_opt": ["no-new-privileges:true"], "pids_limit": 64, "mem_limit": "64m", "cpus": 0.5}
	  }}`)
	commit, err := ResolveCommit(repository, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if code, err := RunDeploy(DeployOptions{Context: repository}); code != exitOK || err != nil {
		t.Fatalf("deploy: %d %v", code, err)
	}

	project := ServiceReleaseProject(id, "locked", commit)
	run := func(script string) string {
		output, _ := exec.Command("docker", "compose", "--project-name", project, "exec", "-T", "locked", "sh", "-c", script).CombinedOutput()
		return strings.TrimSpace(string(output))
	}
	if got := run("touch /written 2>/dev/null && echo wrote || echo refused"); got != "refused" {
		t.Errorf("the read only root took a write: %s", got)
	}
	if got := run("touch /tmp/written && echo wrote"); got != "wrote" {
		t.Errorf("the tmpfs refused a write: %s", got)
	}
	if got := run("grep CapEff /proc/self/status"); !strings.HasSuffix(got, "0000000000000000") {
		t.Errorf("capabilities survived cap_drop: %s", got)
	}
	if got := run("grep NoNewPrivs /proc/self/status"); !strings.HasSuffix(got, "1") {
		t.Errorf("no-new-privileges is off: %s", got)
	}

	container := strings.TrimSpace(func() string {
		output, _ := exec.Command("docker", "compose", "--project-name", project, "ps", "-q", "locked").Output()
		return string(output)
	}())
	limits, err := exec.Command("docker", "inspect", "--format", "{{.HostConfig.PidsLimit}} {{.HostConfig.Memory}} {{.HostConfig.NanoCpus}}", container).Output()
	if err != nil || strings.TrimSpace(string(limits)) != "64 67108864 500000000" {
		t.Errorf("limits did not reach the container: %q %v", limits, err)
	}
}
