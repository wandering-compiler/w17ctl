package stack

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/wandering-compiler/w17ctl/internal/autosync"
	plan "github.com/wandering-compiler/w17ctl/internal/plan"
	"github.com/wandering-compiler/w17ctl/internal/schema"
	"github.com/wandering-compiler/w17ctl/internal/snapstore"
	"github.com/wandering-compiler/w17ctl/internal/storageclient"

	codegen "github.com/wandering-compiler/w17ctl/internal/codegen"
	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/devconfig"
	"github.com/wandering-compiler/w17ctl/internal/docker"
	"github.com/wandering-compiler/w17ctl/internal/protoscan"
	"github.com/wandering-compiler/w17ctl/internal/reconcile"
	"github.com/wandering-compiler/w17ctl/internal/remotecompose"
	"github.com/wandering-compiler/sdk/go/tooling/migrate"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/factory"
)

// BuildCmd is `w17ctl stack build` — compile + build images, then **dev
// diff-apply** (docs/specs/storage/dev-db-lifecycle.md S5/S6): bring the
// developer's local stores up to the current branch proto by applying
// checkpoint→current directly, persisting no migration. No `up`.
//
// The dev diff-apply step runs only when both `--proto` and `--target`
// are supplied (the local store DSNs to apply to) — otherwise build is
// image-only and prints a one-line skip note. Zero-flag auto-resolution
// of protos + local DSNs from the running stack is a documented
// follow-up; today the explicit flags mirror `migrate generate`.
type BuildCmd struct {
	Services []string `arg:"" optional:"" help:"Services to build; empty = the whole stack."`

	Protos          []string `name:"proto" short:"p" placeholder:"PROTO" help:"Model proto file(s) to compile into the current IR for dev diff-apply. Repeatable. Empty = auto-discover the model protos (those declaring (w17.db.table)) under the project's proto dir."`
	Imports         []string `name:"import" short:"I" placeholder:"DIR" help:"IGNORED — the console compiles the IR and resolves imports from the uploaded proto tree. Kept so existing scripts do not break; it warns."`
	Targets         []string `name:"target" short:"t" placeholder:"CONN=DSN" help:"Local store target(s) to dev diff-apply to, <connection>=<dsn>. Repeatable. Empty = auto-resolved from the lock's connections + the host ports 'stack up' published."`
	Project         string   `name:"project" placeholder:"ID" help:"Project id. Empty = read from the current project's lock."`
	CompilerVersion string   `name:"compiler-version" placeholder:"VER" default:"dev" help:"Compiler version pinned into the advanced checkpoint."`
	By              string   `name:"by" placeholder:"ACTOR" help:"Actor stamp (checkpoint user_id). Empty = local OS user."`
	Console         string   `name:"console" placeholder:"HOST:PORT" env:"CONSOLE_STORAGE_ADDR" help:"Console storage endpoint (holds the checkpoints). Defaults to the logged-in console (w17ctl login), else the compiled-in default."`
	Reconcile       bool     `name:"reconcile" help:"Force the branch-switch reconcile even when the project's autosync mode is off. (When on — the default — reconcile already runs on an initiative change.)"`
	NoCodegen       bool     `name:"no-codegen" help:"Skip the codegen step (assume the generated code is already current). By default 'stack build' runs codegen first so the images compile against fresh generated code."`
	Lossy           string   `name:"lossy" default:"refuse" help:"What to do when the sync would DESTROY data (drop a table or column, retype one): refuse | apply | snapshot. snapshot takes one of the affected stores first."`
	NoBuild         bool     `name:"no-build" help:"Skip building images and sync the local database only. The schema sync and the image build are independent steps that happen to share this command; with this flag the sync needs no Docker daemon, no build context and no compose file at all. Use it when you changed a proto and want the database to match."`
	modeFlags

	// cc is the compose control reconcile's Quiesce uses to stop non-store
	// services. Set to the remote runner in remote mode (Run); nil ⇒
	// buildReconcileDeps defaults to the local daemon.
	cc composeCtl

	// root pins the project directory instead of discovering it from the
	// working directory. Set by SyncStores, whose caller already knows which
	// project it is running and does not necessarily stand in it.
	root string
}

// runCodegenFn regenerates all derived code (the codegen step `stack
// build` runs before compiling images). A package var so the build's
// tests can stub it. Force overwrites the existing git-ignored generated
// tree; CodegenCmd resolves its own console address from the lock/env.
var runCodegenFn = func() error {
	return codegen.Run("", true, false)
}

