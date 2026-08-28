package migrate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"google.golang.org/grpc"

	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/docker"
	"github.com/wandering-compiler/w17ctl/internal/schema"
	w17registrypb "github.com/wandering-compiler/sdk/go/pb/w17registry"
)

// Cmd is the single `w17ctl migrate` parent — every
// migration-related verb lives here:
//
//	migrate generate   — compile the proto schema + push it to the
//	                     console, which plans + stores the SQL
//	                     migrations and pins the lock target
//	migrate list       — browse stored migration history (console)
//	migrate push-raw   — push a hand-authored YAML body (escape hatch)
//	migrate reset      — DEV-ONLY destructive recreate: drop the DB,
//	                     discard the console's migration history, and
//	                     push a fresh baseline
//	migrate verify-history — ask whether the recorded history reproduces
//	                     the stored schema (the clean-gate; read-only)
//	migrate squash     — freeze a change request with its migrations:
//	                     collapse the history into one baseline the
//	                     collapsed rows point at (they are KEPT, which
//	                     is what separates this from reset)
//
// The console is the single source of truth: it diffs the schema and
// emits the migrations. The old offline migrator tools (a local SQL
// `generate`, `verify`, `inspect`) are gone — there is no offline
// migration pipeline; `migrate generate` always goes through the
// console.
type Cmd struct {
	Generate      GenerateCmd      `cmd:"" help:"Compile the proto schema and push it to the console, which plans + stores the SQL migrations and pins the lock target. Auto-detects initial (create) vs revision (update + diff); --initial forces the initial push."`
	List          ListCmd          `cmd:"" help:"List migrations stored on the console for a project. Optional connection-name filter + pagination."`
	PushRaw       PushRawCmd       `cmd:"" name:"push-raw" help:"Push a hand-authored YAML data migration body to console (escape hatch for TRANSFORM_FIELD + complex transitions)."`
	Fetch         FetchCmd         `cmd:"" help:"Download every migration up to each connection's pinned target from the console, ready for apply. Online step; everything after 'apply' is offline."`
	Apply         ApplyCmd         `cmd:"" help:"Apply pending migrations to each connection's target store (DSN via W17_TARGET_<CONN> env). Offline; reads on-disk artifacts + the DB-side wc_migrations table."`
	Adopt         AdoptCmd         `cmd:"" help:"Bring a database that ALREADY has the schema under migration management: record every migration up to the pinned target without running its DDL. Refuses unless the database proves it holds what those migrations introduce, and refuses outright if it is already managed. Offline; DSN via W17_TARGET_<CONN> env."`
	Rollback      RollbackCmd      `cmd:"" help:"Roll back applied migrations newer than --to, in reverse, against each connection's target store. Offline; DSN via W17_TARGET_<CONN> env."`
	Status        StatusCmd        `cmd:"" help:"Show each connection's pinned target migration + how many fetched artifacts are on disk. Offline, read-only."`
	Reset         ResetCmd         `cmd:"" help:"DEV-ONLY destructive recreate (pre-prod): drop the local DB volume, discard the project's migration history on the console, and derive a fresh baseline from the proto. ⚠️ Loses ALL data + every hand-authored data migration; not stage-gated."`
	VerifyHistory VerifyHistoryCmd `cmd:"" name:"verify-history" help:"Ask the console whether this project's recorded migration history reproduces its stored schema — the clean-gate. Read-only. Answers consistent / drifted / unknown; unknown means no evidence (a project pushed before revisions were recorded), not drift."`
	Squash        SquashCmd        `cmd:"" help:"Freeze a change request together with its migrations: collapse the project's history into one baseline the collapsed migrations point at. Unlike reset the rows are KEPT, and a deployment already at the old head records the baseline without running it."`
}

