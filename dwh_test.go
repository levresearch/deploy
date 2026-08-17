package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDwhWritesTheSecretAddsTheConfigAndTestsTheWebhook(t *testing.T) {
	posted := make(chan []byte, 2)
	webhook := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body := make([]byte, request.ContentLength)
		request.Body.Read(body)
		posted <- body
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer webhook.Close()

	repository := newRepository(t)
	writeFile(t, repository, configFileName, `{
  "version": 1,
  "id": "a3f19c02",
  "name": "shop",
  "services": {
    "web": {"image": "busybox:latest"}
  }
}`)

	// the url has to look like a webhook, and the test server does not, so the
	// path is what makes it pass the check
	webhookURL := webhook.URL + "/api/webhooks/1/token"

	exitCode, err := RunConfigureWebhook(DeployOptions{Context: repository}, webhookURL, webhook.Client())
	if err != nil {
		t.Fatalf("RunConfigureWebhook: %v", err)
	}
	if exitCode != exitOK {
		t.Errorf("exit code = %d", exitCode)
	}

	// the secret is on disk, readable only by its owner
	secretsPath := filepath.Join(repository, deployDirectoryName, secretsFileName)
	info, err := os.Stat(secretsPath)
	if err != nil {
		t.Fatalf("stat secrets: %v", err)
	}
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		t.Errorf("secrets file mode is %#o, which lets other users read a credential", mode)
	}
	raw, err := os.ReadFile(secretsPath)
	if err != nil {
		t.Fatalf("reading secrets: %v", err)
	}
	if ParseEnvFile(raw)[defaultWebhookVariable] != webhookURL {
		t.Errorf("the webhook was not stored, got %q", ParseEnvFile(raw)[defaultWebhookVariable])
	}

	// the config points at it, and never holds it
	project, err := LoadProject(filepath.Join(repository, configFileName))
	if err != nil {
		t.Fatalf("the config should still parse: %v", err)
	}
	if project.Notify == nil || project.Notify.DiscordWebhookFrom != defaultWebhookVariable {
		t.Fatalf("notify = %+v, want it to name %s", project.Notify, defaultWebhookVariable)
	}
	config, err := os.ReadFile(filepath.Join(repository, configFileName))
	if err != nil {
		t.Fatalf("reading config: %v", err)
	}
	if strings.Contains(string(config), webhookURL) {
		t.Error("the webhook url must never end up in the committed config")
	}

	// .deploy/ is ignored, so the secret cannot be committed
	ignore, err := os.ReadFile(filepath.Join(repository, ".gitignore"))
	if err != nil || !strings.Contains(string(ignore), deployDirectoryName+"/") {
		t.Errorf(".deploy/ should be gitignored, got %q", ignore)
	}

	// and it proved the webhook works rather than only saving it
	select {
	case body := <-posted:
		var message struct {
			Embeds []struct {
				Title string `json:"title"`
			} `json:"embeds"`
		}
		json.Unmarshal(body, &message)
		if len(message.Embeds) == 0 || !strings.Contains(message.Embeds[0].Title, "shop") {
			t.Errorf("the test message should name the project, got %+v", message)
		}
	default:
		t.Error("dwh should post a test message so a wrong url is caught now")
	}
}

// A webhook that does not answer is a webhook that is wrong, and saying so at
// configure time is the entire reason this command posts anything.
func TestDwhFailsWhenTheWebhookDoesNotAnswer(t *testing.T) {
	rejecting := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNotFound)
	}))
	defer rejecting.Close()

	repository := newRepository(t)
	writeFile(t, repository, configFileName, `{
  "version": 1, "id": "a3f19c02", "name": "shop",
  "services": {"web": {"image": "busybox:latest"}}
}`)

	_, err := RunConfigureWebhook(DeployOptions{Context: repository}, rejecting.URL+"/api/webhooks/1/gone", rejecting.Client())
	if err == nil {
		t.Fatal("a webhook that answers 404 should be reported, not saved quietly")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("the error should say what discord answered, got: %v", err)
	}
}

