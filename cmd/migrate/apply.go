package migrate

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/lockfile"
	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	migratesdk "github.com/wandering-compiler/sdk/go/tooling/migrate"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/factory"
)

// applierFactory is the package var tests swap to inject a stub applier
// without going through DSN resolution. Production leaves it nil; Apply /
// Rollback build a real factory from W17_TARGET_<CONN> env vars. Keeping
// it a package-level seam (rather than a flag) mirrors the old
// w17migrate CLI and keeps DSNs env-only — credentials never land on a
// flag, in shell history, or in a process listing.
var applierFactory migratesdk.ApplierFor

// connTargets builds the apply-tier ConnTargets from the lock's
// connections. The apply orchestrator is lock-proto-free (public-split):
// w17ctl reads the pins from its local lockfile view and passes them in as
// plain data. Only connections with a pinned target_migration_id participate —
// an unpinned connection has never been schema-pushed, so there is nothing
// to apply.
func connTargets(lk *lockfile.Lock) []migratesdk.ConnTarget {
	var tgts []migratesdk.ConnTarget
	for _, c := range lk.Connections {
		tgts = append(tgts, migratesdk.ConnTarget{
			Connection:          c.Name,
			TargetMigrationID:   c.TargetMigrationID,
			TargetContentSha256: c.TargetContentSha256,
		})
	}
	return tgts
}

// FetchCmd downloads every migration up to each connection's pinned
// target from the console's public MigrationFetch service, writing one
// artifact set per migration into the migrations directory ready for an
// offline `apply`. This is the only online step — everything after it
// (apply / rollback) runs without console reachability.
//
// It dials the public MigrationFetch service (NOT the private registry
// client), so w17ctl's apply path carries zero console/client
// (registrypb) dependency.
type FetchCmd struct {
	LockPath      string `name:"lock" placeholder:"PATH" default:"w17/lock.yaml" help:"Path to the lock file."`
	Console       string `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console MigrationFetch service. Optional — falls back to the console you are logged into (w17ctl login), then the binary's compile-time default."`
	MigrationsDir string `name:"out" placeholder:"DIR" default:"w17/migrations" help:"Output directory for fetched migrations."`
}

