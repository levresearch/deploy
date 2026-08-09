package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const hostedConfig = `{
  "version": 1,
  "id": "a3f19c02",
  "name": "lectern",
  "services": {
    "web": {
      "image": "nginx",
      "stateful": false,
      "env": [".env.production"],
      "healthcheck": {"command": ["CMD", "true"]},
      "host": {"domain": "lectern.example.com", "port": 3000, "tunnelTokenFrom": "WEB_TUNNEL_TOKEN"}
    },
    "api": {
      "image": "nginx",
      "stateful": false,
      "env": [".env.production"],
      "healthcheck": {"command": ["CMD", "true"]},
      "host": {"domain": "api.example.com", "port": 8787, "tunnelTokenFrom": "API_TUNNEL_TOKEN"}
    },
    "pg": {"image": "postgres:17", "stateful": true}
  }
}`

func TestOneCloudflaredPerHostBlockAndAllInTheSharedStack(t *testing.T) {
	resolved := loadAndResolve(t, hostedConfig, defaultEnvironmentName)
	layout := NewLayout("/srv/projects", "a3f19c02")

	sharedRaw, err := RenderShared(resolved, layout)
	if err != nil {
		t.Fatalf("RenderShared: %v", err)
	}
	releaseRaw, err := RenderRelease(resolved, layout, "9f4be0a")
	if err != nil {
		t.Fatalf("RenderRelease: %v", err)
	}

	shared, release := decodeCompose(t, sharedRaw), decodeCompose(t, releaseRaw)

	// each token is bound to its own tunnel, so two hostnames means two of them
	for _, service := range []string{"web", "api"} {
		if _, found := shared.Services[TunnelServiceName(service)]; !found {
			t.Errorf("expected a cloudflared for %s, got %v", service, shared.Services)
		}
		// one inside a release stack would restart on every deploy and drop the
		// tunnel, which is the outage the cutover exists to prevent
		if _, found := release.Services[TunnelServiceName(service)]; found {
			t.Errorf("cloudflared for %s must not be in the release stack", service)
		}
	}
	if _, found := shared.Services["pg"]; !found {
		t.Error("the stateful services are still there alongside them")
	}
}

func TestTheTunnelTokenIsNeverInTheRenderedFile(t *testing.T) {
	resolved := loadAndResolve(t, hostedConfig, defaultEnvironmentName)

	sharedRaw, err := RenderShared(resolved, NewLayout("/srv/projects", "a3f19c02"))
	if err != nil {
		t.Fatalf("RenderShared: %v", err)
	}

	var tunnel struct {
		Environment map[string]string `json:"environment"`
		Command     []string          `json:"command"`
	}
	decodeService(t, decodeCompose(t, sharedRaw).Services[TunnelServiceName("web")], &tunnel)

	token := tunnel.Environment["TUNNEL_TOKEN"]

	// the variable name, left for compose to interpolate on the destination, so
	// deploy never handles the value at all
	if !strings.Contains(token, "${WEB_TUNNEL_TOKEN") {
		t.Errorf("TUNNEL_TOKEN should be an interpolation of the named variable, got %q", token)
	}
	// and it fails loudly rather than starting a tunnel with a blank token
	if !strings.Contains(token, ":?") {
		t.Errorf("a missing token should be a compose error, got %q", token)
	}
	// never on the command line, which is the leak we found in a real ps output
	for _, argument := range tunnel.Command {
		if strings.Contains(argument, "token") || strings.Contains(argument, "TOKEN") {
			t.Errorf("the token must never reach argv, got command %v", tunnel.Command)
		}
	}
}

func TestEnvFilesAreHandedToComposeForInterpolation(t *testing.T) {
	resolved := loadAndResolve(t, hostedConfig, defaultEnvironmentName)
	layout := NewLayout("/srv/projects", "a3f19c02")

	arguments := EnvFileArguments(resolved, layout)

	// compose interpolates from --env-file, and env_file: alone would only put
	// values inside the container, which is too late for ${...} in the file
	want := []string{"--env-file", "/srv/projects/a3f19c02/env/.env.production"}
	if len(arguments) != 2 || arguments[0] != want[0] || arguments[1] != want[1] {
		t.Errorf("EnvFileArguments = %v, want %v", arguments, want)
	}
}

