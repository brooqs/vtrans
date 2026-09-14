package main

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// Pause is a file. The interface creates ~/.local/state/vtrans/pause and the
// worker notices within a second; removing it resumes. A file rather than a
// signal or a socket because the two sides already talk through the state
// directory, and a request that is a file survives the worker restarting:
// a paused service stays paused across systemd's ten-minute cycle until
// someone presses Resume.
//
// Pausing acts at two levels. The running ffmpeg is frozen with SIGSTOP, so
// the effect is immediate and the GPU goes quiet, and the run loop does not
// start the next file while the request stands. Nothing else is interrupted:
// a commit (the rename and the move to the trash) is a few milliseconds and
// is left alone, so a pause never leaves a half-replaced file behind.

func pausePath() string { return filepath.Join(stateDir(), "pause") }

// PauseRequested reports whether a pause has been asked for.
func PauseRequested() bool {
	_, err := os.Stat(pausePath())
	return err == nil
}

// RequestPause / RequestResume are what the interface calls.
func RequestPause() error {
	if err := os.MkdirAll(stateDir(), 0o755); err != nil {
		return err
	}
	return os.WriteFile(pausePath(), []byte(time.Now().Format(time.RFC3339)+"\n"), 0o644)
}

func RequestResume() error {
	err := os.Remove(pausePath())
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// pauser tracks the ffmpeg child so the watcher can freeze and thaw it.
type pauser struct {
	mu      sync.Mutex
	child   *os.Process
	stopped bool // child is currently frozen
	paused  bool // a pause request is in effect (child or not)
}

var pause pauser

// Register hands the current ffmpeg child to the pauser. If a pause is already
// in effect the child is frozen before it gets going.
func (p *pauser) Register(proc *os.Process) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.child = proc
	p.stopped = false
	if p.paused && proc != nil {
		if syscall.Kill(proc.Pid, syscall.SIGSTOP) == nil {
			p.stopped = true
		}
	}
}

// Unregister forgets the child once it has exited.
func (p *pauser) Unregister() {
	p.mu.Lock()
	p.child, p.stopped = nil, false
	p.mu.Unlock()
}

// Paused reports whether a pause is in effect right now.
func (p *pauser) Paused() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.paused
}

// apply brings the child in line with the request. Returns whether the paused
// state changed, so the caller can report it once rather than every tick.
func (p *pauser) apply(want bool) (changed bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	changed = p.paused != want
	p.paused = want
	if p.child == nil {
		return changed
	}
	switch {
	case want && !p.stopped:
		if syscall.Kill(p.child.Pid, syscall.SIGSTOP) == nil {
			p.stopped = true
		}
	case !want && p.stopped:
		if syscall.Kill(p.child.Pid, syscall.SIGCONT) == nil {
			p.stopped = false
		}
	}
	return changed
}

// Watch polls the request file until ctx ends. While paused it keeps the run
// record's heartbeat going, otherwise the interface would take the silence
// for a dead worker after thirty seconds.
func (p *pauser) Watch(ctx context.Context, rep *RunReporter) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			// Never leave a frozen child behind: ctx ending means shutdown, and
			// a SIGTERM to a stopped process is only delivered once it runs.
			p.apply(false)
			return
		case <-t.C:
			want := PauseRequested()
			if p.apply(want) {
				rep.SetPaused(want)
			} else if want {
				rep.Heartbeat()
			}
		}
	}
}

// WaitIfPaused blocks at a file boundary while a pause request stands.
func (p *pauser) WaitIfPaused(ctx context.Context, rep *RunReporter) error {
	for PauseRequested() {
		if p.apply(true) {
			rep.SetPaused(true)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	if p.apply(false) {
		rep.SetPaused(false)
	}
	return nil
}
