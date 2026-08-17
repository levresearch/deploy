package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path"
	"strings"
	"time"
)

// Notifications say what happened after the fact, and are never allowed to
// change what happened. A webhook that is down is a webhook that is down, not a
// failed release, so nothing in this file can turn a deploy that worked into one
// that reports an error.

// notifyTimeout keeps a hung webhook from holding a deploy open. The release is
// already live by the time most notifications go out, so waiting on Discord is
// waiting for nothing.
const notifyTimeout = 10 * time.Second

// Discord's own limits. Going over any of them is a 400 with a body nobody
// reads, so the payload is trimmed to fit rather than sent hopefully.
const (
	discordTitleLimit = 256
	discordFieldLimit = 1024
)

// Notify is configured by the name of an environment variable rather than by the
// webhook itself, for the same reason as tunnelTokenFrom: a webhook url is a
// credential, anyone holding it can post to the channel, and .deploy.json is
// committed and often pushed somewhere public.
type Notify struct {
	DiscordWebhookFrom string `json:"discordWebhookFrom,omitempty"`
}

// deployAction is what the notification is about. Rollback and destroy change
// what is serving just as much as a deploy does, and a channel that only hears
// about deploys is a channel that misses the interesting half.
type deployAction string

const (
	actionDeploy   deployAction = "Deploy"
	actionRollback deployAction = "Rollback"
	actionDestroy  deployAction = "Destroy"
)

// DeployReport is everything a notification says. It is filled in as the work
// runs, so a failure halfway through still reports how far it got.
type DeployReport struct {
	Action      deployAction
	Project     string
	Commit      string
	Environment string
	// Updated is the stateless services this release replaced, which is what
	// someone reading the channel actually wants to know.
	Updated []string
	// Recreated is the stateful services that took an outage because the shared
	// stack changed. Almost always empty, and worth shouting about when it is not.
	Recreated []string
	// From is the release a rollback left behind, which is the thing anyone
	// reading about a rollback wants to know as much as where it landed.
	From string
	// VolumesRemoved separates a destroy that kept the data from one that did
	// not, since only one of those is recoverable.
	VolumesRemoved bool
	ExitCode       int
	Failure        error
}

func (report DeployReport) succeeded() bool {
	return report.Failure == nil && report.ExitCode == exitOK
}

// title says the outcome in the one line a phone notification shows.
func (report DeployReport) title() string {
	subject := strings.TrimSpace(report.Project + " " + report.Commit)

	if !report.succeeded() {
		if report.ExitCode == exitLiveButNeedsAHuman {
			return fmt.Sprintf("%s is live but needs a human", subject)
		}

		return fmt.Sprintf("%s failed: %s", report.action(), subject)
	}

	switch report.Action {
	case actionRollback:
		return fmt.Sprintf("Rolled %s back to %s", report.Project, report.Commit)
	case actionDestroy:
		if report.VolumesRemoved {
			return fmt.Sprintf("Destroyed %s and its data", report.Project)
		}

		return fmt.Sprintf("Destroyed %s, data kept", report.Project)
	default:
		return fmt.Sprintf("Deployed %s", subject)
	}
}

// action defaults to a deploy, so a caller that fills in nothing else still
// produces a sentence rather than a blank.
func (report DeployReport) action() deployAction {
	if report.Action == "" {
		return actionDeploy
	}

	return report.Action
}

// colour is read at a glance, so the three outcomes are three colours rather
// than green and red with the interesting one folded into either.
func (report DeployReport) colour() int {
	switch {
	case report.succeeded():
		return 0x2ecc71
	case report.ExitCode == exitLiveButNeedsAHuman:
		return 0xf39c12
	default:
		return 0xe74c3c
	}
}

func (report DeployReport) services() string {
	if len(report.Updated) == 0 {
		return "none, this project is all stateful services"
	}

	return strings.Join(report.Updated, ", ")
}