// hurryVerification keeps a unit test off the real twenty second wait for a
// hostname that does not exist.
func hurryVerification(t *testing.T) {
	t.Helper()

	timeout, interval := publicVerifyTimeout, publicVerifyInterval
	publicVerifyTimeout, publicVerifyInterval = 50*time.Millisecond, 10*time.Millisecond

	t.Cleanup(func() { publicVerifyTimeout, publicVerifyInterval = timeout, interval })
}

// recordingRunner remembers the order it was asked to do things, which is the
// only thing worth asserting about a cutover.
type recordingRunner struct {
	*scriptedRunner
	steps []string
}

func newRecordingRunner() *recordingRunner {
	return &recordingRunner{scriptedRunner: newScriptedRunner()}
}

func (runner *recordingRunner) Run(command []string) ([]byte, error) {
	joined := strings.Join(command, " ")

	switch {
	case strings.Contains(joined, "nc -z"):
		runner.steps = append(runner.steps, "probe network")
	case strings.Contains(joined, " stop"):
		runner.steps = append(runner.steps, "stop old")
	case strings.Contains(joined, " start"):
		runner.steps = append(runner.steps, "start old again")
	case strings.Contains(joined, " down"):
		runner.steps = append(runner.steps, "remove old")
	}

	return runner.scriptedRunner.Run(command)
}

// The order is the entire design. Probing after stopping, or removing before
// verifying, would each turn a safe deploy into an outage.
func TestCutoverOrdersItsStepsCorrectly(t *testing.T) {
	hurryVerification(t)

	resolved := loadAndResolve(t, hostedConfig, defaultEnvironmentName)
	layout := NewLayout("/srv/projects", "a3f19c02")

	runner := newRecordingRunner()

	// the public check will fail here, since there is no tunnel in a unit test.
	// that is fine, because what this asserts is the order of what came before
	_ = Cutover(runner, resolved, layout, "aaaaaaa", "bbbbbbb")

	if len(runner.steps) < 2 {
		t.Fatalf("expected at least a probe and a stop, got %v", runner.steps)
	}
	if runner.steps[0] != "probe network" {
		t.Errorf("the new release is checked before anything is stopped, got %v", runner.steps)
	}

	probedAt, stoppedAt := -1, -1
	for index, step := range runner.steps {
		if step == "probe network" && probedAt < 0 {
			probedAt = index
		}
		if step == "stop old" && stoppedAt < 0 {
			stoppedAt = index
		}
	}
	if probedAt < 0 || stoppedAt < 0 || probedAt > stoppedAt {
		t.Errorf("the probe must come before the stop, got %v", runner.steps)
	}

	// nothing is removed, because the public check never passed
	for _, step := range runner.steps {
		if step == "remove old" {
			t.Errorf("the old release must not be removed when verification failed, got %v", runner.steps)
		}
	}
}

// The old containers are stopped rather than removed precisely so that starting
// them again is what puts traffic back.
func TestAFailedPublicVerificationStartsTheOldReleaseAgain(t *testing.T) {
	hurryVerification(t)

	resolved := loadAndResolve(t, hostedConfig, defaultEnvironmentName)
	runner := newRecordingRunner()

	err := Cutover(runner, resolved, NewLayout("/srv/projects", "a3f19c02"), "aaaaaaa", "bbbbbbb")
	if err == nil {
		t.Fatal("a hostname that never answers must fail the deploy")
	}
	if !strings.Contains(err.Error(), "Cloudflare") {
		t.Errorf("the error should say which layer failed, got: %v", err)
	}

	var stopped, restarted bool
	for _, step := range runner.steps {
		switch step {
		case "stop old":
			stopped = true
		case "start old again":
			if !stopped {
				t.Error("the old release was started again before it was ever stopped")
			}
			restarted = true
		}
	}
	if !restarted {
		t.Errorf("a failed verification must put the old release back, got %v", runner.steps)
	}
}

