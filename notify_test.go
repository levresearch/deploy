package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// decodeEmbed reads back what would go on the wire, so these tests assert about
// the payload Discord actually receives rather than about our own structs.
func decodeEmbed(t *testing.T, payload []byte) map[string]any {
	t.Helper()

	var message struct {
		Embeds []map[string]any `json:"embeds"`
	}
	if err := json.Unmarshal(payload, &message); err != nil {
		t.Fatalf("the payload is not valid json: %v", err)
	}
	if len(message.Embeds) != 1 {
		t.Fatalf("expected one embed, got %d", len(message.Embeds))
	}

	return message.Embeds[0]
}

func fieldsOf(t *testing.T, embed map[string]any) map[string]string {
	t.Helper()

	found := map[string]string{}
	raw, _ := embed["fields"].([]any)
	for _, entry := range raw {
		field, _ := entry.(map[string]any)
		name, _ := field["name"].(string)
		value, _ := field["value"].(string)
		found[name] = value
	}

	return found
}

func TestTheNotificationSaysWhatHappenedAndWhichServicesMoved(t *testing.T) {
	report := DeployReport{
		Project: "shop", Commit: "9f4be0a", Environment: "production",
		Updated: []string{"api", "web"}, ExitCode: exitOK,
	}

	payload, err := report.DiscordPayload()
	if err != nil {
		t.Fatalf("DiscordPayload: %v", err)
	}

	embed := decodeEmbed(t, payload)
	if title, _ := embed["title"].(string); !strings.Contains(title, "shop") || !strings.Contains(title, "9f4be0a") {
		t.Errorf("the title should name the project and commit, got %q", title)
	}
	if colour, _ := embed["color"].(float64); int(colour) != 0x2ecc71 {
		t.Errorf("a success should be green, got %#x", int(colour))
	}

	fields := fieldsOf(t, embed)
	if fields["Services updated"] != "api, web" {
		t.Errorf("services updated = %q, want the list of stateless services", fields["Services updated"])
	}
	if fields["Environment"] != "production" {
		t.Errorf("environment = %q", fields["Environment"])
	}
	// a destination and a duration are deliberately absent: the channel wants to
	// know what moved, not where from or how long it sat there
	for _, unwanted := range []string{"Destination", "Took"} {
		if _, present := fields[unwanted]; present {
			t.Errorf("the embed should not carry a %s field", unwanted)
		}
	}
	if _, reported := fields["Error"]; reported {
		t.Error("a successful deploy should carry no error field")
	}
}

func TestAFailedDeployIsRedCarriesTheErrorAndDoesNotClaimAnythingUpdated(t *testing.T) {
	report := DeployReport{
		Project: "shop", Commit: "9f4be0a", Environment: "production",
		Updated:  []string{"api", "web"},
		ExitCode: exitDeployFailed,
		Failure:  errors.New("api never became healthy"),
	}

	embed := decodeEmbed(t, mustPayload(t, report))
	if colour, _ := embed["color"].(float64); int(colour) != 0xe74c3c {
		t.Errorf("a failure should be red, got %#x", int(colour))
	}
	if title, _ := embed["title"].(string); !strings.Contains(title, "failed") {
		t.Errorf("the title should say it failed, got %q", title)
	}

	fields := fieldsOf(t, embed)
	if fields["Error"] != "api never became healthy" {
		t.Errorf("the error should be reported, got %q", fields["Error"])
	}
	// the deploy failed, so nothing was updated, and a field saying otherwise
	// would be a notification that lies
	if _, claimed := fields["Services updated"]; claimed {
		t.Error("a failed deploy must not report services as updated")
	}
	if fields["Services attempted"] != "api, web" {
		t.Errorf("a failure should still say what it was trying to move, got %q", fields["Services attempted"])
	}
}

// Exit code 3 is its own outcome: the release is serving but something after the
// cutover needs a human. Folding it into either green or red loses the one case
// where somebody has to go and look.
func TestLiveButNeedsAHumanIsItsOwnColour(t *testing.T) {
	report := DeployReport{
		Project: "shop", Commit: "9f4be0a",
		ExitCode: exitLiveButNeedsAHuman,
		Failure:  errors.New("pruning old releases failed"),
	}

	embed := decodeEmbed(t, mustPayload(t, report))
	if colour, _ := embed["color"].(float64); int(colour) != 0xf39c12 {
		t.Errorf("live but needs a human should be amber, got %#x", int(colour))
	}
	if title, _ := embed["title"].(string); !strings.Contains(title, "live") {
		t.Errorf("the title should say it is live, got %q", title)
	}
}

