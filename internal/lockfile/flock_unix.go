//go:build unix

package lockfile

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// ForUpdate takes an exclusive, cross-process advisory lock guarding the
// lock file at `path` and returns a release closure. Hold it across a
// read-modify-write of the lock (Load → mutate → Save) so two concurrent
// `w17ctl push` runs pinning per-connection targets can't lose one
// another's pins to a last-writer-wins race (Q58-console-1): without it
// both load the same baseline, each updates its own connection, and the
// second Save (a non-atomic os.WriteFile) clobbers the first.
//
// The lock is an flock(2) on a `<path>.lock` sentinel — held by the open
// file description and dropped by the kernel on close OR process death,
// so a crash mid-update leaves no stale lock to wedge the next run.
// Distinct open fds contend even within one process, so this also
// serialises concurrent in-process callers. The sentinel's directory is
// created if missing (a fresh project has no lock dir yet).
func ForUpdate(path string) (func(), error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("lock: mkdir for update lock %q: %w", dir, err)
		}
	}
	sentinel := path + ".lock"
	f, err := os.OpenFile(sentinel, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("lock: open update lock %q: %w", sentinel, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("lock: flock %q: %w", sentinel, err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
