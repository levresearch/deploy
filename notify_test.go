package main

import (
	"encoding/json"
	"fmt"
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

// The stages all land within seconds of each other, because everything slow
// happens before the first one, so the arc is one message that changes rather
// than three that arrive together.
func TestTheStagesEditOneMessageRatherThanPostingSeveral(t *testing.T) {
	type call struct {
		method string
		path   string
		query  string
		title  string
	}

	var calls []call

	discord := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)

		var message struct {
			Embeds []struct {
				Title string `json:"title"`
			} `json:"embeds"`
		}
		if err := json.Unmarshal(body, &message); err != nil || len(message.Embeds) != 1 {
			t.Errorf("%s %s did not carry one embed: %v", request.Method, request.URL.Path, err)
		}

		calls = append(calls, call{
			method: request.Method,
			path:   request.URL.Path,
			query:  request.URL.Query().Get("wait"),
			title:  message.Embeds[0].Title,
		})

		writer.Header().Set("Content-Type", "application/json")
		fmt.Fprint(writer, `{"id": "1417"}`)
	}))
	defer discord.Close()

	notifier := &Notifier{webhookURL: discord.URL + "/api/webhooks/1/abc", client: discord.Client()}
	versions := StageVersions{Previous: "abc1234", Incoming: "def5678"}
	for _, stage := range []DeployStage{stageReady, stageSwitching, stageLive} {
		notifier.SendStage(stage, "shop", []string{"shop.example.com"}, versions)
	}

	if len(calls) != 3 {
		t.Fatalf("expected three calls, got %d: %+v", len(calls), calls)
	}

	// the first one has to ask for the message back, since a plain webhook post
	// answers 204 with no body and there is then no id to edit
	if calls[0].method != http.MethodPost || calls[0].query != "true" {
		t.Errorf("the first stage should post with wait=true, got %s %s?wait=%s",
			calls[0].method, calls[0].path, calls[0].query)
	}
	for _, later := range calls[1:] {
		if later.method != http.MethodPatch {
			t.Errorf("later stages should edit, got %s %s", later.method, later.path)
		}
		if !strings.HasSuffix(later.path, "/messages/1417") {
			t.Errorf("the edit should address the message that was created, got %s", later.path)
		}
	}

	if titles := []string{calls[0].title, calls[1].title, calls[2].title}; !slices.Equal(
		titles, []string{"staging new deployment", "switching over", "update live"},
	) {
		t.Errorf("the one message should move through the arc, got %q", titles)
	}
}

// A webhook posting into a thread carries thread_id, and an edit that dropped it
// would move the message to the parent channel.
func TestEditingKeepsTheQueryTheWebhookCameWith(t *testing.T) {
	const webhook = "https://discord.com/api/webhooks/1/abc?thread_id=99"

	edit := messageURL(webhook, "1417")
	if !strings.Contains(edit, "/api/webhooks/1/abc/messages/1417") {
		t.Errorf("the edit url should address the message, got %s", edit)
	}
	if !strings.Contains(edit, "thread_id=99") {
		t.Errorf("the edit url dropped the thread, got %s", edit)
	}

	created := addQuery(webhook, "wait", "true")
	if !strings.Contains(created, "thread_id=99") || !strings.Contains(created, "wait=true") {
		t.Errorf("the create url should keep the thread and ask to wait, got %s", created)
	}
}