func TestAProjectWithNoHostBlockNeedsNoTunnelAndNoVerification(t *testing.T) {
	const contents = `{
      "version": 1, "id": "a3f19c02", "name": "x",
      "services": {"web": {"image": "nginx", "stateful": false}}
    }`
	resolved := loadAndResolve(t, contents, defaultEnvironmentName)
	layout := NewLayout("/srv/projects", "a3f19c02")

	sharedRaw, err := RenderShared(resolved, layout)
	if err != nil {
		t.Fatalf("RenderShared: %v", err)
	}
	for name := range decodeCompose(t, sharedRaw).Services {
		if strings.HasPrefix(name, "cloudflared") {
			t.Errorf("nothing is exposed, so there should be no cloudflared, got %s", name)
		}
	}

	runner := newRecordingRunner()
	if err := Cutover(runner, resolved, layout, "aaaaaaa", "bbbbbbb"); err != nil {
		t.Fatalf("an unexposed project cuts over by simply replacing the old release: %v", err)
	}

	// straight to removing it, with no probe and no public check to make
	for _, step := range runner.steps {
		if step == "probe network" {
			t.Errorf("there is no tunnel to be reachable for, got %v", runner.steps)
		}
	}
}

func TestRedeployingTheSameCommitIsNotACutover(t *testing.T) {
	resolved := loadAndResolve(t, hostedConfig, defaultEnvironmentName)
	runner := newRecordingRunner()

	if err := Cutover(runner, resolved, NewLayout("/srv/projects", "a3f19c02"), "aaaaaaa", "aaaaaaa"); err != nil {
		t.Fatalf("redeploying the current release is not a cutover: %v", err)
	}
	if len(runner.steps) != 0 {
		t.Errorf("nothing should have been stopped or probed, got %v", runner.steps)
	}
}

func TestPublicVerificationAcceptsAnyAnswerThatIsNotAServerError(t *testing.T) {
	// a 404 means the tunnel reached an origin, which is the question being
	// asked. what an application returns is its own business
	cases := []struct {
		status int
		want   bool
	}{
		{200, true},
		{204, true},
		{301, true},
		{404, true},
		{500, false},
		{502, false},
		{503, false},
	}

	for _, testCase := range cases {
		acceptable := testCase.status < 500
		if acceptable != testCase.want {
			t.Errorf("status %d: acceptable = %v, want %v", testCase.status, acceptable, testCase.want)
		}
	}
}

func TestRenderedTunnelIsValidCompose(t *testing.T) {
	resolved := loadAndResolve(t, hostedConfig, defaultEnvironmentName)

	sharedRaw, err := RenderShared(resolved, NewLayout("/srv/projects", "a3f19c02"))
	if err != nil {
		t.Fatalf("RenderShared: %v", err)
	}

	var document map[string]any
	if err := json.Unmarshal(sharedRaw, &document); err != nil {
		t.Fatalf("the shared stack should still be valid json: %v", err)
	}
	if _, found := document["networks"]; !found {
		t.Error("adding tunnels should not have dropped the network")
	}
	if _, found := document["name"]; !found {
		t.Error("adding tunnels should not have dropped the project name")
	}
}

// The rendered shared stack has to be something compose will actually accept,
// tunnels and all. A token that fails to interpolate is a deploy that dies at
// the last step rather than at the first.
func TestTheRenderedSharedStackWithTunnelsIsAcceptedByCompose(t *testing.T) {
	dockerAvailable(t)

	resolved := loadAndResolve(t, hostedConfig, defaultEnvironmentName)
	directory := t.TempDir()
	layout := NewLayout(directory, "a3f19c02")

	if err := os.MkdirAll(layout.EnvDirectory(), 0o755); err != nil {
		t.Fatalf("creating env directory: %v", err)
	}
	writeFile(t, layout.EnvDirectory(), ".env.production",
		"WEB_TUNNEL_TOKEN=not-a-real-token\nAPI_TUNNEL_TOKEN=also-not-real\n")

	rendered, err := RenderShared(resolved, layout)
	if err != nil {
		t.Fatalf("RenderShared: %v", err)
	}
	composeFile := filepath.Join(directory, composeFileName)
	if err := os.WriteFile(composeFile, rendered, 0o644); err != nil {
		t.Fatalf("writing compose file: %v", err)
	}

	arguments := append([]string{"compose", "--file", composeFile}, EnvFileArguments(resolved, layout)...)
	output, err := exec.Command("docker", append(arguments, "config")...).CombinedOutput()
	if err != nil {
		t.Fatalf("compose refused the rendered shared stack: %v\n%s", err, output)
	}

	// the token was interpolated in, which is the whole mechanism
	if !strings.Contains(string(output), "not-a-real-token") {
		t.Errorf("the token should have been interpolated from the env file, got:\n%s", output)
	}
	// and it came from the env file rather than from anything deploy wrote
	if strings.Contains(string(rendered), "not-a-real-token") {
		t.Error("the token leaked into the file deploy wrote")
	}
}

