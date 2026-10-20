package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// sealedRepository is a throwaway repository with a fresh age key outside it,
// so nothing here ever touches a real secret.
func sealedRepository(t *testing.T) (string, Secrets) {
	t.Helper()
	if _, err := exec.LookPath("age"); err != nil {
		t.Skip("age is not installed")
	}
	repository := t.TempDir()
	identity := filepath.Join(t.TempDir(), "age.key")
	if output, err := exec.Command("age-keygen", "-o", identity).CombinedOutput(); err != nil {
		t.Fatalf("age-keygen: %v %s", err, output)
	}
	if err := os.Chmod(identity, 0o600); err != nil {
		t.Fatal(err)
	}
	recipient, err := exec.Command("age-keygen", "-y", identity).Output()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repository, "secrets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "secrets", "recipients.txt"), recipient, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(ageIdentityVariable, "")

	return repository, Secrets{Directory: "secrets", Identity: identity}
}

func seal(t *testing.T, repository string, secrets Secrets, environment, name, contents string) {
	t.Helper()
	if _, err := secrets.Seal(repository, secrets.SealedPath(repository, environment, name), []byte(contents)); err != nil {
		t.Fatalf("seal %s/%s: %v", environment, name, err)
	}
}

func TestValidateSecrets(t *testing.T) {
	valid := Secrets{Directory: "secrets", Identity: "~/.config/project/age.key"}
	if err := validateSecrets(&valid); err != nil {
		t.Fatalf("valid secrets refused: %v", err)
	}
	for name, secrets := range map[string]Secrets{
		"empty directory":       {Identity: valid.Identity},
		"absolute directory":    {Directory: "/etc", Identity: valid.Identity},
		"escaping directory":    {Directory: "../elsewhere", Identity: valid.Identity},
		"repository root":       {Directory: ".", Identity: valid.Identity},
		"relative identity":     {Directory: "secrets", Identity: "secrets/age.key"},
		"escaping recipients":   {Directory: "secrets", Recipients: "../keys", Identity: valid.Identity},
		"identity with no path": {Directory: "secrets"},
	} {
		if err := validateSecrets(&secrets); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestSealOpensInMemoryAndLeavesUnchangedFilesAlone(t *testing.T) {
	repository, secrets := sealedRepository(t)
	sealed := secrets.SealedPath(repository, "production", "auth.env")
	if changed, err := secrets.Seal(repository, sealed, []byte("AUTH_SECRET=synthetic\n")); err != nil || !changed {
		t.Fatalf("first seal: changed=%v err=%v", changed, err)
	}
	raw, err := os.ReadFile(sealed)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(raw), ageHeader) || strings.Contains(string(raw), "synthetic") {
		t.Fatal("the sealed file is not age ciphertext")
	}
	opened, err := secrets.Decrypt(repository, sealed)
	if err != nil || string(opened) != "AUTH_SECRET=synthetic\n" {
		t.Fatalf("decrypt: %q %v", opened, err)
	}
	// age output differs every run, so an unchanged secret must not be rewritten
	if changed, err := secrets.Seal(repository, sealed, []byte("AUTH_SECRET=synthetic\n")); err != nil || changed {
		t.Fatalf("resealing unchanged content: changed=%v err=%v", changed, err)
	}
	after, _ := os.ReadFile(sealed)
	if string(after) != string(raw) {
		t.Fatal("an unchanged secret was rewritten")
	}
}

func TestIdentityMustBePrivateAndOutsideTheRepository(t *testing.T) {
	repository, secrets := sealedRepository(t)
	seal(t, repository, secrets, "production", "auth.env", "A=1\n")
	sealed := secrets.SealedPath(repository, "production", "auth.env")

	if err := os.Chmod(secrets.Identity, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := secrets.Decrypt(repository, sealed); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("a readable key was used: %v", err)
	}
	if err := os.Chmod(secrets.Identity, 0o600); err != nil {
		t.Fatal(err)
	}

	inside := filepath.Join(repository, "age.key")
	raw, _ := os.ReadFile(secrets.Identity)
	if err := os.WriteFile(inside, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(ageIdentityVariable, inside)
	if _, err := secrets.Decrypt(repository, sealed); err == nil || !strings.Contains(err.Error(), "inside the repository") {
		t.Fatalf("a key inside the repository was used: %v", err)
	}

	// the variable overrides the configured path
	t.Setenv(ageIdentityVariable, secrets.Identity)
	broken := secrets
	broken.Identity = "/nonexistent/age.key"
	if _, err := broken.Decrypt(repository, sealed); err != nil {
		t.Fatalf("%s did not override the configured identity: %v", ageIdentityVariable, err)
	}
}

func TestVerifyTrackedNeedsNoKeyAndRefusesPlaintext(t *testing.T) {
	repository, secrets := sealedRepository(t)
	seal(t, repository, secrets, "production", "auth.env", "A=1\n")
	if problems, err := secrets.VerifyTracked(repository); err != nil || len(problems) != 0 {
		t.Fatalf("clean tree: %v %v", problems, err)
	}
	// no key needed: verify works with the identity gone
	secrets.Identity = "/nonexistent"
	for relative, contents := range map[string]string{
		"production/api.env":             "API=leaked\n",
		"production/core.env.age":        "CORE=leaked\n",
		"stray.age":                      ageHeader,
		"production/nested/deep.env.age": ageHeader,
	} {
		file := filepath.Join(repository, "secrets", relative)
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
		problems, err := secrets.VerifyTracked(repository)
		if err != nil || len(problems) == 0 {
			t.Errorf("%s was not refused", relative)
		}
		if err := os.Remove(file); err != nil {
			t.Fatal(err)
		}
	}
	recipients := filepath.Join(repository, "secrets", "recipients.txt")
	if err := os.WriteFile(recipients, []byte("AGE-SECRET-KEY-1SYNTHETIC\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if problems, _ := secrets.VerifyTracked(repository); len(problems) == 0 {
		t.Error("a private key in the recipients file was not refused")
	}
}

func TestEnvPushSendsTheSealedCopyAndRefusesPlaintext(t *testing.T) {
	repository, secrets := sealedRepository(t)
	seal(t, repository, secrets, "production", "auth.env", "AUTH_SECRET=sealed\n")
	seal(t, repository, secrets, "staging", "tunnel.env", "TOKEN=staging\n")
	resolved := ResolvedProject{Environment: "production", Secrets: &secrets}

	name, contents, err := envPushContents(repository, resolved, "auth.env", false)
	if err != nil || name != "auth.env" || string(contents) != "AUTH_SECRET=sealed\n" {
		t.Fatalf("bare name: %q %q %v", name, contents, err)
	}
	name, contents, err = envPushContents(repository, resolved, secrets.SealedPath(repository, "production", "auth.env"), false)
	if err != nil || name != "auth.env" || string(contents) != "AUTH_SECRET=sealed\n" {
		t.Fatalf("sealed path: %q %q %v", name, contents, err)
	}
	// a staging secret is never pushed to production by mistake
	if _, _, err := envPushContents(repository, resolved, secrets.SealedPath(repository, "staging", "tunnel.env"), false); err == nil || !strings.Contains(err.Error(), "add -e staging") {
		t.Fatalf("a staging secret went to production: %v", err)
	}

	plaintext := filepath.Join(t.TempDir(), "auth.env")
	if err := os.WriteFile(plaintext, []byte("AUTH_SECRET=plaintext\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := envPushContents(repository, resolved, plaintext, false); err == nil || !strings.Contains(err.Error(), "--allow-plaintext") {
		t.Fatalf("plaintext was pushed without asking: %v", err)
	}
	if name, contents, err := envPushContents(repository, resolved, plaintext, true); err != nil || name != "auth.env" || string(contents) != "AUTH_SECRET=plaintext\n" {
		t.Fatalf("allowed plaintext: %q %q %v", name, contents, err)
	}
	if _, _, err := envPushContents(repository, resolved, "missing.env", false); err == nil || !strings.Contains(err.Error(), "no sealed copy") {
		t.Fatalf("a name with no sealed copy: %v", err)
	}

	// without secrets configured nothing changes: the file is read as it always was
	if name, contents, err := envPushContents(repository, ResolvedProject{Environment: "production"}, plaintext, false); err != nil || name != "auth.env" || string(contents) != "AUTH_SECRET=plaintext\n" {
		t.Fatalf("an unsealed project: %q %q %v", name, contents, err)
	}
}

func TestCheckSealedNamesEveryMissingEnvFile(t *testing.T) {
	repository, secrets := sealedRepository(t)
	resolved := ResolvedProject{
		Environment: "production",
		Secrets:     &secrets,
		Services: map[string]Service{
			"api": {Env: []string{"api.env", "./config/public.env"}, Host: &Host{TunnelEnv: "tunnel.env"}},
		},
		Release: map[string]Service{"migrate": {Env: []string{"migrate.env"}}},
	}
	err := secrets.CheckSealed(repository, resolved)
	if err == nil || !strings.Contains(err.Error(), "api.env, migrate.env, tunnel.env") {
		t.Fatalf("missing files were not named: %v", err)
	}
	for _, name := range []string{"api.env", "migrate.env", "tunnel.env"} {
		seal(t, repository, secrets, "production", name, name+"=1\n")
	}
	if err := secrets.CheckSealed(repository, resolved); err != nil {
		t.Fatalf("a fully sealed project was refused: %v", err)
	}
	// a path that ships with the code is not deploy's to seal
	if got := sealedEnvFiles(resolved); strings.Join(got, ",") != "api.env,migrate.env,tunnel.env" {
		t.Fatalf("sealedEnvFiles = %v", got)
	}
}

func TestSealedProjectsReadTheWebhookFromTheSealedSecretsFile(t *testing.T) {
	repository, secrets := sealedRepository(t)
	resolved := ResolvedProject{Environment: "production", Secrets: &secrets, Notify: &Notify{DiscordWebhookFrom: "DEPLOY_TEST_WEBHOOK"}}
	t.Setenv("DEPLOY_TEST_WEBHOOK", "")

	if _, err := NewProjectNotifier(repository, resolved); err == nil || !strings.Contains(err.Error(), "deploy secrets edit secrets.env") {
		t.Fatalf("a missing sealed webhook: %v", err)
	}
	// a plaintext secrets file is not consulted once the project is sealed
	if err := os.MkdirAll(filepath.Join(repository, deployDirectoryName), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, deployDirectoryName, secretsFileName), []byte("DEPLOY_TEST_WEBHOOK=https://discord.com/api/webhooks/plaintext\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewProjectNotifier(repository, resolved); err == nil {
		t.Fatal("the plaintext secrets file was used for a sealed project")
	}

	seal(t, repository, secrets, "production", secretsFileName, "DEPLOY_TEST_WEBHOOK=https://discord.com/api/webhooks/sealed\n")
	notifier, err := NewProjectNotifier(repository, resolved)
	if err != nil || notifier.webhookURL != "https://discord.com/api/webhooks/sealed" {
		t.Fatalf("sealed webhook: %v", err)
	}
	t.Setenv("DEPLOY_TEST_WEBHOOK", "https://discord.com/api/webhooks/shell")
	if notifier, err := NewProjectNotifier(repository, resolved); err != nil || notifier.webhookURL != "https://discord.com/api/webhooks/shell" {
		t.Fatalf("the shell did not win: %v", err)
	}
	t.Setenv("DEPLOY_TEST_WEBHOOK", "")

	// notify is project wide: staging without its own secrets.env uses production's
	staging := resolved
	staging.Environment = "staging"
	if notifier, err := NewProjectNotifier(repository, staging); err != nil || notifier.webhookURL != "https://discord.com/api/webhooks/sealed" {
		t.Fatalf("staging did not fall back to the default environment's webhook: %v", err)
	}
	// and one with its own uses its own
	seal(t, repository, secrets, "staging", secretsFileName, "DEPLOY_TEST_WEBHOOK=https://discord.com/api/webhooks/staging\n")
	if notifier, err := NewProjectNotifier(repository, staging); err != nil || notifier.webhookURL != "https://discord.com/api/webhooks/staging" {
		t.Fatalf("staging's own webhook was not used: %v", err)
	}
}

func TestRemoteDigestsReadsEveryFileInOneCall(t *testing.T) {
	root := t.TempDir()
	layout := NewLayout(root, "6074be63")
	if err := os.MkdirAll(layout.EnvDirectory(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(layout.EnvDirectory(), "auth.env"), []byte("A=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	digests, err := remoteDigests(LocalRunner{}, layout, []string{"auth.env", "missing.env", "odd name.env"})
	if err != nil {
		t.Fatal(err)
	}
	if digests["auth.env"] != digest([]byte("A=1\n")) || digests["missing.env"] != "" || len(digests) != 1 {
		t.Fatalf("digests = %v", digests)
	}
	// an env directory that does not exist yet reads as nothing pushed
	if digests, err := remoteDigests(LocalRunner{}, NewLayout(root, "missing0"), []string{"auth.env"}); err != nil || len(digests) != 0 {
		t.Fatalf("missing directory: %v %v", digests, err)
	}
}
