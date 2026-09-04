package fixtures

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/structpb"

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
	Domain      string `name:"domain" placeholder:"DOMAIN" help:"Domain that owns the fixture. Required with --name (single fixture); optional in group mode (empty = every domain)."`
	Name        string `name:"name" placeholder:"NAME" help:"Single fixture name to render + apply. Omit to apply a whole group (see --group)."`
	Group       string `name:"group" placeholder:"GROUP" help:"Fixture group (subdirectory under the domain: fixtures/<domain>/<group>/<name>.json). Empty = the default group (files directly under fixtures/<domain>/). With --name omitted, applies every fixture in this group."`
	Connection  string `name:"connection" placeholder:"CONN" required:"" help:"Target connection name. For a LOCAL dev store the DSN is auto-resolved from the published port + dev credentials (zero-config, same as 'stack build'); set W17_TARGET_<CONN_UPPER> (hyphens → underscores) only to override with a REMOTE/prod DSN, which stays env-only so the secret never leaks into a flag."`
	LockPath    string `name:"lock" placeholder:"PATH" default:"w17/lock.yaml" help:"Path to the lock file."`
	Console     string `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console FixtureFetch service. Optional — falls back to the console you are logged into (w17ctl login), then the binary's compile-time default."`
	FixturesDir string `name:"fixtures-dir" default:"fixtures" placeholder:"DIR" help:"Working-tree fixtures directory, compared against the registry copy before rendering. A fixture present here and different there is refused (see --allow-stale) — the registry is what gets rendered, so a stale one produces confident, wrong output."`
	AllowStale  bool   `name:"allow-stale" help:"Render the registry's copy even when the working tree disagrees with it."`
	Out         string `name:"out" placeholder:"PATH" help:"Write the seed as plain SQL to PATH ('-' for stdout) INSTEAD of applying it. No target store is dialled and --connection needs no reachable DSN, so this works for a database that does not exist yet — a db/init/*.sql for an ephemeral e2e Postgres, which is seeded only from that directory and has no stable DSN to apply against."`
}

