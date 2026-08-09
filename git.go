package main

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
)

func runGit(repositoryPath string, arguments ...string) (string, error) {
	command := append([]string{"-C", repositoryPath}, arguments...)
	process := exec.Command("git", command...)

	var out, failure bytes.Buffer
	process.Stdout = &out
	process.Stderr = &failure

	if err := process.Run(); err != nil {
		return "", fmt.Errorf("git %s: %s", strings.Join(arguments, " "), strings.TrimSpace(failure.String()))
	}

	return strings.TrimSpace(out.String()), nil
}

// FindRepository walks up from startPath the way git itself does, so running
// deploy anywhere inside a checkout works.
func FindRepository(startPath string) (string, error) {
	root, err := runGit(startPath, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("%s is not inside a git repository, and there is nothing to deploy without one", startPath)
	}

	return root, nil
}

func ResolveCommit(repositoryPath, revision string) (string, error) {
	commit, err := runGit(repositoryPath, "rev-parse", "--verify", revision+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("no commit named %q in %s", revision, repositoryPath)
	}

	return commit, nil
}

func IsWorkingTreeDirty(repositoryPath string) (bool, error) {
	status, err := runGit(repositoryPath, "status", "--porcelain")
	if err != nil {
		return false, err
	}

	return status != "", nil
}

// CommitExists answers whether a commit is reachable in a repository, which is
// how a deploy tells "push first" from a genuine failure.
func CommitExists(repositoryPath, commit string) bool {
	_, err := runGit(repositoryPath, "cat-file", "-e", commit+"^{commit}")

	return err == nil
}

func ShortCommit(commit string) string {
	if len(commit) > 7 {
		return commit[:7]
	}

	return commit
}

// PlaceRelease streams the tracked tree at commit straight into the destination,
// so it never lands on disk in between. That tar is exactly what the commit
// contains, which is what stops the release and the image disagreeing later.
func PlaceRelease(runner Runner, repositoryPath, commit, releaseDirectory string) error {
	if err := runner.MkdirAll(releaseDirectory); err != nil {
		return fmt.Errorf("creating %s: %w", releaseDirectory, err)
	}

	reader, archiveFailed := startArchive(repositoryPath, commit)
	defer reader.Close()

	extractErr := runner.Pipe([]string{"tar", "-x", "-C", releaseDirectory}, reader)
	if archiveErr := <-archiveFailed; archiveErr != nil {
		return archiveErr
	}

	return extractErr
}
