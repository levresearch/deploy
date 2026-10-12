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
	"regexp"
	"time"
)

// Operation marks a mutation whose outcome deploy has not observed yet. It is
// written before the first change and removed once the command sees how it
// ended. A crash, a signal or a lost connection leaves it behind, because a
// remote command can keep running after the session that started it is gone,
// and repeating a release task over an unknown result is how data gets hurt.
type Operation struct {
	Version   int       `json:"version"`
	ID        string    `json:"id"`
	Command   string    `json:"command"`
	Target    string    `json:"target,omitempty"`
	Host      string    `json:"host"`
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"startedAt"`
}

const operationVersion = 1

var operationIDPattern = regexp.MustCompile(`^[0-9a-f]{16}$`)

// removing the record is synced like writing it, or a power loss could bring
// back a finished operation and block the next deploy for no reason.
const durableOperationRemoval = `set -eu
target=$1
rm -f -- "$target"
sync -- "$(dirname -- "$target")"
`

// operationFile lives beside the lock rather than in the project, so destroy
// cannot remove the record of its own unfinished run.
func operationFile(layout Layout) string {
	return path.Join(lockDirectory(layout), path.Base(layout.Root)+".operation.json")
}

func readOperation(runner stateReader, file string) (Operation, bool, error) {
	raw, err := runner.ReadFile(file)
	if errors.Is(err, fs.ErrNotExist) {
		return Operation{}, false, nil
	}
	if err != nil {
		return Operation{}, false, fmt.Errorf("reading %s: %w", file, err)
	}

	var operation Operation
	if err := json.Unmarshal(raw, &operation); err != nil {
		return Operation{}, true, fmt.Errorf("%s is unreadable; inspect the deployment, then remove it by hand", file)
	}
	if operation.Version != operationVersion || !operationIDPattern.MatchString(operation.ID) {
		return Operation{}, true, fmt.Errorf("%s was written by an incompatible deploy version; use that version to resolve it", file)
	}

	return operation, true, nil
}

func unresolvedOperation(runner stateReader, operation Operation) error {
	described := operation.Command
	if operation.Target != "" {
		described += " " + operation.Target
	}

	return fmt.Errorf(
		"%s on %s did not finish (started %s from %s, pid %d), so what it changed is unknown. "+
			"check that it is no longer running and inspect the deployment, then run deploy resolve %s",
		described, runner.Describe(), operation.StartedAt.Format(time.RFC3339), operation.Host, operation.PID, operation.ID,
	)
}

func newOperation(command, target string) (Operation, error) {
	id := make([]byte, 8)
	if _, err := rand.Read(id); err != nil {
		return Operation{}, err
	}
	host, _ := os.Hostname()

	return Operation{
		Version:   operationVersion,
		ID:        hex.EncodeToString(id),
		Command:   command,
		Target:    target,
		Host:      host,
		PID:       os.Getpid(),
		StartedAt: time.Now().UTC().Truncate(time.Second),
	}, nil
}

func writeOperation(runner Runner, file string, operation Operation) error {
	encoded, err := json.MarshalIndent(operation, "", "  ")
	if err != nil {
		return err
	}
	if err := writeStateFile(runner, file, append(encoded, '\n')); err != nil {
		return fmt.Errorf("recording the operation before changing anything: %w", err)
	}

	return nil
}

func removeOperation(runner Runner, file string) error {
	_, err := runner.Run([]string{"sh", "-c", durableOperationRemoval, "deploy-operation", file})
	return err
}

// beginOperation runs under the lock, so no other deploy can be between the
// check and the write.
func (lock *Lock) beginOperation(command, target string) error {
	existing, found, err := readOperation(lock.lockOwner, lock.operationPath)
	if err != nil {
		return err
	}
	if found {
		return unresolvedOperation(lock.lockOwner, existing)
	}

	operation, err := newOperation(command, target)
	if err != nil {
		return err
	}
	if err := writeOperation(lock.lockOwner, lock.operationPath, operation); err != nil {
		return err
	}
	lock.operation = &operation

	return nil
}

// Finish ends the operation and releases the lock. It is deferred directly so
// that it can see a panic. The record is cleared only when the command observed
// how it ended; a panic or a release that is live but unrecorded keeps it.
func (lock *Lock) Finish(exitCode *int) {
	failure := recover()
	if lock.operation != nil {
		if failure == nil && *exitCode != exitLiveButNeedsAHuman {
			lock.endOperation()
		} else {
			fmt.Fprintf(os.Stderr, "the operation stays recorded until someone inspects the deployment and runs deploy resolve %s\n", lock.operation.ID)
		}
	}
	lock.Release()
	if failure != nil {
		panic(failure)
	}
}

func (lock *Lock) endOperation() {
	select {
	case <-lock.exited:
		fmt.Fprintf(os.Stderr, "warning: the lock was lost, so operation %s stays recorded until deploy resolve %s\n", lock.operation.ID, lock.operation.ID)
		return
	default:
	}
	if err := removeOperation(lock.lockOwner, lock.operationPath); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not clear operation %s, so the next change will ask for deploy resolve %s: %v\n", lock.operation.ID, lock.operation.ID, err)
		return
	}
	lock.operation = nil
}

// ResolveOperation clears a record left by an operation that did not finish.
// It takes the id rather than a yes or no, so whoever runs it has to have read
// the message that named it, and a newer record is never cleared by accident.
func ResolveOperation(runner Runner, layout Layout, id string) error {
	if !operationIDPattern.MatchString(id) {
		return fmt.Errorf("%q is not an operation id; deploy prints the id when it refuses to continue", id)
	}

	lock, err := holdLock(runner, layout, false)
	if err != nil {
		return err
	}
	defer lock.Release()

	existing, found, err := readOperation(runner, lock.operationPath)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("nothing to resolve on %s", runner.Describe())
	}
	if existing.ID != id {
		return fmt.Errorf("the unfinished operation on %s is %s, not %s", runner.Describe(), existing.ID, id)
	}

	return removeOperation(runner, lock.operationPath)
}

func RunResolve(options DeployOptions, id string) (int, error) {
	startPath := options.Context
	if startPath == "" {
		working, err := os.Getwd()
		if err != nil {
			return exitPreconditionNotMet, err
		}
		startPath = working
	}

	repositoryPath, err := FindRepository(startPath)
	if err != nil {
		return exitPreconditionNotMet, err
	}

	resolved, err := loadResolvedConfig(repositoryPath, options.Environment)
	if err != nil {
		return exitPreconditionNotMet, err
	}

	destinationText := options.Destination
	if destinationText == "" {
		destinationText = resolved.Destination
	}
	destination, err := ParseDestination(destinationText)
	if err != nil {
		return exitPreconditionNotMet, err
	}

	runner, closeRunner, err := OpenRunner(destination)
	if err != nil {
		return exitPreconditionNotMet, err
	}
	defer closeRunner()

	destination, err = AbsoluteDestination(runner, destination)
	if err != nil {
		return exitPreconditionNotMet, err
	}

	if err := ResolveOperation(runner, NewLayout(destination.Path, resolved.RuntimeID()), id); err != nil {
		return exitPreconditionNotMet, err
	}
	fmt.Printf("resolved operation %s on %s\n", id, destination)

	return exitOK, nil
}