func TestRecreatedStatefulServicesAreCalledOut(t *testing.T) {
	report := DeployReport{
		Project: "shop", Commit: "9f4be0a", ExitCode: exitOK,
		Updated: []string{"web"}, Recreated: []string{"pg"},
	}

	fields := fieldsOf(t, decodeEmbed(t, mustPayload(t, report)))
	if fields["Stateful services recreated"] != "pg" {
		t.Errorf("a stateful outage should be reported, got %q", fields["Stateful services recreated"])
	}

	// and stays absent on the ordinary deploy, which is almost all of them
	quiet := fieldsOf(t, decodeEmbed(t, mustPayload(t, DeployReport{ExitCode: exitOK, Updated: []string{"web"}})))
	if _, reported := quiet["Stateful services recreated"]; reported {
		t.Error("a deploy that recreated nothing should not mention it")
	}
}

// Discord rejects an oversized field with a 400 nobody reads, and a deploy
// failure message can carry a whole compose log.
func TestAnEnormousErrorIsTrimmedToFitRatherThanRejected(t *testing.T) {
	report := DeployReport{
		Project: "shop", Commit: "9f4be0a", ExitCode: exitDeployFailed,
		Failure: errors.New(strings.Repeat("x", 5000)),
	}

	fields := fieldsOf(t, decodeEmbed(t, mustPayload(t, report)))
	if length := len(fields["Error"]); length > discordFieldLimit {
		t.Errorf("the error field is %d characters, over Discord's %d limit", length, discordFieldLimit)
	}
	if !strings.HasSuffix(fields["Error"], "...") {
		t.Error("a trimmed value should show that it was trimmed")
	}
}

func TestTheNotifierPostsToTheWebhook(t *testing.T) {
	var got []byte
	var contentType string

	webhook := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		got, _ = io.ReadAll(request.Body)
		contentType = request.Header.Get("Content-Type")
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer webhook.Close()

	notifier := &Notifier{webhookURL: webhook.URL, client: webhook.Client()}
	notifier.Send(DeployReport{Project: "shop", Commit: "9f4be0a", ExitCode: exitOK, Updated: []string{"web"}})

	if contentType != "application/json" {
		t.Errorf("content type = %q", contentType)
	}
	if title, _ := decodeEmbed(t, got)["title"].(string); !strings.Contains(title, "shop") {
		t.Errorf("the webhook received %q", title)
	}
}

// A webhook that is down is a webhook that is down, not a failed release. This
// is the property that matters most in the whole file.
func TestAWebhookThatFailsNeverAffectsTheDeploy(t *testing.T) {
	rejecting := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusInternalServerError)
	}))
	defer rejecting.Close()

	// a 500, a connection refused, and no notifier configured at all
	(&Notifier{webhookURL: rejecting.URL, client: rejecting.Client()}).Send(DeployReport{ExitCode: exitOK})
	(&Notifier{webhookURL: "http://127.0.0.1:1", client: &http.Client{Timeout: time.Second}}).Send(DeployReport{ExitCode: exitOK})

	var absent *Notifier
	absent.Send(DeployReport{ExitCode: exitOK})
}