// GenerateCmd backs `w17ctl migrate generate` — the compiled-IR
// push to the console (formerly `w17ctl schema create/update`). It
// auto-detects create vs update by probing whether the project already
// has a stored schema, so the caller never picks the mode by hand;
// --initial forces the initial (create) push. The push machinery lives
// in internal/schema (RunSchemaPush).
type GenerateCmd struct {
	Protos     []string `name:"proto" short:"p" placeholder:"PROTO" required:"" help:"Path to a .proto schema. Repeatable — multi-file schemas pass each file as its own --proto flag."`
	Imports    []string `name:"import" short:"I" placeholder:"DIR" help:"Additional proto import path. Repeatable. Each --proto's directory is always included; this flag points at the w17/*.proto vocabulary + project-shared trees."`
	ProjectID  string   `name:"project" placeholder:"ID" required:"" help:"Project identifier matching the consuming repo's w17/lock.yaml project_id."`
	Console    string   `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console MigrationRegistry. Optional — falls back to console_addr in w17/lock.yaml, then to the binary's compile-time default."`
	LockPath   string   `name:"lock" placeholder:"PATH" default:"w17/lock.yaml" help:"Path to the lock file. Created/updated with target_migration_id pinned to each connection's latest stored migration after a successful push."`
	NoLock     bool     `name:"no-lock" help:"Skip the lock-file write (advanced/script use; the default writes the lock so the consuming repo commit can pin the deploy target)."`
	Initial    bool     `name:"initial" help:"Force initial-schema (create) mode; errors if the project already has a schema. Default auto-detects: create when no schema exists yet, else update + diff against the stored revision."`
	Decide     []string `name:"decide" placeholder:"DECISION" help:"Resolve a NEEDS_CONFIRM finding so the push isn't blocked. Form: <table>.<col>[:<axis>]=<strategy> (e.g. users.age=drop_and_create) or =custom:<sql-file> for a CUSTOM_MIGRATION body. Repeatable; the console parses + applies them."`
	Initiative string   `name:"initiative" placeholder:"ID" help:"Change-request (initiative) id these migrations belong to. Empty = the initiative of the current git branch, the same one 'w17ctl initiative current' shows. What 'w17ctl migrate squash --initiative' later collapses."`
}

func (c *GenerateCmd) Run() error {
	return schema.RunSchemaPush(schema.SchemaPushArgs{
		Protos: c.Protos, Imports: c.Imports, ProjectID: c.ProjectID, Console: c.Console,
		LockPath: c.LockPath, NoLock: c.NoLock,
		Mode:         schema.SchemaPushAuto,
		ForceInitial: c.Initial,
		Decide:       c.Decide,
		Initiative:   c.Initiative,
	})
}

// ====================================================================
// `w17ctl migrate reset` — recreate (docs/decisions/lifecycle-processes-decomposition.md)
// ====================================================================
//
// The productized answer to "I'm pre-prod and want to make a breaking
// schema change (e.g. add a required NOT-NULL column) without
// authoring a data-preserving migration." The differ would otherwise
// flag it (engine/risk.go) and demand a `--decide` body; pre-prod, the
// right escape is to blow the schema away and regenerate a fresh
// baseline from the proto.
//
// The command has always been specified as four steps. Three of them
// now exist:
//
//  1. Stage-gate. Refuse once the project is `prod` — reset is a
//     pre-prod-only escape. STILL MISSING: the consuming project's
//     stage isn't carried on the lock, so this is caller discipline.
//     See the TODO below.
//  2. Drop the DB. Today that is the local throwaway-compose case —
//     `docker compose down -v` from the project root. It cannot touch
//     a managed / shared DB; against one, do the drop yourself and
//     pass --local-only=false with a reachable console.
//  3. Clear the migration ledger — `ResetHistory(DISCARD)` on the
//     console's ProjectRegistry. The registry forgets the project's
//     migrations AND its stored schema, and reports the raw
//     (hand-authored) bodies it dropped, because those are the ones no
//     amount of re-deriving from the schema brings back.
//  4. Regenerate a fresh baseline. NOT a second code path: with the
//     history gone, an ordinary push takes its bootstrap (create)
//     route, which is exactly what a project's first push does. Pass
//     --proto and this command runs it for you.
//
// Why the DB drop runs BEFORE the ledger reset, matching the step
// order: a dropped DB with its history intact is a consistent project
// (replaying the history rebuilds it), whereas a forgotten history
// over a live DB is not. Everything checkable — the project id, the
// console address, a round-trip to the console — is verified BEFORE
// the drop, so an unreachable console refuses while the data is still
// there.
//
// TODO(stage): add the step-1 gate once the lock carries a project
// stage. Until then `--force` is the only thing standing between this
// command and a production database.

