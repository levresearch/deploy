package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Secrets keeps env files in the repository as age ciphertext, one file per
// environment and name: <directory>/<environment>/<name>.age. Once a project
// configures it, the sealed copy is the source of truth: env push sends it,
// deploy refuses to start without it, and a plaintext env file needs saying so
// out loud. The private key never belongs in the repository, so it is a path on
// this machine, and DEPLOY_AGE_IDENTITY overrides it for a one-off or for CI.
type Secrets struct {
	Directory string `json:"directory"`
	// Recipients defaults to recipients.txt inside Directory: the public keys
	// every sealed file is encrypted to.
	Recipients string `json:"recipients,omitempty"`
	Identity   string `json:"identity"`
}

const (
	ageIdentityVariable = "DEPLOY_AGE_IDENTITY"
	ageHeader           = "age-encryption.org/v1\n"
	sealedSuffix        = ".age"
	// an env file is small; anything this large is a mistake, not a secret
	sealedSizeLimit = 1 << 20
)

var (
	sealedName   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	ageRecipient = regexp.MustCompile(`^age1[02-9ac-hj-np-z]{58}$`)
)

func validateSecrets(secrets *Secrets) error {
	if secrets == nil {
		return nil
	}
	directory := path.Clean(secrets.Directory)
	if secrets.Directory == "" || path.IsAbs(directory) || directory == "." || strings.HasPrefix(directory, "..") {
		return errors.New("secrets.directory must be a path inside the repository, such as \"secrets\"")
	}
	if secrets.Recipients != "" {
		recipients := path.Clean(secrets.Recipients)
		if path.IsAbs(recipients) || strings.HasPrefix(recipients, "..") {
			return errors.New("secrets.recipients must be a path inside the repository")
		}
	}
	if !strings.HasPrefix(secrets.Identity, "~/") && !path.IsAbs(secrets.Identity) {
		return errors.New("secrets.identity must be an absolute or ~/ path to the age private key on this machine, never a file in the repository")
	}

	return nil
}

func (secrets Secrets) recipientsPath(repositoryPath string) string {
	if secrets.Recipients != "" {
		return filepath.Join(repositoryPath, secrets.Recipients)
	}

	return filepath.Join(repositoryPath, secrets.Directory, "recipients.txt")
}

// SealedPath is where one environment's copy of an env file lives.
func (secrets Secrets) SealedPath(repositoryPath, environment, name string) string {
	return filepath.Join(repositoryPath, secrets.Directory, environment, name+sealedSuffix)
}

// identityPath refuses a key anyone else on the machine could read, and one that
// sits inside the repository, where a careless add would publish it.
func (secrets Secrets) identityPath(repositoryPath string) (string, error) {
	identity := strings.TrimSpace(os.Getenv(ageIdentityVariable))
	if identity == "" {
		identity = secrets.Identity
	}
	if rest, found := strings.CutPrefix(identity, "~/"); found {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		identity = filepath.Join(home, rest)
	}
	info, err := os.Stat(identity)
	if errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("no age key at %s; restore it from wherever you keep it, or point %s at it", identity, ageIdentityVariable)
	}
	if err != nil {
		return "", err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("%s is readable by other users; tighten it with chmod 600", identity)
	}
	if inside, _ := filepath.Rel(repositoryPath, identity); !strings.HasPrefix(inside, "..") && !filepath.IsAbs(inside) {
		return "", fmt.Errorf("%s is inside the repository; keep the age key outside it", identity)
	}

	return identity, nil
}

// requireAge is the requirement made explicit: a project that seals its secrets
// cannot be deployed from a machine that cannot open them.
func requireAge() error {
	if _, err := exec.LookPath("age"); err != nil {
		return errors.New("this project keeps its env files sealed with age, and age is not installed here. install it (for example apt install age, brew install age, or xbps-install age) and try again")
	}

	return nil
}

// Decrypt opens a sealed file straight into memory: age writes the plaintext to
// a pipe this process reads, so it never touches a disk.
func (secrets Secrets) Decrypt(repositoryPath, sealed string) ([]byte, error) {
	if err := requireAge(); err != nil {
		return nil, err
	}
	identity, err := secrets.identityPath(repositoryPath)
	if err != nil {
		return nil, err
	}
	var plaintext, stderr bytes.Buffer
	command := exec.Command("age", "--decrypt", "--identity", identity, sealed)
	command.Stdout = &plaintext
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("could not open %s: %s", sealed, firstLine(stderr.Bytes()))
	}
	if plaintext.Len() > sealedSizeLimit {
		return nil, fmt.Errorf("%s opens to more than %d bytes, which is not an env file", sealed, sealedSizeLimit)
	}

	return plaintext.Bytes(), nil
}

