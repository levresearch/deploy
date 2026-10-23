package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
)

const externalProject = `{"version": 2, "id": "ab000001", "name": "externals",
  "destinations": {"main": "/srv/projects"},
  "services": {"api": {"destination": "main", "image": "busybox:latest"}},
  "external": {
    "site": {"destination": "main", "image": "busybox:latest", "env": ["site.env"],
      "run": ["vercel", "deploy", "--prod", "--yes"],
      "reference": "^(https://\\S+\\.vercel\\.app)$",
      "verify": {"url": "https://site.example/health", "status": [200, 204], "timeout": "1m"},
      "rollback": ["vercel", "promote", "{reference}", "--yes"],
      "timeout": "20m"}
  },
  "environments": {
    "production": {},
    "staging": {"external": {"site": {"run": ["vercel", "deploy", "--yes"]}}}
  }}`

func resolveExternal(t *testing.T, config, environment string) ResolvedProject {
	t.Helper()
	var project Project
	if err := decodeOwned([]byte(config), &project); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	resolved, err := project.ResolveEnvironment(environment)
	if err != nil {
		t.Fatalf("resolving %s: %v", environment, err)
	}
	return resolved
}

func TestExternalServicesParseMergeAndDigestTheirCommand(t *testing.T) {
	production := resolveExternal(t, externalProject, "production")
	staging := resolveExternal(t, externalProject, "staging")
	if err := production.Validate(); err != nil {
		t.Fatalf("a valid external was refused: %v", err)
	}
	site := production.External["site"]
	if strings.Join(site.Run, " ") != "vercel deploy --prod --yes" || site.Verify.Status[1] != 204 || site.Timeout != "20m" {
		t.Fatalf("parsed %+v", site)
	}
	if _, extra := site.Extra["run"]; extra {
		t.Fatal("run leaked into the keys handed to compose")
	}
	// an environment replaces the whole command and keeps the rest
	if got := strings.Join(staging.External["site"].Run, " "); got != "vercel deploy --yes" {
		t.Fatalf("staging run = %q", got)
	}
	if staging.External["site"].Rollback == nil || staging.External["site"].Reference == "" {
		t.Fatal("staging lost the keys it did not override")
	}
	if nodeDigest(site) == nodeDigest(staging.External["site"]) {
		t.Fatal("a different command did not change the digest")
	}
	encoded, err := json.Marshal(site)
	if err != nil {
		t.Fatal(err)
	}
	var again Service
	if err := json.Unmarshal(encoded, &again); err != nil || nodeDigest(again) != nodeDigest(site) {
		t.Fatalf("a round trip changed the service: %v %s", err, encoded)
	}
	if production.on("main").External["site"].Destination != "main" {
		t.Fatal("the destination view dropped the external")
	}
	if got := sealedEnvFiles(production); len(got) != 1 || got[0] != "site.env" {
		t.Fatalf("sealed env files = %v", got)
	}
	if _, built := buildableServices(production)["site"]; !built {
		t.Fatal("an external is not built with the rest")
	}
}

func TestExternalServicesAreHeldToWhatACommandCanBe(t *testing.T) {
	cases := map[string]struct{ external, want string }{
		"stateful":         {`"stateful": true`, "cannot be stateful"},
		"host":             {`"host": {"domain": "a.example", "port": 80, "tunnelTokenFrom": "T"}`, "cannot publish"},
		"healthcheck":      {`"healthcheck": {"command": ["CMD", "true"]}`, "takes verify"},
		"compose command":  {`"command": ["true"]`, "use run"},
		"ports":            {`"ports": ["80:80"]`, "sets ports"},
		"empty argument":   {`"run": ["vercel", ""]`, "no empty ones"},
		"no run argument":  {`"run": []`, "at least one argument"},
		"two groups":       {`"reference": "(a)(b)"`, "one capture group"},
		"broken pattern":   {`"reference": "("`, "reference"},
		"unrecorded":       {`"rollback": ["promote", "{reference}"]`, "no reference pattern"},
		"short timeout":    {`"timeout": "10s"`, "outside"},
		"bad timeout":      {`"timeout": "soon"`, "not a duration"},
		"verify scheme":    {`"verify": {"url": "ftp://a.example"}`, "absolute http"},
		"verify password":  {`"verify": {"url": "https://u:p@a.example"}`, "no credentials"},
		"verify status":    {`"verify": {"url": "https://a.example", "status": [42]}`, "not an http status"},
		"verify too long":  {`"verify": {"url": "https://a.example", "timeout": "2h"}`, "outside"},
		"unknown verify":   {`"verify": {"url": "https://a.example", "retries": 3}`, "unknown field"},
		"waits on started": {`"dependsOn": {"api": "started"}`, ""},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			config := `{"version": 2, "id": "ab000001", "name": "x", "destinations": {"main": "/srv"},
			  "services": {"api": {"destination": "main", "image": "busybox:latest"}},
			  "external": {"site": {"destination": "main", "image": "busybox:latest", ` + test.external + `}}}`
			var project Project
			if err := decodeOwned([]byte(config), &project); err != nil {
				if test.want == "" || !strings.Contains(err.Error(), test.want) {
					t.Fatalf("decoding: %v", err)
				}
				return
			}
			resolved, err := project.ResolveEnvironment(defaultEnvironmentName)
			if err != nil {
				t.Fatal(err)
			}
			err = resolved.Validate()
			if test.want == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("want %q, got %v", test.want, err)
			}
		})
	}
}

