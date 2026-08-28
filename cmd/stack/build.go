package stack

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/wandering-compiler/w17ctl/internal/autosync"
	plan "github.com/wandering-compiler/w17ctl/internal/plan"
	"github.com/wandering-compiler/w17ctl/internal/schema"
	"github.com/wandering-compiler/w17ctl/internal/storageclient"

	codegen "github.com/wandering-compiler/w17ctl/internal/codegen"
	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/devconfig"
	"github.com/wandering-compiler/w17ctl/internal/docker"
	"github.com/wandering-compiler/w17ctl/internal/reconcile"
	"github.com/wandering-compiler/w17ctl/internal/remotecompose"
	"github.com/wandering-compiler/w17ctl/internal/vocab"
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
	Imports         []string `name:"import" short:"I" placeholder:"DIR" help:"Additional proto import path(s). Repeatable."`
	Targets         []string `name:"target" short:"t" placeholder:"CONN=DSN" help:"Local store target(s) to dev diff-apply to, <connection>=<dsn>. Repeatable. Empty = auto-resolved from the lock's connections + the host ports 'stack up' published."`
	Project         string   `name:"project" placeholder:"ID" help:"Project id. Empty = read from the current project's lock."`
	CompilerVersion string   `name:"compiler-version" placeholder:"VER" default:"dev" help:"Compiler version pinned into the advanced checkpoint."`
	By              string   `name:"by" placeholder:"ACTOR" help:"Actor stamp (checkpoint user_id). Empty = local OS user."`
	Console         string   `name:"console" placeholder:"HOST:PORT" env:"CONSOLE_STORAGE_ADDR" help:"Console storage endpoint (holds the checkpoints). Defaults to the logged-in console (w17ctl login), else the compiled-in default."`
	Reconcile       bool     `name:"reconcile" help:"Force the branch-switch reconcile even when the project's autosync mode is off. (When on — the default — reconcile already runs on an initiative change.)"`
	NoCodegen       bool     `name:"no-codegen" help:"Skip the codegen step (assume the generated code is already current). By default 'stack build' runs codegen first so the images compile against fresh generated code."`
	modeFlags

	// cc is the compose control reconcile's Quiesce uses to stop non-store
	// services. Set to the remote runner in remote mode (Run); nil ⇒
	// buildReconcileDeps defaults to the local daemon.
	cc composeCtl
}

// runCodegenFn regenerates all derived code (the codegen step `stack
// build` runs before compiling images). A package var so the build's
// tests can stub it. Force overwrites the existing git-ignored generated
// tree; CodegenCmd resolves its own console address from the lock/env.
var runCodegenFn = func() error {
	return codegen.Run("", true)
}

func (c *BuildCmd) Run() error {
	root, err := core.FindProjectRoot()
	if err != nil {
		return err
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
	} else {
		// Compile Go + build images locally.
		if err := docker.RunComposeFn(root, append([]string{"build"}, c.Services...)...); err != nil {
			return fmt.Errorf("stack build: compose build: %w", err)
		}
	}

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
	models, err := discoverModelProtos(base)
	if err != nil {
		return nil, nil, cleanup, fmt.Errorf("stack build: discover model protos: %w", err)
	}
	if len(models) == 0 {
		return nil, nil, cleanup, nil
	}
	vocabDir, vcleanup, err := vocab.ExtractW17Vocab()
	if err != nil {
		return nil, nil, cleanup, fmt.Errorf("stack build: stage w17 vocab: %w", err)
	}
	// Import roots: the proto dir (cross-domain model imports) + the
	// staged vocab (w17/*.proto) + any explicit --import.
	imports = append([]string{base, vocabDir}, c.Imports...)
	return models, imports, vcleanup, nil
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

	logf := func(format string, args ...any) { fmt.Fprintf(core.Stdout, format+"\n", args...) }
	if err := runDevDiffApply(sc, project, actor, initiative, currentBytes, applierFor, c.CompilerVersion, logf); err != nil {
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
func runDevDiffApply(sc *storageclient.StorageClients, project, actor, initiative string, currentBytes []byte, applierFor migrate.ApplierFor, compilerVersion string, logf func(string, ...any)) error {
	ckpt, err := sc.GetCheckpoint(project, actor, initiative)
	if err != nil {
		return fmt.Errorf("read checkpoint: %w", err)
	}
	// The checkpoint IR is the diff BASE — passed through as opaque bytes
	// (nil/empty for a brand-new initiative → full create), never decoded.
	var baseBytes []byte
	if ckpt != nil {
		baseBytes = ckpt.GetIrSchema()
	}

	// currentBytes is the opaque compiled IR (the client never decodes it) —
	// the plan/compat RPCs + the checkpoint advance all consume it verbatim.
	ctx, cancel := context.WithTimeout(context.Background(), 120e9)
	defer cancel()
	if _, err := plan.DevPlanAndApply(ctx, baseBytes, currentBytes, applierFor, logf); err != nil {
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