// ResetCmd implements `w17ctl migrate reset`.
type ResetCmd struct {
	Force     bool     `name:"force" short:"f" help:"Acknowledge the destructive action (drops the DB volume + the console's migration history — ALL data lost, hand-authored data migrations included). Required: without it the command refuses."`
	Up        bool     `name:"up" help:"After tearing down, bring the stack back up (docker compose up -d) so you land on a clean, empty, running slate."`
	ProjectID string   `name:"project" placeholder:"ID" help:"Project whose migration history is discarded. Empty = read project_id from the lock."`
	Console   string   `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console ProjectRegistry. Optional — falls back to console_addr in w17/lock.yaml, then to the binary's compile-time default."`
	Protos    []string `name:"proto" short:"p" placeholder:"PROTO" help:"Path to a .proto schema. Repeatable. Supplied = the fresh baseline is pushed for you (step 4); omitted = the command stops after the reset and tells you to run 'migrate generate --initial'."`
	Imports   []string `name:"import" short:"I" placeholder:"DIR" help:"Additional proto import path for the baseline push. Repeatable. Only read when --proto is given."`
	LockPath  string   `name:"lock" placeholder:"PATH" default:"w17/lock.yaml" help:"Path to the lock file, re-pinned by the baseline push. Only read when --proto is given."`
	NoLock    bool     `name:"no-lock" help:"Skip the lock-file write on the baseline push. Only read when --proto is given."`
	LocalOnly bool     `name:"local-only" help:"Tear the local stack + DB volume down WITHOUT touching the console's migration history. The dev-loop escape from before the reset RPC existed — the project keeps its history, so the next apply replays it onto the empty DB."`
}

// docker.RunComposeFn execs `docker compose <args...>` in dir. Package var
// so tests inject a recorder instead of shelling out.

// docker.RunComposeEnvFn is the env-aware compose runner used by `stack up` to
// inject w17ctl's per-machine host-port overrides (and a preset's extra
// env) into the subprocess without touching any .env file. Tests
// override it to capture the env + args.

func (c *ResetCmd) Run() error {
	root, err := core.FindProjectRoot()
	if err != nil {
		return err
	}

	// Loud banner naming everything that dies. It runs before the
	// --force check on purpose: someone who typed the command without
	// the flag should still read what it would have done.
	fmt.Fprintln(core.Stdout, "⚠️  w17ctl migrate reset is DESTRUCTIVE and DEV-ONLY. It loses:")
	fmt.Fprintln(core.Stdout, "      • the local DB volume — ALL data in it (docker compose down -v)")
	if !c.LocalOnly {
		fmt.Fprintln(core.Stdout, "      • the project's migration history on the console, its stored schema with it")
		fmt.Fprintln(core.Stdout, "      • every hand-authored data migration (migrate push-raw) — those bodies are")
		fmt.Fprintln(core.Stdout, "        author-owned and CANNOT be re-derived from the schema; they are counted")
		fmt.Fprintln(core.Stdout, "        and named below before they go")
		fmt.Fprintln(core.Stdout, "      • the ACL permission-id allocation — retired ids are re-handed out, so any")
		fmt.Fprintln(core.Stdout, "        grant issued against the old lock now points somewhere else")
	}
	fmt.Fprintln(core.Stdout, "    There is no project-stage gate yet: nothing here checks that this is not prod.")
	fmt.Fprintln(core.Stdout)

	if !c.Force {
		return fmt.Errorf("refusing: `migrate reset` drops the DB volume and the console's migration history, and loses ALL data — pass --force to confirm")
	}

	// The project's primary launcher is the root compose.yaml (the
	// teardown is `docker compose down -v`, no -f, default file
	// resolution). Guard on its presence so we
	// fail clearly outside a generated project rather than running a
	// stray compose.
	if _, statErr := os.Stat(filepath.Join(root, "compose.yaml")); statErr != nil {
		return fmt.Errorf("no compose.yaml at project root %s — `migrate reset` only handles the local compose dev loop today (see `migrate reset --help`)", root)
	}

	// --- preflight (before anything is destroyed) --------------------
	//
	// Resolve and CONNECT first. grpc.NewClient is lazy, so a dial alone
	// proves nothing; the ListMigrations round-trip is what proves the
	// console is reachable, the bearer is accepted, and the project is
	// in the caller's org. Doing it after the teardown would mean losing
	// the DB to a reset that was never going to run.
	var registry projectRegistryResetter
	var projectID string
	if !c.LocalOnly {
		projectID = c.ProjectID
		if projectID == "" {
			projectID = core.LockProjectIDBestEffort()
		}
		if projectID == "" {
			return fmt.Errorf("no project id: pass --project, or run inside a project whose lock carries project_id — pass --local-only to tear the stack down WITHOUT resetting the history")
		}

		addr, addrErr := core.ResolveConsoleAddr(c.Console)
		if addrErr != nil {
			return addrErr
		}
		cl, conn, dialErr := core.DialProjectRegistry(addr)
		if dialErr != nil {
			return fmt.Errorf("connect %s: %w", addr, dialErr)
		}
		defer func() { _ = conn.Close() }()

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, probeErr := cl.ListMigrations(ctx, &w17registrypb.ListMigrationsRequest{ProjectId: projectID}); probeErr != nil {
			return fmt.Errorf("console %s is not answering for project %s — nothing was destroyed: %w", addr, projectID, probeErr)
		}
		registry = cl
	}

	// --- step 2: drop the DB ----------------------------------------
	fmt.Fprintln(core.Stdout, "tearing down: docker compose down -v")
	if err := docker.RunComposeFn(root, "down", "-v"); err != nil {
		return fmt.Errorf("docker compose down -v: %w", err)
	}

	// --- step 3: forget the history ---------------------------------
	if registry != nil {
		if err := resetHistory(registry, projectID); err != nil {
			return err
		}
	}

	if c.Up {
		fmt.Fprintln(core.Stdout, "bringing stack back up: docker compose up -d")
		if err := docker.RunComposeFn(root, "up", "-d"); err != nil {
			return fmt.Errorf("docker compose up -d: %w", err)
		}
	}

	// --- step 4: the fresh baseline ---------------------------------
	if c.LocalOnly {
		fmt.Fprintln(core.Stdout, "done (--local-only: the console's migration history is UNTOUCHED).")
		fmt.Fprintln(core.Stdout, "Next: re-run migrate fetch + apply to replay the existing history onto the empty DB.")
		return nil
	}
	if len(c.Protos) == 0 {
		fmt.Fprintln(core.Stdout, "done. Next: `w17ctl migrate generate --initial --proto <file>` to derive the fresh")
		fmt.Fprintln(core.Stdout, "baseline, then migrate fetch + apply it.")
		return nil
	}

	fmt.Fprintln(core.Stdout, "pushing the fresh baseline")
	// --initial (ForceInitial) rather than letting AUTO probe: after the
	// reset the probe would resolve to create anyway, so asking for it
	// explicitly costs nothing and buys a post-condition — the console
	// refuses an initial push for a project that still has a schema, so
	// this push failing means the reset did not actually clear it.
	if err := schema.RunSchemaPush(schema.SchemaPushArgs{
		Protos: c.Protos, Imports: c.Imports, ProjectID: projectID, Console: c.Console,
		LockPath: c.LockPath, NoLock: c.NoLock,
		Mode:         schema.SchemaPushAuto,
		ForceInitial: true,
	}); err != nil {
		return fmt.Errorf("baseline push after reset: %w", err)
	}

	fmt.Fprintln(core.Stdout, "done. Next: migrate fetch + apply the new baseline, then re-seed.")
	return nil
}

