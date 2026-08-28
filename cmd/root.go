// Package cmd is the root of the w17ctl command tree: it registers every
// cmd/<command> subpackage into the kong root and runs it (Run). main.go is
// pure wiring — it only calls cmd.Run. Keeping the assembly + the
// parse/dispatch flow + the per-command lock-integrity guard here (rather than
// in an internal package the cmd/* children would have to be imported back
// into) gives a one-way dependency — cmd → cmd/* → internal — and a
// unit-testable surface without a process exec.
package cmd

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/alecthomas/kong"

	admissioncmd "github.com/wandering-compiler/w17ctl/cmd/admission"
	certs "github.com/wandering-compiler/w17ctl/cmd/certs"
	codegencmd "github.com/wandering-compiler/w17ctl/cmd/codegen"
	compatcmd "github.com/wandering-compiler/w17ctl/cmd/compat"
	connectioncmd "github.com/wandering-compiler/w17ctl/cmd/connection"
	dbcmd "github.com/wandering-compiler/w17ctl/cmd/db"
	domaincmd "github.com/wandering-compiler/w17ctl/cmd/domain"
	envcmd "github.com/wandering-compiler/w17ctl/cmd/env"
	fixturescmd "github.com/wandering-compiler/w17ctl/cmd/fixtures"
	guidecmd "github.com/wandering-compiler/w17ctl/cmd/guide"
	initcmd "github.com/wandering-compiler/w17ctl/cmd/init"
	initiativecmd "github.com/wandering-compiler/w17ctl/cmd/initiative"
	logincmd "github.com/wandering-compiler/w17ctl/cmd/login"
	logoutcmd "github.com/wandering-compiler/w17ctl/cmd/logout"
	migratecmd "github.com/wandering-compiler/w17ctl/cmd/migrate"
	modulecmd "github.com/wandering-compiler/w17ctl/cmd/module"
	orgcmd "github.com/wandering-compiler/w17ctl/cmd/org"
	plugincmd "github.com/wandering-compiler/w17ctl/cmd/plugin"
	projectcmd "github.com/wandering-compiler/w17ctl/cmd/project"
	pushcmd "github.com/wandering-compiler/w17ctl/cmd/push"
	reviewcmd "github.com/wandering-compiler/w17ctl/cmd/review"
	sdkcmd "github.com/wandering-compiler/w17ctl/cmd/sdk"
	secrets "github.com/wandering-compiler/w17ctl/cmd/secrets"
	stackcmd "github.com/wandering-compiler/w17ctl/cmd/stack"
	stresscmd "github.com/wandering-compiler/w17ctl/cmd/stress"
	targetcmd "github.com/wandering-compiler/w17ctl/cmd/target"
	templatecmd "github.com/wandering-compiler/w17ctl/cmd/template"
	testcmd "github.com/wandering-compiler/w17ctl/cmd/test"
	uicmd "github.com/wandering-compiler/w17ctl/cmd/ui"
	verify "github.com/wandering-compiler/w17ctl/cmd/verify"
	versioncmd "github.com/wandering-compiler/w17ctl/cmd/version"
	whoamicmd "github.com/wandering-compiler/w17ctl/cmd/whoami"
	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/lockfile"
)

