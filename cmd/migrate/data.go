package migrate

// DQL data migrations: a data fix written in DQL over the project's models,
// stored in the migration history (push-data) and applied in order by every
// deployed environment, or tried against a database first (try-data).
//
// A local stack never runs the history (`stack build` applies the schema as a
// diff), so nothing applies a data migration there by itself: try-data is how
// a fix is checked, against a dump loaded locally, before it is pushed.

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql" // try-data on a MySQL connection
	_ "github.com/jackc/pgx/v5/stdlib" // try-data on a Postgres connection
	"gopkg.in/yaml.v3"
	_ "modernc.org/sqlite" // try-data on a SQLite connection

	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/localtarget"
	"github.com/wandering-compiler/w17ctl/internal/lockfile"
	"github.com/wandering-compiler/w17ctl/internal/storageclient"
	w17registrypb "github.com/wandering-compiler/sdk/go/pb/w17registry"
)

// DataFile is a data migration as written, `w17/data-migrations/<name>.yaml`
// by convention:
//
//	connection: billing-postgres
//	description: |
//	  Invoices paid twice before the double-charge fix were left UNPAID.
//	statements:
//	  - UPDATE billing.Invoice SET status = 'PAID' WHERE status = 'UNPAID' AND paid_at IS NOT NULL
type DataFile struct {
	Connection  string   `yaml:"connection"`
	Description string   `yaml:"description"`
	Statements  []string `yaml:"statements"`
}

func readDataFile(path string) (*DataFile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f DataFile
	if err := yaml.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	switch {
	case f.Connection == "":
		return nil, fmt.Errorf("%s: no connection (the database the fix runs on, as w17/lock.yaml names it)", path)
	case strings.TrimSpace(f.Description) == "":
		return nil, fmt.Errorf("%s: no description — say what the data migration fixes and why; it is kept with it", path)
	case len(f.Statements) == 0:
		return nil, fmt.Errorf("%s: no statements", path)
	}
	return &f, nil
}

func projectOr(flag string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	if id := core.LockProjectIDBestEffort(); id != "" {
		return id, nil
	}
	return "", fmt.Errorf("no project: run inside a project (w17/lock.yaml) or pass --project")
}

// PushDataCmd is `w17ctl migrate push-data <file>`.
type PushDataCmd struct {
	File       string `arg:"" placeholder:"FILE" help:"The data migration: connection, description and DQL statements (YAML)."`
	ProjectID  string `name:"project" placeholder:"ID" help:"Project ID. Default: the lock's."`
	Console    string `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console. Optional — falls back to the console you are logged in to, then the binary's compile-time default."`
	Initiative string `name:"initiative" placeholder:"ID" help:"Change-request (initiative) id this migration belongs to. Empty = the initiative of the current git branch."`
}

func (c *PushDataCmd) Run() error {
	f, err := readDataFile(c.File)
	if err != nil {
		return fmt.Errorf("migrate push-data: %w", err)
	}
	project, err := projectOr(c.ProjectID)
	if err != nil {
		return fmt.Errorf("migrate push-data: %w", err)
	}
	cl, closeFn, err := registryClient(c.Console)
	if err != nil {
		return fmt.Errorf("migrate push-data: %w", err)
	}
	defer closeFn()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	initiative := storageclient.ResolveCurrentInitiative(c.Console, project, c.Initiative)
	fmt.Fprintf(core.Stdout, "migrate push-data: %s\n", initiative.Describe())
	resp, err := cl.PushDataMigration(ctx, &w17registrypb.PushDataMigrationRequest{
		ProjectId: project, Connection: f.Connection, Statements: f.Statements, Description: f.Description, InitiativeId: initiative.ID,
	})
	if err != nil {
		return fmt.Errorf("migrate push-data: %w", err)
	}
	printCompiled(resp.GetCompiled())
	m := resp.GetMigration()
	fmt.Fprintf(core.Stdout, "migrate push-data: stored %s on %s (sha256 %s)\n", m.GetId(), m.GetConnection(), m.GetContentSha256())
	fmt.Fprintln(core.Stdout, "  every deployed environment applies it with the next deploy, in order; pin it with `w17ctl migrate generate` or the next lock")
	return nil
}

// TryDataCmd is `w17ctl migrate try-data <file>`.
type TryDataCmd struct {
	File      string `arg:"" placeholder:"FILE" help:"The data migration (as push-data takes it)."`
	ProjectID string `name:"project" placeholder:"ID" help:"Project ID. Default: the lock's."`
	Console   string `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console."`
	Commit    bool   `name:"commit" help:"Keep the changes. Without it the run is rolled back: you see what the fix would do and the database is as it was."`
}

