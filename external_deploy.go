package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// errExternalUnverified marks an external release that went out but never
// answered as verify expects, with nothing to roll it back to. it is live, so
// the deploy exits as needing a human rather than as failed.
var errExternalUnverified = errors.New("an external release is live but failed verification")

const verifyInterval = 2 * time.Second

// RenderExternalRelease is one external service as a stack nothing brings up:
// it is run once with compose run, from its own file so a rollback can run
// the same release again.
func RenderExternalRelease(resolved ResolvedProject, layout Layout, name, commit string) ([]byte, error) {
	service, found := resolved.External[name]
	if !found {
		return nil, fmt.Errorf("%s is not an external service", name)
	}
	if service.Run != nil {
		command, err := json.Marshal(service.Run)
		if err != nil {
			return nil, err
		}
		service.Extra = maps.Clone(service.Extra)
		if service.Extra == nil {
			service.Extra = map[string]json.RawMessage{}
		}
		service.Extra["command"] = command
	}

	return renderProject(resolved, layout, ServiceReleaseProject(resolved.RuntimeID(), name, commit), map[string]Service{name: service}, commit)
}

// runExternal runs one external service's release and records it. the run is
// written down as indeterminate before it starts, like a release task, so a
// crash or a timeout is never mistaken for a run that did not happen.
func (executor *serviceExecutor) runExternal(state *ServiceState, name string) error {
	service := executor.resolved.External[name]
	digest := nodeDigest(service)
	record := state.Services[name]
	if executor.unchanged[name] && record.Current != nil && record.Current.ConfigDigest == digest {
		fmt.Printf("  %s is unchanged, so it stays on %s\n", name, record.Current.Commit)
		return nil
	}

	release := serviceRelease(executor.resolved.RuntimeID(), name, executor.commit, digest)
	rendered, err := RenderExternalRelease(executor.resolved, executor.layout, name, executor.commit)
	if err != nil {
		return err
	}
	if err := executor.runner.WriteFile(executor.composeFile(release), rendered); err != nil {
		return err
	}

	if state.Tasks == nil {
		state.Tasks = map[string][]TaskRecord{}
	}
	state.Tasks[name] = append(state.Tasks[name], TaskRecord{Operation: executor.operation, Commit: release.Commit, ConfigDigest: digest, Outcome: outcomeIndeterminate, RecordedAt: time.Now().UTC().Truncate(time.Second)})
	if *state, err = WriteServiceState(executor.runner, executor.layout, *state); err != nil {
		return err
	}

	fmt.Printf("  running external %s\n", name)
	lines, outcome, runErr := executor.runExternalCommand(name, release, nil, service)
	runs := state.Tasks[name]
	runs[len(runs)-1].Outcome = outcome
	if *state, err = WriteServiceState(executor.runner, executor.layout, *state); err != nil {
		return err
	}
	switch outcome {
	case outcomeIndeterminate:
		return fmt.Errorf("external %s did not finish (%v), so whether it released is unknown; check it, then run deploy resolve %s", name, runErr, executor.operation)
	case outcomeFailed:
		return fmt.Errorf("external %s failed, so it stays on its previous release: %w", name, runErr)
	}

	reference := ""
	if service.Reference != "" {
		pattern, err := referencePattern(service.Reference)
		if err != nil {
			return err
		}
		if reference = findReference(lines, pattern); reference == "" {
			fmt.Fprintf(os.Stderr, "warning: %s printed nothing matching its reference pattern, so none is recorded\n", name)
		}
	}

	if service.Verify != nil {
		if err := verifyExternal(*service.Verify); err != nil {
			return executor.unverified(state, name, release, reference, err)
		}
	}
	executor.recordExternal(state, name, release, reference, outcomeLive)
	if *state, err = WriteServiceState(executor.runner, executor.layout, *state); err != nil {
		return err
	}
	executor.released = append(executor.released, name)
	if reference != "" {
		fmt.Printf("  %s released %s at %s\n", name, release.Commit, reference)
	} else {
		fmt.Printf("  %s released %s\n", name, release.Commit)
	}

	return nil
}