func (c *BuildCmd) Run() error {
	root := c.root
	if root == "" {
		var err error
		if root, err = core.FindProjectRoot(); err != nil {
			return err
		}
	}
	// Regenerate code so the images compile against fresh generated code
	// (codegen is deterministic — a no-op in effect when the proto is
	// unchanged). `codegen` also stays a standalone command for the
	// proto-only / no-build edits. --no-codegen skips it.
	if !c.NoCodegen {
		if err := runCodegenFn(); err != nil {
			return fmt.Errorf("stack build: codegen: %w", err)
		}
	}

	// Resolve the mode. Remote build pushes the freshly-generated tree to
	// the server and compiles there (remote-local, fast incremental); the
	// dev diff-apply then runs against the REMOTE stores through a
	// transient DB tunnel (Slice 6). The diff-apply TAIL is identical for
	// both modes — only the image build + DB reachability differ.
	mode, cfg, err := c.resolveMode(root)
	if err != nil {
		return err
	}
	// applyWrap runs the dev diff-apply with the stores reachable. Local:
	// pass-through (already local). Remote: image build over SSH + wrap the
	// apply in a transient DB tunnel.
	applyWrap := func(fn func() error) error { return fn() }
	if mode == devconfig.ModeRemote {
		name, err := projectNameFromLock(root)
		if err != nil {
			return err
		}
		tgt, err := c.resolveRemote(cfg, root, name)
		if err != nil {
			return err
		}
		if err := syncTree(root, tgt, name); err != nil {
			return fmt.Errorf("stack build: rsync: %w", err)
		}
		if err := remotecompose.Run(tgt.Runner, nil, append([]string{"build"}, c.Services...)...); err != nil {
			return fmt.Errorf("stack build: remote compose build: %w", err)
		}
		var ports map[string]int
		if _, p := cfg.FindByPath(root); p != nil {
			ports = p.Ports
		}
		applyWrap = func(fn func() error) error { return withRemoteDB(name, tgt.Dest, ports, fn) }
		// Reconcile's Quiesce must stop the REMOTE non-store services (over
		// SSH), not local ones.
		c.cc = remoteComposeCtl(tgt.Runner)
	} else if !c.NoBuild {
		// Compile Go + build images locally.
		if err := docker.RunComposeFn(root, append(append(docker.FileArgs(root), "build"), c.Services...)...); err != nil {
			return fmt.Errorf("stack build: compose build: %w", err)
		}
	}
	// --no-build stops HERE and falls through to the diff-apply. The two steps
	// are independent and only share a command: building images has nothing to
	// do with reconciling a schema, and a consumer who asked for the second
	// could not reach it because the first failed on a compose file that was
	// none of w17's business. Reported as the half that mattered most of the
	// three they asked for.

	return c.diffApplyTail(root, applyWrap)
}

// diffApplyTail resolves the dev-diff-apply inputs (model protos + local
// store targets, both auto-resolved so the zero-flag build just works) and
// applies the current schema through applyWrap — the DB-reachability
// wrapper (a no-op locally, a transient tunnel remotely). It short-circuits
// with a skip note when there is nothing to apply, WITHOUT opening a tunnel.
func (c *BuildCmd) diffApplyTail(root string, applyWrap func(func() error) error) error {
	protos, imports, cleanup, err := c.resolveProtos(root)
	if err != nil {
		return err
	}
	defer cleanup()
	if len(protos) == 0 {
		fmt.Fprintln(core.Stdout, "stack build: images built; dev diff-apply skipped (no model protos found under the proto dir; pass --proto)")
		return nil
	}
	specs, err := c.ResolveTargets(root)
	if err != nil {
		return err
	}
	if len(specs) == 0 {
		fmt.Fprintln(core.Stdout, "stack build: images built; dev diff-apply skipped (no resolvable local store targets — run 'stack up' first, or pass --target)")
		return nil
	}
	return applyWrap(func() error { return c.devDiffApply(root, specs, protos, imports) })
}

