package main

import (
	"fmt"
	"io"
	"os"
	"path"
	"slices"
	"strings"
	"time"
)

// docker keeps a container's log only as long as the container, and a
// version 2 deploy removes a release's container once it has moved off it, so
// every deploy used to throw away the history anyone would need to work out
// what went wrong. right before a container goes, its whole log is written
// once, compressed, under logs/<commit>/<service>/, beside releases/ rather
// than in it because a release directory is the repository's own files. it is
// one write per replaced container, not a stream, so a drive sees no more
// churn than the deploy itself causes. kept logs go when pruning drops their
// release, so there are as many as there are releases kept.

// Logs holds the kept logs. pruning removes what is in here and nothing else.
func (layout Layout) Logs() string {
	return path.Join(layout.Root, "logs")
}

func (layout Layout) ServiceLogs(commit, name string) string {
	return path.Join(layout.Logs(), ShortCommit(commit), name)
}

// keptLogsScript runs on the destination, so the log never crosses the network.
// the exit status of docker logs is carried out of the pipe through a file,
// because sh has no pipefail; a failed read leaves no partial file behind.
const keptLogsScript = `set -eu
directory=$1 stamp=$2 project=$3 service=$4
ids=$(docker ps --all --quiet --filter "label=com.docker.compose.project=$project" --filter "label=com.docker.compose.service=$service")
[ -n "$ids" ] || exit 0
mkdir -p "$directory"
partial=
trap 'rm -f "$partial" "$partial.status"' EXIT
index=0
for id in $ids; do
  index=$((index + 1))
  target="$directory/$stamp-$(printf '%02d' "$index").log.gz"
  partial="$target.partial"
  { status=0; docker logs --timestamps "$id" 2>&1 || status=$?; echo "$status" > "$partial.status"; } | gzip -c > "$partial"
  status=$(cat "$partial.status" 2>/dev/null || echo 1)
  rm -f "$partial.status"
  if [ "$status" -ne 0 ]; then
    echo "docker logs $id exited $status" >&2
    exit "$status"
  fi
  mv "$partial" "$target"
done`

// keepLogs writes a container's log before deploy removes it. Losing a log is
// worth a warning, never a failed cutover: the deploy itself still has to finish.
func keepLogs(runner Runner, layout Layout, commit, project, name string) {
	directory := layout.ServiceLogs(commit, name)
	stamp := time.Now().UTC().Format("20060102T150405Z")
	if output, err := runner.Run([]string{"sh", "-c", keptLogsScript, "deploy-logs", directory, stamp, project, name}); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not keep the logs of %s from %s: %s\n", name, project, firstLine(output))
	}
}

// pruneKeptLogs removes the kept logs of every commit no service refers to any
// more, behind the same fences as the release directories.
func pruneKeptLogs(runner Runner, layout Layout, referenced map[string]bool) error {
	commits, err := runner.ListDirectory(layout.Logs())
	if err != nil {
		return err
	}
	for _, commit := range commits {
		if referenced[commit] || !commitPattern.MatchString(commit) {
			continue
		}
		directory, err := releasePathWithin(layout.Logs(), commit)
		if err != nil {
			return err
		}
		if err := resolvedWithinRoot(layout.Logs(), directory); err != nil {
			return err
		}
		if err := runner.RemoveAll(directory); err != nil {
			return fmt.Errorf("pruning the logs of %s: %w", commit, err)
		}
	}

	return nil
}

// pruneServiceLogs removes one service's kept logs from a release it no longer
// keeps, while other services may still keep that commit.
func pruneServiceLogs(runner Runner, layout Layout, commit, name string) error {
	directory, err := releasePathWithin(layout.Logs(), ShortCommit(commit))
	if err != nil {
		return err
	}
	target := path.Join(directory, name)
	if err := resolvedWithinRoot(layout.Logs(), target); err != nil {
		return err
	}
	if err := runner.RemoveAll(target); err != nil {
		return fmt.Errorf("pruning the logs of %s from %s: %w", name, commit, err)
	}

	return nil
}

// keptLogFiles lists one service's kept logs from one release, oldest first.
func keptLogFiles(runner Runner, layout Layout, commit, name string) ([]string, error) {
	entries, err := runner.ListDirectory(layout.ServiceLogs(commit, name))
	if err != nil {
		return nil, err
	}
	var files []string
	for _, entry := range entries {
		if strings.HasSuffix(entry, ".log.gz") {
			files = append(files, path.Join(layout.ServiceLogs(commit, name), entry))
		}
	}
	slices.Sort(files)

	return files, nil
}

// keptLogReleases is every release a service has kept logs from.
func keptLogReleases(runner Runner, layout Layout, name string) ([]string, error) {
	commits, err := runner.ListDirectory(layout.Logs())
	if err != nil {
		return nil, err
	}
	var found []string
	for _, commit := range commits {
		if !commitPattern.MatchString(commit) {
			continue
		}
		files, err := keptLogFiles(runner, layout, commit, name)
		if err != nil {
			return nil, err
		}
		if len(files) > 0 {
			found = append(found, commit)
		}
	}

	return found, nil
}

// printKeptLogs writes one service's kept logs from a release, decompressed
// on the destination. Each file is one container's whole life, so more than
// one gets a header saying which.
func printKeptLogs(runner Runner, layout Layout, commit, name string, output io.Writer) error {
	if !commitPattern.MatchString(commit) {
		return fmt.Errorf("%q is not a commit", commit)
	}
	files, err := keptLogFiles(runner, layout, commit, name)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		releases, err := keptLogReleases(runner, layout, name)
		if err != nil {
			return err
		}
		if len(releases) == 0 {
			return fmt.Errorf("no logs of %s are kept yet; they are kept when a deploy or rollback moves off a release", name)
		}
		return fmt.Errorf("no logs of %s are kept from %s; there are some from %s", name, ShortCommit(commit), strings.Join(releases, ", "))
	}
	for _, file := range files {
		if len(files) > 1 {
			fmt.Fprintf(output, "== %s\n", path.Base(file))
		}
		if err := runner.Stream([]string{"gzip", "-dc", file}, output); err != nil {
			return fmt.Errorf("reading %s: %w", file, err)
		}
	}

	return nil
}