func (c *FetchCmd) Run() error {
	lk, err := lockfile.Load(c.LockPath)
	if err != nil {
		return fmt.Errorf("fetch: %w", err)
	}

	addr, err := core.ResolveConsoleAddr(c.Console)
	if err != nil {
		return err
	}

	// Build one Target per lock connection that has a non-empty
	// target_migration_id — unpinned connections have nothing to fetch.
	var targets []*applyfetchpb.Target
	for _, conn := range lk.Connections {
		target := conn.TargetMigrationID
		if target == "" {
			fmt.Fprintf(core.Stdout, "fetch: %s — no target_migration_id pinned, skipping\n", conn.Name)
			continue
		}
		targets = append(targets, &applyfetchpb.Target{
			Connection:      conn.Name,
			UpToMigrationId: target,
		})
	}
	if len(targets) == 0 {
		fmt.Fprintln(core.Stdout, "fetch: no pinned connections — nothing to fetch")
		return nil
	}

	cl, conn, err := core.DialMigrationFetch(addr)
	if err != nil {
		return fmt.Errorf("connect %s: %w", addr, err)
	}
	defer func() { _ = conn.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	resp, err := cl.FetchMigrations(ctx, &applyfetchpb.FetchMigrationsRequest{
		ProjectId: lk.ProjectID,
		Targets:   targets,
	})
	if err != nil {
		return fmt.Errorf("fetch migrations: %w", err)
	}

	for _, m := range resp.GetMigrations() {
		if err := migratesdk.WriteMigration(c.MigrationsDir, m); err != nil {
			return fmt.Errorf("write %s/%s: %w", m.GetConnection(), m.GetId(), err)
		}
	}
	fmt.Fprintf(core.Stdout, "fetch: %d migration(s) written to %s\n", len(resp.GetMigrations()), c.MigrationsDir)
	return nil
}

// ApplyCmd applies pending migrations against each connection's target
// store. The DB-side wc_migrations table is the source of truth for what
// is already applied; pending = on-disk ∩ (id > applied_head, id ≤
// target). Offline — never contacts the console. DSNs resolve from
// W17_TARGET_<CONN_UPPER> env vars ONLY (never a flag) so credentials
// don't leak into shell history / process listings / CI logs.
type ApplyCmd struct {
	LockPath      string `name:"lock" placeholder:"PATH" default:"w17/lock.yaml" help:"Path to the lock file."`
	MigrationsDir string `name:"migrations" placeholder:"DIR" default:"w17/migrations" help:"Directory holding artifacts produced by fetch."`
	DryRun        bool   `name:"dry-run" help:"Print pending migrations without applying."`
	LogFormat     string `name:"log-format" placeholder:"text|json" default:"text" enum:"text,json" help:"Per-migration log line format. text = human-readable; json = one structured slog line per migration (CI / SIEM-friendly)."`
	Parallel      int    `name:"parallel" placeholder:"N" help:"Force-override per-op worker count for KV YAML data migrations (Redis, S3). 0 = use the migration's project-side parallel:; otherwise overrides at deploy time. Other dialects ignore."`
}

func (c *ApplyCmd) Run() error {
	lk, err := lockfile.Load(c.LockPath)
	if err != nil {
		return fmt.Errorf("apply: %w", err)
	}

	af, err := resolveApplier(lk, c.Parallel)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	return migratesdk.Run(ctx, migratesdk.Config{
		Targets:       connTargets(lk),
		MigrationsDir: c.MigrationsDir,
		ApplierFor:    af,
		Out:           core.Stdout,
		DryRun:        c.DryRun,
		LogFormat:     c.LogFormat,
	})
}

// RollbackCmd walks every applied migration with id strictly greater
// than --to, in REVERSE order, and runs each migration's down body
// against the per-connection target. Offline — never contacts the
// console. `--to` is the highest id to KEEP applied; --to="" rolls back
// everything currently applied. DSNs are env-only, same as apply.
type RollbackCmd struct {
	LockPath      string `name:"lock" placeholder:"PATH" default:"w17/lock.yaml" help:"Path to the lock file."`
	MigrationsDir string `name:"migrations" placeholder:"DIR" default:"w17/migrations" help:"Directory holding artifacts produced by fetch."`
	To            string `name:"to" placeholder:"MIGRATION_ID" required:"" help:"Highest migration id to KEEP applied. Empty = roll back everything currently applied. Required (no default; rollback is destructive)."`
	DryRun        bool   `name:"dry-run" help:"Print migrations that would roll back without running them."`
	LogFormat     string `name:"log-format" placeholder:"text|json" default:"text" enum:"text,json" help:"Per-migration log line format. text = human-readable; json = one structured slog line per migration."`
	Parallel      int    `name:"parallel" placeholder:"N" help:"Force-override per-op worker count for KV YAML data migrations (Redis, S3). 0 = use the migration's project-side parallel:; otherwise overrides at deploy time. Other dialects ignore."`
}

func (c *RollbackCmd) Run() error {
	lk, err := lockfile.Load(c.LockPath)
	if err != nil {
		return fmt.Errorf("rollback: %w", err)
	}

	af, err := resolveApplier(lk, c.Parallel)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	return migratesdk.RunRollback(ctx, migratesdk.RollbackConfig{
		Targets:       connTargets(lk),
		MigrationsDir: c.MigrationsDir,
		ApplierFor:    af,
		Out:           core.Stdout,
		DryRun:        c.DryRun,
		LogFormat:     c.LogFormat,
		ToMigrationID: c.To,
	})
}

// resolveApplier returns the applier factory for an apply / rollback run:
// the package-level test override when set, else one built from the lock's
// per-connection DSN env (resolveDSNs) with the deploy-time parallel override.
// Shared by ApplyCmd and RollbackCmd so the test-seam + env-DSN cascade lives
// in one place.
//
// W17CTL-DRYRUN — a dry run STILL needs a live applier: migrate.Plan opens each
// connection to read its AppliedHead so it can compute which pending migrations
// a real apply would run. The earlier `|| dryRun` short-circuit returned the
// (nil, in production) test override, so `migrate apply --dry-run` died with
// "migrate.Plan: ApplierFor is nil". Only the terminal Apply() is skipped, and
// the orchestrator's own DryRun branch already handles that — so resolve DSNs
// here regardless of dry-run.
func resolveApplier(lk *lockfile.Lock, parallel int) (migratesdk.ApplierFor, error) {
	if applierFactory != nil {
		return applierFactory, nil
	}
	specs, err := resolveDSNs(lk, os.Getenv)
	if err != nil {
		return nil, err
	}
	return factory.FromTargets(specs, factory.WithParallel(parallel)), nil
}

// resolveDSNs builds the per-connection DSN list from the
// W17_TARGET_<CONN_UPPER> env-var convention. A connection with a target
// pinned in the lock but no DSN is a clear error listing the connection
// name + the env var the operator should set.
//
// envLookup is a parameterised getenv so tests drive it without
// t.Setenv (production wires os.Getenv). DSNs are always env-only —
// never a flag — so credentials don't leak into shell history or process
// listings.
func resolveDSNs(lk *lockfile.Lock, envLookup func(string) string) ([]factory.TargetSpec, error) {
	var specs []factory.TargetSpec
	var missing []string
	for _, conn := range lk.Connections {
		if conn.TargetMigrationID == "" {
			continue
		}
		name := conn.Name
		dsn := envLookup(envVarName(name))
		if dsn == "" {
			missing = append(missing, name)
			continue
		}
		specs = append(specs, factory.TargetSpec{Connection: name, DSN: dsn})
	}
	if len(missing) > 0 {
		first := missing[0]
		return nil, fmt.Errorf(
			"apply: no DSN for connection(s) %s — set env var %s",
			strings.Join(missing, ", "), envVarName(first),
		)
	}
	return specs, nil
}

// envVarName turns a schema-declared connection name into the
// W17_TARGET_<CONN_UPPER> convention. Hyphens collapse to underscores so
// connection name `read-replica` resolves via `W17_TARGET_READ_REPLICA`.
func envVarName(connection string) string {
	return "W17_TARGET_" + strings.ToUpper(strings.ReplaceAll(connection, "-", "_"))
}

// AdoptCmd backs `w17ctl migrate adopt` — bringing a database that already
// HAS the schema under migration management, without running the DDL that
// would build it.
//
// It is its own command rather than a flag on apply, and that is the whole
// design: an operation whose purpose is to skip the DDL must never be
// reached by inference. `apply` decides what to run; `adopt` is the operator
// saying "this database is already there", and the tool's job is to check
// that claim rather than take it.
//
// The check is the console's: each artifact carries an
// `adopt_preflight_sql` that ensures the ledger exists and then fails unless
// the objects the migration introduces are present. A migration without one
// cannot be adopted — see
// [migratesdk.RunAdopt] for what that covers and why it refuses rather than
// waves through.
//
// Offline, like apply. DSNs come from W17_TARGET_<CONN> exactly as they do
// there, so credentials stay off the flag surface.
type AdoptCmd struct {
	LockPath      string   `name:"lock" placeholder:"PATH" default:"w17/lock.yaml" help:"Path to the lock file."`
	MigrationsDir string   `name:"migrations" placeholder:"DIR" default:"w17/migrations" help:"Directory holding artifacts produced by fetch."`
	Connections   []string `name:"connection" placeholder:"NAME" help:"Adopt only these connections (repeatable). Default: every connection the lock pins. A connection whose migration introduces nothing durable — a KV keyspace, an ALTER — cannot be adopted and does not need to be: apply it instead."`
	LogFormat     string   `name:"log-format" placeholder:"text|json" default:"text" enum:"text,json" help:"Per-migration log line format."`
}

func (c *AdoptCmd) Run() error {
	lk, err := lockfile.Load(c.LockPath)
	if err != nil {
		return fmt.Errorf("adopt: %w", err)
	}
	af, err := resolveApplier(lk, 0)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	return migratesdk.RunAdopt(ctx, migratesdk.Config{
		Targets:          connTargets(lk),
		MigrationsDir:    c.MigrationsDir,
		AdoptConnections: c.Connections,
		ApplierFor:       af,
		Out:              core.Stdout,
		LogFormat:        c.LogFormat,
	})
}