// resolveProtos returns the model protos + import paths for the IR
// build: explicit --proto wins; otherwise it auto-discovers the model
// protos (the `.proto` files under the project's proto dir that declare
// a `(w17.db.table)` — query/mutation/service protos don't) and stages
// the embedded w17 vocabulary as an import so the build resolves
// `w17/*.proto` standalone. The returned cleanup removes the staged
// vocab (no-op for the explicit path).
func (c *BuildCmd) resolveProtos(root string) (protos, imports []string, cleanup func(), err error) {
	cleanup = func() {}
	if len(c.Protos) > 0 {
		return c.Protos, c.Imports, cleanup, nil
	}
	// proto dir from the console's lock projection (best-effort: a lock-less /
	// console-down project falls back to the conventional "proto").
	protoDir := "proto"
	if view := core.DescribeLockBestEffort(c.Console); view != nil && view.GetProtoDir() != "" {
		protoDir = view.GetProtoDir()
	}
	base := filepath.Join(root, protoDir)
	models, modules, err := protoscan.DiscoverModelProtos(base)
	if err != nil {
		return nil, nil, cleanup, fmt.Errorf("stack build: discover model protos: %w", err)
	}
	if len(models) == 0 {
		return nil, nil, cleanup, nil
	}
	// Connection-declaring modules ride along: the IR build resolves every
	// `(w17.field).upload.connection` against the registry it assembles from
	// the files it is handed, and a KV/LOCAL_FS module declares no table, so
	// the model walk alone leaves its connection looking undeclared. Appended
	// AFTER the emptiness check so they can never make a table-less project
	// diff-apply an empty schema.
	models = append(models, modules...)
	// ⚠️ No vocabulary is staged here any more, and no import roots are
	// computed, because the CONSOLE resolves both.
	//
	// The IR compile moved server-side (`CompileIR`): the client uploads the
	// project's proto tree and the console loads `w17/*.proto` from its own
	// proto root — `schema.compileIRBytesViaConsole` says so in as many words,
	// and ignores the `imports` argument entirely. What was left behind was
	// the client extracting 576 KB of embedded vocabulary to a temp directory
	// on every run, to build a list nothing read
	// (docs/decisions/client-carries-compiler-payloads.md §2).
	return models, nil, cleanup, nil
}

// restoreFlags are the flags the printed restore command needs to reach the
// same stores and the same console THIS run was pointed at.
//
// A line telling you to run a command has to work when you run it. A sync
// driven with `--target` is talking to a database the dev registry does not
// know about, so a bare `db snapshot activate <name>` answers "no resolvable
// local store targets" — the second way this same line has sent somebody
// somewhere that does not work.
func (c *BuildCmd) restoreFlags() string {
	var b strings.Builder
	for _, t := range c.Targets {
		fmt.Fprintf(&b, " --target %s", t)
	}
	if c.Console != "" {
		fmt.Fprintf(&b, " --console %s", c.Console)
	}
	return b.String()
}

// schemaHashOf is the hash the checkpoint will carry once this sync lands —
// the same one adoptCheckpoint records, so a savepoint's "way back from" and
// the initiative's later "current" are the same string or the match never
// fires.
func schemaHashOf(currentBytes []byte) string {
	sum := sha256.Sum256(currentBytes)
	return hex.EncodeToString(sum[:])
}

// checkpointLockHash is the schema lineage a savepoint taken right now
// belongs to — the checkpoint's own hash, read BEFORE this sync advances it.
//
// Empty when the console cannot be reached or the initiative has no
// checkpoint yet. A savepoint carrying no hash is still a savepoint; the
// listing says so rather than showing an empty slot, and `activate` simply
// cannot check it matches.
func checkpointLockHash(sc *storageclient.StorageClients, project, actor, initiative string) string {
	ckpt, err := sc.GetCheckpoint(project, actor, initiative)
	if err != nil || ckpt == nil {
		return ""
	}
	return ckpt.GetLockHash()
}

// lossyMode validates --lossy. An unknown value is refused rather than
// treated as the default: a typo in the flag that meant "apply" would
// otherwise silently refuse, and one that meant "refuse" would silently
// destroy.
func (c *BuildCmd) lossyMode() (string, error) {
	switch c.Lossy {
	case "", plan.LossyRefuse:
		return plan.LossyRefuse, nil
	case plan.LossyApply, plan.LossySnapshot:
		return c.Lossy, nil
	default:
		return "", fmt.Errorf("stack build: --lossy=%q is not one of: %s", c.Lossy, plan.ValidLossyModes())
	}
}