func TestOnlyExternalsTakeTheirKeysAndOthersWaitOnThemCompleted(t *testing.T) {
	for _, config := range []string{
		`"services": {"api": {"destination": "main", "image": "busybox:latest", "run": ["true"]}}`,
		`"services": {"api": {"destination": "main", "image": "busybox:latest"}}, "release": {"migrate": {"destination": "main", "image": "busybox:latest", "verify": {"url": "https://a.example"}}}`,
	} {
		resolved := resolveExternal(t, `{"version": 2, "id": "ab000001", "name": "x", "destinations": {"main": "/srv"}, `+config+`}`, defaultEnvironmentName)
		if err := resolved.Validate(); err == nil || !strings.Contains(err.Error(), "only an external service takes") {
			t.Fatalf("an external key was accepted elsewhere: %v", err)
		}
	}

	waits := func(condition string) error {
		return resolveExternal(t, `{"version": 2, "id": "ab000001", "name": "x", "destinations": {"main": "/srv"},
		  "services": {"api": {"destination": "main", "image": "busybox:latest", "dependsOn": {"site": "`+condition+`"}}},
		  "external": {"site": {"destination": "main", "image": "busybox:latest"}}}`, defaultEnvironmentName).Validate()
	}
	if err := waits("completed"); err != nil {
		t.Fatalf("waiting for an external to complete was refused: %v", err)
	}
	if err := waits("healthy"); err == nil || !strings.Contains(err.Error(), "external dependency site requires completed") {
		t.Fatalf("waiting for an external to be healthy was accepted: %v", err)
	}

	resolved := resolveExternal(t, `{"version": 2, "id": "ab000001", "name": "x", "destinations": {"main": "/srv"},
	  "services": {"site": {"destination": "main", "image": "busybox:latest"}},
	  "external": {"site": {"destination": "main", "image": "busybox:latest"}}}`, defaultEnvironmentName)
	if err := resolved.Validate(); err == nil || !strings.Contains(err.Error(), "is external and also") {
		t.Fatalf("a name used twice was accepted: %v", err)
	}

	var legacy Project
	if err := json.Unmarshal([]byte(`{"version": 1, "id": "ab000001", "name": "x", "destination": "/srv",
	  "services": {"api": {"image": "busybox:latest"}}, "external": {"site": {"image": "busybox:latest"}}}`), &legacy); err != nil {
		t.Fatal(err)
	}
	version1, err := legacy.ResolveEnvironment(defaultEnvironmentName)
	if err != nil {
		t.Fatal(err)
	}
	if err := version1.Validate(); err == nil || !strings.Contains(err.Error(), "need config version 2") {
		t.Fatalf("version 1 took an external: %v", err)
	}
}

