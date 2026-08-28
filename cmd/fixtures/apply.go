package fixtures

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/localtarget"
	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
)

// fixtureExecer is the minimal target-store surface the apply step
// needs: open a transaction, run pre-bound parameterized statements,
// commit / roll back. Production wires a pgx-backed adapter; tests swap
// connectFixtureStore for a fake so they never need a real Postgres.
type fixtureExecer interface {
	BeginTx(ctx context.Context) (fixtureTx, error)
	Close(ctx context.Context) error
}

// fixtureTx is one open transaction. Exec runs a single SeedStmt with
// its args passed as PARAMETERS — never interpolated into the SQL.
type fixtureTx interface {
	Exec(ctx context.Context, sql string, args ...any) error
	Commit(ctx context.Context) error
	Rollback(ctx context.Context) error
}

// connectFixtureStore opens the target store at dsn. It is a package var
// so tests inject an in-memory fake; production keeps the pgx adapter
// and never needs a real DB in unit tests.
var connectFixtureStore = realConnectFixtureStore

// ApplyCmd fetches console-rendered fixture seeds (sets of parameterized
// upserts) and executes them against a connection's target store in one
// transaction. It has two modes:
//
//   - SINGLE fixture — `--domain D --name N [--group G]`: apply exactly one
//     fixture.
//   - GROUP (batch) — `--group G` (or nothing for the default group), `--name`
//     omitted: apply EVERY fixture in group G, optionally scoped to `--domain`
//     (omit for all domains). This is the deploy/CI shape — "seed the default
//     group" or "seed the dev group" in a single, atomic call.
//
// The render is schema-aware and done server-side (the console's public
// FixtureFetch service); the client only executes the returned, ordered,
// pre-bound statements. Each statement already carries $1..$N placeholders +
// the ordered arg values — w17ctl binds those values as query PARAMETERS,
// never interpolating them into the SQL text.
//
// The target DSN resolves from the W17_TARGET_<CONN_UPPER> env var ONLY
// (never a flag) so credentials don't leak into shell history / process
// listings / CI logs.
type ApplyCmd struct {
	Domain     string `name:"domain" placeholder:"DOMAIN" help:"Domain that owns the fixture. Required with --name (single fixture); optional in group mode (empty = every domain)."`
	Name       string `name:"name" placeholder:"NAME" help:"Single fixture name to render + apply. Omit to apply a whole group (see --group)."`
	Group      string `name:"group" placeholder:"GROUP" help:"Fixture group (subdirectory under the domain: fixtures/<domain>/<group>/<name>.json). Empty = the default group (files directly under fixtures/<domain>/). With --name omitted, applies every fixture in this group."`
	Connection string `name:"connection" placeholder:"CONN" required:"" help:"Target connection name. For a LOCAL dev store the DSN is auto-resolved from the published port + dev credentials (zero-config, same as 'stack build'); set W17_TARGET_<CONN_UPPER> (hyphens → underscores) only to override with a REMOTE/prod DSN, which stays env-only so the secret never leaks into a flag."`
	LockPath   string `name:"lock" placeholder:"PATH" default:"w17/lock.yaml" help:"Path to the lock file."`
	Console    string `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console FixtureFetch service. Optional — falls back to the console you are logged into (w17ctl login), then the binary's compile-time default."`
}