// snapshotFn snapshots the named stores before a destructive sync, into the
// same branch-scoped store `db snapshot` and the branch-switch reconcile use —
// so what it keeps is restorable by a command that already exists.
func (c *BuildCmd) snapshotFn(root string, specs []factory.TargetSpec, initiative, schemaHash, undoes string) func([]string) error {
	return func(conns []string) error {
		wanted := map[string]bool{}
		for _, c := range conns {
			wanted[c] = true
		}
		var mine []factory.TargetSpec
		for _, s := range specs {
			if wanted[s.Connection] {
				mine = append(mine, s)
			}
		}
		snapConns, skipped, err := SnapshotConns(mine)
		if err != nil {
			return err
		}
		for _, s := range skipped {
			fmt.Fprintf(core.Stdout, "stack build: snapshot skipping store %s\n", s)
		}
		if len(snapConns) == 0 {
			return fmt.Errorf("none of the stores that would lose data can be snapshotted (%s)", strings.Join(conns, ", "))
		}
		name := "before-lossy-sync-" + time.Now().UTC().Format("20060102T150405Z")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		// The schema this savepoint belongs to, so `db snapshot activate` can
		// warn when it is put back onto a different one. Empty was what
		// produced a listing reading `(schema )` — an empty slot nobody could
		// tell from a lost value (deinvo, 2026-09-19).
		//
		// It is the schema the database holds NOW, i.e. the checkpoint BEFORE
		// this sync advances it: the snapshot is of what is there, not of
		// what is about to be.
		// `undoes` is where this sync is GOING, so the savepoint knows which
		// change it is the way back from. Without it the consistency guard
		// refuses the return trip: the schema has moved by then, which is the
		// whole reason the savepoint was taken.
		if err := snapstore.New(root).SaveNamedUndoing(ctx, initiative, name, schemaHash, undoes, snapConns); err != nil {
			// A snapshot is taken by the database's OWN client tools, and a
			// machine without them cannot take one. Said plainly, because the
			// raw failure is `exec: "pg_dump": executable file not found` —
			// which reads like a bug in w17 rather than a missing package,
			// and arrives at the moment somebody chose the careful option.
			if strings.Contains(err.Error(), "executable file not found") {
				return fmt.Errorf("%w\n\n"+
					"  why: a snapshot is taken with the database's own client tools (pg_dump /\n"+
					"       mysqldump), and this machine does not have them\n"+
					"  fix: install them, or choose --lossy=apply having decided the data can go", err)
			}
			return err
		}
		// The verb is `activate`, and getting it wrong here costs more than
		// anywhere else: this line is read by somebody who has just destroyed
		// data and is copying the command out of it. `restore` was refused
		// with "unexpected argument", at the one moment nobody goes looking
		// through --help (deinvo, 2026-09-19).
		fmt.Fprintf(core.Stdout, "stack build: snapshot %q taken before applying (put it back: w17ctl db snapshot activate %s%s)\n",
			name, name, c.restoreFlags())
		return nil
	}
}

// SyncStores reconciles the given stores to the project's protos, through the
// console — the whole of `stack build --no-build`, with the targets supplied
// rather than resolved.
//
// Exported for `w17ctl test`, whose stack publishes its stores on ports it
// allocates itself, so the addresses are known to the caller and to nobody
// else. The reconcile is otherwise identical: compile the protos, ask the
// console for the plan against what those databases HOLD, apply it.
func SyncStores(root, console string, targets []factory.TargetSpec) error {
	cmd := &BuildCmd{NoBuild: true, NoCodegen: true, Console: console, root: root}
	for _, t := range targets {
		cmd.Targets = append(cmd.Targets, t.Connection+"="+t.DSN)
	}
	return cmd.Run()
}

// ResolveTargets returns the dev-diff-apply targets: explicit --target
// wins; otherwise they are auto-resolved from the lock's connections +
// the dev-machine port allocation (the same ports `stack up` publishes).
// Skipped connections are reported so the dev sees why a store was left
// untouched.
func (c *BuildCmd) ResolveTargets(root string) ([]factory.TargetSpec, error) {
	if len(c.Targets) > 0 {
		specs, err := factory.ParseTargets(c.Targets)
		if err != nil {
			return nil, fmt.Errorf("stack build: %w", err)
		}
		return specs, nil
	}
	cfg, err := core.LoadDevConfigFn()
	if err != nil {
		return nil, fmt.Errorf("stack build: load dev config: %w", err)
	}
	_, p := cfg.FindByPath(root)
	// Connection names from the console's lock projection (best-effort: a
	// lock-less / console-down project yields no auto-resolved targets).
	var connNames []string
	if view := core.DescribeLockBestEffort(c.Console); view != nil {
		for _, conn := range view.GetConnections() {
			connNames = append(connNames, conn.GetName())
		}
	}
	specs, skipped := resolveLocalTargets(connNames, p)
	for _, s := range skipped {
		fmt.Fprintf(core.Stdout, "stack build: skipping store %s\n", s)
	}
	return specs, nil
}