// Editing is the nice path, not a load bearing one, so anything that goes wrong
// with it falls back to a separate message rather than to silence.
func TestAMessageThatCannotBeEditedIsPostedAgain(t *testing.T) {
	t.Run("when discord returns no id to edit", func(t *testing.T) {
		var methods []string

		discord := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			methods = append(methods, request.Method)
			writer.WriteHeader(http.StatusNoContent)
		}))
		defer discord.Close()

		notifier := &Notifier{webhookURL: discord.URL, client: discord.Client()}
		notifier.SendStage(stageReady, "shop", nil, StageVersions{})
		notifier.SendStage(stageLive, "shop", nil, StageVersions{})

		if !slices.Equal(methods, []string{http.MethodPost, http.MethodPost}) {
			t.Errorf("with no id to edit both stages should post, got %v", methods)
		}
	})

	t.Run("when the message it was told about is gone", func(t *testing.T) {
		var methods []string

		discord := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			methods = append(methods, request.Method)

			if request.Method == http.MethodPatch {
				writer.WriteHeader(http.StatusNotFound)

				return
			}

			writer.Header().Set("Content-Type", "application/json")
			fmt.Fprint(writer, `{"id": "1417"}`)
		}))
		defer discord.Close()

		notifier := &Notifier{webhookURL: discord.URL, client: discord.Client()}
		notifier.SendStage(stageReady, "shop", nil, StageVersions{})
		notifier.SendStage(stageLive, "shop", nil, StageVersions{})

		if !slices.Equal(methods, []string{http.MethodPost, http.MethodPatch, http.MethodPost}) {
			t.Errorf("a deleted message should be replaced by a new one, got %v", methods)
		}
	})
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

	// how notifications actually go missing, which is a webhook that was saved and
	// then lost its notify block to a later edit of the config. that deploys
	// silently and looks the same as wanting no notifications at all
	t.Run("a webhook with no notify block naming it is said out loud", func(t *testing.T) {
		repository := t.TempDir()
		writeSecretFixture(t, repository, variable+"="+webhook)

		var notifier *Notifier
		var err error
		warning := captureStderr(t, func() {
			notifier, err = NewNotifier(repository, nil)
		})

		if err != nil || notifier != nil {
			t.Errorf("a stranded webhook is still not notifications, got %v %v", notifier, err)
		}
		if !strings.Contains(warning, variable) || !strings.Contains(warning, "deploy dwh") {
			t.Errorf("the warning should name the variable and how to fix it, got: %q", warning)
		}
		// the whole point of naming a variable is that the url is not shown around,
		// and a warning is no more allowed to print it than anything else is
		if strings.Contains(warning, webhook) {
			t.Errorf("the warning printed the webhook itself: %q", warning)
		}
	})

	// leaving notifications off is allowed, so an ordinary secrets file holding
	// something that is not a webhook has to stay quiet
	t.Run("a secrets file with no webhook in it stays quiet", func(t *testing.T) {
		repository := t.TempDir()
		writeSecretFixture(t, repository, "SOME_TOKEN=not-a-webhook")

		warning := captureStderr(t, func() {
			if _, err := NewNotifier(repository, nil); err != nil {
				t.Errorf("NewNotifier: %v", err)
			}
		})

		if warning != "" {
			t.Errorf("nothing to warn about here, got: %q", warning)
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
		wantLine   string
		wantColour int
		wantVerion string
	}{
		{stageReady, "staging new deployment", "there is a new version being staged", 0x3498db, "abc1234 -> def5678"},
		{
			stageSwitching, "switching over",
			"switching over to the new version now, usually without any interruption",
			0xf39c12, "abc1234 -> def5678",
		},
		{stageLive, "update live", "the new version is up and answering", 0x2ecc71, "abc1234 -> def5678"},
		// the one stage that ends up back where it started, so the arrow turns
		{
			stageCancelled, "reverting changes",
			"we are reverting changes while we fix some issues with the deployment",
			0xe74c3c, "abc1234 <- def5678",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.wantTitle, func(t *testing.T) {
			log := appendLine(nil, testCase.stage, time.Date(2026, 8, 17, 14, 5, 9, 0, time.UTC))

			payload, err := StagePayload(testCase.stage, log, "lmc", []string{"lecternmc.com", "bot"}, versions)
			if err != nil {
				t.Fatalf("StagePayload: %v", err)
			}

			embed := decodeEmbed(t, payload)
			if title, _ := embed["title"].(string); title != testCase.wantTitle {
				t.Errorf("title = %q, want %q", title, testCase.wantTitle)
			}
			// the clock is there because the stages land seconds apart and a log
			// that does not say when says nothing about the order
			body, _ := embed["description"].(string)
			if body != "14:05:09  "+testCase.wantLine {
				t.Errorf("body = %q, want the line stamped with the time", body)
			}
			if colour, _ := embed["color"].(float64); int(colour) != testCase.wantColour {
				t.Errorf("colour = %#x, want %#x", int(colour), testCase.wantColour)
			}

			fields := fieldsOf(t, embed)
			if fields["version"] != testCase.wantVerion {
				t.Errorf("version = %q, want %q", fields["version"], testCase.wantVerion)
			}
			// hostnames one per line, because somebody reading the channel knows the
			// site by its domain and has never heard of the container behind it
			if fields["affected"] != "```\nlecternmc.com\nbot\n```" {
				t.Errorf("affected = %q", fields["affected"])
			}
			for _, jargon := range []string{"commit", "container", "stateless", "cutover", "healthcheck"} {
				if strings.Contains(strings.ToLower(body), jargon) {
					t.Errorf("the body says %q, which means nothing to somebody using the site", jargon)
				}
			}
		})
	}
}

// The version and the affected list sit beside each other rather than stacked,
// since both are short and the log underneath them is what needs the width.
func TestTheVersionAndAffectedFieldsSitSideBySide(t *testing.T) {
	payload, err := StagePayload(stageLive, nil, "lmc", []string{"lecternmc.com"}, StageVersions{Incoming: "def5678"})
	if err != nil {
		t.Fatalf("StagePayload: %v", err)
	}

	raw, _ := decodeEmbed(t, payload)["fields"].([]any)
	if len(raw) != 2 {
		t.Fatalf("expected two fields, got %d", len(raw))
	}
	for _, entry := range raw {
		field, _ := entry.(map[string]any)
		if inline, _ := field["inline"].(bool); !inline {
			t.Errorf("%v should be inline", field["name"])
		}
	}
}

// The message is a log, so what it said before has to still be there afterwards.
// Somebody who looks at the channel once, after the fact, should be able to read
// the whole release out of the one message.
func TestTheLogKeepsEveryLineAndTheEmbedTracksTheLatestStage(t *testing.T) {
	at := time.Date(2026, 8, 17, 14, 5, 9, 0, time.UTC)

	var log []string
	for offset, stage := range []DeployStage{stageReady, stageSwitching, stageLive} {
		log = appendLine(log, stage, at.Add(time.Duration(offset*11)*time.Second))
	}

	payload, err := StagePayload(stageLive, log, "lmc", nil, StageVersions{Incoming: "def5678"})
	if err != nil {
		t.Fatalf("StagePayload: %v", err)
	}

	embed := decodeEmbed(t, payload)
	body, _ := embed["description"].(string)

	want := "14:05:09  there is a new version being staged\n" +
		"14:05:20  switching over to the new version now, usually without any interruption\n" +
		"14:05:31  the new version is up and answering"
	if body != want {
		t.Errorf("the log reads\n%s\n\nwant\n%s", body, want)
	}

	// the last stage is the one the colour and the title are about, since that is
	// the state the thing is actually in
	if colour, _ := embed["color"].(float64); int(colour) != 0x2ecc71 {
		t.Errorf("colour = %#x, want the green of the last line", int(colour))
	}
	if title, _ := embed["title"].(string); title != "update live" {
		t.Errorf("title = %q, want the last stage", title)
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

	payload, err := StagePayload(stage, nil, "lmc", nil, versions)
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

	received, webhookURL := fakeDiscord(t)
	t.Setenv(variable, webhookURL)

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

	titlesOf(t, received, []string{"staging new deployment", "update live"})

	// a second deploy has something to switch away from, so all three fire
	second := commitFile(t, repository, "one.txt", "second")
	t.Cleanup(func() {
		exec.Command("docker", "compose", "--project-name", ProjectName("dd000017", second), "down").Run()
	})
	if _, err := RunDeploy(options); err != nil {
		t.Fatalf("the second deploy should succeed: %v", err)
	}

	arc := titlesOf(t, received, []string{"staging new deployment", "switching over", "update live"})
	wantLine := ShortCommit(first) + " -> " + ShortCommit(second)
	for _, next := range arc {
		if version := fieldsOf(t, next.embed)["version"]; version != wantLine {
			t.Errorf("version = %q, want %q", version, wantLine)
		}
	}

	// the three land within seconds of each other, so they are one message being
	// rewritten rather than three arriving together
	if methods := methodsOf(arc); !slices.Equal(
		methods, []string{http.MethodPost, http.MethodPatch, http.MethodPatch},
	) {
		t.Errorf("the arc should be one message that changes, got %v", methods)
	}

	// a dirty tree never reaches a build, and is nobody's business but the
	// operator's, so the channel hears nothing at all
	writeFile(t, repository, "one.txt", "uncommitted")
	if _, err := RunDeploy(options); err == nil {
		t.Fatal("a dirty tree should fail")
	}

	select {
	case next := <-received:
		t.Errorf("a failure before any build told users %q", next.embed["title"])
	case <-time.After(2 * time.Second):
	}
}

// notification is one thing the channel was told, and how. The method matters
// because the arc is supposed to be a single message being rewritten.
type notification struct {
	method string
	embed  map[string]any
}

// fakeDiscord answers the way Discord does, which means handing back the created
// message so there is an id to edit. A server that only ever answered 204 would
// quietly push these tests down the fallback path and prove nothing about the
// arc being one message.
func fakeDiscord(t *testing.T) (chan notification, string) {
	t.Helper()

	received := make(chan notification, 16)

	webhook := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)

		var message struct {
			Embeds []map[string]any `json:"embeds"`
		}
		if err := json.Unmarshal(body, &message); err != nil || len(message.Embeds) != 1 {
			t.Errorf("%s carried no embed: %v", request.Method, err)

			return
		}

		received <- notification{method: request.Method, embed: message.Embeds[0]}

		writer.Header().Set("Content-Type", "application/json")
		fmt.Fprint(writer, `{"id": "1417"}`)
	}))
	t.Cleanup(webhook.Close)

	return received, webhook.URL + "/api/webhooks/1/abc"
}