func TestDwhRefusesSomethingThatIsNotAWebhookURL(t *testing.T) {
	repository := newRepository(t)
	writeFile(t, repository, configFileName, `{
  "version": 1, "id": "a3f19c02", "name": "shop",
  "services": {"web": {"image": "busybox:latest"}}
}`)

	cases := map[string]string{
		"a channel link":  "https://discord.com/channels/123/456",
		"plain http":      "http://discord.com/api/webhooks/1/abc",
		"not a url":       "my-webhook",
		"the word itself": "webhook",
	}

	for name, candidate := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := RunConfigureWebhook(DeployOptions{Context: repository}, candidate, nil); err == nil {
				t.Errorf("%q should be refused", candidate)
			}
		})
	}

	// nothing was written on the way to refusing
	if _, err := os.Stat(filepath.Join(repository, deployDirectoryName, secretsFileName)); err == nil {
		t.Error("a refused url should leave no secrets file behind")
	}
}

// The config is edited as text on purpose, so this is the test that says the
// rest of somebody's file survives it.
func TestDwhPreservesTheRestOfTheConfigExactly(t *testing.T) {
	repository := newRepository(t)
	const original = `{
  "version": 1,
  "id": "7a3ec071",
  "name": "lmc",
  "destination": "git:Projects",
  "retention": 3,
  "services": {
    "pg": {
      "image": "postgres:17",
      "stateful": true
    },
    "redis": {
      "image": "redis:7-alpine",
      "stateful": true
    },
    "api": {
      "image": "busybox:latest"
    }
  }
}`
	writeFile(t, repository, configFileName, original)

	project, err := LoadProject(filepath.Join(repository, configFileName))
	if err != nil {
		t.Fatalf("LoadProject: %v", err)
	}
	added, err := ensureNotifyBlock(repository, project, defaultWebhookVariable)
	if err != nil {
		t.Fatalf("ensureNotifyBlock: %v", err)
	}
	if !added {
		t.Fatal("a config with no notify block should get one")
	}

	updated, err := os.ReadFile(filepath.Join(repository, configFileName))
	if err != nil {
		t.Fatalf("reading config: %v", err)
	}

	// service order is the thing a re-encode would destroy, since go maps have
	// no order and marshalling would alphabetise them
	text := string(updated)
	pg, redis, api := strings.Index(text, `"pg"`), strings.Index(text, `"redis"`), strings.Index(text, `"api"`)
	if !(pg < redis && redis < api) {
		t.Error("the services were reordered, which means the config was re-encoded rather than edited")
	}
	if !strings.Contains(text, `"destination": "git:Projects"`) {
		t.Error("an untouched key was changed")
	}
	if strings.Count(text, `"stateful": true`) != 2 {
		t.Error("the body of the services changed")
	}

	// and it is still a config deploy can read
	reloaded, err := LoadProject(filepath.Join(repository, configFileName))
	if err != nil {
		t.Fatalf("the edited config should still parse: %v", err)
	}
	if reloaded.Notify.DiscordWebhookFrom != defaultWebhookVariable {
		t.Errorf("notify = %+v", reloaded.Notify)
	}
	if len(reloaded.Services) != 3 {
		t.Errorf("services = %d, want 3", len(reloaded.Services))
	}

	// running it twice does not add a second block
	againAdded, err := ensureNotifyBlock(repository, reloaded, defaultWebhookVariable)
	if err != nil {
		t.Fatalf("second ensureNotifyBlock: %v", err)
	}
	if againAdded {
		t.Error("a config that already names a webhook should be left alone")
	}
}

func TestUpsertEnvLine(t *testing.T) {
	cases := []struct {
		name     string
		existing string
		want     string
	}{
		{"empty file", "", "WEBHOOK=new\n"},
		{"appends to others", "OTHER=keep\n", "OTHER=keep\nWEBHOOK=new\n"},
		{"replaces rather than duplicating", "WEBHOOK=old\n", "WEBHOOK=new\n"},
		{"replaces in place, keeping neighbours and comments", "# note\nA=1\nWEBHOOK=old\nB=2\n", "# note\nA=1\nWEBHOOK=new\nB=2\n"},
		{"replaces an exported one", "export WEBHOOK=old\n", "WEBHOOK=new\n"},
		{"leaves a similar name alone", "WEBHOOK_OLD=keep\n", "WEBHOOK_OLD=keep\nWEBHOOK=new\n"},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := string(upsertEnvLine([]byte(testCase.existing), "WEBHOOK", "new"))
			if got != testCase.want {
				t.Errorf("got %q, want %q", got, testCase.want)
			}
		})
	}
}