func (c *TryDataCmd) Run() error {
	f, err := readDataFile(c.File)
	if err != nil {
		return fmt.Errorf("migrate try-data: %w", err)
	}
	project, err := projectOr(c.ProjectID)
	if err != nil {
		return fmt.Errorf("migrate try-data: %w", err)
	}
	cl, closeFn, err := registryClient(c.Console)
	if err != nil {
		return fmt.Errorf("migrate try-data: %w", err)
	}
	defer closeFn()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	compiled, err := cl.CompileDataMigration(ctx, &w17registrypb.CompileDataMigrationRequest{
		ProjectId: project, Connection: f.Connection, Statements: f.Statements,
	})
	if err != nil {
		return fmt.Errorf("migrate try-data: %w", err)
	}
	printCompiled(compiled)
	dsn, where, err := tryDataDSN(f.Connection)
	if err != nil {
		return fmt.Errorf("migrate try-data: %w", err)
	}
	fmt.Fprintf(core.Stdout, "migrate try-data: on %s\n", where)
	return runDataTry(ctx, compiled, dsn, c.Commit)
}

// runDataTry runs the compiled statements in ONE transaction against dsn and
// reports the rows each affected; rolls back unless commit.
func runDataTry(ctx context.Context, compiled *w17registrypb.CompileDataMigrationResponse, dsn string, commit bool) error {
	driver := map[string]string{"postgres": "pgx", "mysql": "mysql", "sqlite": "sqlite"}[compiled.GetDialect()]
	if driver == "" {
		return fmt.Errorf("migrate try-data: no driver for dialect %q", compiled.GetDialect())
	}
	db, err := sql.Open(driver, dsn)
	if err != nil {
		return fmt.Errorf("migrate try-data: %w", err)
	}
	defer func() { _ = db.Close() }()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("migrate try-data: %w", err)
	}
	done := false
	defer func() {
		if !done {
			_ = tx.Rollback()
		}
	}()
	total := int64(0)
	for i, st := range compiled.GetStatements() {
		for _, q := range st.GetSql() {
			res, err := tx.ExecContext(ctx, q)
			if err != nil {
				return fmt.Errorf("migrate try-data: statement %d (%s %s) failed — nothing is kept: %w", i+1, st.GetKind(), st.GetTable(), err)
			}
			n, _ := res.RowsAffected()
			total += n
			fmt.Fprintf(core.Stdout, "  %d. %s %s: %d row(s)\n", i+1, st.GetKind(), st.GetTable(), n)
		}
	}
	if !commit {
		done = true
		if err := tx.Rollback(); err != nil {
			return fmt.Errorf("migrate try-data: rollback: %w", err)
		}
		fmt.Fprintf(core.Stdout, "migrate try-data: %d row(s) in all — rolled back, the database is as it was (--commit keeps it)\n", total)
		return nil
	}
	done = true
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate try-data: commit: %w", err)
	}
	fmt.Fprintf(core.Stdout, "migrate try-data: %d row(s) in all — committed. This database is changed outside the history: a deploy that applies the pushed migration runs it again here.\n", total)
	return nil
}

func printCompiled(c *w17registrypb.CompileDataMigrationResponse) {
	if c == nil {
		return
	}
	fmt.Fprintf(core.Stdout, "compiled for %s:\n", c.GetDialect())
	for i, st := range c.GetStatements() {
		fmt.Fprintf(core.Stdout, "  %d. %s\n", i+1, strings.TrimSpace(st.GetDql()))
		for _, q := range st.GetSql() {
			fmt.Fprintf(core.Stdout, "     → %s;\n", q)
		}
	}
}

// tryDataDSN is the database try-data runs on: W17_TARGET_<CONN> when set (a
// dump loaded anywhere), else the local stack's store for the connection.
func tryDataDSN(connection string) (dsn, where string, err error) {
	env := "W17_TARGET_" + strings.ToUpper(strings.ReplaceAll(connection, "-", "_"))
	if v := os.Getenv(env); v != "" {
		return v, env, nil
	}
	root, err := core.FindProjectRoot()
	if err != nil {
		return "", "", err
	}
	cfg, err := core.LoadDevConfigFn()
	if err != nil {
		return "", "", fmt.Errorf("load dev config: %w", err)
	}
	name := ""
	if lk, lerr := lockfile.Load(filepath.Join(root, "w17", "lock.yaml")); lerr == nil && lk != nil {
		name = lk.Project
	}
	_, p, _, rerr := cfg.ResolveProject(name, root)
	if rerr != nil {
		return "", "", rerr
	}
	published, asked := localtarget.PublishedPorts(root)
	dsn, skip, _ := localtarget.ResolveLive(connection, p, published, asked)
	if dsn == "" {
		return "", "", fmt.Errorf("no database for connection %q — %s (or set %s to the database to try it on)", connection, skip, env)
	}
	return dsn, "the local stack's " + connection, nil
}

// registryClient dials the console's project registry.
var registryClient = func(console string) (w17registrypb.ProjectRegistryClient, func(), error) {
	addr, err := core.ResolveConsoleAddr(console)
	if err != nil {
		return nil, nil, err
	}
	cl, conn, err := core.DialProjectRegistry(addr)
	if err != nil {
		return nil, nil, fmt.Errorf("connect %s: %w", addr, err)
	}
	return cl, func() { _ = conn.Close() }, nil
}