// servicesLabel keeps the field honest. A failed deploy updated nothing, so
// calling the same list "updated" would be a notification that lies.
func (report DeployReport) servicesLabel() string {
	if !report.succeeded() {
		return "Services attempted"
	}
	if report.action() == actionDestroy {
		return "Services removed"
	}

	return "Services updated"
}

// DiscordPayload is the request body, built without touching the network so the
// interesting part can be tested without one.
func (report DeployReport) DiscordPayload() ([]byte, error) {
	fields := []discordField{
		{Name: "Environment", Value: truncate(report.Environment, discordFieldLimit), Inline: true},
	}

	// a rollback is defined by the pair, so saying only where it landed leaves out
	// half of what happened
	if report.action() == actionRollback && report.From != "" {
		fields = append(fields, discordField{Name: "Rolled back from", Value: report.From, Inline: true})
	}
	if report.action() == actionDestroy {
		volumes := "kept, the data is still there"
		if report.VolumesRemoved {
			volumes = "REMOVED, the data is gone"
		}
		fields = append(fields, discordField{Name: "Volumes", Value: volumes})
	}

	fields = append(fields, discordField{
		Name: report.servicesLabel(), Value: truncate(report.services(), discordFieldLimit),
	})

	if len(report.Recreated) > 0 {
		fields = append(fields, discordField{
			Name:  "Stateful services recreated",
			Value: truncate(strings.Join(report.Recreated, ", "), discordFieldLimit),
		})
	}
	if report.Failure != nil {
		fields = append(fields, discordField{
			Name:  "Error",
			Value: truncate(report.Failure.Error(), discordFieldLimit),
		})
	}

	return json.Marshal(discordMessage{
		Embeds: []discordEmbed{{
			Title:     truncate(report.title(), discordTitleLimit),
			Color:     report.colour(),
			Fields:    fields,
			Timestamp: time.Now().UTC().Format(time.RFC3339),
		}},
	})
}

type discordMessage struct {
	Embeds []discordEmbed `json:"embeds"`
}

type discordEmbed struct {
	Title     string         `json:"title"`
	Color     int            `json:"color"`
	Fields    []discordField `json:"fields,omitempty"`
	Timestamp string         `json:"timestamp"`
}

type discordField struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline,omitempty"`
}

// truncate keeps the payload inside Discord's limits. An error message is the
// one field that can genuinely run long, and a trimmed error still says more
// than a rejected request.
func truncate(text string, limit int) string {
	if text == "" {
		return "-"
	}
	if len(text) <= limit {
		return text
	}

	return text[:limit-3] + "..."
}

// Notifier holds a resolved webhook. A nil one is the ordinary case of a project
// that configured no notifications, and every method tolerates it, so callers
// never have to ask whether notifications are on.
type Notifier struct {
	webhookURL string
	client     *http.Client
}

// secretsFileName holds credentials deploy itself needs, as opposed to the env
// files it pushes for containers. It lives under .deploy/, which is gitignored
// and therefore never reaches the server either: git archive ships tracked
// content, so an untracked file cannot travel with the code.
const secretsFileName = "secrets.env"

// NewNotifier fails when notifications are configured and the webhook cannot be
// found, because the alternative is a project that believes it is being watched
// and is not. It runs before anything is built, so this costs a typo rather than
// a deploy.
func NewNotifier(repositoryPath string, notify *Notify) (*Notifier, error) {
	if notify == nil || notify.DiscordWebhookFrom == "" {
		return nil, nil
	}

	webhookURL, err := lookupSecret(repositoryPath, notify.DiscordWebhookFrom)
	if err != nil {
		return nil, err
	}

	// a warning rather than a refusal, because Discord serves webhooks from more
	// than one hostname and being wrong here would block a deploy over nothing
	if !strings.Contains(webhookURL, "/api/webhooks/") {
		fmt.Fprintf(os.Stderr,
			"warning: %s does not look like a discord webhook url, which usually means a channel link got pasted instead\n",
			notify.DiscordWebhookFrom,
		)
	}

	return &Notifier{webhookURL: webhookURL, client: newNotifyClient()}, nil
}

