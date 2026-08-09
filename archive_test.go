package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestSameHostDetection(t *testing.T) {
	cases := []struct {
		name        string
		gitStorage  string
		destination string
		want        bool
	}{
		{
			name:        "one host for both, which is the whole point",
			gitStorage:  "git:Repositories/lectern.git",
			destination: "git:Projects",
			want:        true,
		},
		{
			name:        "two different servers",
			gitStorage:  "git:Repositories/lectern.git",
			destination: "vps:Projects",
			want:        false,
		},
		{
			name:        "repo here, deploying there",
			gitStorage:  "/srv/git/lectern.git",
			destination: "git:Projects",
			want:        false,
		},
		{
			name:        "repo there, deploying here",
			gitStorage:  "git:Repositories/lectern.git",
			destination: "/srv/projects",
			want:        false,
		},
		{
			name:        "both local, which is still one machine",
			gitStorage:  "/srv/git/lectern.git",
			destination: "/srv/projects",
			want:        true,
		},
		{
			name:        "the same host spelled with a user is not the same string",
			gitStorage:  "ethan@pi:Repositories/x.git",
			destination: "pi:Projects",
			want:        false,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			gitStorage, err := ParseDestination(testCase.gitStorage)
			if err != nil {
				t.Fatalf("parsing gitStorage: %v", err)
			}
			destination, err := ParseDestination(testCase.destination)
			if err != nil {
				t.Fatalf("parsing destination: %v", err)
			}

			if got := SameHost(gitStorage, destination); got != testCase.want {
				t.Errorf("SameHost(%q, %q) = %v, want %v",
					testCase.gitStorage, testCase.destination, got, testCase.want)
			}
		})
	}
}

// newBareRepository stands in for the git server's Repositories directory, which
// is what the destination extracts from when it holds the repo itself.
func newBareRepository(t *testing.T, source string) string {
	t.Helper()

	bare := filepath.Join(t.TempDir(), "project.git")
	if _, err := runGit(t.TempDir(), "clone", "--bare", source, bare); err != nil {
		t.Fatalf("cloning bare: %v", err)
	}

	return bare
}

func TestBothPlacementPathsProduceTheSameTree(t *testing.T) {
	repository := newRepository(t)
	commitFile(t, repository, "one.txt", "first file")
	commitFile(t, repository, "nested/two.txt", "second file")
	commit := commitFile(t, repository, "three.txt", "third file")

	bare := newBareRepository(t, repository)

	// an untracked file, which must reach neither tree
	writeFile(t, repository, "untracked.txt", "nope")

	fromLocal := filepath.Join(t.TempDir(), "local")
	if err := PlaceRelease(
		LocalRunner{}, repository, Destination{}, Destination{}, commit, fromLocal,
	); err != nil {
		t.Fatalf("placing from the local repository: %v", err)
	}

	// both paths local means SameHost is true, so this takes the server side route
	fromBare := filepath.Join(t.TempDir(), "bare")
	if err := PlaceRelease(
		LocalRunner{}, repository,
		Destination{Path: bare}, Destination{Path: filepath.Dir(fromBare)},
		commit, fromBare,
	); err != nil {
		t.Fatalf("placing from the bare repository: %v", err)
	}

	localTree, bareTree := treeOf(t, fromLocal), treeOf(t, fromBare)

	if !slices.Equal(localTree, bareTree) {
		t.Fatalf("the two placement paths disagree.\n  local: %v\n  bare:  %v", localTree, bareTree)
	}
	if len(localTree) == 0 {
		t.Fatal("both trees are empty, so this proved nothing")
	}
	if slices.Contains(localTree, "untracked.txt") {
		t.Error("an untracked file reached the release")
	}

	// and the contents match, not just the names
	for _, name := range localTree {
		fromOne, err := os.ReadFile(filepath.Join(fromLocal, name))
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		fromTwo, err := os.ReadFile(filepath.Join(fromBare, name))
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		if string(fromOne) != string(fromTwo) {
			t.Errorf("%s differs between the two paths", name)
		}
	}
}

func treeOf(t *testing.T, root string) []string {
	t.Helper()

	var found []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		found = append(found, relative)

		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	slices.Sort(found)

	return found
}