func TestExternalsRunInTheirReadinessWaveAndPlanAsExternal(t *testing.T) {
	resolved := resolveExternal(t, `{"version": 2, "id": "ab000001", "name": "x", "destinations": {"main": "/srv"},
	  "services": {"api": {"destination": "main", "image": "busybox:latest"}},
	  "release": {"migrate": {"destination": "main", "image": "busybox:latest"}},
	  "external": {"site": {"destination": "main", "image": "busybox:latest", "dependsOn": {"migrate": "completed"}}}}`, defaultEnvironmentName)
	waves, err := readinessWaves(resolved)
	if err != nil {
		t.Fatal(err)
	}
	if len(waves) != 2 || strings.Join(waves[1], ",") != "site" {
		t.Fatalf("waves = %v", waves)
	}
	if _, err := selectNodes(resolved, []string{"site"}); err != nil {
		t.Fatalf("--service could not name an external: %v", err)
	}
	plan, err := planTopology(resolved, "abc1234")
	if err != nil {
		t.Fatal(err)
	}
	for _, service := range plan.Services {
		if service.Name == "site" && service.Kind != "external" {
			t.Fatalf("site planned as %s", service.Kind)
		}
	}
}

func TestAnExternalRendersItsRunAsTheCommand(t *testing.T) {
	resolved := resolveExternal(t, externalProject, "production")
	rendered, err := RenderExternalRelease(resolved, NewLayout("/srv/projects", "ab000001"), "site", "abc1234def")
	if err != nil {
		t.Fatal(err)
	}
	project := decodeCompose(t, rendered)
	if project.Name != ServiceReleaseProject("ab000001", "site", "abc1234def") {
		t.Fatalf("project %s", project.Name)
	}
	var site struct {
		Command []string `json:"command"`
		EnvFile []string `json:"env_file"`
		Image   string   `json:"image"`
	}
	decodeService(t, project.Services["site"], &site)
	if strings.Join(site.Command, " ") != "vercel deploy --prod --yes" || len(site.EnvFile) != 1 || site.Image != "busybox:latest" {
		t.Fatalf("rendered %+v", site)
	}
}

func TestTheReferenceIsTheLastSafeMatchOfCleanOutput(t *testing.T) {
	tail := &outputTail{}
	tail.Write([]byte("Vercel CLI 48.0.0\n\x1b[32mBuilding\x1b[0m 10%\rBuilding 100%\n"))
	tail.Write([]byte("https://old-abc.vercel.app\nhttps://site-xyz.vercel.app"))
	tail.flush()
	if tail.lines[1] != "Building 100%" {
		t.Fatalf("progress was not cleaned: %q", tail.lines)
	}
	pattern, err := referencePattern(`^(https://\S+\.vercel\.app)$`)
	if err != nil {
		t.Fatal(err)
	}
	if got := findReference(tail.lines, pattern); got != "https://site-xyz.vercel.app" {
		t.Fatalf("reference = %q", got)
	}
	whole, _ := referencePattern(`deployed \S+`)
	if got := findReference([]string{"deployed v1", "deployed v2 now"}, whole); got != "" {
		t.Fatalf("a match with whitespace was recorded: %q", got)
	}
	if got := findReference([]string{"nothing here"}, pattern); got != "" {
		t.Fatalf("recorded %q from nothing", got)
	}
	for i := 0; i < outputTailLines+10; i++ {
		tail.Write([]byte("line\n"))
	}
	if len(tail.lines) != outputTailLines {
		t.Fatalf("the tail kept %d lines", len(tail.lines))
	}
}

func TestATimedOutOrDisconnectedRunIsIndeterminate(t *testing.T) {
	exit := func(code string) error { return exec.Command("sh", "-c", "exit "+code).Run() }
	cases := map[string]string{"0": outcomeCompleted, "1": outcomeFailed, "3": outcomeFailed, "124": outcomeIndeterminate, "137": outcomeIndeterminate, "255": outcomeIndeterminate}
	for code, want := range cases {
		if got := runOutcome(exit(code)); got != want {
			t.Errorf("exit %s = %s, want %s", code, got, want)
		}
	}
}

func TestVerifyWaitsForAnExpectedStatusAndFollowsNoRedirects(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		switch r.URL.Path {
		case "/warming":
			if calls < 2 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case "/moved":
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	if err := verifyExternal(Verify{URL: server.URL + "/warming", Status: []int{204}, Timeout: "10s"}); err != nil {
		t.Fatalf("a release that came up was not verified: %v", err)
	}
	if err := verifyExternal(Verify{URL: server.URL + "/moved", Status: []int{302}, Timeout: "5s"}); err != nil {
		t.Fatalf("a redirect was followed: %v", err)
	}
	err := verifyExternal(Verify{URL: server.URL + "/moved", Timeout: "1s"})
	if err == nil || !strings.Contains(err.Error(), "status 302") {
		t.Fatalf("a wrong status passed: %v", err)
	}
}