// Seal encrypts plaintext from memory to every recipient. A file whose content
// has not changed is left alone, since age output differs on every run and a
// rewrite would show up as a change in git for nothing.
func (secrets Secrets) Seal(repositoryPath, sealed string, plaintext []byte) (bool, error) {
	if err := requireAge(); err != nil {
		return false, err
	}
	if current, err := secrets.Decrypt(repositoryPath, sealed); err == nil && bytes.Equal(current, plaintext) {
		return false, nil
	}
	recipients := secrets.recipientsPath(repositoryPath)
	if _, err := readRecipients(recipients); err != nil {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(sealed), 0o755); err != nil {
		return false, err
	}
	temporary := sealed + ".tmp"
	var stderr bytes.Buffer
	command := exec.Command("age", "--encrypt", "--recipients-file", recipients, "--output", temporary)
	command.Stdin = bytes.NewReader(plaintext)
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		_ = os.Remove(temporary)
		return false, fmt.Errorf("could not seal %s: %s", sealed, firstLine(stderr.Bytes()))
	}

	return true, os.Rename(temporary, sealed)
}

func readRecipients(file string) ([]string, error) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("reading the age recipients: %w", err)
	}
	var keys []string
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !ageRecipient.MatchString(line) {
			return nil, fmt.Errorf("%s holds something other than age public keys", file)
		}
		keys = append(keys, line)
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("%s names no age recipient", file)
	}

	return keys, nil
}

// VerifyTracked needs no key, so CI can run it: the secrets directory may hold
// only age ciphertext in an environment directory and the public recipients.
// Anything else there is either plaintext that should never be committed or a
// file in the wrong place.
func (secrets Secrets) VerifyTracked(repositoryPath string) ([]string, error) {
	root := filepath.Join(repositoryPath, secrets.Directory)
	recipients := secrets.recipientsPath(repositoryPath)
	var problems []string
	err := filepath.WalkDir(root, func(file string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		relative, _ := filepath.Rel(root, file)
		if file == recipients {
			if _, err := readRecipients(file); err != nil {
				problems = append(problems, err.Error())
			}
			return nil
		}
		parts := strings.Split(filepath.ToSlash(relative), "/")
		if len(parts) != 2 || !sealedName.MatchString(parts[0]) || !strings.HasSuffix(parts[1], sealedSuffix) || !sealedName.MatchString(strings.TrimSuffix(parts[1], sealedSuffix)) {
			problems = append(problems, fmt.Sprintf("%s is not <environment>/<name>.age", relative))
			return nil
		}
		head := make([]byte, len(ageHeader))
		opened, err := os.Open(file)
		if err != nil {
			return err
		}
		defer opened.Close()
		if n, _ := opened.Read(head); n != len(ageHeader) || string(head) != ageHeader {
			problems = append(problems, fmt.Sprintf("%s is not age ciphertext", relative))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(recipients); err != nil {
		problems = append(problems, fmt.Sprintf("missing %s", recipients))
	}
	slices.Sort(problems)

	return problems, nil
}

// sealedEnvFiles lists what an environment's project needs sealed: every env
// file a service or release task reads from the env directory.
func sealedEnvFiles(resolved ResolvedProject) []string {
	wanted := map[string]bool{}
	for _, service := range resolved.Services {
		collectProjectEnvFiles(service, wanted)
	}
	for _, task := range resolved.Release {
		collectProjectEnvFiles(task, wanted)
	}
	names := make([]string, 0, len(wanted))
	for name := range wanted {
		names = append(names, name)
	}
	slices.Sort(names)

	return names
}

// CheckSealed runs before a deploy touches anything: age is installed, the key
// opens, and every env file the project needs has a sealed copy for this
// environment. It catches the deploy that would otherwise succeed with whatever
// stale file happens to sit on the destination.
func (secrets Secrets) CheckSealed(repositoryPath string, resolved ResolvedProject) error {
	if err := requireAge(); err != nil {
		return err
	}
	var missing []string
	for _, name := range sealedEnvFiles(resolved) {
		sealed := secrets.SealedPath(repositoryPath, resolved.Environment, name)
		if _, err := os.Stat(sealed); err != nil {
			missing = append(missing, name)
			continue
		}
		if _, err := secrets.Decrypt(repositoryPath, sealed); err != nil {
			return err
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf(
			"%s has no sealed copy for %s under %s. seal them with deploy secrets edit <name> -e %s",
			strings.Join(missing, ", "), resolved.Environment, filepath.Join(secrets.Directory, resolved.Environment), resolved.Environment,
		)
	}

	return nil
}

func digest(contents []byte) string {
	sum := sha256.Sum256(contents)

	return hex.EncodeToString(sum[:])
}

// sealedValue reads one variable from an environment's sealed env file, which is
// how a sealed project finds its webhook.
func (secrets Secrets) sealedValue(repositoryPath, environment, file, name string) (string, error) {
	sealed := secrets.SealedPath(repositoryPath, environment, file)
	if _, err := os.Stat(sealed); err != nil {
		return "", fmt.Errorf("%s is set neither in this shell nor in %s. add it with deploy secrets edit %s -e %s", name, sealed, file, environment)
	}
	plaintext, err := secrets.Decrypt(repositoryPath, sealed)
	if err != nil {
		return "", err
	}
	value := strings.TrimSpace(ParseEnvFile(plaintext)[name])
	if value == "" {
		return "", fmt.Errorf("%s has no %s. add it with deploy secrets edit %s -e %s", sealed, name, file, environment)
	}

	return value, nil
}
