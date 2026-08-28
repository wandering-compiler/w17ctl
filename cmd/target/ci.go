package target

import (
	"fmt"
	"os"

	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/lockfile"
	codegenpb "github.com/wandering-compiler/sdk/go/pb/w17compiler"
)

// CiCmd is the parent of `w17ctl target ci <leaf>` — add / list / remove. It
// manages the lock's `ci_configs[]` surface (the opt-in for generated
// e2e CI). Each entry names one CI provider; codegen renders a
// self-contained, w17ctl-driven CI config under `w17/ci/<provider>/`
// that the consumer copies into its CI's canonical location (the
// compiler never touches the repo root — Zero-Code Isolation).
//
// Block 2 §8.2: the console owns the lock. add / remove go through the
// console's EditLock (which validates the provider name + re-signs); list
// reads through DescribeLock.
type CiCmd struct {
	Add    CiAddCmd    `cmd:"" help:"Opt a CI provider into generated e2e CI (github|gitlab|circleci|azure|bitbucket|jenkins|generic). Re-signs on save."`
	List   CiListCmd   `cmd:"" help:"List the CI providers declared in the lock."`
	Remove CiRemoveCmd `cmd:"" help:"Remove a CI provider's declaration (stop generating its config). Re-signs on save."`
}

// CiAddCmd implements `w17ctl target ci add <provider>`. It appends (or no-ops
// on) the provider's `ci_configs[]` entry. The output path
// (`w17/ci/<provider>/`) is derived, so there's nothing else to name.
type CiAddCmd struct {
	Provider string `arg:"" help:"CI provider to generate e2e CI for: github|gitlab|circleci|azure|bitbucket|jenkins|generic."`
	LockPath string `name:"lock" placeholder:"PATH" default:"w17/lock.yaml" help:"Path to the lock file."`
	Console  string `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console (owns the lock). Optional — falls back to the binary's compile-time default."`
}

func (c *CiAddCmd) Run() error {
	if c.Provider == "" {
		return fmt.Errorf("ci add: provider required")
	}
	// Q58-console-1: serialise the read → EditLock → write below against
	// concurrent lock-mutating runs so the second write can't clobber the
	// first's edit. Held until this function returns.
	release, lockErr := lockfile.ForUpdate(c.LockPath)
	if lockErr != nil {
		return fmt.Errorf("ci add: lock for update: %w", lockErr)
	}
	defer release()

	lockBytes, err := os.ReadFile(c.LockPath)
	if err != nil {
		return fmt.Errorf("ci add: read lock %s: %w", c.LockPath, err)
	}
	// Read the current providers so an already-declared one prints the
	// no-change note (the console's EditLock is idempotent regardless).
	view, err := core.DescribeLock(c.Console, lockBytes)
	if err != nil {
		return fmt.Errorf("ci add: %w", err)
	}
	for _, p := range view.GetCiProviders() {
		if p == c.Provider {
			fmt.Fprintf(core.Stdout, "ci add: %s already declared (no change) (%s)\n", c.Provider, c.LockPath)
			return nil
		}
	}

	newBytes, err := core.EditLock(c.Console, lockBytes, &codegenpb.LockEditIntent{
		Intent: &codegenpb.LockEditIntent_AddCiConfig{
			AddCiConfig: &codegenpb.AddCiConfigIntent{Provider: c.Provider},
		},
	})
	if err != nil {
		return fmt.Errorf("ci add: %w", err)
	}
	if err := lockfile.WriteAtomic(c.LockPath, newBytes, 0o644); err != nil {
		return fmt.Errorf("ci add: write lock: %w", err)
	}
	fmt.Fprintf(core.Stdout, "ci add: %s → w17/ci/%s/ (%s)\n", c.Provider, c.Provider, c.LockPath)
	return nil
}

// CiListCmd implements `w17ctl target ci list`.
type CiListCmd struct {
	LockPath string `name:"lock" placeholder:"PATH" default:"w17/lock.yaml" help:"Path to the lock file."`
	Console  string `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console (owns the lock). Optional — falls back to the binary's compile-time default."`
}

func (c *CiListCmd) Run() error {
	view, err := core.DescribeLockAt("ci list", c.Console, c.LockPath)
	if err != nil {
		return err
	}
	cis := view.GetCiProviders()
	if len(cis) == 0 {
		fmt.Fprintln(core.Stdout, "no CI providers declared (no w17/ci/ tree generated)")
		return nil
	}
	for _, p := range cis {
		fmt.Fprintf(core.Stdout, "%s → w17/ci/%s/\n", p, p)
	}
	return nil
}

// CiRemoveCmd implements `w17ctl target ci remove <provider>`.
type CiRemoveCmd struct {
	Provider string `arg:"" help:"CI provider whose declaration to remove (stop generating its config)."`
	LockPath string `name:"lock" placeholder:"PATH" default:"w17/lock.yaml" help:"Path to the lock file."`
	Console  string `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console (owns the lock). Optional — falls back to the binary's compile-time default."`
}

func (c *CiRemoveCmd) Run() error {
	if err := core.EditLockOnDisk("ci remove", c.Console, c.LockPath, &codegenpb.LockEditIntent{
		Intent: &codegenpb.LockEditIntent_RemoveCiConfig{
			RemoveCiConfig: &codegenpb.RemoveCiConfigIntent{Provider: c.Provider},
		},
	}); err != nil {
		return err
	}
	fmt.Fprintf(core.Stdout, "ci remove: %s no longer generates CI config (%s)\n", c.Provider, c.LockPath)
	return nil
}