// titlesOf drains exactly the expected number of notifications and checks their
// order, since the order is the message.
func titlesOf(t *testing.T, received chan notification, want []string) []notification {
	t.Helper()

	var got []string
	var arrived []notification

	for range want {
		select {
		case next := <-received:
			title, _ := next.embed["title"].(string)
			got = append(got, title)
			arrived = append(arrived, next)
		case <-time.After(20 * time.Second):
			t.Fatalf("expected %v, only got %v", want, got)
		}
	}

	if !slices.Equal(got, want) {
		t.Errorf("the arc was %v, want %v", got, want)
	}

	return arrived
}

// methodsOf is how each of them arrived, so a run can assert that one message
// was rewritten rather than that several were posted.
func methodsOf(arrived []notification) []string {
	var methods []string
	for _, next := range arrived {
		methods = append(methods, next.method)
	}

	return methods
}

func TestTheRollbackAndDowntimeMessages(t *testing.T) {
	// a rollback lands on the earlier release, so the arrow turns the same way
	// the cancelled one does
	rollingBack := decodeEmbed(t, mustStage(t, stageRollingBack, "lmc",
		StageVersions{Previous: "abc1234", Incoming: "def5678"}))

	if title, _ := rollingBack["title"].(string); title != "rolling back" {
		t.Errorf("title = %q", title)
	}
	if body, _ := rollingBack["description"].(string); body != "14:05:09  we are going back to an earlier version, usually without any interruption" {
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
	if body, _ := failed["description"].(string); body != "14:05:09  the earlier version could not be started, so nothing changed" {
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

	log := appendLine(nil, stage, time.Date(2026, 8, 17, 14, 5, 9, 0, time.UTC))

	payload, err := StagePayload(stage, log, projectName, []string{"lecternmc.com"}, versions)
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

	received, webhookURL := fakeDiscord(t)
	t.Setenv(variable, webhookURL)

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
	titlesOf(t, received, []string{"staging new deployment", "update live"})

	second := commitFile(t, repository, "one.txt", "second")
	t.Cleanup(func() {
		exec.Command("docker", "compose", "--project-name", ProjectName("dd000018", second), "down").Run()
	})
	if _, err := RunDeploy(options); err != nil {
		t.Fatalf("second deploy: %v", err)
	}
	titlesOf(t, received, []string{"staging new deployment", "switching over", "update live"})

	if _, err := RunRollback(options, ""); err != nil {
		t.Fatalf("the rollback should succeed: %v", err)
	}

	rolled := titlesOf(t, received, []string{"rolling back"})
	wantLine := ShortCommit(first) + " <- " + ShortCommit(second)
	if version := fieldsOf(t, rolled[0].embed)["version"]; version != wantLine {
		t.Errorf("version = %q, want %q", version, wantLine)
	}

	// a rollback is its own thing rather than a late edit of the deploy that came
	// before it, so it starts a message of its own
	if rolled[0].method != http.MethodPost {
		t.Errorf("a rollback should start its own message, got %s", rolled[0].method)
	}

	if _, err := RunDestroy(options, false, strings.NewReader("lifecycle\n")); err != nil {
		t.Fatalf("the destroy should succeed: %v", err)
	}

	titlesOf(t, received, []string{"lifecycle is experiencing downtime"})
}
