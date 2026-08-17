package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path"
	"strings"
)

// deploy dwh is the only command that writes a credential, so it is also the
// only place that has to get file modes, gitignoring, and "does this thing even
// work" right. Doing all three by hand is how a webhook ends up in a commit.

// defaultWebhookVariable is what dwh writes when the project names no variable
// of its own. Editing the name later still works, since the config holds the
// name and this only picks a default for it.
const defaultWebhookVariable = "DISCORD_WEBHOOK_URL"

// RunConfigureWebhook stores a webhook and points the project at it. The url
// arrives on the command line, which is the one place a secret is allowed to be
// here, because the alternative is somebody pasting it into a committed file.
//
// The client is a parameter because the https check below is real, so testing
// the posting means a tls server, and trusting its certificate means handing in
// its client rather than building one in here.
func RunConfigureWebhook(options DeployOptions, webhookURL string, client *http.Client) (int, error) {
	if client == nil {
		client = newNotifyClient()
	}

	startPath := options.Context
	if startPath == "" {
		working, err := os.Getwd()
		if err != nil {
			return exitPreconditionNotMet, err
		}
		startPath = working
	}

	repositoryPath, err := FindRepository(startPath)
	if err != nil {
		return exitPreconditionNotMet, err
	}

	if _, err := EnsureProjectConfig(repositoryPath, options.Destination); err != nil {
		return exitPreconditionNotMet, err
	}

	project, err := LoadProject(path.Join(repositoryPath, configFileName))
	if err != nil {
		return exitPreconditionNotMet, err
	}

	variable := defaultWebhookVariable
	if project.Notify != nil && project.Notify.DiscordWebhookFrom != "" {
		variable = project.Notify.DiscordWebhookFrom
	}

	// with no url this reports rather than configures, so there is a way to ask
	// whether a project is wired up that does not involve printing the secret
	if webhookURL == "" {
		return describeWebhook(repositoryPath, project, variable)
	}

	if err := checkWebhookURL(webhookURL); err != nil {
		return exitPreconditionNotMet, err
	}

	if err := writeSecret(repositoryPath, variable, webhookURL); err != nil {
		return exitPreconditionNotMet, err
	}
	fmt.Printf("  wrote %s to %s\n", variable, path.Join(deployDirectoryName, secretsFileName))

	added, err := ensureNotifyBlock(repositoryPath, project, variable)
	if err != nil {
		return exitPreconditionNotMet, err
	}
	if added {
		fmt.Printf("  added a notify block to %s naming %s\n", configFileName, variable)
	}

	if _, err := ignoreDeployDirectory(repositoryPath); err != nil {
		return exitPreconditionNotMet, err
	}

	// posting once is what turns "configured" into "works". a webhook that was
	// revoked or pasted wrong looks identical on disk to one that is fine
	notifier := &Notifier{webhookURL: webhookURL, client: client}
	if err := notifier.SendTest(project.Name); err != nil {
		return exitPreconditionNotMet, fmt.Errorf(
			"the webhook was saved, but posting a test message to it failed, so it is probably wrong: %w", err,
		)
	}

	fmt.Printf("  posted a test message, check the channel\n")

	return exitOK, nil
}

func describeWebhook(repositoryPath string, project Project, variable string) (int, error) {
	if project.Notify == nil || project.Notify.DiscordWebhookFrom == "" {
		fmt.Printf("%s has no notify block, so nothing is notified\n", project.Name)
		fmt.Printf("  set one up with: deploy dwh <webhook url>\n")

		return exitOK, nil
	}

	fmt.Printf("%s notifies discord using %s\n", project.Name, variable)

	// the value is never printed, only where it was found, since the whole point
	// of the variable indirection is that the url does not get shown around
	if value := strings.TrimSpace(os.Getenv(variable)); value != "" {
		fmt.Printf("  found in this shell\n")

		return exitOK, nil
	}

	secretsPath := path.Join(repositoryPath, deployDirectoryName, secretsFileName)
	raw, err := os.ReadFile(secretsPath)
	if errors.Is(err, fs.ErrNotExist) {
		fmt.Printf("  NOT found, neither in this shell nor in %s\n", secretsPath)

		return exitOK, nil
	}
	if err != nil {
		return exitPreconditionNotMet, err
	}
	if _, found := ParseEnvFile(raw)[variable]; !found {
		fmt.Printf("  NOT found, %s exists but has no %s in it\n", secretsPath, variable)

		return exitOK, nil
	}

	fmt.Printf("  found in %s\n", secretsPath)

	return exitOK, nil
}

