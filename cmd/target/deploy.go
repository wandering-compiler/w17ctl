package target

import (
	"fmt"
	"os"

	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/lockfile"
	codegenpb "github.com/wandering-compiler/sdk/go/pb/w17compiler"
)

// DeployCmd is the parent of `w17ctl target deploy <leaf>` — add / list /
// remove. It manages the lock's `deploy_targets[]` surface: the OPTIONAL
// deploy artefacts a project wants generated.
//
// Docker is not on this surface and cannot be: dev runs on it, e2e cannot run
// without it, and the generated Compose files are what every project actually
// uses. That is a known, accepted dependency rather than an accident.
//
// Kubernetes was the opposite — emitted into every bundle of every project
// until 2026-09-22, for a platform most of them will never deploy to. It is
// opt-in now, and the next codegen SWEEPS the manifests of a target that is
// not declared, so a project does not keep a file nothing regenerates.
//
// Block 2 §8.2: the console owns the lock. add / remove go through the
// console's EditLock (which validates the target name + re-signs); list reads
// through DescribeLock.
type DeployCmd struct {
	Add    DeployAddCmd    `cmd:"" help:"Opt a deploy target into generated artefacts (today: kubernetes). Re-signs on save."`
	List   DeployListCmd   `cmd:"" help:"List the optional deploy targets declared in the lock. Docker is always emitted and is never listed."`
	Remove DeployRemoveCmd `cmd:"" help:"Remove a deploy target's declaration. The next codegen also deletes the artefacts it had emitted. Re-signs on save."`
}

// DeployAddCmd implements `w17ctl target deploy add <target>`.
type DeployAddCmd struct {
	Target   string `arg:"" help:"Deploy target to generate artefacts for. Today: kubernetes (renders deploy/prod/k8s.yaml in each bundle)."`
	LockPath string `name:"lock" placeholder:"PATH" default:"w17/lock.yaml" help:"Path to the lock file."`
	Console  string `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console (owns the lock). Optional — falls back to the binary's compile-time default."`
}

func (c *DeployAddCmd) Run() error {
	if c.Target == "" {
		return fmt.Errorf("deploy add: target required")
	}
	// Serialise read → EditLock → write against concurrent lock-mutating runs,
	// the same way `ci add` does, so the second write cannot clobber the
	// first's edit. Held until this function returns.
	release, lockErr := lockfile.ForUpdate(c.LockPath)
	if lockErr != nil {
		return fmt.Errorf("deploy add: lock for update: %w", lockErr)
	}
	defer release()

	lockBytes, err := os.ReadFile(c.LockPath)
	if err != nil {
		return fmt.Errorf("deploy add: read lock %s: %w", c.LockPath, err)
	}
	// Read the current targets so an already-declared one prints the no-change
	// note (the console's EditLock is idempotent regardless).
	view, err := core.DescribeLock(c.Console, lockBytes)
	if err != nil {
		return fmt.Errorf("deploy add: %w", err)
	}
	for _, t := range view.GetDeployTargets() {
		if t == c.Target {
			fmt.Fprintf(core.Stdout, "deploy add: %s already declared (no change) (%s)\n", c.Target, c.LockPath)
			return nil
		}
	}

	newBytes, err := core.EditLock(c.Console, lockBytes, &codegenpb.LockEditIntent{
		Intent: &codegenpb.LockEditIntent_AddDeployTarget{
			AddDeployTarget: &codegenpb.AddDeployTargetIntent{Target: c.Target},
		},
	})
	if err != nil {
		return fmt.Errorf("deploy add: %w", err)
	}
	if err := lockfile.WriteAtomic(c.LockPath, newBytes, 0o644); err != nil {
		return fmt.Errorf("deploy add: write lock: %w", err)
	}
	fmt.Fprintf(core.Stdout, "deploy add: %s (%s) — run `w17ctl codegen` to emit its artefacts\n", c.Target, c.LockPath)
	return nil
}

// DeployListCmd implements `w17ctl target deploy list`.
type DeployListCmd struct {
	LockPath string `name:"lock" placeholder:"PATH" default:"w17/lock.yaml" help:"Path to the lock file."`
	Console  string `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console (owns the lock). Optional — falls back to the binary's compile-time default."`
}

func (c *DeployListCmd) Run() error {
	view, err := core.DescribeLockAt("deploy list", c.Console, c.LockPath)
	if err != nil {
		return err
	}
	// Docker is stated rather than listed. An empty list would otherwise read
	// as "this project generates no deploy artefacts", which is false — the
	// Compose files every project actually uses are always emitted.
	fmt.Fprintln(core.Stdout, "docker → compose.yaml + deploy/prod/compose.yaml (always emitted, cannot be removed)")
	ts := view.GetDeployTargets()
	if len(ts) == 0 {
		fmt.Fprintln(core.Stdout, "no optional deploy targets declared")
		return nil
	}
	for _, t := range ts {
		fmt.Fprintf(core.Stdout, "%s → deploy/prod/k8s.yaml per bundle\n", t)
	}
	return nil
}

// DeployRemoveCmd implements `w17ctl target deploy remove <target>`.
type DeployRemoveCmd struct {
	Target   string `arg:"" help:"Deploy target whose declaration to remove (stop generating its artefacts)."`
	LockPath string `name:"lock" placeholder:"PATH" default:"w17/lock.yaml" help:"Path to the lock file."`
	Console  string `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console (owns the lock). Optional — falls back to the binary's compile-time default."`
}

func (c *DeployRemoveCmd) Run() error {
	if err := core.EditLockOnDisk("deploy remove", c.Console, c.LockPath, &codegenpb.LockEditIntent{
		Intent: &codegenpb.LockEditIntent_RemoveDeployTarget{
			RemoveDeployTarget: &codegenpb.RemoveDeployTargetIntent{Target: c.Target},
		},
	}); err != nil {
		return err
	}
	fmt.Fprintf(core.Stdout, "deploy remove: %s no longer generates artefacts (%s) — the next `w17ctl codegen` deletes the ones already on disk\n", c.Target, c.LockPath)
	return nil
}