// unverified handles a release that went out but did not answer as expected:
// it goes back to the previous release when there is one, and is recorded as
// live and unverified when there is not.
func (executor *serviceExecutor) unverified(state *ServiceState, name string, release ServiceRelease, reference string, failure error) error {
	record := state.Services[name]
	if record.Current != nil && record.Current.Commit != release.Commit {
		target := *record.Current
		fmt.Printf("  %s did not verify (%v), so it goes back to %s\n", name, failure, target.Commit)
		err := executor.rollExternal(name, target, record.References[target.Commit])
		if err == nil {
			return fmt.Errorf("external %s %s failed verification and was rolled back to %s: %w", name, release.Commit, target.Commit, failure)
		}
		failure = fmt.Errorf("%w, and rolling back to %s failed too: %v", failure, target.Commit, err)
	}
	executor.recordExternal(state, name, release, reference, outcomeUnverified)
	executor.released = append(executor.released, name)
	if _, err := WriteServiceState(executor.runner, executor.layout, *state); err != nil {
		return fmt.Errorf("%w: external %s %s is live but failed verification (%v), and recording it failed: %v", errExternalUnverified, name, release.Commit, failure, err)
	}

	return fmt.Errorf("%w: %s %s is live but %v", errExternalUnverified, name, release.Commit, failure)
}

// recordExternal makes release the current one, keeping the reference of
// every retained release so a rollback can name it.
func (executor *serviceExecutor) recordExternal(state *ServiceState, name string, release ServiceRelease, reference, outcome string) {
	record := state.Services[name]
	record.External = true
	if record.Current != nil && record.Current.Commit != release.Commit {
		record.Previous = record.Current
	}
	record.Current = &release
	record.Outcome = outcome
	record.Releases = append([]string{release.Commit}, slices.DeleteFunc(slices.Clone(record.Releases), func(existing string) bool { return existing == release.Commit })...)
	record.References = maps.Clone(record.References)
	if reference != "" {
		if record.References == nil {
			record.References = map[string]string{}
		}
		record.References[release.Commit] = reference
	} else {
		delete(record.References, release.Commit)
	}
	if len(record.References) == 0 {
		record.References = nil
	}
	state.Services[name] = record
}

// rollExternal puts an external service back on target: the configured
// rollback command with the target's reference, or else the target release's
// own command run again. it is verified like a release.
func (executor *serviceExecutor) rollExternal(name string, target ServiceRelease, reference string) error {
	service, configured := executor.resolved.External[name]
	var argv []string
	if configured && service.Rollback != nil {
		for _, argument := range service.Rollback {
			if strings.Contains(argument, referencePlaceholder) {
				if reference == "" {
					return fmt.Errorf("rollback needs the reference of %s, and none was recorded for it", target.Commit)
				}
				argument = strings.ReplaceAll(argument, referencePlaceholder, reference)
			}
			argv = append(argv, argument)
		}
	}
	if _, err := executor.runner.ReadFile(executor.composeFile(target)); err != nil {
		return fmt.Errorf("%s %s is no longer retained here", name, target.Commit)
	}
	fmt.Printf("  rolling external %s back to %s\n", name, target.Commit)
	_, outcome, err := executor.runExternalCommand(name, target, argv, service)
	switch outcome {
	case outcomeIndeterminate:
		return fmt.Errorf("the rollback did not finish (%v), so whether %s moved is unknown", err, name)
	case outcomeFailed:
		return fmt.Errorf("the rollback failed: %w", err)
	}
	if configured && service.Verify != nil {
		if err := verifyExternal(*service.Verify); err != nil {
			return fmt.Errorf("%s %s did not verify after the rollback: %w", name, target.Commit, err)
		}
	}

	return nil
}