func TestTheWebhookIsReadFromTheEnvironmentOrTheGitignoredSecretsFile(t *testing.T) {
	const variable = "DEPLOY_TEST_WEBHOOK"
	const webhook = "https://discord.com/api/webhooks/1/abc"

	t.Run("from the environment", func(t *testing.T) {
		t.Setenv(variable, webhook)

		notifier, err := NewNotifier(t.TempDir(), &Notify{DiscordWebhookFrom: variable})
		if err != nil {
			t.Fatalf("NewNotifier: %v", err)
		}
		if notifier.webhookURL != webhook {
			t.Errorf("webhook = %q", notifier.webhookURL)
		}
	})

	t.Run("from .deploy/secrets.env when the environment has none", func(t *testing.T) {
		t.Setenv(variable, "")

		repository := t.TempDir()
		writeSecretFixture(t, repository, variable+"="+webhook)

		notifier, err := NewNotifier(repository, &Notify{DiscordWebhookFrom: variable})
		if err != nil {
			t.Fatalf("NewNotifier: %v", err)
		}
		if notifier.webhookURL != webhook {
			t.Errorf("webhook = %q", notifier.webhookURL)
		}
	})

	// CI has no secrets file and sets the variable instead, so the environment
	// has to win rather than the file being preferred because it is more specific
	t.Run("the environment wins over the file", func(t *testing.T) {
		const fromEnvironment = "https://discord.com/api/webhooks/2/from-env"
		t.Setenv(variable, fromEnvironment)

		repository := t.TempDir()
		writeSecretFixture(t, repository, variable+"="+webhook)

		notifier, err := NewNotifier(repository, &Notify{DiscordWebhookFrom: variable})
		if err != nil {
			t.Fatalf("NewNotifier: %v", err)
		}
		if notifier.webhookURL != fromEnvironment {
			t.Errorf("webhook = %q, want the environment to win", notifier.webhookURL)
		}
	})

	// a project that means to be told about its deploys should find out it cannot
	// be before it waits on a build, not after
	t.Run("neither is a precondition failure that names both places", func(t *testing.T) {
		t.Setenv(variable, "")

		_, err := NewNotifier(t.TempDir(), &Notify{DiscordWebhookFrom: variable})
		if err == nil {
			t.Fatal("a configured webhook that cannot be found must fail loudly")
		}
		if !strings.Contains(err.Error(), variable) || !strings.Contains(err.Error(), secretsFileName) {
			t.Errorf("the message should name the variable and the file, got: %v", err)
		}
	})

	t.Run("no notify block means no notifier and no error", func(t *testing.T) {
		notifier, err := NewNotifier(t.TempDir(), nil)
		if err != nil || notifier != nil {
			t.Errorf("a project without notifications should get none, got %v %v", notifier, err)
		}
	})
}

func TestParseEnvFile(t *testing.T) {
	parsed := ParseEnvFile([]byte(`
# a comment
WEBHOOK=https://discord.com/api/webhooks/1/abc
QUOTED="quoted value"
SINGLE='single value'
export EXPORTED=exported
EMPTY=
NOT_AN_ASSIGNMENT
`))

	for name, want := range map[string]string{
		"WEBHOOK":  "https://discord.com/api/webhooks/1/abc",
		"QUOTED":   "quoted value",
		"SINGLE":   "single value",
		"EXPORTED": "exported",
		"EMPTY":    "",
	} {
		if parsed[name] != want {
			t.Errorf("%s = %q, want %q", name, parsed[name], want)
		}
	}
	if _, found := parsed["NOT_AN_ASSIGNMENT"]; found {
		t.Error("a line with no = is not an assignment")
	}
	if _, found := parsed["# a comment"]; found {
		t.Error("comments are not assignments")
	}
}

func writeSecretFixture(t *testing.T, repository, line string) {
	t.Helper()

	directory := filepath.Join(repository, deployDirectoryName)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatalf("creating %s: %v", directory, err)
	}
	if err := os.WriteFile(filepath.Join(directory, secretsFileName), []byte(line+"\n"), 0o600); err != nil {
		t.Fatalf("writing secrets: %v", err)
	}
}

func mustPayload(t *testing.T, report DeployReport) []byte {
	t.Helper()

	payload, err := report.DiscordPayload()
	if err != nil {
		t.Fatalf("DiscordPayload: %v", err)
	}

	return payload
}

