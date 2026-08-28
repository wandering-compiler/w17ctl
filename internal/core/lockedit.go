package core

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"google.golang.org/grpc"

	"github.com/wandering-compiler/w17ctl/internal/lockfile"
	codegenpb "github.com/wandering-compiler/sdk/go/pb/w17compiler"
)

// EditLockOnDisk runs the read → EditLock → write lock-mutation pipeline
// under the cross-process update lock (Q58-console-1): it acquires the
// ForUpdate lock on lockPath, reads the current bytes, applies intent
// through the console's EditLock (which validates + re-signs), and writes
// the result atomically — holding the lock across the whole read→write
// window so a concurrent run can't clobber the edit. prefix labels every
// wrapped error (`<prefix>: lock for update: …`, etc.); the caller prints
// its own success line after this returns.
//
// This is the shared core of every `target …` / `connection …` lock-edit
// command whose intent is built entirely from flags (no inspection of the
// current lock bytes between read and edit). Commands that must read the
// current lock first (dup-checks, interactive wizards) keep their own
// inline read→edit→write so the inspection stays inside the lock.
func EditLockOnDisk(prefix, console, lockPath string, intent *codegenpb.LockEditIntent) error {
	release, lockErr := lockfile.ForUpdate(lockPath)
	if lockErr != nil {
		return fmt.Errorf("%s: lock for update: %w", prefix, lockErr)
	}
	defer release()

	lockBytes, err := os.ReadFile(lockPath)
	if err != nil {
		return fmt.Errorf("%s: read lock %s: %w", prefix, lockPath, err)
	}
	newBytes, err := EditLock(console, lockBytes, intent)
	if err != nil {
		return fmt.Errorf("%s: %w", prefix, err)
	}
	if err := lockfile.WriteAtomic(lockPath, newBytes, 0o644); err != nil {
		return fmt.Errorf("%s: write lock: %w", prefix, err)
	}
	return nil
}

// EditLock ships the current lock bytes + a typed intent to the console's
// EditLock RPC and returns the re-signed bytes to write back (public-split
// Block 2 §8.2). The console owns the lock: the thin client constructs no
// lockpb and signs nothing — it just sends the intent and writes the bytes.
//
// The console address is resolved WITHOUT the lock (nil) — w17_url no longer
// rides the lock for the client (you can't dial the console to learn where
// it is); it comes from the --console flag / W17_CONSOLE_ADDR env / compiled
// default.
func EditLock(console string, lockBytes []byte, intent *codegenpb.LockEditIntent) ([]byte, error) {
	cl, conn, err := dialLockConsole(console)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := cl.EditLock(ctx, &codegenpb.EditLockRequest{Lock: lockBytes, Intent: intent})
	if err != nil {
		return nil, fmt.Errorf("edit lock: %w", err)
	}
	return resp.GetLock(), nil
}

// DescribeLock asks the console for the lock's routing projection (the
// connection set, default, dir layout) — replacing client-side
// lock.Effective* / lockpb parsing (Block 2 §8.2).
func DescribeLock(console string, lockBytes []byte) (*codegenpb.LockView, error) {
	cl, conn, err := dialLockConsole(console)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	view, err := cl.DescribeLock(ctx, &codegenpb.DescribeLockRequest{Lock: lockBytes})
	if err != nil {
		return nil, fmt.Errorf("describe lock: %w", err)
	}
	return view, nil
}

// DescribeLockAt reads the lock at lockPath and returns its routing
// projection from the console — the read-side counterpart to
// EditLockOnDisk for the `… list` commands that render one of the lock's
// declared surfaces. prefix labels the wrapped read / describe errors
// (`<prefix>: read lock <path>: …`, `<prefix>: …`).
func DescribeLockAt(prefix, console, lockPath string) (*codegenpb.LockView, error) {
	lockBytes, err := os.ReadFile(lockPath)
	if err != nil {
		return nil, fmt.Errorf("%s: read lock %s: %w", prefix, lockPath, err)
	}
	view, err := DescribeLock(console, lockBytes)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", prefix, err)
	}
	return view, nil
}

// DescribeLockFromRoot reads the project's `<root>/w17/lock.yaml` and returns
// its routing projection from the console — the read-site counterpart to
// EditLock for commands that only need the lock's dir layout / project name
// (e.g. `domain add`, `module add`). A missing lock surfaces the read error.
func DescribeLockFromRoot(console, root string) (*codegenpb.LockView, error) {
	lockBytes, err := os.ReadFile(filepath.Join(root, "w17", "lock.yaml"))
	if err != nil {
		return nil, fmt.Errorf("read lock: %w", err)
	}
	return DescribeLock(console, lockBytes)
}

// DescribeLockBestEffort finds the project root + returns the lock's routing
// projection, or nil on ANY failure (no root / no lock / console unreachable).
// The best-effort counterpart to the old LoadLockBestEffort — for read sites
// that fall back to defaults when the lock can't be resolved (e.g. stack's
// proto-dir / connection auto-resolution).
func DescribeLockBestEffort(console string) *codegenpb.LockView {
	root, err := FindProjectRoot()
	if err != nil {
		return nil
	}
	view, err := DescribeLockFromRoot(console, root)
	if err != nil {
		return nil
	}
	return view
}

func dialLockConsole(console string) (codegenpb.CodegenServiceClient, *grpc.ClientConn, error) {
	addr, err := ResolveConsoleAddr(console)
	if err != nil {
		return nil, nil, err
	}
	cl, conn, err := DialCodegen(addr)
	if err != nil {
		return nil, nil, fmt.Errorf("connect %s: %w", addr, err)
	}
	return cl, conn, nil
}