// runExternalCommand runs a release's container to completion under the
// service's timeout, streaming its output and keeping the tail for the
// reference. argv replaces the release's command when it is set.
func (executor *serviceExecutor) runExternalCommand(name string, release ServiceRelease, argv []string, service Service) ([]string, string, error) {
	timeout, err := externalTimeout(service)
	if err != nil {
		timeout = defaultExternalTimeout
	}
	command := []string{
		"timeout", "--kill-after=30s", strconv.Itoa(int(timeout.Seconds())) + "s",
		"docker", "compose",
		"--file", executor.composeFile(release),
		"--project-name", release.Project,
		"run", "--rm", "--no-deps", "-T", name,
	}
	command = append(command, argv...)
	tail := &outputTail{}
	err = executor.runner.Stream(command, io.MultiWriter(indented{os.Stdout}, tail))
	tail.flush()

	return tail.lines, runOutcome(err), err
}

// runOutcome reads a finished run. a timeout, a kill or a lost ssh connection
// says nothing about whether the work happened, so it stays indeterminate.
func runOutcome(err error) string {
	if err == nil {
		return outcomeCompleted
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		switch exit.ExitCode() {
		case 124, 137, 255:
			return outcomeIndeterminate
		}
	}

	return outcomeFailed
}

// verifyExternal polls the release from here until it answers with an
// expected status. redirects are not followed, so a 302 is a 302.
func verifyExternal(verify Verify) error {
	timeout, err := verifyTimeout(verify)
	if err != nil {
		return err
	}
	expected := verify.Status
	if len(expected) == 0 {
		expected = defaultVerifyStatus
	}
	client := &http.Client{
		Timeout:       10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	deadline := time.Now().Add(timeout)
	last := "no answer"
	for attempt := 0; attempt == 0 || time.Now().Before(deadline); attempt++ {
		if attempt > 0 {
			time.Sleep(verifyInterval)
		}
		response, err := client.Get(verify.URL)
		if err != nil {
			last = err.Error()
			continue
		}
		response.Body.Close()
		if slices.Contains(expected, response.StatusCode) {
			fmt.Printf("  %s answered %d\n", verify.URL, response.StatusCode)
			return nil
		}
		last = fmt.Sprintf("status %d", response.StatusCode)
	}

	return fmt.Errorf("%s did not answer %v within %s (%s)", verify.URL, expected, timeout, last)
}

// outputTail keeps the last lines a run printed, without terminal escapes or
// the progress a carriage return overwrote.
type outputTail struct {
	partial []byte
	lines   []string
}

const outputTailLines = 500

var terminalEscape = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

func (tail *outputTail) Write(text []byte) (int, error) {
	tail.partial = append(tail.partial, text...)
	for {
		end := slices.Index(tail.partial, '\n')
		if end < 0 {
			break
		}
		tail.add(string(tail.partial[:end]))
		tail.partial = tail.partial[end+1:]
	}
	if len(tail.partial) > 64*1024 {
		tail.flush()
	}

	return len(text), nil
}

func (tail *outputTail) flush() {
	if len(tail.partial) > 0 {
		tail.add(string(tail.partial))
		tail.partial = nil
	}
}

func (tail *outputTail) add(line string) {
	if index := strings.LastIndex(strings.TrimRight(line, "\r"), "\r"); index >= 0 {
		line = line[index+1:]
	}
	line = strings.TrimSpace(terminalEscape.ReplaceAllString(line, ""))
	tail.lines = append(tail.lines, line)
	if len(tail.lines) > outputTailLines {
		tail.lines = tail.lines[len(tail.lines)-outputTailLines:]
	}
}

// findReference takes the last line the pattern matches, its group when it
// has one, and only a value that is safe to record and pass back.
func findReference(lines []string, pattern *regexp.Regexp) string {
	for index := len(lines) - 1; index >= 0; index-- {
		match := pattern.FindStringSubmatch(lines[index])
		if match == nil {
			continue
		}
		value := match[0]
		if len(match) > 1 {
			value = match[1]
		}
		if validReference(value) {
			return value
		}
	}

	return ""
}