func (c *ApplyCmd) Run() error {
	if c.Name != "" && c.Domain == "" {
		return fmt.Errorf("fixtures apply: --name requires --domain (a single fixture is addressed by domain + name)")
	}

	projectID := core.LockProjectIDBestEffort()
	if projectID == "" {
		return fmt.Errorf("fixtures apply: could not resolve project_id from the lock (run inside a w17 project with %s)", c.LockPath)
	}

	// Resolve the target DSN BEFORE any RPC — no point fetching a seed we
	// can't apply.
	dsn, err := c.resolveTargetDSN()
	if err != nil {
		return err
	}

	addr, err := core.ResolveConsoleAddr(c.Console)
	if err != nil {
		return err
	}

	cl, conn, err := core.DialFixtureFetch(addr)
	if err != nil {
		return fmt.Errorf("connect %s: %w", addr, err)
	}
	defer func() { _ = conn.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	if c.Name != "" {
		return c.applyOne(ctx, cl, projectID, dsn)
	}
	return c.applyGroup(ctx, cl, projectID, dsn)
}

// applyOne applies a single fixture addressed by (domain, group, name).
func (c *ApplyCmd) applyOne(ctx context.Context, cl applyfetchpb.FixtureFetchClient, projectID, dsn string) error {
	// The registry keys fixtures by (domain, name); the group is folded into
	// the name as <group>/<name> — the same flat encoding push/fetch use, so
	// there's no wire-level group field.
	name := registryFixtureName(c.Group, c.Name)
	resp, err := cl.FetchFixtureSeed(ctx, &applyfetchpb.FetchFixtureSeedRequest{
		ProjectId: projectID,
		Domain:    c.Domain,
		Name:      name,
	})
	if err != nil {
		return fmt.Errorf("fetch fixture seed %s/%s: %w", c.Domain, name, err)
	}

	stmts := resp.GetStatements()
	if err := c.execSeed(ctx, dsn, stmts); err != nil {
		return err
	}

	fmt.Fprintf(core.Stdout, "fixtures apply: %s/%s — %d statement(s) on %s\n",
		c.Domain, name, len(stmts), c.Connection)
	return nil
}

// applyGroup applies EVERY fixture in group c.Group (optionally scoped to
// c.Domain) as one atomic transaction. It lists the project's fixtures
// (FetchFixtureFiles), keeps those whose name belongs to the requested group,
// renders each server-side (FetchFixtureSeed), and runs all statements in a
// single tx. Fixtures apply in (domain, name) order; a cross-fixture FK
// dependency is the author's responsibility to order via naming (each fixture
// is internally topo-sorted, but the group is not resolved as one graph).
func (c *ApplyCmd) applyGroup(ctx context.Context, cl applyfetchpb.FixtureFetchClient, projectID, dsn string) error {
	filesResp, err := cl.FetchFixtureFiles(ctx, &applyfetchpb.FetchFixtureFilesRequest{
		ProjectId: projectID,
		Domain:    c.Domain, // "" = every domain
	})
	if err != nil {
		return fmt.Errorf("list fixtures: %w", err)
	}

	type ref struct{ domain, name string }
	var refs []ref
	for _, f := range filesResp.GetFiles() {
		if fixtureGroup(f.GetName()) == c.Group {
			refs = append(refs, ref{domain: f.GetDomain(), name: f.GetName()})
		}
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].domain != refs[j].domain {
			return refs[i].domain < refs[j].domain
		}
		return refs[i].name < refs[j].name
	})

	scope := groupLabel(c.Group)
	if c.Domain != "" {
		scope += " in domain " + c.Domain
	}
	if len(refs) == 0 {
		fmt.Fprintf(core.Stdout, "fixtures apply: no fixtures in group %s\n", scope)
		return nil
	}

	var all []*applyfetchpb.SeedStmt
	for _, r := range refs {
		resp, err := cl.FetchFixtureSeed(ctx, &applyfetchpb.FetchFixtureSeedRequest{
			ProjectId: projectID,
			Domain:    r.domain,
			Name:      r.name,
		})
		if err != nil {
			return fmt.Errorf("fetch fixture seed %s/%s: %w", r.domain, r.name, err)
		}
		all = append(all, resp.GetStatements()...)
	}

	if err := c.execSeed(ctx, dsn, all); err != nil {
		return err
	}

	fmt.Fprintf(core.Stdout, "fixtures apply: group %s — %d fixture(s), %d statement(s) on %s\n",
		scope, len(refs), len(all), c.Connection)
	return nil
}

// registryFixtureName folds a (group, name) pair into the flat registry name
// the FixtureFetch/registry contract keys on: the default group ("") is just
// <name>; a named group is <group>/<name>. This mirrors the on-disk
// fixtures/<domain>/<group>/<name>.json layout onto the (domain, name) key
// without a wire-level group field — the same encoding `push` derives from the
// file path and `fetch` writes back out.
func registryFixtureName(group, name string) string {
	if group == "" {
		return name
	}
	return group + "/" + name
}

// fixtureGroup is the inverse of registryFixtureName: the group a flat registry
// name belongs to (the path up to the last "/"; "" for a default-group name).
func fixtureGroup(name string) string {
	if i := strings.LastIndex(name, "/"); i >= 0 {
		return name[:i]
	}
	return ""
}