// devDiffApply runs the checkpoint→current dev diff against the local
// stores and advances the checkpoint.
func (c *BuildCmd) devDiffApply(root string, specs []factory.TargetSpec, protos, imports []string) error {
	project, err := storageclient.ResolveProjectID(c.Project)
	if err != nil {
		return err
	}
	// Resolve the initiative (the checkpoint + snapshot scope) and whether
	// to reconcile, from the workflow mode + state (no flag). branchFn==nil
	// ⇒ manual mode with no active initiative: a single "default"
	// initiative, no branch-switch reconcile (just keep diff-applying).
	initiative, branchFn, err := autosync.ResolveActiveInitiative(root)
	if err != nil {
		return err
	}
	actor := c.By
	if actor == "" {
		actor = storageclient.SelfActor()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120e9)
	defer cancel()
	currentBytes, err := schema.LoadIRBytes(ctx, protos, imports, c.Console)
	if err != nil {
		return fmt.Errorf("stack build: load current schema: %w", err)
	}

	applierFor := factory.FromTargets(specs)

	// Branch-switch reconcile: snapshot the outgoing initiative + restore/
	// rebuild the incoming one BEFORE the diff-apply converges to the
	// current proto. Runs when the mode resolved a branch source
	// (branch-driven, or explicit --initiative) or --reconcile forces it.
	// A no-op when the initiative is unchanged since the last build.
	if branchFn != nil || c.Reconcile {
		bf := branchFn
		if bf == nil {
			bf = storageclient.GitCurrentBranchFn // --reconcile forced in manual no-flag mode
		}
		deps, derr := buildReconcileDeps(root, c.cc, bf, currentBytes, applierFor, specs, c.Console)
		if derr != nil {
			return fmt.Errorf("stack build: reconcile setup: %w", derr)
		}
		if _, rerr := reconcile.Run(ctx, deps); rerr != nil {
			return fmt.Errorf("stack build: reconcile: %w", rerr)
		}
	}

	sc, err := storageclient.DialStorageFn(c.Console)
	if err != nil {
		return err
	}
	defer sc.Close()

	mode, err := c.lossyMode()
	if err != nil {
		return err
	}
	logf := func(format string, args ...any) { fmt.Fprintf(core.Stdout, format+"\n", args...) }
	if err := runDevDiffApplyLossy(sc, project, actor, initiative, currentBytes, applierFor,
		specConnections(specs), c.CompilerVersion, logf, mode,
		c.snapshotFn(root, specs, initiative,
			checkpointLockHash(sc, project, actor, initiative), schemaHashOf(currentBytes))); err != nil {
		return fmt.Errorf("stack build: dev diff-apply: %w", err)
	}
	fmt.Fprintf(core.Stdout, "stack build: dev diff-apply complete (%s/%s, checkpoint advanced)\n", initiative, actor)
	return nil
}

// runDevDiffApply is the dev DB lifecycle's per-build orchestration,
// extracted so it can be tested against a fake checkpoint store +
// applier:
//
//  1. read the stored checkpoint as the diff BASE (nil for a brand-new
//     initiative → full create);
//  2. devDiffApply(base → current): log destructive changes (compat over
//     the API), plan with the same engine review uses (planner over the
//     API), apply to the local stores;
//  3. advance the checkpoint to `current` ONLY on a clean apply (a
//     failed apply leaves the checkpoint at the last good state, so the
//     next build re-attempts the same diff).
func runDevDiffApply(sc *storageclient.StorageClients, project, actor, initiative string, currentBytes []byte, applierFor migrate.ApplierFor, conns []string, compilerVersion string, logf func(string, ...any)) error {
	return runDevDiffApplyLossy(sc, project, actor, initiative, currentBytes, applierFor, conns, compilerVersion, logf, plan.LossyApply, nil)
}

