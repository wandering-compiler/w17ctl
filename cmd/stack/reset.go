package stack

import (
	"context"
	"fmt"

	"github.com/wandering-compiler/w17ctl/internal/autosync"
	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/schema"
	"github.com/wandering-compiler/w17ctl/internal/storageclient"
)

// ResetCmd is `w17ctl stack reset` — the one recovery from a tangled local dev
// DB state (a wiped volume, a checkpoint out of sync with the store, the
// first-build db/init collision). It reconciles the two halves of the dev DB
// lifecycle so the next `stack build` is a clean no-op:
//
//  1. wipe the local stores  — docker compose down -v (drops all volumes);
//  2. bring the stack up      — a fresh volume re-applies db/init's full
//     current schema via docker-entrypoint-initdb.d;
//  3. adopt the checkpoint    — record the CURRENT schema as the initiative's
//     checkpoint WITHOUT applying any DDL. Safe precisely because step 2 just
//     re-created that schema from db/init, so the live store already equals
//     current — re-applying would collide, adopting cannot.
//
// This is the deliberate escape hatch: any local DB tangle is always
// recoverable through w17ctl, never a hand-run psql. It assumes the generated
// db/init is current (run `w17ctl codegen` first if the protos changed) — it
// adopts the current protos as the baseline. Fixture seed data (e.g. a turnkey
// acl_roles admin role) is a separate step: `w17ctl fixtures apply`.
type ResetCmd struct {
	Protos          []string `name:"proto" short:"p" placeholder:"PROTO" help:"Model proto(s) whose schema to adopt as the checkpoint baseline. Empty = auto-discover the (w17.db.table) protos under the project's proto dir (same as 'stack build')."`
	Imports         []string `name:"import" short:"I" placeholder:"DIR" help:"Additional proto import path. Repeatable."`
	Project         string   `name:"project" placeholder:"ID" help:"Project id. Empty = read from the lock."`
	Console         string   `name:"console" placeholder:"HOST:PORT" env:"CONSOLE_STORAGE_ADDR" help:"Console storage endpoint (holds the checkpoints). Defaults to the logged-in console, else the compiled-in default."`
	By              string   `name:"by" placeholder:"ACTOR" help:"Actor stamp (checkpoint user_id). Empty = local OS user."`
	CompilerVersion string   `name:"compiler-version" placeholder:"VER" default:"dev" help:"Compiler version pinned into the adopted checkpoint."`
	KeepData        bool     `name:"keep-data" help:"Skip the volume wipe (down --keep-volumes) — only re-adopt the checkpoint against the existing stores. Use when the stores are fine but the checkpoint drifted."`
}

func (c *ResetCmd) Run() error {
	root, err := core.FindProjectRoot()
	if err != nil {
		return err
	}

	// 1. Wipe local stores (drop volumes) unless --keep-data.
	if c.KeepData {
		fmt.Fprintln(core.Stdout, "stack reset: --keep-data — skipping volume wipe; re-adopting checkpoint only.")
	} else {
		// Wipe the stores (drop volumes) via the mode-aware DownCmd so remote
		// mode wipes the REMOTE volumes (and tears its tunnels), not local
		// ones. DownCmd drops volumes by default (-v). UpCmd then re-applies
		// db/init on a fresh volume — also mode-aware. Adopt (step 3) records
		// the checkpoint on the CONSOLE (not the project DB), so it is
		// mode-agnostic.
		fmt.Fprintln(core.Stdout, "stack reset: wiping stores (compose down -v)…")
		if err := (&DownCmd{}).Run(); err != nil {
			return fmt.Errorf("stack reset: down -v: %w", err)
		}
		// 2. Bring the stack back up — a fresh volume re-applies db/init (the
		//    full current schema) via docker-entrypoint-initdb.d.
		fmt.Fprintln(core.Stdout, "stack reset: bringing the stack up (fresh volume re-applies db/init)…")
		if err := (&UpCmd{}).Run(); err != nil {
			return fmt.Errorf("stack reset: up: %w", err)
		}
	}

	// 3. Adopt: record the current schema as the checkpoint baseline WITHOUT
	//    applying DDL. db/init just created it, so the live store already
	//    equals current — the next 'stack build' diffs current→current = no-op.
	bc := &BuildCmd{Protos: c.Protos, Imports: c.Imports, Console: c.Console}
	protos, imports, cleanup, err := bc.resolveProtos(root)
	if err != nil {
		return fmt.Errorf("stack reset: %w", err)
	}
	defer cleanup()
	if len(protos) == 0 {
		fmt.Fprintln(core.Stdout, "stack reset: no model protos found — stores reset; nothing to adopt (no relational schema).")
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120e9)
	defer cancel()
	currentBytes, err := schema.LoadIRBytes(ctx, protos, imports, c.Console)
	if err != nil {
		return fmt.Errorf("stack reset: load current schema: %w", err)
	}

	project, err := storageclient.ResolveProjectID(c.Project)
	if err != nil {
		return err
	}
	actor := c.By
	if actor == "" {
		actor = storageclient.SelfActor()
	}
	initiative, _, err := autosync.ResolveActiveInitiative(root)
	if err != nil {
		return err
	}

	sc, err := storageclient.DialStorageFn(c.Console)
	if err != nil {
		return err
	}
	defer sc.Close()

	if err := adoptCheckpoint(sc, project, actor, initiative, currentBytes, c.CompilerVersion); err != nil {
		return fmt.Errorf("stack reset: adopt checkpoint: %w", err)
	}
	fmt.Fprintf(core.Stdout,
		"stack reset: done — stores fresh at the current schema, checkpoint adopted (%s/%s). Next 'stack build' is a clean no-op.\n"+
			"  seed data (e.g. acl_roles admin role) is separate: w17ctl push, then w17ctl fixtures apply.\n",
		initiative, actor)
	return nil
}