// root is the kong command tree. Each command (`init`, `migrate`, `target`,
// `stack`, …) is a struct field bound to a `cmd/<command>/` package;
// subcommands of a subtree live in that package.
//
// `w17ctl --help` renders with NoExpandSubcommands (see Run), so the top
// level lists only these section commands + their one-line help — it does
// NOT recurse into subcommands. Drill into a section for its verbs, e.g.
// `w17ctl plugin --help`. The fields are still ordered by developer workflow
// (setup → declare → generate → schema → …) so the flat list reads
// top-to-bottom; the `// --- … ---` comments mark those blocks for readers,
// not the help output.
var root struct {
	// --- Getting started (read me first — no project state; safe before init) ---
	Guide   guidecmd.Cmd   `cmd:"" help:"Write AGENTS.md — the AI-agent usage guide for driving w17ctl (golden rules + canonical workflow + task→command cheat-sheet). Coding agents read AGENTS.md into context automatically. Runs in an empty dir before 'init'; --stdout to print, --force to refresh."`
	Version versioncmd.Cmd `cmd:"" help:"Print this binary's version, commit and build date, plus the console address compiled into it. A released binary reports its release; a locally built one reports \"dev\" rather than inventing a number."`

	// --- Authentication (console identity, machine-local ~/.w17/auth.yaml) ---
	Login  logincmd.Cmd  `cmd:"" help:"Log in to a console via OAuth (loopback + PKCE, like 'claude login'). Opens a browser to authorize, stores the bearer + org memberships in ~/.w17/auth.yaml. 'login https://console.w17.dev' (or an enterprise self-host URL)."`
	Logout logoutcmd.Cmd `cmd:"" help:"Log out of a console — drop its stored credential from ~/.w17/auth.yaml. Defaults to the active console."`
	Whoami whoamicmd.Cmd `cmd:"" help:"Show the stored identity + organizations for the active console (--all for every logged-in console)."`
	Org    orgcmd.Cmd    `cmd:"" help:"Organizations on the active console — list the ones you belong to (list) and pick the default (use <slug>). The default org scopes subsequent commands."`

	// --- Project setup ---
	Init       initcmd.Cmd       `cmd:"" help:"Wizard — bootstrap a new w17 project. Prompts for project name + generated-code paths, registers the project with the console, writes a signed w17/lock.yaml. Refuses on existing lock."`
	Connection connectioncmd.Cmd `cmd:"" help:"Connection-level operations — wizard adds a new connection to the project's lock."`
	Domain     domaincmd.Cmd     `cmd:"" help:"Domain-level scaffolding — creates the proto/domains/<NAME>/w17.proto cascade sentinel; --with-example also seeds a four-layer example module."`
	Module     modulecmd.Cmd     `cmd:"" help:"Module-level scaffolding — creates proto/domains/<DOMAIN>/<MODULE>/w17.proto under an existing domain; --with-example also populates the four-layer (queries/mutations/business/types) shape."`
	Template   templatecmd.Cmd   `cmd:"" help:"Print a commented example .proto for one w17 surface (domain|module|types|queries|mutations|business|rest|admin|events|subscribers|rpc|mcp|cli) — a 'show me how to annotate X' reference. Run 'template --list'."`

	// --- Codegen / deploy targets (lock's generated_code[] declarations) ---
	Target targetcmd.Cmd `cmd:"" help:"Declare what codegen emits + how it deploys — the lock's generated_code entries: client (FE clients), grpc-client (Go client pkg), business (business bundle), binary (composed binaries), ci (CI configs), scale (PROD replicas). All edit the signed lock. See 'target --help'."`

	// --- Code generation ---
	// codegen generates EVERYTHING (gRPC stubs + the ACL / eventbus /
	// MCP bundles — it runs those sub-generators internally), and
	// verify checks every committed lock against the proto. The former
	// standalone `acl` / `eventbus` / `mcp` commands are gone: their
	// generation is folded into codegen and their drift checks into
	// verify.
	Codegen codegencmd.Cmd `cmd:"" help:"Generate ALL derived code: Storage-layer Go gRPC handlers + the ACL / eventbus / MCP bundles. Walks parent dirs to the .w17/ project root, reads w17/lock.yaml, uploads every .proto under proto/ to the console, writes returned files under <root>/<gen_dir>/."`
	Verify  verify.Cmd     `cmd:"" help:"Verify every committed generated lock (ACL + eventbus) still matches the current proto — the CI drift hook paired with codegen. Non-zero exit on drift. Requires console access: the recompute + signature check run server-side (--console / W17_CONSOLE_ADDR)."`
	// admission reads the counters behind the console's codegen gate. It sits
	// next to codegen because it answers a question ABOUT codegen — how much
	// of it the daemon is turning away, and how far its memory cost model has
	// drifted — neither of which the per-decision log lines accumulate.
	Admission admissioncmd.Cmd `cmd:"" help:"Read the console's codegen admission-gate counters: whether the gate is on, its limits, the queue depth right now, decisions by outcome, wait times, and the estimate-vs-observed drift that says when to recalibrate. A console read — takes no project."`

	// --- Schema & migration lifecycle ---
	Push    pushcmd.Cmd    `cmd:"" help:"Push everything (schema + fixtures + future artifact types) to console in one call. Server diffs and stores. Idempotent — safe to re-run any time inputs change."`
	Migrate migratecmd.Cmd `cmd:"" help:"Migration surface — generate (compile + push schema; console plans the SQL migrations), list history, push a raw body, reset (DEV-only destructive recreate). See 'migrate --help'."`
	// Fixtures — fetch a console-rendered fixture seed (parameterized
	// upserts, schema-aware render done server-side) and execute it
	// against a local target store. The thin-client successor to
	// w17migrate's apply-fixtures.
	Fixtures fixturescmd.Cmd `cmd:"" help:"Apply console-rendered fixture seeds (parameterized upserts) to a connection's target store (DSN via W17_TARGET_<CONN> env). See 'fixtures --help'."`

	// --- Console state (server-side, PG-backed) ---
	// Initiatives + snapshots (console v2 part B substrate). Git-sync:
	// the active initiative is derived from the current git branch.
	Initiative initiativecmd.Cmd `cmd:"" help:"Initiatives + snapshots — current / list / show / materialize / push. Git branch → initiative (main/master → trunk); lazy materialize on first push. See 'initiative --help'."`
	// Compat engine surface — semantic diff + policy gate (dev=warn /
	// prod=block breaking). The functions a future console GUI mirrors.
	Compat compatcmd.Cmd `cmd:"" help:"Compatibility report between two snapshots (DB / WIRE / API findings + severity) with a dev/prod policy gate. report --base --head, or against-trunk. See 'compat --help'."`
	// Review (PR analog) + environments + prod-mode gate (console v2 part B).
	Review reviewcmd.Cmd `cmd:"" help:"Review flow (PR analog): open / show / approve / merge a set of schema changes. Merge into a prod-mode env blocks BREAKING changes. See 'review --help'."`
	Env    envcmd.Cmd    `cmd:"" help:"Deployment environments + mode (dev/prod). A prod-mode env turns the compat engine's BREAKING verdict into a hard merge block. See 'env --help'."`

	// --- Plugins ---
	Plugin plugincmd.Cmd `cmd:"" help:"Plugin catalog operations — list the embedded + installed plugins and install one into this project."`

	// --- Secrets & TLS ---
	Sdk     sdkcmd.Cmd  `cmd:"" help:"Public sdk/go operations — move this project onto a new SDK release (update). Needs no local Go toolchain: the require + go.sum hashes are resolved from the module proxy."`
	Secrets secrets.Cmd `cmd:"" help:"Production-secrets operations (age tier) — mint a project age keypair (init) and encrypt a plain .secrets into a committable .secrets.age (encrypt). The runtime decrypts when a key is present, reads plain otherwise: seamless dev, strong devops."`
	Certs   certs.Cmd   `cmd:"" help:"Fill a directory with a local dev PKI (self-signed CA + CA-signed server leaf) for the internal-mesh TLS switch (W17_INTERNAL_TLS=on). Idempotent — never overwrites existing files. init seeds w17/certs/dev.local/ automatically; use this to refill or to scaffold a prod cert dir's missing material."`

	// --- Testing ---
	Test   testcmd.Cmd   `cmd:"" help:"Run the generated e2e suite against a deployed gateway. Walks parent dirs to the w17 project root, locates the e2erunner module, and runs it in Docker against --target (a pure HTTP/MCP client — never touches a DB)."`
	Stress stresscmd.Cmd `cmd:"" help:"Load-test a deployed gateway with the generated stress presets. 'stress <target>' — hammers the URL under each preset's stress.yaml plan (concurrency/duration/thresholds), prints a throughput/latency report, and fails on a breached threshold (a CI load gate). Owns no stack; one run per invocation."`

	// --- Local stack (docker compose) ---
	// Project-management verbs (formerly the generated manage.sh).
	// Compose verbs shell out to `docker compose` in the project root.
	// `project` keeps the dev-machine-local registry (unique host-port
	// assignments + run presets) that `stack up` drives through.
	Project projectcmd.Cmd    `cmd:"" help:"Local project registry — list / import / remove / ports / ps + run presets. Keeps unique host ports across all installed projects (no env-file juggling) in ~/.w17/config.yaml. See 'project --help'."`
	Stack   stackcmd.Cmd      `cmd:"" help:"Local docker-compose lifecycle — build / up / down / logs / ps / restart. See 'stack --help'."`
	Db      dbcmd.Cmd         `cmd:"" help:"Local dev DB — manual snapshots scoped to the current initiative (save / list / activate / delete). See 'db snapshot --help'."`
	Ui      uicmd.Cmd         `cmd:"" help:"Serve the local web app — a human-facing view + controller over the project registry, presets, and local Docker stacks (the GUI counterpart to project/stack). On-demand, 127.0.0.1 only."`
	Clean   stackcmd.CleanCmd `cmd:"" help:"Remove codegen output (pb stubs / FE clients / languages / generated bundle files) while preserving hand-written packages."`
}

