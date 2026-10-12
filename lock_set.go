package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/signal"
	"slices"
	"sync"
	"syscall"
)

// lockTarget is one deployment root as the host sees it, not as an alias names
// it, so two aliases can never be given different locks for the same root.
type lockTarget struct {
	Name        string
	Destination Destination
	Observed    DestinationObservation
}

func (target lockTarget) key() string {
	return target.Observed.HostIdentity + "\x00" + target.Observed.CanonicalPath
}

// lockOrder is the one order every mutating command takes locks in: by host
// identity, then canonical path. Taking them in any other order is how two
// deploys each end up holding what the other needs.
func lockOrder(destinations map[string]Destination, observations map[string]DestinationObservation) ([]lockTarget, error) {
	if len(destinations) == 0 {
		return nil, errors.New("no destinations to lock")
	}
	grouped := map[string]lockTarget{}
	for _, name := range slices.Sorted(maps.Keys(destinations)) {
		observed, found := observations[name]
		if !found || observed.HostIdentity == "" || observed.CanonicalPath == "" {
			return nil, fmt.Errorf("destination %s has not been observed", name)
		}
		target := lockTarget{Name: name, Destination: destinations[name], Observed: observed}
		// one root recorded under two names would have two owners
		if existing, found := grouped[target.key()]; found {
			return nil, fmt.Errorf("destinations %s and %s are the same directory on the same host; use one destination name", existing.Name, name)
		}
		grouped[target.key()] = target
	}

	ordered := make([]lockTarget, 0, len(grouped))
	for _, key := range slices.Sorted(maps.Keys(grouped)) {
		ordered = append(ordered, grouped[key])
	}

	return ordered, nil
}

// LockSet holds every destination a multi-destination command changes.
type LockSet struct {
	locks    []*Lock
	targets  []lockTarget
	closers  []func()
	released chan struct{}
	once     sync.Once
}

type lockSetOptions struct {
	runtimeID string
	// generations are the state generations the plan was made from, by
	// destination name; zero means no version 2 state was recorded.
	generations map[string]int
	open        func(Destination) (Runner, func(), error)
	observe     func(context.Context, Destination) (DestinationObservation, error)
}

// acquireLockSet takes every lock before anything changes, then checks that
// the destinations are still the ones that were planned. Nothing is mutated
// while only some locks are held, and any failure releases all of them.
func acquireLockSet(ctx context.Context, targets []lockTarget, options lockSetOptions) (*LockSet, error) {
	set := &LockSet{released: make(chan struct{})}
	for _, target := range targets {
		runner, closeRunner, err := options.open(target.Destination)
		if err != nil {
			set.Release()
			return nil, fmt.Errorf("destination %s: %w", target.Name, err)
		}
		set.closers = append(set.closers, closeRunner)
		lock, err := holdLock(runner, NewLayout(target.Observed.CanonicalPath, options.runtimeID), false)
		if err != nil {
			set.Release()
			return nil, fmt.Errorf("destination %s: %w; no destination was changed", target.Name, err)
		}
		set.locks = append(set.locks, lock)
		set.targets = append(set.targets, target)
	}

	if err := set.verify(ctx, options); err != nil {
		set.Release()
		return nil, err
	}

	return set, nil
}

// verify re-reads what the plan relied on, now that nobody else can change it.
func (set *LockSet) verify(ctx context.Context, options lockSetOptions) error {
	for index, target := range set.targets {
		current, err := options.observe(ctx, target.Destination)
		if err != nil {
			return fmt.Errorf("destination %s: %w", target.Name, err)
		}
		planned := target.Observed
		if current.HostIdentity != planned.HostIdentity || current.CanonicalPath != planned.CanonicalPath || current.bootID != planned.bootID || current.daemonID != planned.daemonID {
			return fmt.Errorf("destination %s changed since it was planned (host, path, boot or Docker daemon); plan again", target.Name)
		}

		lock := set.locks[index]
		if err := lock.checkNoOperation(); err != nil {
			return fmt.Errorf("destination %s: %w", target.Name, err)
		}
		state, found, err := ReadServiceState(lock.lockOwner, NewLayout(planned.CanonicalPath, options.runtimeID))
		if err != nil {
			return fmt.Errorf("destination %s: %w", target.Name, err)
		}
		if found && (state.Project != options.runtimeID || state.Destination != target.Name) {
			return fmt.Errorf("destination %s holds state recorded for %s at destination %s; refusing to change it", target.Name, state.Project, state.Destination)
		}
		if expected := options.generations[target.Name]; state.Generation != expected {
			return fmt.Errorf("destination %s state moved from generation %d to %d since it was planned; plan again", target.Name, expected, state.Generation)
		}
	}

	return nil
}

// Begin records one operation, under one id, on every destination before any
// of them changes. If a record cannot be written, the ones already written
// are cleared, since nothing has changed yet.
func (set *LockSet) Begin(command, target string) (Operation, error) {
	operation, err := newOperation(command, target)
	if err != nil {
		return Operation{}, err
	}
	for _, lock := range set.locks {
		if err := lock.recordOperation(operation); err != nil {
			for _, recorded := range set.locks {
				if recorded.operation != nil {
					recorded.endOperation()
				}
			}
			return Operation{}, err
		}
	}

	return operation, nil
}

// Finish is deferred directly, like Lock.Finish. Every destination keeps the
// record unless the command observed how the whole operation ended.
func (set *LockSet) Finish(exitCode *int) {
	failure := recover()
	for _, lock := range set.locks {
		if lock.operation == nil {
			continue
		}
		if failure == nil && *exitCode != exitLiveButNeedsAHuman {
			lock.endOperation()
		} else {
			fmt.Fprintf(os.Stderr, "the operation stays recorded on %s until someone inspects it and runs deploy resolve %s\n", lock.lockOwner.Describe(), lock.operation.ID)
		}
	}
	set.Release()
	if failure != nil {
		panic(failure)
	}
}

// Release lets go in reverse order and closes the connections. It is safe to
// call more than once.
func (set *LockSet) Release() {
	set.once.Do(func() { close(set.released) })
	for index := len(set.locks) - 1; index >= 0; index-- {
		set.locks[index].Release()
	}
	for index := len(set.closers) - 1; index >= 0; index-- {
		set.closers[index]()
	}
	set.locks, set.targets, set.closers = nil, nil, nil
}

// Runner is the connection that holds the lock on the index-th destination in
// lock order.
func (set *LockSet) Runner(index int) Runner {
	return set.locks[index].lockOwner
}

// ReleaseOnSignal stops a command between steps when it is interrupted or when
// any lock's connection ends, leaving the operation recorded.
func (set *LockSet) ReleaseOnSignal() {
	interrupted := make(chan os.Signal, 1)
	signal.Notify(interrupted, os.Interrupt, syscall.SIGTERM)
	lost := make(chan struct{}, len(set.locks))
	for _, lock := range set.locks {
		go func() {
			select {
			case <-lock.exited:
				lost <- struct{}{}
			case <-lock.done:
			}
		}()
	}
	go func() {
		defer signal.Stop(interrupted)
		select {
		case <-interrupted:
			set.Release()
			os.Exit(exitDeployFailed)
		case <-lost:
			select {
			case <-set.released:
				return
			default:
			}
			fmt.Fprintln(os.Stderr, "deployment lock connection ended; stopping before further changes")
			os.Exit(exitDeployFailed)
		case <-set.released:
		}
	}()
}