func (c *ApplyCmd) Run() error {
	if c.Name != "" && c.Domain == "" {
		return fmt.Errorf("fixtures apply: --name requires --domain (a single fixture is addressed by domain + name)")
	}
	// Normalise ONCE, here, rather than at each reader. `--group .` has to
	// mean the default group in the group LISTING too, and that path
	// compares c.Group against a derived name — so a fix confined to
	// registryFixtureName would have left `--group .` matching nothing
	// and reporting "no fixtures in group .", which is the same silence
	// one layer over.
	c.Group = normalizeGroup(c.Group)

	projectID := core.LockProjectIDBestEffort()
	if projectID == "" {
		return fmt.Errorf("fixtures apply: could not resolve project_id from the lock (run inside a w17 project with %s)", c.LockPath)
	}

	// Resolve the target DSN BEFORE any RPC — no point fetching a seed we
	// can't apply. Skipped when emitting: --out writes SQL and dials
	// nothing, which is the whole point for a database that has no DSN
	// yet (deinvo, 2026-08-29: an ephemeral e2e Postgres is seeded only
	// from db/init/*.sql and exists only for the length of a run, so
	// "apply against the live target" has nothing to aim at).
	var dsn string
	if c.Out == "" {
		var derr error
		dsn, derr = c.resolveTargetDSN()
		if derr != nil {
			return derr
		}
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
	if !c.AllowStale {
		if err := checkFixtureFresh(ctx, cl, projectID, c.FixturesDir, c.Domain, c.Group, c.Name); err != nil {
			return err
		}
	}
	resp, err := cl.FetchFixtureSeed(ctx, &applyfetchpb.FetchFixtureSeedRequest{
		ProjectId: projectID,
		Domain:    c.Domain,
		Name:      name,
	})
	if err != nil {
		return fmt.Errorf("fetch fixture seed %s/%s: %w", c.Domain, name, err)
	}

	stmts := resp.GetStatements()
	if c.Out != "" {
		return c.emitSeed(stmts, fmt.Sprintf("%s/%s", c.Domain, name))
	}
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
		if !c.AllowStale {
			// r.name is the FLAT registry key; the on-disk path needs it
			// split back into (group, leaf) — the same encoding, inverted.
			if err := checkFixtureFresh(ctx, cl, projectID, c.FixturesDir,
				r.domain, fixtureGroup(r.name), fixtureLeaf(r.name)); err != nil {
				return err
			}
		}
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

// normalizeGroup maps the shapes that MEAN the default group onto "".
//
// "." and "./" are the natural way to write "no group" and read as such,
// and our own published recipe used `--group .`. It produced the registry
// key `./acl-roles`, which matches nothing, and surfaced as a bare
// NotFound naming a key the author never wrote (deinvo, 2026-09-04).
func normalizeGroup(group string) string {
	group = strings.TrimSuffix(strings.TrimSpace(group), "/")
	if group == "." {
		return ""
	}
	return group
}

// fixtureGroup is the inverse of registryFixtureName: the group a flat registry
// name belongs to (the path up to the last "/"; "" for a default-group name).
func fixtureGroup(name string) string {
	if i := strings.LastIndex(name, "/"); i >= 0 {
		return name[:i]
	}
	return ""
}

// fixtureLeaf is the name part of a flat registry key — everything after
// the last "/", i.e. the file's stem on disk. Pairs with fixtureGroup.
func fixtureLeaf(name string) string {
	if i := strings.LastIndex(name, "/"); i >= 0 {
		return name[i+1:]
	}
	return name
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

// emitSeed writes the fetched statements as plain, self-contained SQL.
//
// The seed arrives PARAMETERISED — `$1, $2, …` plus a value list — which
// is what the apply path binds. A file for `docker-entrypoint-initdb.d`
// gets no binder: psql runs it as literal SQL, so the parameters have to
// be rendered INTO the statement or the output is unusable.
//
// That inlining is the whole reason this is not a two-line dump, and it
// is why the rendering below is per-type rather than a Sprintf("%v"):
// `%v` turns a string into an unquoted token, a bool into `true` where
// some dialects want TRUE, and a nil into `<nil>`. Each of those is
// valid-looking output that fails at initdb time, in a container whose
// logs nobody reads until the suite is already red.
func (c *ApplyCmd) emitSeed(stmts []*applyfetchpb.SeedStmt, label string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "-- Generated by `w17ctl fixtures apply --out`. DO NOT EDIT.\n")
	fmt.Fprintf(&b, "--\n-- Fixture: %s (connection %s), %d statement(s).\n", label, c.Connection, len(stmts))
	fmt.Fprintf(&b, "-- Parameters are inlined as literals: this file is executed by psql\n")
	fmt.Fprintf(&b, "-- with no binder, so `$1` placeholders would not resolve.\n--\n")
	fmt.Fprintf(&b, "-- Regenerate rather than edit — the fixture is the source of truth,\n")
	fmt.Fprintf(&b, "-- and a hand-edited copy is the second one this project does not want.\n\n")

	for i, st := range stmts {
		sql, err := inlineSeedArgs(st.GetSql(), st.GetArgs())
		if err != nil {
			return fmt.Errorf("fixtures apply --out: statement %d/%d of %s: %w", i+1, len(stmts), label, err)
		}
		b.WriteString(strings.TrimRight(sql, " \t\n;"))
		b.WriteString(";\n")
	}

	if c.Out == "-" {
		_, err := io.WriteString(core.Stdout, b.String())
		return err
	}
	if err := os.MkdirAll(filepath.Dir(c.Out), 0o755); err != nil {
		return fmt.Errorf("fixtures apply --out: %w", err)
	}
	if err := os.WriteFile(c.Out, []byte(b.String()), 0o644); err != nil {
		return fmt.Errorf("fixtures apply --out: %w", err)
	}
	fmt.Fprintf(core.Stdout, "fixtures apply: %s — %d statement(s) written to %s (not applied)\n",
		label, len(stmts), c.Out)
	return nil
}

// inlineSeedArgs substitutes $N placeholders with SQL literals.
//
// Scans the statement rather than doing a blind ReplaceAll: `$1` is a
// prefix of `$10`, so replacing in ascending order corrupts every
// statement with ten or more parameters — and an ACL role seed has
// hundreds of rows, so that is the normal case here, not an edge one.
func inlineSeedArgs(sql string, args []*structpb.Value) (string, error) {
	var b strings.Builder
	for i := 0; i < len(sql); i++ {
		if sql[i] != '$' {
			b.WriteByte(sql[i])
			continue
		}
		j := i + 1
		for j < len(sql) && sql[j] >= '0' && sql[j] <= '9' {
			j++
		}
		if j == i+1 { // a bare '$' — not a placeholder
			b.WriteByte(sql[i])
			continue
		}
		n, err := strconv.Atoi(sql[i+1 : j])
		if err != nil || n < 1 || n > len(args) {
			return "", fmt.Errorf("placeholder %s has no argument (statement carries %d)", sql[i:j], len(args))
		}
		lit, err := sqlLiteral(args[n-1])
		if err != nil {
			return "", err
		}
		b.WriteString(lit)
		i = j - 1
	}
	return b.String(), nil
}

// sqlLiteral renders one fixture argument as a SQL literal.
//
// Refuses what it cannot render exactly rather than guessing: a value
// that reached SQL as the wrong type is a silent data defect, and this
// file is committed and then trusted for the life of the project.
func sqlLiteral(v *structpb.Value) (string, error) {
	switch k := v.GetKind().(type) {
	case *structpb.Value_NullValue:
		return "NULL", nil
	case *structpb.Value_BoolValue:
		if k.BoolValue {
			return "TRUE", nil
		}
		return "FALSE", nil
	case *structpb.Value_NumberValue:
		// %v would print 1e+06 for a large id. Render integral values
		// without an exponent so a bigint column receives a bigint.
		if k.NumberValue == float64(int64(k.NumberValue)) {
			return strconv.FormatInt(int64(k.NumberValue), 10), nil
		}
		return strconv.FormatFloat(k.NumberValue, 'f', -1, 64), nil
	case *structpb.Value_StringValue:
		s := k.StringValue
		if strings.ContainsRune(s, 0) {
			return "", fmt.Errorf("string argument contains a NUL byte, which no PostgreSQL text literal can carry")
		}
		// Standard SQL escaping: double the single quotes. Backslashes
		// need no treatment while standard_conforming_strings is on,
		// which it has been by default since PostgreSQL 9.1.
		return "'" + strings.ReplaceAll(s, "'", "''") + "'", nil
	default:
		// Lists and structs would need a dialect-specific rendering
		// (array literal vs json) and no fixture emits one today.
		return "", fmt.Errorf("argument of type %T cannot be rendered as a SQL literal", k)
	}
}
