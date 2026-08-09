package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// GenerateProjectID is 8 hex characters, which is 32 bits. Note that is 8
// characters and not 8 bits, since 8 bits is 256 values and would collide after
// about twenty projects.
func GenerateProjectID() (string, error) {
	raw := make([]byte, 4)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generating a project id: %w", err)
	}

	return hex.EncodeToString(raw), nil
}

// EnsureProjectConfig writes a config for a project that has none, so deploy is
// one command rather than two. It reports whether it created one.
//
// It cannot invent services, so the deploy that follows will stop at validation.
// What it can fill in, it does, which is the id and the name, and those are
// exactly the parts nobody should have to make up.
func EnsureProjectConfig(repositoryPath, destination string) (bool, error) {
	configPath := path.Join(repositoryPath, configFileName)

	if _, err := os.Stat(configPath); err == nil {
		return false, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}

	projectID, err := GenerateProjectID()
	if err != nil {
		return false, err
	}

	project := Project{
		Version:     supportedConfigVersion,
		ID:          projectID,
		Name:        filepath.Base(repositoryPath),
		Destination: destination,
		Services:    map[string]Service{},
	}

	encoded, err := json.MarshalIndent(project, "", "  ")
	if err != nil {
		return false, err
	}
	if err := os.WriteFile(configPath, append(encoded, '\n'), 0o644); err != nil {
		return false, fmt.Errorf("writing %s: %w", configPath, err)
	}

	if err := ignoreDeployDirectory(repositoryPath); err != nil {
		return true, err
	}

	return true, nil
}

// ignoreDeployDirectory keeps machine-local state out of the repository. Adding
// one line beats leaving someone to discover a build cache in their next commit.
func ignoreDeployDirectory(repositoryPath string) error {
	const entry = ".deploy/"

	ignorePath := path.Join(repositoryPath, ".gitignore")

	existing, err := os.ReadFile(ignorePath)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	for _, line := range strings.Split(string(existing), "\n") {
		if strings.TrimSpace(line) == entry {
			return nil
		}
	}

	updated := string(existing)
	if updated != "" && !strings.HasSuffix(updated, "\n") {
		updated += "\n"
	}
	updated += entry + "\n"

	return os.WriteFile(ignorePath, []byte(updated), 0o644)
}

// describeFreshConfig is what somebody sees the first time they run deploy in a
// project. It names the file, what is in it, and the one thing still missing.
func describeFreshConfig(projectID string) {
	fmt.Printf("wrote %s with id %s\n", configFileName, projectID)
	fmt.Printf("  and added .deploy/ to .gitignore, since that is machine-local\n\n")
	fmt.Printf("  it has no services yet, and deploy cannot guess them. a service either\n")
	fmt.Printf("  names an image to run or a build to make one:\n\n")
	fmt.Printf("    \"services\": {\n")
	fmt.Printf("      \"web\": {\n")
	fmt.Printf("        \"build\": { \"dockerfile\": \"Dockerfile\" },\n")
	fmt.Printf("        \"stateful\": false,\n")
	fmt.Printf("        \"healthcheck\": { \"command\": [\"CMD\", \"true\"] }\n")
	fmt.Printf("      }\n")
	fmt.Printf("    }\n\n")
	fmt.Printf("  fill that in, commit it, and run deploy again.\n\n")
}