func newNotifyClient() *http.Client {
	return &http.Client{Timeout: notifyTimeout}
}

// SendTest returns its error instead of swallowing it, which is the opposite of
// Send and the whole point: here the person is waiting to be told whether the
// webhook they just pasted actually works.
func (notifier *Notifier) SendTest(projectName string) error {
	payload, err := json.Marshal(discordMessage{
		Embeds: []discordEmbed{{
			Title:     truncate(fmt.Sprintf("deploy is now wired up for %s", projectName), discordTitleLimit),
			Color:     0x3498db,
			Fields:    []discordField{{Name: "This is a test", Value: "you will get one of these when a deploy, rollback, or destroy finishes"}},
			Timestamp: time.Now().UTC().Format(time.RFC3339),
		}},
	})
	if err != nil {
		return err
	}

	response, err := notifier.client.Post(notifier.webhookURL, "application/json", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	defer response.Body.Close()

	if response.StatusCode >= 300 {
		return fmt.Errorf("discord answered %s", response.Status)
	}

	return nil
}

// lookupSecret checks the environment before the file, so CI, where secrets
// arrive as environment variables and no file exists, needs no special case, and
// a one-off override is just a variable in front of the command.
func lookupSecret(repositoryPath, name string) (string, error) {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value, nil
	}

	secretsPath := path.Join(repositoryPath, deployDirectoryName, secretsFileName)

	raw, err := os.ReadFile(secretsPath)
	if errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf(
			"this project notifies a discord webhook, and %s is set neither in this shell nor in %s. put it there with:\n\n    mkdir -p %s && umask 077 && echo '%s=<your webhook url>' >> %s\n\n%s is gitignored, so the webhook stays out of the repo and never travels to the server",
			name, secretsPath, path.Join(repositoryPath, deployDirectoryName), name, secretsPath, deployDirectoryName,
		)
	}
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", secretsPath, err)
	}

	// a secrets file anyone on the machine can read is worth saying out loud,
	// since the whole point of moving the webhook out of the config was to stop
	// it being somewhere it should not be
	if info, err := os.Stat(secretsPath); err == nil && info.Mode().Perm()&0o077 != 0 {
		fmt.Fprintf(os.Stderr,
			"warning: %s is readable by other users, tighten it with chmod 600\n", secretsPath,
		)
	}

	value, found := ParseEnvFile(raw)[name]
	if !found || value == "" {
		return "", fmt.Errorf(
			"this project notifies a discord webhook, and %s is set neither in this shell nor in %s", name, secretsPath,
		)
	}

	return value, nil
}

// ParseEnvFile reads the KEY=value form every .env file in the world uses. It is
// deliberately small: deploy only ever reads its own secrets file with this, and
// anything richer would be a second, worse dotenv implementation.
func ParseEnvFile(raw []byte) map[string]string {
	values := map[string]string{}

	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")

		name, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}

		name = strings.TrimSpace(name)
		value = strings.TrimSpace(value)

		// quotes are what a shell would strip, so a value pasted with them means
		// the same thing as one without
		if len(value) >= 2 && (value[0] == '"' && value[len(value)-1] == '"' ||
			value[0] == '\'' && value[len(value)-1] == '\'') {
			value = value[1 : len(value)-1]
		}

		values[name] = value
	}

	return values
}

// Send reports the outcome and swallows its own failures on purpose. It is
// called from a defer after the deploy has already decided what happened, and
// there is no answer to "the notification failed" that helps anyone at that
// point beyond saying so.
func (notifier *Notifier) Send(report DeployReport) {
	if notifier == nil {
		return
	}

	payload, err := report.DiscordPayload()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not build the discord notification: %v\n", err)

		return
	}

	response, err := notifier.client.Post(notifier.webhookURL, "application/json", bytes.NewReader(payload))
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not notify discord: %v\n", err)

		return
	}
	defer response.Body.Close()

	if response.StatusCode >= 300 {
		fmt.Fprintf(os.Stderr, "warning: discord rejected the notification with %s\n", response.Status)
	}
}
