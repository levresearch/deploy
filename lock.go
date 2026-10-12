package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"sync"
	"syscall"
	"time"
)

type Lock struct {
	Version       int       `json:"version,omitempty"`
	PID           int       `json:"pid"`
	Host          string    `json:"host"`
	Commit        string    `json:"commit"`
	TakenAt       time.Time `json:"takenAt"`
	lockPath      string
	operationPath string
	operation     *Operation
	lockOwner     Runner
	process       *exec.Cmd
	input         io.WriteCloser
	release       *sync.Once
	done          chan struct{}
	exited        chan struct{}
}

func (runner LocalRunner) lockCommand(script string) *exec.Cmd {
	return exec.Command("sh", "-c", script)
}

func (runner *SSHRunner) lockCommand(script string) *exec.Cmd {
	return runner.ssh(ShellCommand([]string{"sh", "-c", script}))
}

// AcquireLock holds the destination lock and records the operation before
// anything changes. An earlier operation that never finished blocks it.
func AcquireLock(runner Runner, layout Layout, command, target string, force bool) (*Lock, error) {
	lock, err := holdLock(runner, layout, force)
	if err != nil {
		return nil, err
	}
	if err := refuseServiceState(runner, layout); err != nil {
		lock.Release()
		return nil, err
	}
	if err := lock.beginOperation(command, target); err != nil {
		lock.Release()
		return nil, err
	}
	if err := runner.MkdirAll(layout.Root); err != nil {
		lock.endOperation()
		lock.Release()
		return nil, err
	}
	lock.PID = os.Getpid()
	lock.Host, _ = os.Hostname()
	lock.Commit = ShortCommit(target)
	lock.TakenAt = time.Now().UTC().Truncate(time.Second)
	encoded, err := json.Marshal(lock)
	if err == nil {
		err = runner.WriteFile(lock.lockPath, append(encoded, '\n'))
	}
	if err != nil {
		lock.endOperation()
		lock.Release()
		return nil, fmt.Errorf("recording lock metadata: %w", err)
	}
	return lock, nil
}

func lockDirectory(layout Layout) string {
	return path.Join(path.Dir(layout.Root), ".deploy-locks")
}

// the persistent inode is outside the project, so destroy cannot replace it.
func holdLock(runner Runner, layout Layout, force bool) (*Lock, error) {
	commandRunner, ok := runner.(interface{ lockCommand(string) *exec.Cmd })
	if !ok {
		return nil, fmt.Errorf("%s cannot hold a deployment lock", runner.Describe())
	}
	locks := lockDirectory(layout)
	if err := runner.MkdirAll(locks); err != nil {
		return nil, err
	}
	file := path.Join(locks, path.Base(layout.Root))
	script := "exec 9>>" + ShellQuote(file) + "; flock -n 9 || exit 1; printf 'locked\\n'; read -r ignored"
	process := commandRunner.lockCommand(script)
	var failure bytes.Buffer
	process.Stderr = &failure
	input, err := process.StdinPipe()
	if err != nil {
		return nil, err
	}
	output, err := process.StdoutPipe()
	if err != nil {
		input.Close()
		return nil, err
	}
	if err := process.Start(); err != nil {
		input.Close()
		return nil, err
	}
	line, err := bufio.NewReader(output).ReadString('\n')
	if err != nil || line != "locked\n" {
		input.Close()
		process.Wait()
		return nil, fmt.Errorf("cannot lock deployment on %s: another operation holds it or flock is unavailable (%s); --force-unlock cannot bypass an active lock", runner.Describe(), firstLine(failure.Bytes()))
	}
	lock := &Lock{Version: 2, lockPath: layout.LockFile(), operationPath: operationFile(layout), lockOwner: runner, process: process, input: input, release: &sync.Once{}, done: make(chan struct{}), exited: make(chan struct{})}
	if existing, held := readLock(runner, layout.LockFile()); held && existing.Version != 2 && !force {
		if _, stale := existing.isStale(); !stale {
			input.Close()
			process.Wait()
			return nil, fmt.Errorf("legacy lock metadata on %s needs operator verification; use --force-unlock only after its old process has stopped", existing.Host)
		}
	}
	go func() {
		process.Wait()
		close(lock.exited)
	}()
	return lock, nil
}

func (lock *Lock) Release() {
	lock.release.Do(func() {
		close(lock.done)
		select {
		case <-lock.exited:
		default:
			if err := lock.lockOwner.RemoveAll(lock.lockPath); err != nil {
				fmt.Fprintf(os.Stderr, "warning: could not clear lock metadata: %v\n", err)
			}
		}
		lock.input.Close()
		<-lock.exited
	})
}

func (lock *Lock) ReleaseOnSignal() {
	interrupted := make(chan os.Signal, 1)
	signal.Notify(interrupted, os.Interrupt, syscall.SIGTERM)
	go func() {
		defer signal.Stop(interrupted)
		select {
		case <-interrupted:
			lock.Release()
			os.Exit(exitDeployFailed)
		case <-lock.done:
		case <-lock.exited:
			select {
			case <-lock.done:
				return
			default:
				fmt.Fprintln(os.Stderr, "deployment lock connection ended; stopping before further changes")
				os.Exit(exitDeployFailed)
			}
		}
	}()
}

// a legacy pid is meaningful only on the originating machine.
func (lock Lock) isStale() (string, bool) {
	hostname, err := os.Hostname()
	if err != nil || hostname == "" || lock.Host != hostname {
		return "", false
	}
	if lock.PID <= 0 {
		return "it names no process", true
	}

	// signal 0 distinguishes a dead process from permission denial.
	process, err := os.FindProcess(lock.PID)
	if err != nil {
		return fmt.Sprintf("pid %d is gone from this machine", lock.PID), true
	}

	err = process.Signal(syscall.Signal(0))
	if errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH) {
		return fmt.Sprintf("pid %d is gone from this machine", lock.PID), true
	}

	return "", false
}

func readLock(runner Runner, lockPath string) (Lock, bool) {
	raw, err := runner.ReadFile(lockPath)
	if errors.Is(err, fs.ErrNotExist) || (err == nil && len(bytes.TrimSpace(raw)) == 0) {
		return Lock{}, false
	}
	if err != nil {
		return Lock{}, true
	}

	var lock Lock
	if err := json.Unmarshal(raw, &lock); err != nil {
		// an unreadable lock file is still a lock, since something wrote it
		return Lock{}, true
	}

	return lock, true
}