// groupLabel renders a group name for human output: the default group ("")
// prints as "(default)".
func groupLabel(group string) string {
	if group == "" {
		return "(default)"
	}
	return group
}

// execSeed opens the target store and runs every SeedStmt in ONE
// transaction, in order. Each statement's args are passed as
// PARAMETERS to Exec ($1..$N placeholders the server already wrote) —
// values are NEVER interpolated into the SQL string, which is the whole
// point of the parameterized-upsert design. Any error rolls the whole
// transaction back.
func (c *ApplyCmd) execSeed(ctx context.Context, dsn string, stmts []*applyfetchpb.SeedStmt) error {
	store, err := connectFixtureStore(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect target store for %s: %w", c.Connection, err)
	}
	defer func() { _ = store.Close(ctx) }()

	tx, err := store.BeginTx(ctx)
	if err != nil {
		return fmt.Errorf("begin tx on %s: %w", c.Connection, err)
	}

	for i, st := range stmts {
		args := make([]any, len(st.GetArgs()))
		for j, a := range st.GetArgs() {
			args[j] = a.AsInterface()
		}
		if err := tx.Exec(ctx, st.GetSql(), args...); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("fixtures apply: statement %d/%d on %s: %w",
				i+1, len(stmts), c.Connection, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		_ = tx.Rollback(ctx)
		return fmt.Errorf("commit fixture seed on %s: %w", c.Connection, err)
	}
	return nil
}

// envVarName turns a schema-declared connection name into the
// W17_TARGET_<CONN_UPPER> convention. Hyphens collapse to underscores so
// connection name `read-replica` resolves via `W17_TARGET_READ_REPLICA`.
//
// A small local copy of the same helper in cmd/migrate — duplicated (not
// exported / shared) so the two apply paths stay independent.
func envVarName(connection string) string {
	return "W17_TARGET_" + strings.ToUpper(strings.ReplaceAll(connection, "-", "_"))
}

// resolveTargetDSN resolves the DSN to apply the seed against. Precedence:
//  1. W17_TARGET_<CONN> env — an explicit override for a REMOTE/prod target,
//     whose DSN is a secret and must never ride a flag;
//  2. otherwise the LOCAL dev store DSN, auto-resolved from the project's
//     devconfig port allocation + the dev credentials codegen bakes into the
//     compose — zero-config, the SAME resolution `stack build` / `db snapshot`
//     use. So seeding a local dev store needs no W17_TARGET_* env at all.
func (c *ApplyCmd) resolveTargetDSN() (string, error) {
	if v := os.Getenv(envVarName(c.Connection)); v != "" {
		return v, nil
	}
	root, err := core.FindProjectRoot()
	if err != nil {
		return "", fmt.Errorf("fixtures apply: %w", err)
	}
	cfg, err := core.LoadDevConfigFn()
	if err != nil {
		return "", fmt.Errorf("fixtures apply: load dev config: %w", err)
	}
	_, p := cfg.FindByPath(root)
	dsn, skip := localtarget.ResolveDSN(c.Connection, p)
	if dsn == "" {
		return "", fmt.Errorf("fixtures apply: no DSN for connection %q — %s (or set %s for a remote target)",
			c.Connection, skip, envVarName(c.Connection))
	}
	return dsn, nil
}

// --- pgx-backed production adapter -----------------------------------

// realConnectFixtureStore opens a pgx connection to the Postgres target
// and wraps it in the fixtureExecer interface.
func realConnectFixtureStore(ctx context.Context, dsn string) (fixtureExecer, error) {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, err
	}
	return &pgxStore{conn: conn}, nil
}

type pgxStore struct{ conn *pgx.Conn }

func (s *pgxStore) BeginTx(ctx context.Context) (fixtureTx, error) {
	tx, err := s.conn.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, err
	}
	return &pgxTx{tx: tx}, nil
}

func (s *pgxStore) Close(ctx context.Context) error { return s.conn.Close(ctx) }

type pgxTx struct{ tx pgx.Tx }

func (t *pgxTx) Exec(ctx context.Context, sql string, args ...any) error {
	_, err := t.tx.Exec(ctx, sql, args...)
	return err
}

func (t *pgxTx) Commit(ctx context.Context) error   { return t.tx.Commit(ctx) }
func (t *pgxTx) Rollback(ctx context.Context) error { return t.tx.Rollback(ctx) }