// projectRegistryResetter is the slice of the ProjectRegistry client the
// reset path uses. Narrowed to two methods so a test can stand one up
// without implementing the whole service.
type projectRegistryResetter interface {
	ListMigrations(ctx context.Context, in *w17registrypb.ListMigrationsRequest, opts ...grpc.CallOption) (*w17registrypb.ListMigrationsResponse, error)
	ResetHistory(ctx context.Context, in *w17registrypb.ResetHistoryRequest, opts ...grpc.CallOption) (*w17registrypb.ResetHistoryResponse, error)
}

// resetHistory runs step 3 and reports what it dropped. The raw bodies are
// NAMED, not just counted: they are the part of a history that cannot be
// re-derived, so an operator has to be able to go find them in version
// control rather than learn the count and nothing else.
func resetHistory(cl projectRegistryResetter, projectID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := cl.ResetHistory(ctx, &w17registrypb.ResetHistoryRequest{
		ProjectId: projectID,
		Policy:    w17registrypb.ResetHistoryPolicy_RESET_HISTORY_POLICY_DISCARD,
		// The console refuses without this. --force is what the operator
		// acknowledged with; this is that acknowledgement crossing the
		// wire, and the code path never reaches here without it.
		AcknowledgeDataLoss: true,
	})
	if err != nil {
		// The DB volume is already gone at this point, so say so — the
		// project is now a live schema-less database against an intact
		// history, and the operator has to know which half ran.
		return fmt.Errorf("reset history for project %s (the DB volume is ALREADY dropped; the console still holds the old history, so re-run this command once the console answers): %w", projectID, err)
	}

	fmt.Fprintf(core.Stdout, "history reset: dropped %d migration(s), %d of them hand-authored\n",
		resp.GetMigrationsDropped(), resp.GetRawMigrationsDropped())
	for _, id := range resp.GetRawMigrationIds() {
		fmt.Fprintf(core.Stdout, "    gone (not re-derivable): %s\n", id)
	}
	if resp.GetRawMigrationsDropped() > 0 {
		fmt.Fprintln(core.Stdout, "    ^ re-push these by hand (`migrate push-raw`) if the new baseline still needs them.")
	}
	return nil
}
