package core

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/wandering-compiler/w17ctl/internal/lockfile"
	codegenpb "github.com/wandering-compiler/sdk/go/pb/w17compiler"
)

// w17/infra.yaml — the project's infrastructure (targets, environments,
// workloads, CI). The person's file, committed: the client only carries its
// bytes to the console, which decodes, validates, edits and renders it, and
// writes back what the console returns.

// InfraPath is w17/infra.yaml beside the lock at lockPath.
func InfraPath(lockPath string) string {
	return filepath.Join(filepath.Dir(lockPath), "infra.yaml")
}

// ReadInfra reads w17/infra.yaml beside the lock; a missing file is empty.
func ReadInfra(lockPath string) ([]byte, error) {
	b, err := os.ReadFile(InfraPath(lockPath)) // #nosec G304 -- the project's own file
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", InfraPath(lockPath), err)
	}
	return b, nil
}

// DescribeInfraAt is DescribeLockAt with w17/infra.yaml: the view's infra is
// the file's (or the lock's own block while the project has not moved it).
// effective asks for the rendered effective view of env ("" = all).
func DescribeInfraAt(prefix, console, lockPath string, effective bool, env string) (*codegenpb.LockView, error) {
	lockBytes, err := os.ReadFile(lockPath) // #nosec G304 -- the project's own lock
	if err != nil {
		return nil, fmt.Errorf("%s: read lock %s: %w", prefix, lockPath, err)
	}
	infra, err := ReadInfra(lockPath)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", prefix, err)
	}
	cl, conn, err := dialLockConsole(console)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", prefix, err)
	}
	defer func() { _ = conn.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	view, err := cl.DescribeLock(ctx, &codegenpb.DescribeLockRequest{
		Lock: lockBytes, Infra: infra, WantInfraEffective: effective, InfraEffectiveEnv: env,
	})
	if err != nil {
		return nil, fmt.Errorf("%s: describe lock: %w", prefix, err)
	}
	return view, nil
}

// EditInfraOnDisk applies an infra intent (set_infra / edit_infra) under the
// lock's update lock: it sends the lock and w17/infra.yaml, writes the
// re-signed lock and the file the console returns — or removes the file
// when the console emptied it — and prints the console's notes. build gets
// the current view first (the wizard's defaults); it returns the intent.
func EditInfraOnDisk(prefix, console, lockPath string, build func(view *codegenpb.LockView) (*codegenpb.LockEditIntent, error)) (*codegenpb.EditLockResponse, error) {
	release, err := lockfile.ForUpdate(lockPath)
	if err != nil {
		return nil, fmt.Errorf("%s: lock for update: %w", prefix, err)
	}
	defer release()
	lockBytes, err := os.ReadFile(lockPath) // #nosec G304 -- the project's own lock
	if err != nil {
		return nil, fmt.Errorf("%s: read lock %s: %w", prefix, lockPath, err)
	}
	infra, err := ReadInfra(lockPath)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", prefix, err)
	}
	cl, conn, err := dialLockConsole(console)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", prefix, err)
	}
	defer func() { _ = conn.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	view, err := cl.DescribeLock(ctx, &codegenpb.DescribeLockRequest{Lock: lockBytes, Infra: infra})
	if err != nil {
		return nil, fmt.Errorf("%s: describe lock: %w", prefix, err)
	}
	intent, err := build(view)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", prefix, err)
	}
	ectx, ecancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer ecancel()
	resp, err := cl.EditLock(ectx, &codegenpb.EditLockRequest{Lock: lockBytes, Intent: intent, Infra: infra})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", prefix, err)
	}
	if resp.GetInfraWritten() {
		path := InfraPath(lockPath)
		if len(resp.GetInfra()) == 0 {
			if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return nil, fmt.Errorf("%s: remove %s: %w", prefix, path, err)
			}
		} else if err := lockfile.WriteAtomic(path, resp.GetInfra(), 0o644); err != nil {
			return nil, fmt.Errorf("%s: write %s: %w", prefix, path, err)
		}
	}
	if err := lockfile.WriteAtomic(lockPath, resp.GetLock(), 0o644); err != nil {
		return nil, fmt.Errorf("%s: write lock: %w", prefix, err)
	}
	for _, n := range resp.GetNotes() {
		fmt.Fprintf(Stdout, "%s: %s\n", prefix, n)
	}
	return resp, nil
}