// exitFn is the process-exit hook kong calls. Production uses the os.Exit
// default; tests override it to drive Run without killing the test runner.
var exitFn = os.Exit

// Run parses args against the command tree, runs the per-command lock-integrity
// guard, and dispatches. It is the single entrypoint main() calls.
func Run(args []string) {
	parser := kong.Must(&root,
		kong.Name("w17ctl"),
		kong.Description("w17 platform developer CLI — codegen, migrations, scaffolding, secrets, and the local stack. Drill into a section with `w17ctl <command> --help`."),
		// Top-level help lists only the section commands (init, plugin,
		// migrate, …) with their one-line help; it does not expand into
		// each section's subcommands. Drill in per section, e.g.
		// `w17ctl plugin --help`.
		kong.ConfigureHelp(kong.HelpOptions{NoExpandSubcommands: true}),
		kong.UsageOnError(),
		kong.Exit(exitFn),
	)
	ctx, err := parser.Parse(args)
	if err != nil {
		parser.FatalIfErrorf(err)
		return
	}
	// Per-command integrity guard: when the cwd is inside a w17 project, the
	// lock is the CLI-managed, ed25519-signed state manifest — verify it
	// before running ANY command so a hand-edited / tampered lock is refused
	// up front, not silently degraded (the LoadLockBestEffort paths swallow
	// the verify error). A fresh tree with no lock (pre-init) is fine: the
	// guard no-ops. Signature failures FatalIfErrorf with the
	// "regenerate via the CLI; do not hand-edit" message lock.Load returns.
	if err := verifyProjectLock(); err != nil {
		ctx.FatalIfErrorf(err)
		return
	}
	if err := ctx.Run(); err != nil {
		ctx.FatalIfErrorf(err)
		return
	}
}

// verifyProjectLock sanity-checks the project lock when the cwd is inside a
// w17 project that has one — a present-but-malformed lock is refused up front
// rather than failing deep in a command. Returns nil when not in a project
// (FindProjectRoot fails) or no lock exists yet (a fresh / pre-init tree).
//
// This is a structural parse check only (the client does ZERO crypto — the
// authoritative ed25519 signature check is the console-side VerifyLock RPC the
// `w17ctl verify` command runs). The lock is read as opaque local config via
// the offline lockfile reader, never the private srcgo lock types.
func verifyProjectLock() error {
	root, err := core.FindProjectRoot()
	if err != nil {
		return nil // not inside a w17 project — nothing to verify
	}
	path := filepath.Join(root, lockfile.DefaultPath)
	if _, statErr := os.Stat(path); errors.Is(statErr, os.ErrNotExist) {
		return nil // fresh project, lock not written yet
	}
	if _, loadErr := lockfile.Load(path); loadErr != nil {
		return loadErr // present but corrupt / unparseable → refuse
	}
	return nil
}
