package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"slices"
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
	discordTitleLimit       = 256
	discordFieldLimit       = 1024
	discordDescriptionLimit = 4096
)

// Notify is configured by the name of an environment variable rather than by the
// webhook itself, for the same reason as tunnelTokenFrom: a webhook url is a
// credential, anyone holding it can post to the channel, and .deploy.json is
// committed and often pushed somewhere public.
type Notify struct {
	DiscordWebhookFrom string `json:"discordWebhookFrom,omitempty"`
}

type discordMessage struct {
	Embeds []discordEmbed `json:"embeds"`
}

type discordEmbed struct {
	Title       string         `json:"title"`
	Description string         `json:"description,omitempty"`
	Color       int            `json:"color"`
	Fields      []discordField `json:"fields,omitempty"`
	Timestamp   string         `json:"timestamp"`
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
//
// messageID is the message the arc is being told in, and log is what it has said
// so far. Everything worth saying happens after the build, so the stages land
// within seconds of each other and three separate messages read as a wall of
// noise arriving at once. One message that grows instead reads as the thing it
// describes, which is a release moving through states, and somebody arriving
// late still sees the whole of it.
type Notifier struct {
	webhookURL string
	client     *http.Client
	messageID  string
	log        []string
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
		warnAboutStrandedWebhook(repositoryPath)

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

// warnAboutStrandedWebhook covers the way notifications actually go missing,
// which is not a bad url but a config that never names one. Deploying with a
// webhook sitting in the secrets file and no notify block is silent and looks
// exactly like a deploy where nobody wanted notifications, so it gets said out
// loud. Rewriting the config out from under somebody is not the answer, because
// leaving the block out on purpose is allowed.
func warnAboutStrandedWebhook(repositoryPath string) {
	secretsPath := path.Join(repositoryPath, deployDirectoryName, secretsFileName)

	raw, err := os.ReadFile(secretsPath)
	if err != nil {
		return
	}

	for name, value := range ParseEnvFile(raw) {
		if !strings.Contains(value, "/api/webhooks/") {
			continue
		}

		fmt.Fprintf(os.Stderr,
			"warning: %s holds %s, but %s names no webhook, so nothing is being notified. wire it up with: deploy dwh <webhook url>\n",
			secretsPath, name, configFileName,
		)

		return
	}
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

// post says the next thing, by editing what was already said if there is
// anything, and by starting the message if there is not. An edit that does not
// take is not worth failing over, so it falls back to a new message and the
// worst case is the behaviour we used to have.
func (notifier *Notifier) post(payload []byte) {
	if notifier.messageID != "" {
		if notifier.edit(payload) {
			return
		}
		notifier.messageID = ""
	}

	notifier.create(payload)
}

// create posts the message and remembers which one it is. Discord answers a
// plain webhook post with 204 and no body, so wait=true is what turns the reply
// into the message itself, which is the only way to learn the id to edit.
func (notifier *Notifier) create(payload []byte) {
	response, err := notifier.client.Post(
		addQuery(notifier.webhookURL, "wait", "true"), "application/json", bytes.NewReader(payload),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not notify discord: %v\n", err)

		return
	}
	defer response.Body.Close()

	if response.StatusCode >= 300 {
		fmt.Fprintf(os.Stderr, "warning: discord rejected the notification with %s\n", response.Status)

		return
	}

	// no id is not a failure, it just means the rest of the arc gets told in
	// separate messages rather than in this one
	var created struct {
		ID string `json:"id"`
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, createdMessageLimit))
	if err != nil {
		return
	}
	if err := json.Unmarshal(body, &created); err != nil {
		return
	}

	notifier.messageID = created.ID
}

// edit reports whether the message was updated. A false means the message is
// gone, deleted by somebody or lost to a webhook that was rotated underneath us,
// and the caller starts a fresh one rather than going quiet.
func (notifier *Notifier) edit(payload []byte) bool {
	request, err := http.NewRequest(
		http.MethodPatch, messageURL(notifier.webhookURL, notifier.messageID), bytes.NewReader(payload),
	)
	if err != nil {
		return false
	}
	request.Header.Set("Content-Type", "application/json")

	response, err := notifier.client.Do(request)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not notify discord: %v\n", err)

		return false
	}
	defer response.Body.Close()

	return response.StatusCode < 300
}

// createdMessageLimit is only ever read for an id, so the rest of what Discord
// returns about a message is not worth holding.
const createdMessageLimit = 1 << 16

// addQuery and messageURL both leave any query the webhook already carries
// alone, since a webhook posting into a thread carries thread_id and dropping it
// would move the message to the parent channel.
func addQuery(webhookURL, name, value string) string {
	parsed, err := url.Parse(webhookURL)
	if err != nil {
		return webhookURL
	}

	query := parsed.Query()
	query.Set(name, value)
	parsed.RawQuery = query.Encode()

	return parsed.String()
}

func messageURL(webhookURL, messageID string) string {
	parsed, err := url.Parse(webhookURL)
	if err != nil {
		return webhookURL
	}

	parsed.Path = strings.TrimSuffix(parsed.Path, "/") + "/messages/" + messageID

	return parsed.String()
}

// Everything below is addressed to the people using the thing being deployed,
// not to whoever ran the command. That audience changes the rules. They have
// never heard of a commit, they know the site by its hostname, and they only
// care about one question, which is whether the thing is about to move under
// them. So nothing is announced until there is a new version actually standing
// up and healthy, and once something has been announced the arc always finishes,
// either live or cancelled.

type DeployStage int

const (
	stageReady DeployStage = iota
	stageSwitching
	stageLive
	stageCancelled
	stageRollingBack
	stageRollbackFailed
	stageDowntime
)

func (stage DeployStage) title(projectName string) string {
	switch stage {
	case stageSwitching:
		return "switching over"
	case stageLive:
		return "update live"
	case stageCancelled:
		return "reverting changes"
	case stageRollingBack:
		return "rolling back"
	case stageRollbackFailed:
		return "rollback failed"
	case stageDowntime:
		return fmt.Sprintf("%s is experiencing downtime", projectName)
	default:
		return "staging new deployment"
	}
}

// line is what this stage adds to the log, rather than what the message says as
// a whole. Each one has to read as an event at a moment, since it lands under
// everything that came before it and stays there.
func (stage DeployStage) line() string {
	switch stage {
	case stageSwitching:
		return "switching over to the new version now, usually without any interruption"
	case stageLive:
		return "the new version is up and answering"
	case stageCancelled:
		return "we are reverting changes while we fix some issues with the deployment"
	case stageRollingBack:
		return "we are going back to an earlier version, usually without any interruption"
	case stageRollbackFailed:
		return "the earlier version could not be started, so nothing changed"
	case stageDowntime:
		// the title says it all, and a line here would only repeat it
		return ""
	default:
		return "there is a new version being staged"
	}
}

// colour runs blue, amber, green so the three good stages read as a progression
// rather than as three unrelated messages.
func (stage DeployStage) colour() int {
	switch stage {
	case stageSwitching:
		return 0xf39c12
	case stageLive:
		return 0x2ecc71
	case stageCancelled, stageRollbackFailed:
		return 0xe74c3c
	case stageRollingBack, stageDowntime:
		return 0xf39c12
	default:
		return 0x3498db
	}
}

// StageVersions is which releases a stage message is about. Previous is empty on
// a first deploy, where there is nothing to move away from.
type StageVersions struct {
	Previous string
	Incoming string
}

// versionLine points the arrow at whichever release is being ended up on, so a
// deploy reads left to right and a revert reads right to left. Previous stays on
// the left either way, so only the arrow has to be read to know the direction.
func (versions StageVersions) versionLine(reverting bool) string {
	if versions.Previous == "" || versions.Previous == versions.Incoming {
		return versions.Incoming
	}
	if reverting {
		return versions.Previous + " <- " + versions.Incoming
	}

	return versions.Previous + " -> " + versions.Incoming
}

// clockFormat is local to whoever ran the deploy, which for a project with one
// operator is the only clock anybody involved is reading. The seconds are there
// because the stages land within a few of each other and a log where three lines
// share a minute says nothing about the order.
const clockFormat = "15:04:05"

// appendLine grows the log rather than replacing it, so the message ends up
// being the whole story of the release instead of only its last moment.
func appendLine(log []string, stage DeployStage, at time.Time) []string {
	line := stage.line()
	if line == "" {
		return log
	}

	return append(log, at.Format(clockFormat)+"  "+line)
}

// StagePayload is built without touching the network, so the wording can be
// tested without a webhook. The log is every line so far including this stage's,
// because the description is rewritten in full on every edit.
func StagePayload(
	stage DeployStage, log []string, projectName string, affected []string, versions StageVersions,
) ([]byte, error) {
	fields := []discordField{}

	// cancelled and rolling back both end up on the earlier release, so they are
	// the stages whose arrow points the other way
	reverting := stage == stageCancelled || stage == stageRollingBack
	if line := versions.versionLine(reverting); line != "" {
		fields = append(fields, discordField{Name: "version", Value: truncate(line, discordFieldLimit), Inline: true})
	}
	if len(affected) > 0 {
		// a code block one name per line, because a comma separated run of
		// hostnames wraps into an unreadable paragraph once there are a few
		fields = append(fields, discordField{
			Name:   "affected",
			Value:  truncate("```\n"+strings.Join(affected, "\n")+"\n```", discordFieldLimit),
			Inline: true,
		})
	}

	embed := discordEmbed{
		Title:     truncate(stage.title(projectName), discordTitleLimit),
		Color:     stage.colour(),
		Fields:    fields,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}
	if body := strings.Join(log, "\n"); body != "" {
		embed.Description = truncate(body, discordDescriptionLimit)
	}

	return json.Marshal(discordMessage{Embeds: []discordEmbed{embed}})
}

// SendStage swallows its failures for the same reason Send does. A channel that
// missed a message is not a reason to stop a release that is already moving.
func (notifier *Notifier) SendStage(
	stage DeployStage, projectName string, affected []string, versions StageVersions,
) {
	if notifier == nil {
		return
	}

	notifier.log = appendLine(notifier.log, stage, time.Now())

	payload, err := StagePayload(stage, notifier.log, projectName, affected, versions)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not build the %q notification: %v\n", stage.title(projectName), err)

		return
	}

	notifier.post(payload)
}

// AffectedNames is what to call the services in a message to users. Somebody
// reading the channel knows the site by its hostname and has never heard of the
// container behind it, so anything with a host block is named by its domain and
// everything else falls back to the service name.
// Sorted by the label rather than by the service behind it, since a reader has
// no idea that lecternmc.com is called web and would see the ordering as random.
func AffectedNames(services map[string]Service) []string {
	var affected []string
	for name, service := range services {
		if host := service.Host; host != nil && host.Domain != "" {
			affected = append(affected, host.Domain)
			continue
		}
		affected = append(affected, name)
	}
	slices.Sort(affected)

	return affected
}
