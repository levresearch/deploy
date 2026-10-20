package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// RunEnvDiff compares each sealed env file with the copy on every destination
// that reads it. Only digests cross the wire, never contents, so it answers
// whether production drifted from the repository without exposing either side.
func RunEnvDiff(options DeployOptions) (int, error) {
	repositoryPath, isService := isServiceConfig(options)
	if !isService {
		return exitPreconditionNotMet, errors.New("env diff needs a version 2 config with secrets")
	}
	resolved, opened, closeAll, err := openServiceDestinations(repositoryPath, options)
	if err != nil {
		return exitPreconditionNotMet, err
	}
	defer closeAll()
	if resolved.Secrets == nil {
		return exitPreconditionNotMet, errors.New("this project configures no secrets to compare against")
	}

	local := map[string]string{}
	drifted := 0
	for _, destination := range opened {
		names := sealedEnvFiles(destination.view)
		if len(names) == 0 {
			continue
		}
		remote, err := remoteDigests(destination.runner, destination.layout, names)
		if err != nil {
			return exitPreconditionNotMet, fmt.Errorf("destination %s: %w", destination.name, err)
		}
		for _, name := range names {
			sum, sealed := local[name]
			if !sealed {
				path := resolved.Secrets.SealedPath(repositoryPath, resolved.Environment, name)
				if _, err := os.Stat(path); err == nil {
					contents, err := resolved.Secrets.Decrypt(repositoryPath, path)
					if err != nil {
						return exitPreconditionNotMet, err
					}
					sum, sealed = digest(contents), true
					local[name] = sum
				}
			}
			status := "same"
			switch {
			case !sealed:
				status = "not sealed"
			case remote[name] == "":
				status = "missing on destination"
			case remote[name] != sum:
				status = "differs"
			}
			if status != "same" {
				drifted++
			}
			fmt.Printf("%-12s %-32s %s\n", destination.name, name, status)
		}
	}
	if drifted > 0 {
		return exitDeployFailed, fmt.Errorf(
			"%d env files differ from their sealed copies; push the sealed copy with deploy env push <name> -e %s, or seal the live one with deploy secrets edit",
			drifted, resolved.Environment,
		)
	}

	return exitOK, nil
}

// remoteDigests asks once for every file rather than paying a round trip each.
func remoteDigests(runner Runner, layout Layout, names []string) (map[string]string, error) {
	quoted := make([]string, len(names))
	for index, name := range names {
		quoted[index] = ShellQuote(name)
	}
	script := fmt.Sprintf("cd %s 2>/dev/null && sha256sum -- %s 2>/dev/null; true", ShellQuote(layout.EnvDirectory()), strings.Join(quoted, " "))
	output, err := runner.Run([]string{"sh", "-c", script})
	if err != nil {
		return nil, fmt.Errorf("reading env digests: %s", firstLine(output))
	}
	digests := map[string]string{}
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && len(fields[0]) == 64 {
			digests[strings.TrimPrefix(fields[1], "*")] = fields[0]
		}
	}

	return digests, nil
}

