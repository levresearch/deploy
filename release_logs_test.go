package main

import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fakeDocker puts a docker on PATH that answers the two commands the kept-log
// script uses, so the script itself runs for real in sh with no daemon.
func fakeDocker(t *testing.T, ids string, failLogs bool) {
	t.Helper()
	bin := t.TempDir()
	script := `#!/bin/sh
case "$1" in
  ps) [ -z "$FAKE_IDS" ] || printf '%s\n' $FAKE_IDS ;;
  logs)
    if [ -n "$FAKE_FAIL" ]; then echo "no such container" >&2; exit 1; fi
    echo "2026-11-17T12:00:00Z out of $3"
    echo "2026-11-17T12:00:01Z err of $3" >&2 ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_IDS", ids)
	if failLogs {
		t.Setenv("FAKE_FAIL", "1")
	} else {
		t.Setenv("FAKE_FAIL", "")
	}
}

func gunzipFile(t *testing.T, file string) string {
	t.Helper()
	compressed, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return string(plain)
}

func keptFiles(t *testing.T, directory string) []string {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func TestKeepLogsWritesEachContainersWholeLogCompressedOnTheDestination(t *testing.T) {
	fakeDocker(t, "abc123 def456", false)
	layout := NewLayout(t.TempDir(), "a3f19c02")

	keepLogs(LocalRunner{}, layout, "1234567890abcdef", "deploy-a3f19c02_api_1234567", "api")

	directory := layout.ServiceLogs("1234567", "api")
	names := keptFiles(t, directory)
	if len(names) != 2 {
		t.Fatalf("one file per container was expected, got %v", names)
	}
	for _, name := range names {
		if !strings.HasSuffix(name, ".log.gz") {
			t.Fatalf("a partial or status file was left behind: %v", names)
		}
	}
	slices.Sort(names)
	first := gunzipFile(t, filepath.Join(directory, names[0]))
	if !strings.Contains(first, "out of abc123") || !strings.Contains(first, "err of abc123") {
		t.Fatalf("the first container's stdout and stderr were not both kept: %q", first)
	}
	if second := gunzipFile(t, filepath.Join(directory, names[1])); !strings.Contains(second, "out of def456") {
		t.Fatalf("the second container's log was not kept apart: %q", second)
	}
}

func TestKeepLogsLeavesNothingWhenThereIsNoContainerOrTheReadFails(t *testing.T) {
	layout := NewLayout(t.TempDir(), "a3f19c02")

	fakeDocker(t, "", false)
	keepLogs(LocalRunner{}, layout, "1234567", "deploy-a3f19c02_api_1234567", "api")
	if names := keptFiles(t, layout.Logs()); len(names) != 0 {
		t.Fatalf("a service with no container left %v", names)
	}

	fakeDocker(t, "abc123", true)
	keepLogs(LocalRunner{}, layout, "1234567", "deploy-a3f19c02_api_1234567", "api")
	if names := keptFiles(t, layout.ServiceLogs("1234567", "api")); len(names) != 0 {
		t.Fatalf("a failed read left %v", names)
	}
}

func writeKept(t *testing.T, layout Layout, commit, name, file, contents string) {
	t.Helper()
	directory := layout.ServiceLogs(commit, name)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write([]byte(contents)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, file), compressed.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestKeptLogsReadOldestFirstAndSayWhereThereAreSome(t *testing.T) {
	layout := NewLayout(t.TempDir(), "a3f19c02")
	writeKept(t, layout, "aaaaaaa", "api", "20261117T120000Z.log.gz", "later\n")
	writeKept(t, layout, "aaaaaaa", "api", "20261116T120000Z.log.gz", "earlier\n")
	writeKept(t, layout, "bbbbbbb", "api", "20261115T120000Z.log.gz", "other\n")

	var output bytes.Buffer
	if err := printKeptLogs(LocalRunner{}, layout, "aaaaaaa", "api", &output); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	if strings.Index(text, "earlier") > strings.Index(text, "later") || !strings.Contains(text, "== 20261116T120000Z.log.gz") {
		t.Fatalf("two kept logs should print oldest first under headers: %q", text)
	}

	err := printKeptLogs(LocalRunner{}, layout, "ccccccc", "api", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "aaaaaaa, bbbbbbb") {
		t.Fatalf("a release with none kept should name the ones that have some: %v", err)
	}
	if err := printKeptLogs(LocalRunner{}, layout, "aaaaaaa", "worker", io.Discard); err == nil || !strings.Contains(err.Error(), "no logs of worker are kept yet") {
		t.Fatalf("a service with none kept should say so: %v", err)
	}
	if err := printKeptLogs(LocalRunner{}, layout, "../etc", "api", io.Discard); err == nil {
		t.Fatal("a release that is not a commit was read")
	}
}

func TestPruningKeepsLogsForTheReleasesItKeepsAndNoOthers(t *testing.T) {
	layout := NewLayout(t.TempDir(), "a3f19c02")
	for _, commit := range []string{"aaaaaaa", "bbbbbbb", "ccccccc", "ddddddd"} {
		writeKept(t, layout, commit, "api", "20261117T120000Z.log.gz", commit)
	}
	writeKept(t, layout, "ccccccc", "worker", "20261117T120000Z.log.gz", "worker")
	// a commit no service refers to any more, such as a failed candidate's
	writeKept(t, layout, "eeeeeee", "api", "20261117T120000Z.log.gz", "failed")

	release := func(commit string) *ServiceRelease { return &ServiceRelease{Commit: commit} }
	state := ServiceState{Services: map[string]ServiceRecord{
		"api": {
			Current:  release("aaaaaaa"),
			Previous: release("bbbbbbb"),
			Releases: []string{"aaaaaaa", "bbbbbbb", "ccccccc", "ddddddd"},
		},
		"worker": {
			Current:  release("ccccccc"),
			Releases: []string{"ccccccc"},
		},
	}}
	if _, err := pruneServiceReleases(LocalRunner{}, layout, "a3f19c02", state, 1); err != nil {
		t.Fatal(err)
	}

	if got := keptFiles(t, layout.Logs()); !slices.Equal(got, []string{"aaaaaaa", "bbbbbbb", "ccccccc"}) {
		t.Fatalf("logs should stay for exactly the kept releases' commits: %v", got)
	}
	if got := keptFiles(t, filepath.Join(layout.Logs(), "ccccccc")); !slices.Equal(got, []string{"worker"}) {
		t.Fatalf("the api's pruned release should lose its logs while the worker keeps that commit: %v", got)
	}
}