// End to end, because everything above tests the notifier in isolation and the
// question that actually matters is whether a real deploy sends the right thing
// and, more importantly, whether a webhook having a bad day can break a release.
func TestARealDeployNotifiesAndABrokenWebhookCannotFailIt(t *testing.T) {
	dockerAvailable(t)

	const variable = "DEPLOY_TEST_E2E_WEBHOOK"

	received := make(chan []byte, 4)
	webhook := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		received <- body
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer webhook.Close()

	repository := newRepository(t)
	writeFile(t, repository, configFileName, `{
      "version": 1,
      "id": "dd000015",
      "name": "notified",
      "notify": {"discordWebhookFrom": "`+variable+`"},
      "services": {
        "app": {
          "image": "busybox:latest",
          "stateful": false,
          "command": ["sh", "-c", "sleep 300"],
          "healthcheck": {"command": ["CMD", "true"], "interval": "1s", "retries": 5}
        }
      }
    }`)
	commit := commitFile(t, repository, "one.txt", "x")

	destination := t.TempDir()
	t.Cleanup(func() {
		exec.Command("docker", "compose", "--project-name", ProjectName("dd000015", commit), "down").Run()
		exec.Command("docker", "network", "rm", NetworkName("dd000015")).Run()
	})

	options := DeployOptions{
		Context: repository, Destination: destination, Environment: defaultEnvironmentName,
	}

	t.Setenv(variable, webhook.URL)
	if _, err := RunDeploy(options); err != nil {
		t.Fatalf("the deploy should succeed: %v", err)
	}

	select {
	case payload := <-received:
		embed := decodeEmbed(t, payload)
		if colour, _ := embed["color"].(float64); int(colour) != 0x2ecc71 {
			t.Errorf("a successful deploy should notify green, got %#x", int(colour))
		}
		if title, _ := embed["title"].(string); !strings.Contains(title, "notified") {
			t.Errorf("the notification should name the project, got %q", title)
		}
		if fields := fieldsOf(t, embed); fields["Services updated"] != "app" {
			t.Errorf("services updated = %q, want app", fields["Services updated"])
		}
	case <-time.After(15 * time.Second):
		t.Fatal("a successful deploy sent no notification")
	}

	// now point it at something that cannot possibly answer, and redeploy. the
	// release still has to go out, because a notification is a report about a
	// deploy and never part of one
	t.Setenv(variable, "http://127.0.0.1:1/api/webhooks/nothing")
	second := commitFile(t, repository, "one.txt", "y")
	t.Cleanup(func() {
		exec.Command("docker", "compose", "--project-name", ProjectName("dd000015", second), "down").Run()
	})

	exitCode, err := RunDeploy(options)
	if err != nil {
		t.Fatalf("a dead webhook must not fail a deploy: %v", err)
	}
	if exitCode != exitOK {
		t.Errorf("exit code = %d, want %d, a dead webhook is not a failed release", exitCode, exitOK)
	}

	state, err := ReadState(LocalRunner{}, NewLayout(destination, "dd000015"))
	if err != nil {
		t.Fatalf("ReadState: %v", err)
	}
	if state.Current != ShortCommit(second) {
		t.Errorf("current = %q, want the release that went out anyway %q", state.Current, ShortCommit(second))
	}
}

func TestARollbackNotificationSaysWhereItCameFromAndWhereItLanded(t *testing.T) {
	report := DeployReport{
		Action: actionRollback, Project: "shop", Commit: "9f4be0a", From: "c0ffee1",
		Environment: "production", Updated: []string{"web"}, ExitCode: exitOK,
	}

	embed := decodeEmbed(t, mustPayload(t, report))
	title, _ := embed["title"].(string)
	if !strings.Contains(title, "Rolled") || !strings.Contains(title, "9f4be0a") {
		t.Errorf("the title should say it rolled back and where to, got %q", title)
	}
	// a rollback is defined by the pair, so the release it left has to be there
	if fields := fieldsOf(t, embed); fields["Rolled back from"] != "c0ffee1" {
		t.Errorf("rolled back from = %q, want c0ffee1", fields["Rolled back from"])
	}

	failed := DeployReport{
		Action: actionRollback, Project: "shop", Commit: "9f4be0a",
		ExitCode: exitDeployFailed, Failure: errors.New("target release is gone"),
	}
	if title, _ := decodeEmbed(t, mustPayload(t, failed))["title"].(string); !strings.Contains(title, "Rollback failed") {
		t.Errorf("a failed rollback should say so rather than blaming a deploy, got %q", title)
	}
}

// A destroy that took the volumes and one that did not are different events, and
// only one of them is recoverable.
func TestADestroyNotificationDistinguishesDataKeptFromDataGone(t *testing.T) {
	kept := DeployReport{
		Action: actionDestroy, Project: "shop", Environment: "production",
		Updated: []string{"pg", "web"}, ExitCode: exitOK,
	}
	keptEmbed := decodeEmbed(t, mustPayload(t, kept))
	if title, _ := keptEmbed["title"].(string); !strings.Contains(title, "data kept") {
		t.Errorf("a destroy that kept the volumes should say so, got %q", title)
	}
	keptFields := fieldsOf(t, keptEmbed)
	if !strings.Contains(keptFields["Volumes"], "kept") {
		t.Errorf("volumes = %q", keptFields["Volumes"])
	}
	if keptFields["Services removed"] != "pg, web" {
		t.Errorf("a destroy removes services rather than updating them, got %q", keptFields["Services removed"])
	}

	gone := DeployReport{
		Action: actionDestroy, Project: "shop", VolumesRemoved: true, ExitCode: exitOK,
	}
	goneEmbed := decodeEmbed(t, mustPayload(t, gone))
	if title, _ := goneEmbed["title"].(string); !strings.Contains(title, "and its data") {
		t.Errorf("a destroy that removed the volumes must say so, got %q", title)
	}
	if volumes := fieldsOf(t, goneEmbed)["Volumes"]; !strings.Contains(volumes, "gone") {
		t.Errorf("volumes = %q, want it to say the data is gone", volumes)
	}
}