// RunSecrets is deploy secrets edit|seal|check|verify.
func RunSecrets(options DeployOptions, action, name string) (int, error) {
	repositoryPath, err := repositoryFor(options)
	if err != nil {
		return exitPreconditionNotMet, err
	}
	resolved, _, err := loadTopology(repositoryPath, options.Environment)
	if err != nil {
		return exitPreconditionNotMet, err
	}
	secrets := resolved.Secrets
	if secrets == nil {
		return exitPreconditionNotMet, errors.New("this project configures no secrets; add a secrets block to " + configFileName)
	}

	switch action {
	case "edit":
		return editSecret(repositoryPath, *secrets, resolved.Environment, name)
	case "seal":
		contents, err := os.ReadFile(name)
		if err != nil {
			return exitPreconditionNotMet, err
		}
		target := filepath.Base(name)
		if !sealedName.MatchString(target) {
			return exitPreconditionNotMet, fmt.Errorf("%q is not a plain env file name", target)
		}
		changed, err := secrets.Seal(repositoryPath, secrets.SealedPath(repositoryPath, resolved.Environment, target), contents)
		if err != nil {
			return exitDeployFailed, err
		}
		reportSealed(resolved.Environment, target, changed)

		return exitOK, nil
	case "check":
		if err := secrets.CheckSealed(repositoryPath, resolved); err != nil {
			return exitDeployFailed, err
		}
		root := filepath.Join(repositoryPath, secrets.Directory)
		count := 0
		err := filepath.WalkDir(root, func(file string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() || !strings.HasSuffix(file, sealedSuffix) {
				return err
			}
			contents, err := secrets.Decrypt(repositoryPath, file)
			if err != nil {
				return err
			}
			relative, _ := filepath.Rel(root, file)
			fmt.Printf("ok %s (%d bytes)\n", relative, len(contents))
			count++
			return nil
		})
		if err != nil {
			return exitDeployFailed, err
		}
		fmt.Printf("%d sealed files open, and %s has every env file it needs\n", count, resolved.Environment)

		return exitOK, nil
	case "verify":
		problems, err := secrets.VerifyTracked(repositoryPath)
		if err != nil {
			return exitDeployFailed, err
		}
		if len(problems) > 0 {
			return exitDeployFailed, errors.New(strings.Join(problems, "\n"))
		}
		fmt.Printf("%s holds only age ciphertext and public recipients\n", secrets.Directory)

		return exitOK, nil
	}

	return exitPreconditionNotMet, fmt.Errorf("unknown secrets action %q; use edit, seal, check or verify", action)
}

func reportSealed(environment, name string, changed bool) {
	if !changed {
		fmt.Printf("unchanged %s/%s\n", environment, name)
		return
	}
	fmt.Printf("sealed %s/%s; commit it, then push it with: deploy env push %s -e %s\n", environment, name, name, environment)
}

// editSecret is the one place plaintext lands in a file, because an editor needs
// one: a private directory on tmpfs where there is one, removed on the way out.
func editSecret(repositoryPath string, secrets Secrets, environment, name string) (int, error) {
	if !sealedName.MatchString(name) {
		return exitPreconditionNotMet, fmt.Errorf("%q is not a plain env file name", name)
	}
	sealed := secrets.SealedPath(repositoryPath, environment, name)
	var before []byte
	exists := false
	if _, err := os.Stat(sealed); err == nil {
		contents, err := secrets.Decrypt(repositoryPath, sealed)
		if err != nil {
			return exitDeployFailed, err
		}
		before, exists = contents, true
	}

	directory, err := os.MkdirTemp(privateScratch(), "deploy-secrets-")
	if err != nil {
		return exitDeployFailed, err
	}
	defer os.RemoveAll(directory)
	file := filepath.Join(directory, name)
	if err := os.WriteFile(file, before, 0o600); err != nil {
		return exitDeployFailed, err
	}

	editor := os.Getenv("VISUAL")
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}
	if editor == "" {
		editor = "vi"
	}
	// through sh, so an editor that takes arguments, such as code --wait, works
	command := exec.Command("sh", "-c", editor+` "$1"`, "editor", file)
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := command.Run(); err != nil {
		return exitDeployFailed, fmt.Errorf("the editor failed, so nothing was sealed: %w", err)
	}
	after, err := os.ReadFile(file)
	if err != nil {
		return exitDeployFailed, err
	}
	if exists && bytes.Equal(before, after) {
		reportSealed(environment, name, false)
		return exitOK, nil
	}
	changed, err := secrets.Seal(repositoryPath, sealed, after)
	if err != nil {
		return exitDeployFailed, err
	}
	reportSealed(environment, name, changed)

	return exitOK, nil
}

// privateScratch prefers memory-backed storage, so an edited secret is never
// written to a disk that outlives the edit.
func privateScratch() string {
	for _, candidate := range []string{os.Getenv("XDG_RUNTIME_DIR"), "/dev/shm"} {
		if candidate == "" {
			continue
		}
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			probe, err := os.MkdirTemp(candidate, ".deploy-probe-")
			if err == nil {
				_ = os.Remove(probe)
				return candidate
			}
		}
	}

	return os.TempDir()
}
