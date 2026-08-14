package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// acquireLock prevents more than one writing vtrans run at a time.
//
// The lock is needed because two concurrent runs write the same recovery
// journal (pending.json) and may pick the same file to process; in replace
// mode that means data loss. The likeliest collision is the service running
// alongside a hand-started run.
//
// flock is used: the kernel releases it when the process dies, so a leftover
// lock file after a crash never blocks the next run.
func acquireLock() (release func(), err error) {
	if err := os.MkdirAll(stateDir(), 0o755); err != nil {
		return nil, err
	}
	p := filepath.Join(stateDir(), "lock")

	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		owner := lockOwner(f)
		_ = f.Close()
		return nil, fmt.Errorf("another vtrans run is in progress%s (lock: %s)", owner, p)
	}

	// Lock acquired: record the owner so a collision shows who holds it.
	_ = f.Truncate(0)
	_, _ = f.Seek(0, 0)
	fmt.Fprintf(f, "%d\n", os.Getpid())

	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

// lockOwner returns the pid holding the lock in a readable form.
func lockOwner(f *os.File) string {
	buf := make([]byte, 32)
	n, err := f.ReadAt(buf, 0)
	if n == 0 || (err != nil && n == 0) {
		return ""
	}
	pid := strings.TrimSpace(string(buf[:n]))
	if pid == "" {
		return ""
	}
	return " (pid " + pid + ")"
}