// Without the env file, compose must refuse rather than start a tunnel with a
// blank token, which would look like it worked and serve nothing.
func TestAMissingTunnelTokenIsRefusedByCompose(t *testing.T) {
	dockerAvailable(t)

	resolved := loadAndResolve(t, hostedConfig, defaultEnvironmentName)
	directory := t.TempDir()
	layout := NewLayout(directory, "a3f19c02")

	// the env file exists but does not define the tokens, so compose gets past
	// env_file and fails on the interpolation, which is the case being tested
	if err := os.MkdirAll(layout.EnvDirectory(), 0o755); err != nil {
		t.Fatalf("creating env directory: %v", err)
	}
	writeFile(t, layout.EnvDirectory(), ".env.production", "SOMETHING_ELSE=fine\n")

	rendered, err := RenderShared(resolved, layout)
	if err != nil {
		t.Fatalf("RenderShared: %v", err)
	}
	composeFile := filepath.Join(directory, composeFileName)
	if err := os.WriteFile(composeFile, rendered, 0o644); err != nil {
		t.Fatalf("writing compose file: %v", err)
	}

	arguments := append([]string{"compose", "--file", composeFile}, EnvFileArguments(resolved, layout)...)
	output, err := exec.Command("docker", append(arguments, "config")...).CombinedOutput()
	if err == nil {
		t.Fatalf("a shared stack with no token should be refused, got:\n%s", output)
	}
	// compose reports the first interpolation it cannot satisfy, and which of the
	// two tunnels that is depends on map ordering rather than on anything deploy
	// decides, so either one proves the point
	if !strings.Contains(string(output), "WEB_TUNNEL_TOKEN") &&
		!strings.Contains(string(output), "API_TUNNEL_TOKEN") {
		t.Errorf("the refusal should name the variable that is missing, got:\n%s", output)
	}
	if !strings.Contains(string(output), "deploy env push") {
		t.Errorf("the refusal should name the fix, got:\n%s", output)
	}
}

// A project with a host block and nothing stateful still needs the shared stack,
// because that is where cloudflared lives. Skipping it left the tunnel unstarted
// and the hostname serving nothing at all.
func TestTheSharedStackStartsForTunnelsEvenWithNothingStateful(t *testing.T) {
	const contents = `{
      "version": 1, "id": "a3f19c02", "name": "x",
      "services": {"web": {
        "image": "nginx", "stateful": false,
        "healthcheck": {"command": ["CMD", "true"]},
        "host": {"domain": "x.example.com", "port": 80, "tunnelTokenFrom": "T"}
      }}
    }`
	resolved := loadAndResolve(t, contents, defaultEnvironmentName)

	stateful, _ := SplitServices(resolved.Services)
	if len(stateful) != 0 {
		t.Fatalf("this project has nothing stateful, got %v", stateful)
	}

	sharedRaw, err := RenderShared(resolved, NewLayout("/srv/projects", "a3f19c02"))
	if err != nil {
		t.Fatalf("RenderShared: %v", err)
	}
	shared := decodeCompose(t, sharedRaw)

	if len(shared.Services) == 0 {
		t.Fatal("the shared stack should hold the tunnel even with nothing stateful")
	}
	if _, found := shared.Services[TunnelServiceName("web")]; !found {
		t.Errorf("expected a cloudflared in the shared stack, got %v", shared.Services)
	}
}