func TestACommitMissingFromTheBareRepositorySaysPushFirst(t *testing.T) {
	repository := newRepository(t)
	commitFile(t, repository, "one.txt", "first")

	bare := newBareRepository(t, repository)

	// a commit made after the clone, so it exists locally and not on the server
	unpushed := commitFile(t, repository, "two.txt", "second")

	release := filepath.Join(t.TempDir(), "release")
	err := PlaceRelease(
		LocalRunner{}, repository,
		Destination{Path: bare}, Destination{Path: filepath.Dir(release)},
		unpushed, release,
	)
	if err == nil {
		t.Fatal("deploying a commit the server has never seen must fail")
	}
	if !strings.Contains(err.Error(), "push first") {
		t.Errorf("the error should say what to do about it, got: %v", err)
	}
	if !strings.Contains(err.Error(), ShortCommit(unpushed)) {
		t.Errorf("the error should name the commit, got: %v", err)
	}

	// nothing was placed, since the check happens before any extraction
	if entries, _ := os.ReadDir(release); len(entries) > 0 {
		t.Errorf("a refused placement should leave nothing behind, found %d entries", len(entries))
	}
}

func TestAPushedCommitPlacesFromTheBareRepository(t *testing.T) {
	repository := newRepository(t)
	commitFile(t, repository, "one.txt", "first")
	bare := newBareRepository(t, repository)

	commitFile(t, repository, "two.txt", "second")

	// push, the way anyone would before deploying
	if _, err := runGit(repository, "push", bare, "main"); err != nil {
		t.Fatalf("pushing: %v", err)
	}
	pushed, err := ResolveCommit(repository, "HEAD")
	if err != nil {
		t.Fatalf("resolving HEAD: %v", err)
	}

	release := filepath.Join(t.TempDir(), "release")
	if err := PlaceRelease(
		LocalRunner{}, repository,
		Destination{Path: bare}, Destination{Path: filepath.Dir(release)},
		pushed, release,
	); err != nil {
		t.Fatalf("placing a pushed commit: %v", err)
	}

	if got := treeOf(t, release); !slices.Equal(got, []string{"one.txt", "two.txt"}) {
		t.Errorf("release tree = %v, want both files", got)
	}
}

// Extracting an older commit from the bare repo has to give that commit rather
// than whatever the branch tip is, which is exactly what rollback depends on and
// what placing HEAD alone can never prove.
func TestTheBareRepositoryPlacesTheNamedCommitRatherThanTheTip(t *testing.T) {
	repository := newRepository(t)
	older := commitFile(t, repository, "version.txt", "first")
	commitFile(t, repository, "version.txt", "second")
	commitFile(t, repository, "version.txt", "third")

	bare := newBareRepository(t, repository)

	release := filepath.Join(t.TempDir(), "release")
	if err := PlaceRelease(
		LocalRunner{}, repository,
		Destination{Path: bare}, Destination{Path: filepath.Dir(release)},
		older, release,
	); err != nil {
		t.Fatalf("placing an older commit: %v", err)
	}

	placed, err := os.ReadFile(filepath.Join(release, "version.txt"))
	if err != nil {
		t.Fatalf("reading the placed file: %v", err)
	}
	if string(placed) != "first" {
		t.Errorf("placed %q, want the older commit's contents rather than the tip's", placed)
	}
}

// gitStorage on another host cannot be used to extract, so it has to fall back to
// streaming from here rather than reaching for a repo that is not there.
func TestGitStorageOnAnotherHostFallsBackToTheLocalStream(t *testing.T) {
	repository := newRepository(t)
	commit := commitFile(t, repository, "one.txt", "first")

	release := filepath.Join(t.TempDir(), "release")
	err := PlaceRelease(
		LocalRunner{}, repository,
		Destination{Host: "elsewhere", Path: "Repositories/x.git"},
		Destination{Path: filepath.Dir(release)},
		commit, release,
	)
	if err != nil {
		t.Fatalf("expected the local stream fallback, got: %v", err)
	}

	if got := treeOf(t, release); !slices.Equal(got, []string{"one.txt"}) {
		t.Errorf("release tree = %v", got)
	}
}