// checkWebhookURL refuses here rather than warning, unlike the check at deploy
// time. Somebody running this command is configuring on purpose, so a typo is
// worth stopping for, and stopping costs them nothing.
func checkWebhookURL(webhookURL string) error {
	if !strings.HasPrefix(webhookURL, "https://") {
		return fmt.Errorf("a discord webhook url starts with https://, and %q does not", webhookURL)
	}
	if !strings.Contains(webhookURL, "/api/webhooks/") {
		return fmt.Errorf(
			"%q does not contain /api/webhooks/, so it is not a webhook url. the one you want is in the channel's integrations settings, not the channel link",
			webhookURL,
		)
	}

	return nil
}

// writeSecret keeps the directory and the file tight, and replaces an existing
// entry rather than appending a second one that the parser would then have to
// pick between.
func writeSecret(repositoryPath, name, value string) error {
	directory := path.Join(repositoryPath, deployDirectoryName)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}

	secretsPath := path.Join(directory, secretsFileName)

	existing, err := os.ReadFile(secretsPath)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	return os.WriteFile(secretsPath, upsertEnvLine(existing, name, value), 0o600)
}

// upsertEnvLine rewrites one assignment and leaves every other line exactly as
// it was, comments included, because this file is a place people keep things by
// hand and a rewriter that reformats it is a rewriter nobody trusts.
func upsertEnvLine(existing []byte, name, value string) []byte {
	assignment := name + "=" + value

	lines := strings.Split(strings.TrimSuffix(string(existing), "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return []byte(assignment + "\n")
	}

	for index, line := range lines {
		bare := strings.TrimPrefix(strings.TrimSpace(line), "export ")
		if key, _, found := strings.Cut(bare, "="); found && strings.TrimSpace(key) == name {
			lines[index] = assignment

			return []byte(strings.Join(lines, "\n") + "\n")
		}
	}

	return []byte(strings.Join(append(lines, assignment), "\n") + "\n")
}

// ensureNotifyBlock adds the key by editing the text rather than by re-encoding
// the config. Go maps have no order, so marshalling a project back out would
// alphabetise everybody's services and hand them a diff they did not ask for.
func ensureNotifyBlock(repositoryPath string, project Project, variable string) (bool, error) {
	if project.Notify != nil && project.Notify.DiscordWebhookFrom != "" {
		return false, nil
	}

	configPath := path.Join(repositoryPath, configFileName)

	raw, err := os.ReadFile(configPath)
	if err != nil {
		return false, err
	}

	// anchored on services, which every valid config has, and which sorts after
	// the identity keys the block belongs beside
	anchor := strings.Index(string(raw), `"services"`)
	if anchor < 0 {
		return false, fmt.Errorf(
			`could not find "services" in %s, so add this to it by hand:  "notify": {"discordWebhookFrom": "%s"}`,
			configPath, variable,
		)
	}

	lineStart := strings.LastIndex(string(raw[:anchor]), "\n") + 1
	indent := string(raw[lineStart:anchor])

	block := fmt.Sprintf("\"notify\": {\n%s  \"discordWebhookFrom\": \"%s\"\n%s},\n%s", indent, variable, indent, indent)

	updated := string(raw[:lineStart]) + indent + block + string(raw[anchor:])

	// the edit is textual, so it is checked before it is kept rather than after
	// somebody's next deploy fails on a config this command broke
	var check Project
	if err := json.Unmarshal([]byte(updated), &check); err != nil {
		return false, fmt.Errorf("adding the notify block to %s would have broken it: %w", configPath, err)
	}

	return true, os.WriteFile(configPath, []byte(updated), 0o644)
}