// runDevDiffApplyLossy is runDevDiffApply with the answer to a destructive
// plan, and the snapshot to take when the answer is to keep what it removes.
func runDevDiffApplyLossy(sc *storageclient.StorageClients, project, actor, initiative string, currentBytes []byte, applierFor migrate.ApplierFor, conns []string, compilerVersion string, logf func(string, ...any), lossyMode string, snapshot func([]string) error) error {
	// The checkpoint the console records carries the compiler that produced
	// it, and an empty one is refused by the column's own constraint. The
	// flag's default fills this in for a command line; a caller that BUILDS a
	// BuildCmd (`stack up`'s sync, `w17ctl test`'s) gets the zero value, and
	// the sync then failed after applying — the database changed, the record
	// of it refused. Defaulted here so no constructor can forget.
	if compilerVersion == "" {
		compilerVersion = "dev"
	}
	ckpt, err := sc.GetCheckpoint(project, actor, initiative)
	if err != nil {
		return fmt.Errorf("read checkpoint: %w", err)
	}
	// The checkpoint IR is NOT the diff base. That has been the LIVE
	// DATABASE since rc.40 — `DevPlanAndApplyLossy` reads what the stores
	// hold and the console plans against that reading.
	//
	// What these bytes feed is `ClassifyCompat`: the destructive-change
	// warning, "this sync would drop a column that was here last time".
	// So advancing the checkpoint changes what a LATER run of the same
	// (project, user, initiative) WARNS about — never what SQL runs.
	//
	// The comment here used to say "the diff BASE", left over from when
	// it was, and a consumer reading it reasonably concluded that a CI
	// run advancing a checkpoint against a throwaway database was moving
	// something load-bearing. It is not. Passed through as opaque bytes
	// (nil/empty for a brand-new initiative), never decoded.
	var baseBytes []byte
	if ckpt != nil {
		baseBytes = ckpt.GetIrSchema()
	}

	// currentBytes is the opaque compiled IR (the client never decodes it) —
	// the plan/compat RPCs + the checkpoint advance all consume it verbatim.
	ctx, cancel := context.WithTimeout(context.Background(), 120e9)
	defer cancel()
	if _, err := plan.DevPlanAndApplyLossy(ctx, baseBytes, currentBytes, applierFor, conns, logf, lossyMode, snapshot); err != nil {
		// Nil-checkpoint "already exists": the store was bootstrapped from
		// db/init (full schema on a fresh volume) but has no dev checkpoint
		// yet, so the first diff-apply — base nil → full create — collides
		// with db/init's tables. Steer to the one w17ctl recovery instead of
		// surfacing the raw driver error.
		if len(baseBytes) == 0 && strings.Contains(err.Error(), "already exists") {
			return errStoreAlreadyBootstrapped(err)
		}
		return err
	}

	return adoptCheckpoint(sc, project, actor, initiative, currentBytes, compilerVersion)
}

// specConnections names the connections this run is pointed at, which is what
// the checkpoint guard has to inspect. The plan cannot supply them: when the
// checkpoint is already at the current schema the plan is EMPTY, and that is
// precisely the case the guard exists for.
func specConnections(specs []factory.TargetSpec) []string {
	out := make([]string, 0, len(specs))
	for _, sp := range specs {
		out = append(out, sp.Connection)
	}
	return out
}

// adoptCheckpoint records `currentBytes` as the initiative's checkpoint WITHOUT
// applying any DDL — the "the store is already at this schema" advance. Used
// after a clean dev diff-apply (the store just converged) AND by `stack reset`
// (db/init just re-created the full current schema on a fresh volume, so
// re-applying would collide). Isolating it keeps the two callers' hash + RPC
// identical.
func adoptCheckpoint(sc *storageclient.StorageClients, project, actor, initiative string, currentBytes []byte, compilerVersion string) error {
	sum := sha256.Sum256(currentBytes)
	if _, err := sc.AdvanceCheckpoint(project, actor, initiative, currentBytes, hex.EncodeToString(sum[:]), compilerVersion); err != nil {
		return fmt.Errorf("advance checkpoint: %w", err)
	}
	return nil
}

// errStoreAlreadyBootstrapped wraps the nil-checkpoint collision in an
// actionable message: the fresh volume already has the schema (db/init), the
// checkpoint (the dev-diff base) starts empty, and the two collide on the first
// build. The fix is a single w17ctl command — never a hand-run psql.
func errStoreAlreadyBootstrapped(cause error) error {
	return fmt.Errorf(`local store already has this schema but has no dev checkpoint yet — the first dev diff-apply tried to re-create it (%w)
  why: a fresh volume boots its full schema from db/init (docker-entrypoint-initdb.d);
       the dev-diff base is the console checkpoint, which starts empty — so they collide.
  fix: run 'w17ctl stack reset' — it wipes the local stores, re-applies db/init, and
       adopts the current schema as the checkpoint baseline so the next build is a no-op`, cause)
}
