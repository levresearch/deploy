package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"filippo.io/age"
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

var sealedName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

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

// identities parses the key file with the age library, so no age binary is
// needed anywhere deploy runs. Its errors never echo key material.
func (secrets Secrets) identities(repositoryPath string) ([]age.Identity, error) {
	path, err := secrets.identityPath(repositoryPath)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	identities, err := age.ParseIdentities(file)
	if err != nil {
		return nil, fmt.Errorf("%s is not an age key: %w", path, err)
	}

	return identities, nil
}

// Decrypt opens a sealed file into memory; the plaintext never touches a disk.
func (secrets Secrets) Decrypt(repositoryPath, sealed string) ([]byte, error) {
	identities, err := secrets.identities(repositoryPath)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(sealed)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	plaintext, err := age.Decrypt(file, identities...)
	if err != nil {
		return nil, fmt.Errorf("could not open %s: %w", sealed, err)
	}
	contents, err := io.ReadAll(io.LimitReader(plaintext, sealedSizeLimit+1))
	if err != nil {
		return nil, fmt.Errorf("could not open %s: %w", sealed, err)
	}
	if len(contents) > sealedSizeLimit {
		return nil, fmt.Errorf("%s opens to more than %d bytes, which is not an env file", sealed, sealedSizeLimit)
	}

	return contents, nil
}

// Seal encrypts plaintext from memory to every recipient. A file whose content
// has not changed is left alone, since age output differs on every run and a
// rewrite would show up as a change in git for nothing.
func (secrets Secrets) Seal(repositoryPath, sealed string, plaintext []byte) (bool, error) {
	if current, err := secrets.Decrypt(repositoryPath, sealed); err == nil && bytes.Equal(current, plaintext) {
		return false, nil
	}
	recipients, err := readRecipients(secrets.recipientsPath(repositoryPath))
	if err != nil {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(sealed), 0o755); err != nil {
		return false, err
	}
	var ciphertext bytes.Buffer
	writer, err := age.Encrypt(&ciphertext, recipients...)
	if err != nil {
		return false, fmt.Errorf("could not seal %s: %w", sealed, err)
	}
	if _, err := writer.Write(plaintext); err != nil {
		return false, fmt.Errorf("could not seal %s: %w", sealed, err)
	}
	if err := writer.Close(); err != nil {
		return false, fmt.Errorf("could not seal %s: %w", sealed, err)
	}
	temporary := sealed + ".tmp"
	if err := os.WriteFile(temporary, ciphertext.Bytes(), 0o644); err != nil {
		return false, err
	}

	return true, os.Rename(temporary, sealed)
}

// Keygen writes a new private key, mode 600, and returns its public key, so a
// project can be set up without the age tools installed. It never overwrites a
// key, since losing one makes every file sealed to it unreadable.
func Keygen(path string) (string, error) {
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return "", fmt.Errorf("%s already exists; deploy never overwrites an age key", path)
	}
	if err != nil {
		return "", err
	}
	recipient := identity.Recipient().String()
	_, err = fmt.Fprintf(file, "# public key: %s\n%s\n", recipient, identity.String())
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}

	return recipient, err
}

func readRecipients(file string) ([]age.Recipient, error) {
	opened, err := os.Open(file)
	if err != nil {
		return nil, fmt.Errorf("reading the age recipients: %w", err)
	}
	defer opened.Close()
	recipients, err := age.ParseRecipients(opened)
	if err != nil {
		return nil, fmt.Errorf("%s holds something other than age public keys: %w", file, err)
	}

	return recipients, nil
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

// CheckSealed runs before a deploy touches anything: the key opens and every env
// file the project needs has a sealed copy for this environment. It catches the deploy that would otherwise succeed with whatever
// stale file happens to sit on the destination.
func (secrets Secrets) CheckSealed(repositoryPath string, resolved ResolvedProject) error {
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