// A report with no action filled in still has to produce a sentence, since that
// is what every existing deploy caller looked like before rollback and destroy
// were wired up.
func TestAReportWithNoActionReadsAsADeploy(t *testing.T) {
	embed := decodeEmbed(t, mustPayload(t, DeployReport{Project: "shop", Commit: "9f4be0a", ExitCode: exitOK}))
	if title, _ := embed["title"].(string); !strings.HasPrefix(title, "Deployed") {
		t.Errorf("title = %q", title)
	}
}

// End to end for the two commands that were not wired up before, because the
// payload tests above would all still pass if RunRollback and RunDestroy simply
// never called the notifier.
func TestARealRollbackAndDestroyBothNotify(t *testing.T) {
	dockerAvailable(t)

	const variable = "DEPLOY_TEST_LIFECYCLE_WEBHOOK"

	received := make(chan []byte, 8)
	webhook := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		received <- body
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer webhook.Close()
	t.Setenv(variable, webhook.URL)

	repository := newRepository(t)
	writeFile(t, repository, configFileName, `{
      "version": 1,
      "id": "dd000016",
      "name": "lifecycle",
      "notify": {"discordWebhookFrom": "`+variable+`"},
      "services": {
        "app": {
          "image": "busybox:latest",
          "stateful": false,
          "command": ["sh", "-c", "sleep 300"],
          "healthcheck": {"command": ["CMD", "true"], "interval": "1s", "retries": 5}
        }
      }
    }`)

	destination := t.TempDir()
	t.Cleanup(func() { exec.Command("docker", "network", "rm", NetworkName("dd000016")).Run() })

	options := DeployOptions{
		Context: repository, Destination: destination, Environment: defaultEnvironmentName,
	}

	// two deploys so there is something to roll back to. committing moves HEAD,
	// so deploying after each one needs no checkout
	var second string
	for _, contents := range []string{"first", "second"} {
		commit := commitFile(t, repository, "one.txt", contents)
		t.Cleanup(func() {
			exec.Command("docker", "compose", "--project-name", ProjectName("dd000016", commit), "down").Run()
		})
		second = commit

		if _, err := RunDeploy(options); err != nil {
			t.Fatalf("deploying %s: %v", ShortCommit(commit), err)
		}
		drain(t, received, "deploy")
	}

	if _, err := RunRollback(options, ""); err != nil {
		t.Fatalf("the rollback should succeed: %v", err)
	}

	embed := awaitNotification(t, received, "rollback")
	title, _ := embed["title"].(string)
	if !strings.Contains(title, "Rolled") {
		t.Errorf("a rollback should notify as a rollback, got %q", title)
	}
	if fields := fieldsOf(t, embed); fields["Rolled back from"] != ShortCommit(second) {
		t.Errorf("rolled back from = %q, want %q", fields["Rolled back from"], ShortCommit(second))
	}

	if _, err := RunDestroy(options, false, strings.NewReader("lifecycle\n")); err != nil {
		t.Fatalf("the destroy should succeed: %v", err)
	}

	destroyed := awaitNotification(t, received, "destroy")
	if title, _ := destroyed["title"].(string); !strings.Contains(title, "Destroyed") {
		t.Errorf("a destroy should notify as a destroy, got %q", title)
	}
	// this destroy kept the volumes, and saying otherwise would be alarming
	if volumes := fieldsOf(t, destroyed)["Volumes"]; !strings.Contains(volumes, "kept") {
		t.Errorf("volumes = %q, want it to say the data was kept", volumes)
	}
}

func awaitNotification(t *testing.T, received chan []byte, what string) map[string]any {
	t.Helper()

	select {
	case payload := <-received:
		return decodeEmbed(t, payload)
	case <-time.After(15 * time.Second):
		t.Fatalf("the %s sent no notification", what)

		return nil
	}
}

func drain(t *testing.T, received chan []byte, what string) {
	t.Helper()

	select {
	case <-received:
	case <-time.After(15 * time.Second):
		t.Fatalf("the %s sent no notification", what)
	}
}
