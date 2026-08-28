package target

import (
	"fmt"

	"github.com/wandering-compiler/w17ctl/internal/core"
	codegenpb "github.com/wandering-compiler/sdk/go/pb/w17compiler"
)

// ScaleCmd is the parent of `w17ctl target scale <leaf>` — set / list /
// unset. It manages the lock's `replicas[]` surface (storage gRPC
// proxy feature): the per-service PROD replica counts the generated
// Compose / k8s render.
//
// Block 2 §8.2: the console owns the lock. These commands treat
// w17/lock.yaml as opaque bytes — the replica edit goes through the
// console's EditLock (which re-signs + runs the pre-sign Validate: count
// >= 1, no folded tier scaled past 1), the list read through DescribeLock.
//
// The one load-bearing effect: a `<domain>-storage` bundle set past 1
// materializes the storage gRPC proxy in the prod artifacts (S4).
// Single-replica tiers and composed single binaries never do.
type ScaleCmd struct {
	Set   ScaleSetCmd   `cmd:"" help:"Set a service's PROD replica count. Re-signs on save. A <domain>-storage bundle past 1 materializes the storage gRPC proxy."`
	List  ScaleListCmd  `cmd:"" help:"List the per-service replica counts declared in the lock."`
	Unset ScaleUnsetCmd `cmd:"" help:"Remove a service's replica-count entry (revert to the single-replica default). Re-signs on save."`
}

// ScaleSetCmd implements `w17ctl target scale set <service> <count>`. It sets
// (or replaces) the service's `replicas[]` entry. The service name is
// the deployable bundle name as it appears in the generated artifacts
// (`<domain>-storage`, `<domain>-gateway`, `<domain>-server`, …).
type ScaleSetCmd struct {
	Service  string `arg:"" help:"Deployable bundle/service name (<domain>-storage, <domain>-gateway, <domain>-server, …)."`
	Count    int32  `arg:"" help:"Desired PROD replica count (>= 1). A <domain>-storage bundle > 1 materializes the storage gRPC proxy."`
	LockPath string `name:"lock" placeholder:"PATH" default:"w17/lock.yaml" help:"Path to the lock file."`
	Console  string `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console (owns the lock). Optional — falls back to the binary's compile-time default."`
}

func (c *ScaleSetCmd) Run() error {
	if c.Service == "" {
		return fmt.Errorf("scale set: service required")
	}
	if c.Count < 1 {
		return fmt.Errorf("scale set: count must be >= 1 (got %d)", c.Count)
	}

	// The console replaces-or-appends the entry + re-signs; its pre-sign
	// Validate rejects a folded tier scaled past 1 with the canonical message.
	if err := core.EditLockOnDisk("scale set", c.Console, c.LockPath, &codegenpb.LockEditIntent{
		Intent: &codegenpb.LockEditIntent_SetReplicas{
			SetReplicas: &codegenpb.SetReplicasIntent{Service: c.Service, Count: c.Count},
		},
	}); err != nil {
		return err
	}
	note := ""
	if isStorageBundle(c.Service) && c.Count > 1 {
		note = " (storage gRPC proxy will be materialized in prod)"
	}
	fmt.Fprintf(core.Stdout, "scale set: %s → %d replica(s)%s (%s)\n", c.Service, c.Count, note, c.LockPath)
	return nil
}

// ScaleListCmd implements `w17ctl target scale list`.
type ScaleListCmd struct {
	LockPath string `name:"lock" placeholder:"PATH" default:"w17/lock.yaml" help:"Path to the lock file."`
	Console  string `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console (owns the lock). Optional — falls back to the binary's compile-time default."`
}

func (c *ScaleListCmd) Run() error {
	view, err := core.DescribeLockAt("scale list", c.Console, c.LockPath)
	if err != nil {
		return err
	}
	reps := view.GetReplicas()
	if len(reps) == 0 {
		fmt.Fprintln(core.Stdout, "no replica counts declared (every service runs a single replica)")
		return nil
	}
	for _, r := range reps {
		note := ""
		if isStorageBundle(r.GetService()) && r.GetCount() > 1 {
			note = "  [proxy]"
		}
		fmt.Fprintf(core.Stdout, "%s → %d%s\n", r.GetService(), r.GetCount(), note)
	}
	return nil
}

// ScaleUnsetCmd implements `w17ctl target scale unset <service>`.
type ScaleUnsetCmd struct {
	Service  string `arg:"" help:"Service whose replica-count entry to remove (revert to single replica)."`
	LockPath string `name:"lock" placeholder:"PATH" default:"w17/lock.yaml" help:"Path to the lock file."`
	Console  string `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console (owns the lock). Optional — falls back to the binary's compile-time default."`
}

func (c *ScaleUnsetCmd) Run() error {
	if err := core.EditLockOnDisk("scale unset", c.Console, c.LockPath, &codegenpb.LockEditIntent{
		Intent: &codegenpb.LockEditIntent_UnsetReplicas{
			UnsetReplicas: &codegenpb.UnsetReplicasIntent{Service: c.Service},
		},
	}); err != nil {
		return err
	}
	fmt.Fprintf(core.Stdout, "scale unset: %s reverted to single replica (%s)\n", c.Service, c.LockPath)
	return nil
}

// isStorageBundle reports whether a deployable bundle name is a
// standalone storage tier (`<domain>-storage`) — the only bundle kind
// that materializes the storage gRPC proxy when scaled past 1. A
// composed `<domain>-server` folds storage in-process and reaches it by
// direct call, so it never fronts the proxy even when scaled. The
// "-storage" suffix is a stable display convention (no lockpb dependency).
func isStorageBundle(service string) bool {
	const suffix = "-storage"
	return len(service) > len(suffix) && service[len(service)-len(suffix):] == suffix
}
