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

// SameHost decides whether the destination can extract from the bare repo
// itself. Two local paths count, since they are both this machine.
func SameHost(gitStorage, destination Destination) bool {
	return gitStorage.Host == destination.Host
}

// PlaceRelease puts the tracked tree at commit into the release directory. When
// the bare repo already lives on the destination nothing is uploaded at all,
// which for a repo and a projects directory on one disk is every deploy.
func PlaceRelease(
	runner Runner,
	repositoryPath string,
	gitStorage Destination,
	destination Destination,
	commit, releaseDirectory string,
) error {
	if err := runner.MkdirAll(releaseDirectory); err != nil {
		return fmt.Errorf("creating %s: %w", releaseDirectory, err)
	}

	if gitStorage.Path != "" && SameHost(gitStorage, destination) {
		return placeFromBareRepository(runner, gitStorage.Path, commit, releaseDirectory)
	}

	return placeFromLocalRepository(runner, repositoryPath, commit, releaseDirectory)
}

// placeFromBareRepository never sends the source anywhere. The repo is already
// sitting on the box that runs it, so the box extracts from itself and the only
// thing that crosses the network is the instruction to do so.
func placeFromBareRepository(runner Runner, bareRepositoryPath, commit, releaseDirectory string) error {
	if !bareRepositoryHasCommit(runner, bareRepositoryPath, commit) {
		return fmt.Errorf(
			"%s is not in %s on %s, so push first and deploy again",
			ShortCommit(commit), bareRepositoryPath, runner.Describe(),
		)
	}

	extract := fmt.Sprintf(
		"git --git-dir=%s archive --format=tar %s | tar -x -C %s",
		ShellQuote(bareRepositoryPath), ShellQuote(commit), ShellQuote(releaseDirectory),
	)
	if output, err := runner.Run([]string{"sh", "-c", extract}); err != nil {
		return fmt.Errorf(
			"extracting %s from %s on %s: %s",
			ShortCommit(commit), bareRepositoryPath, runner.Describe(), firstLine(output),
		)
	}

	return nil
}

func bareRepositoryHasCommit(runner Runner, bareRepositoryPath, commit string) bool {
	_, err := runner.Run([]string{
		"git", "--git-dir=" + bareRepositoryPath, "cat-file", "-e", commit + "^{commit}",
	})

	return err == nil
}

// placeFromLocalRepository streams the tree straight across, so it never lands on
// disk in between. This is the fallback for a repo that does not live on the
// destination.
func placeFromLocalRepository(runner Runner, repositoryPath, commit, releaseDirectory string) error {
	reader, archiveFailed := startArchive(repositoryPath, commit)
	defer reader.Close()

	extractErr := runner.Pipe([]string{"tar", "-x", "-C", releaseDirectory}, reader)
	if archiveErr := <-archiveFailed; archiveErr != nil {
		return archiveErr
	}

	return extractErr
}
