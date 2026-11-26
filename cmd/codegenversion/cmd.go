// Package codegenversion wires `w17ctl codegen-version` — the lock's
// `codegen_version`, the codegen engine the project generates with.
package codegenversion

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"golang.org/x/mod/semver"

	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/sdkupdate"
	codegenpb "github.com/wandering-compiler/sdk/go/pb/w17compiler"
)

// Cmd implements `w17ctl codegen-version <version>`.
//
// Like `sdk pin` it ships a LockEditIntent and writes back what the console
// returns: the lock is signed, and the pin's shape is checked server-side.
// `w17ctl update --all` sets it for you, to the minor of the SDK it pins; this
// is for the other two cases — an exact release candidate, and going back.
type Cmd struct {
	Version  string `arg:"" optional:"" name:"version" help:"A release minor (v0.1) — the project then gets that minor's newest patch — or an exact release candidate (v0.2.0-rc.3), which the console serves only to a project opted in on its page in the console (Codegen). Omit it with --clear."`
	Clear    bool   `name:"clear" help:"Remove the pin. A lock without one generates with v0.1, whatever newer minor the console carries; a new project is always given a pin."`
	LockPath string `name:"lock" placeholder:"PATH" default:"w17/lock.yaml" help:"Path to the lock file."`
	Console  string `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console (owns the lock). Optional — falls back to the console you are logged in to, then the binary's compile-time default."`
}

func (c *Cmd) Run() error {
	switch {
	case c.Clear && c.Version != "":
		return fmt.Errorf("codegen-version: pass a version or --clear, not both")
	case !c.Clear && c.Version == "":
		return fmt.Errorf("codegen-version: name a version (v0.1, or v0.2.0-rc.3) — or --clear to remove the pin")
	}
	if err := core.EditLockOnDisk("codegen-version", c.Console, c.LockPath, &codegenpb.LockEditIntent{
		Intent: &codegenpb.LockEditIntent_SetCodegenVersion{
			SetCodegenVersion: &codegenpb.SetCodegenVersionIntent{Version: c.Version},
		},
	}); err != nil {
		return err
	}
	if c.Clear {
		fmt.Fprintf(core.Stdout, "codegen-version: cleared (lock re-signed — the console generates with its newest minor)\n")
		return nil
	}
	fmt.Fprintf(core.Stdout, "codegen-version: %s (lock re-signed — the next codegen generates with it)\n", c.Version)
	return c.followSdk()
}

// followSdk keeps the lock's SDK pin on the codegen minor: the code a minor's
// engine writes builds against that minor's SDK, in both directions. Back
// (v0.2 → v0.1), the engine's code may not build against a newer SDK; forward,
// the SDK floor refuses the codegen until the SDK moves. Either way the pin
// moves to that minor's newest release, and nothing else would move it.
func (c *Cmd) followSdk() error {
	v := c.Version
	if !semver.IsValid(v) || semver.MajorMinor(v) != v {
		return nil // an exact release candidate is pinned by hand, SDK included
	}
	lockAbs, err := filepath.Abs(c.LockPath)
	if err != nil {
		return err
	}
	root := filepath.Dir(filepath.Dir(lockAbs))
	cur := sdkupdate.LockedVersion(root)
	if !semver.IsValid(cur) {
		return nil
	}
	cmp := semver.Compare(semver.MajorMinor(cur), v)
	if cmp == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	target, err := latestOnMinor(ctx, v)
	if cmp < 0 {
		// Forward: the lock's SDK pin moves with the codegen (codegen writes
		// every generated go.mod from it); the hand-written modules move
		// with `sdk update`.
		if err != nil {
			fmt.Fprintf(core.Stdout, "codegen-version: the project's SDK pin is %s, and no %s release of the SDK is published yet — `w17ctl sdk pin <a %s version>` and `w17ctl sdk update --version <it>` move it\n", cur, v, v)
			return nil
		}
		if err := pinSdkVia(c.Console, c.LockPath, target); err != nil {
			return fmt.Errorf("codegen-version: %s is pinned, but moving the SDK pin %s to %s failed: %w", v, cur, target, err)
		}
		fmt.Fprintf(core.Stdout, "codegen-version: SDK pin %s → %s, the newest %s release; `w17ctl sdk update --version %s` moves the hand-written modules too\n", cur, target, v, target)
		return nil
	}
	if err != nil {
		return fmt.Errorf("codegen-version: %s is pinned, but the SDK pin %s is newer than %s and could not be moved back: %w — `w17ctl sdk pin <a %s release>`", v, cur, v, err, v)
	}
	if err := pinSdkVia(c.Console, c.LockPath, target); err != nil {
		return fmt.Errorf("codegen-version: %s is pinned, but moving the SDK pin %s back to %s failed: %w", v, cur, target, err)
	}
	fmt.Fprintf(core.Stdout, "codegen-version: SDK pin %s → %s, the newest %s release (the next codegen writes it into every go.mod)\n", cur, target, v)
	return nil
}

// latestOnMinor is a seam for tests.
var latestOnMinor = sdkupdate.LatestOnMinor

// pinSdkVia sets the lock's sdk_version through the console, which re-signs it.
func pinSdkVia(console, lockPath, version string) error {
	return core.EditLockOnDisk("codegen-version", console, lockPath, &codegenpb.LockEditIntent{
		Intent: &codegenpb.LockEditIntent_SetSdkVersion{SetSdkVersion: &codegenpb.SetSdkVersionIntent{Version: version}},
	})
}

// pinSdk is pinSdkVia on the default console (tests).
func pinSdk(lockPath, version string) error { return pinSdkVia("", lockPath, version) }
