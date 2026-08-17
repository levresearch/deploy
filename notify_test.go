package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
	notifier.SendStage(stageLive, "shop", []string{"shop.example.com"}, StageVersions{Incoming: "9f4be0a"})

	if contentType != "application/json" {
		t.Errorf("content type = %q", contentType)
	}
	if title, _ := decodeEmbed(t, got)["title"].(string); title != "update live" {
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
	(&Notifier{webhookURL: rejecting.URL, client: rejecting.Client()}).SendStage(stageLive, "shop", nil, StageVersions{})
	(&Notifier{webhookURL: "http://127.0.0.1:1", client: &http.Client{Timeout: time.Second}}).
		SendStage(stageLive, "shop", nil, StageVersions{})

	var absent *Notifier
	absent.SendStage(stageLive, "shop", nil, StageVersions{})
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

// The channel is read by the people using the thing, so the wording is checked
// here rather than left to whatever the code happens to say.
func TestTheStageMessagesReadForSomebodyWhoDoesNotKnowWhatACommitIs(t *testing.T) {
	versions := StageVersions{Previous: "abc1234", Incoming: "def5678"}

	cases := []struct {
		stage      DeployStage
		wantTitle  string
		wantBody   string
		wantColour int
		wantLine   string
	}{
		{stageReady, "new version ready", "lmc is built and healthy, but nothing has changed for you yet", 0x3498db, "abc1234 -> def5678"},
		{stageSwitching, "switching over", "moving to the new version now, usually without any interruption", 0xf39c12, "abc1234 -> def5678"},
		{stageLive, "update live", "the new version is up and answering", 0x2ecc71, "abc1234 -> def5678"},
		// the one stage that ends up back where it started, so the arrow turns
		{stageCancelled, "update cancelled", "something went wrong, so the previous version is back", 0xe74c3c, "abc1234 <- def5678"},
	}

	for _, testCase := range cases {
		t.Run(testCase.wantTitle, func(t *testing.T) {
			payload, err := StagePayload(testCase.stage, "lmc", []string{"lecternmc.com", "bot"}, versions)
			if err != nil {
				t.Fatalf("StagePayload: %v", err)
			}

			embed := decodeEmbed(t, payload)
			if title, _ := embed["title"].(string); title != testCase.wantTitle {
				t.Errorf("title = %q, want %q", title, testCase.wantTitle)
			}
			if body, _ := embed["description"].(string); body != testCase.wantBody {
				t.Errorf("body = %q, want %q", body, testCase.wantBody)
			}
			if colour, _ := embed["color"].(float64); int(colour) != testCase.wantColour {
				t.Errorf("colour = %#x, want %#x", int(colour), testCase.wantColour)
			}

			fields := fieldsOf(t, embed)
			if fields["version"] != testCase.wantLine {
				t.Errorf("version = %q, want %q", fields["version"], testCase.wantLine)
			}
			// hostnames, because somebody reading the channel knows the site by its
			// domain and has never heard of the container behind it
			if fields["affected"] != "lecternmc.com, bot" {
				t.Errorf("affected = %q", fields["affected"])
			}
			for _, jargon := range []string{"commit", "container", "stateless", "cutover", "healthcheck"} {
				body, _ := embed["description"].(string)
				if strings.Contains(strings.ToLower(body), jargon) {
					t.Errorf("the body says %q, which means nothing to somebody using the site", jargon)
				}
			}
		})
	}
}

// A first deploy has nothing to move away from, so an arrow would be pointing
// away from nothing.
func TestAFirstDeployNamesOneVersionRatherThanATransition(t *testing.T) {
	fields := fieldsOf(t, decodeEmbed(t, mustStagePayload(t, stageLive, StageVersions{Incoming: "def5678"})))
	if fields["version"] != "def5678" {
		t.Errorf("version = %q, want just the one release", fields["version"])
	}

	// and redeploying what is already current is not a transition either
	same := fieldsOf(t, decodeEmbed(t, mustStagePayload(t, stageLive, StageVersions{Previous: "def5678", Incoming: "def5678"})))
	if same["version"] != "def5678" {
		t.Errorf("version = %q, want just the one release", same["version"])
	}
}

func TestAffectedNamesPrefersDomainsOverServiceNames(t *testing.T) {
	hosted := func(domain string) Service {
		return Service{Host: &Host{Domain: domain, Port: 3000}}
	}

	got := AffectedNames(map[string]Service{
		"web":     hosted("lecternmc.com"),
		"api":     hosted("api.lecternmc.com"),
		"support": hosted("support.lecternmc.com"),
		"bot":     {},
	})

	want := []string{"api.lecternmc.com", "bot", "lecternmc.com", "support.lecternmc.com"}
	if !slices.Equal(got, want) {
		t.Errorf("AffectedNames = %v, want %v", got, want)
	}
}

func mustStagePayload(t *testing.T, stage DeployStage, versions StageVersions) []byte {
	t.Helper()

	payload, err := StagePayload(stage, "lmc", nil, versions)
	if err != nil {
		t.Fatalf("StagePayload: %v", err)
	}

	return payload
}

// End to end, because everything above tests the wording and none of it proves
// RunDeploy says the right things at the right moments, which is the part that
// actually reaches anybody.
func TestARealDeploySendsTheArcInOrderAndSaysNothingBeforeThereIsAnythingToSay(t *testing.T) {
	dockerAvailable(t)

	const variable = "DEPLOY_TEST_ARC_WEBHOOK"

	received := make(chan map[string]any, 16)
	webhook := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		var message struct {
			Embeds []map[string]any `json:"embeds"`
		}
		json.Unmarshal(body, &message)
		received <- message.Embeds[0]
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer webhook.Close()
	t.Setenv(variable, webhook.URL)

	repository := newRepository(t)
	writeFile(t, repository, configFileName, `{
      "version": 1,
      "id": "dd000017",
      "name": "arc",
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
	t.Cleanup(func() { exec.Command("docker", "network", "rm", NetworkName("dd000017")).Run() })

	options := DeployOptions{
		Context: repository, Destination: destination, Environment: defaultEnvironmentName,
	}

	// nothing to move away from yet, so the arc is ready then live
	first := commitFile(t, repository, "one.txt", "first")
	t.Cleanup(func() {
		exec.Command("docker", "compose", "--project-name", ProjectName("dd000017", first), "down").Run()
	})
	if _, err := RunDeploy(options); err != nil {
		t.Fatalf("the first deploy should succeed: %v", err)
	}

	titlesOf(t, received, []string{"new version ready", "update live"})

	// a second deploy has something to switch away from, so all three fire
	second := commitFile(t, repository, "one.txt", "second")
	t.Cleanup(func() {
		exec.Command("docker", "compose", "--project-name", ProjectName("dd000017", second), "down").Run()
	})
	if _, err := RunDeploy(options); err != nil {
		t.Fatalf("the second deploy should succeed: %v", err)
	}

	arc := titlesOf(t, received, []string{"new version ready", "switching over", "update live"})
	wantLine := ShortCommit(first) + " -> " + ShortCommit(second)
	for _, embed := range arc {
		if version := fieldsOf(t, embed)["version"]; version != wantLine {
			t.Errorf("version = %q, want %q", version, wantLine)
		}
	}

	// a dirty tree never reaches a build, and is nobody's business but the
	// operator's, so the channel hears nothing at all
	writeFile(t, repository, "one.txt", "uncommitted")
	if _, err := RunDeploy(options); err == nil {
		t.Fatal("a dirty tree should fail")
	}

	select {
	case embed := <-received:
		t.Errorf("a failure before any build told users %q", embed["title"])
	case <-time.After(2 * time.Second):
	}
}

// titlesOf drains exactly the expected number of notifications and checks their
// order, since the order is the message.
func titlesOf(t *testing.T, received chan map[string]any, want []string) []map[string]any {
	t.Helper()

	var got []string
	var embeds []map[string]any

	for range want {
		select {
		case embed := <-received:
			title, _ := embed["title"].(string)
			got = append(got, title)
			embeds = append(embeds, embed)
		case <-time.After(20 * time.Second):
			t.Fatalf("expected %v, only got %v", want, got)
		}
	}

	if !slices.Equal(got, want) {
		t.Errorf("the arc was %v, want %v", got, want)
	}

	return embeds
}

func TestTheRollbackAndDowntimeMessages(t *testing.T) {
	// a rollback lands on the earlier release, so the arrow turns the same way
	// the cancelled one does
	rollingBack := decodeEmbed(t, mustStage(t, stageRollingBack, "lmc",
		StageVersions{Previous: "abc1234", Incoming: "def5678"}))

	if title, _ := rollingBack["title"].(string); title != "rolling back" {
		t.Errorf("title = %q", title)
	}
	if body, _ := rollingBack["description"].(string); body != "we are going back to an earlier version, usually without any interruption" {
		t.Errorf("body = %q", body)
	}
	if colour, _ := rollingBack["color"].(float64); int(colour) != 0xf39c12 {
		t.Errorf("colour = %#x, want amber", int(colour))
	}
	if version := fieldsOf(t, rollingBack)["version"]; version != "abc1234 <- def5678" {
		t.Errorf("version = %q, want the arrow pointing at the release being returned to", version)
	}

	// a rollback that never moved anything says so, and names the release still
	// serving rather than an arrow for a move that did not happen
	failed := decodeEmbed(t, mustStage(t, stageRollbackFailed, "lmc", StageVersions{Incoming: "def5678"}))
	if title, _ := failed["title"].(string); title != "rollback failed" {
		t.Errorf("title = %q", title)
	}
	if body, _ := failed["description"].(string); body != "the earlier version could not be started, so nothing changed" {
		t.Errorf("body = %q", body)
	}
	if version := fieldsOf(t, failed)["version"]; version != "def5678" {
		t.Errorf("version = %q, want the release still serving with no arrow", version)
	}

	// downtime says the one thing worth saying and nothing about volumes, since
	// what happened to the data is not the channel's business
	downtime := decodeEmbed(t, mustStage(t, stageDowntime, "lmc", StageVersions{}))
	if title, _ := downtime["title"].(string); title != "lmc is experiencing downtime" {
		t.Errorf("title = %q", title)
	}
	if body, present := downtime["description"]; present {
		t.Errorf("downtime should carry no body, got %q", body)
	}
	for _, unwanted := range []string{"version", "Volumes"} {
		if _, present := fieldsOf(t, downtime)[unwanted]; present {
			t.Errorf("a downtime notice should not carry a %s field", unwanted)
		}
	}
	for _, leak := range []string{"data", "volume", "destroy"} {
		title, _ := downtime["title"].(string)
		if strings.Contains(strings.ToLower(title), leak) {
			t.Errorf("the downtime title says %q, which is not the channel's business", leak)
		}
	}
}

func mustStage(t *testing.T, stage DeployStage, projectName string, versions StageVersions) []byte {
	t.Helper()

	payload, err := StagePayload(stage, projectName, []string{"lecternmc.com"}, versions)
	if err != nil {
		t.Fatalf("StagePayload: %v", err)
	}

	return payload
}

// Rollback and destroy change what people are looking at just as much as a
// deploy does, so they get told, in the same voice.
func TestARealRollbackAndDestroyBothSpeakToUsers(t *testing.T) {
	dockerAvailable(t)

	const variable = "DEPLOY_TEST_LIFECYCLE_WEBHOOK"

	received := make(chan map[string]any, 16)
	webhook := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		var message struct {
			Embeds []map[string]any `json:"embeds"`
		}
		json.Unmarshal(body, &message)
		received <- message.Embeds[0]
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer webhook.Close()
	t.Setenv(variable, webhook.URL)

	repository := newRepository(t)
	writeFile(t, repository, configFileName, `{
      "version": 1,
      "id": "dd000018",
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
	t.Cleanup(func() { exec.Command("docker", "network", "rm", NetworkName("dd000018")).Run() })

	options := DeployOptions{
		Context: repository, Destination: destination, Environment: defaultEnvironmentName,
	}

	first := commitFile(t, repository, "one.txt", "first")
	t.Cleanup(func() {
		exec.Command("docker", "compose", "--project-name", ProjectName("dd000018", first), "down").Run()
	})
	if _, err := RunDeploy(options); err != nil {
		t.Fatalf("first deploy: %v", err)
	}
	titlesOf(t, received, []string{"new version ready", "update live"})

	second := commitFile(t, repository, "one.txt", "second")
	t.Cleanup(func() {
		exec.Command("docker", "compose", "--project-name", ProjectName("dd000018", second), "down").Run()
	})
	if _, err := RunDeploy(options); err != nil {
		t.Fatalf("second deploy: %v", err)
	}
	titlesOf(t, received, []string{"new version ready", "switching over", "update live"})

	if _, err := RunRollback(options, ""); err != nil {
		t.Fatalf("the rollback should succeed: %v", err)
	}

	rolled := titlesOf(t, received, []string{"rolling back"})
	wantLine := ShortCommit(first) + " <- " + ShortCommit(second)
	if version := fieldsOf(t, rolled[0])["version"]; version != wantLine {
		t.Errorf("version = %q, want %q", version, wantLine)
	}

	if _, err := RunDestroy(options, false, strings.NewReader("lifecycle\n")); err != nil {
		t.Fatalf("the destroy should succeed: %v", err)
	}

	titlesOf(t, received, []string{"lifecycle is experiencing downtime"})
}
