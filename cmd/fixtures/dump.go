package fixtures

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/localtarget"
	"github.com/wandering-compiler/w17ctl/internal/schema"
	codegenpb "github.com/wandering-compiler/sdk/go/pb/w17compiler"
)

// DumpCmd implements `w17ctl fixtures dump` — read a live store back into an
// authorable fixture.
//
// The client composes NOTHING here. It ships the IR to the console, receives
// one statement per model, runs them against a store the console cannot
// reach, and writes what comes back. The mapping from tables to models and
// the per-carrier conversions live on the console because they are compiler
// knowledge: a public client carrying them turns a conversion fix into a
// client release every consumer has to take.
type DumpCmd struct {
	Connection  string   `name:"connection" placeholder:"CONN" required:"" help:"Connection to read. Its tables are dumped; others are left alone."`
	Domain      string   `name:"domain" placeholder:"DOMAIN" help:"Write under this domain's directory. REQUIRED: the domain decides whether the fixture renders at all, and it cannot be derived from the connection."`
	Out         string   `name:"out" placeholder:"DIR" default:"fixtures" help:"Authorable fixtures root to write into."`
	Name        string   `name:"name" placeholder:"NAME" default:"dump" help:"File name (without .json) to write."`
	Limit       int64    `name:"limit" placeholder:"N" default:"1000" help:"Rows per table. A table with more is REFUSED rather than truncated; pass 0 for no cap."`
	Protos      []string `name:"proto" short:"p" placeholder:"PROTO" help:"Path to a .proto schema. Repeatable; discovered from the project when omitted."`
	Imports     []string `name:"import" short:"I" placeholder:"DIR" help:"IGNORED — the console compiles."`
	ProtoDirFlg string   `name:"proto-dir" placeholder:"DIR" help:"Where to discover model protos. Defaults to the lock's proto dir."`
	Console     string   `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console."`
}

func (c *DumpCmd) Run() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	rc := &RenderCmd{Protos: c.Protos, Imports: c.Imports, FixturesDir: c.Out, Console: c.Console}
	protos, imports, cleanup, err := rc.resolveProtos()
	if err != nil {
		return fmt.Errorf("fixtures dump: %w", err)
	}
	defer cleanup()
	if len(protos) == 0 {
		return fmt.Errorf(
			"fixtures dump: no model protos found\n" +
				"  why: the console needs the schema to know which table is which model, and an empty set describes nothing\n" +
				"  fix: pass --proto explicitly, or run this from the project root")
	}
	ir, err := schema.LoadIRBytes(ctx, protos, imports, c.Console)
	if err != nil {
		return fmt.Errorf("fixtures dump: load schema: %w", err)
	}

	addr, err := core.ResolveConsoleAddr(c.Console)
	if err != nil {
		return err
	}
	cl, conn, err := core.DialCodegen(addr)
	if err != nil {
		return fmt.Errorf("connect %s: %w", addr, err)
	}
	defer func() { _ = conn.Close() }()

	resp, err := cl.DumpFixtures(ctx, &codegenpb.DumpFixturesRequest{
		Ir:         ir,
		Connection: c.Connection,
		Limit:      c.Limit,
	})
	if err != nil {
		return fmt.Errorf("fixtures dump: %w", err)
	}

	dsn, err := c.resolveDSN()
	if err != nil {
		return err
	}
	db, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return fmt.Errorf("fixtures dump: connect %s: %w", c.Connection, err)
	}
	defer func() { _ = db.Close(ctx) }()

	rows, err := c.readAll(ctx, db, resp.GetQueries())
	if err != nil {
		return err
	}
	return c.write(rows)
}

// readAll runs the console's statements in the order they came — that order
// is an FK toposort, so re-applying what this writes never puts a child
// before its parent — and collects the JSON values.
func (c *DumpCmd) readAll(ctx context.Context, db *pgx.Conn, queries []*codegenpb.DumpFixtureQuery) ([]json.RawMessage, error) {
	var out []json.RawMessage
	for _, q := range queries {
		rs, err := db.Query(ctx, q.GetSql())
		if err != nil {
			return nil, fmt.Errorf("fixtures dump: %s: %w", q.GetModel(), err)
		}
		var got []json.RawMessage
		for rs.Next() {
			var raw []byte
			if err := rs.Scan(&raw); err != nil {
				rs.Close()
				return nil, fmt.Errorf("fixtures dump: %s: %w", q.GetModel(), err)
			}
			got = append(got, json.RawMessage(raw))
		}
		rs.Close()
		if err := rs.Err(); err != nil {
			return nil, fmt.Errorf("fixtures dump: %s: %w", q.GetModel(), err)
		}
		if err := overflow(q.GetModel(), q.GetLimit(), len(got)); err != nil {
			return nil, err
		}
		out = append(out, got...)
	}
	return out, nil
}

// overflow reports the table that does not fit.
//
// The statement asked for one row MORE than the cap, so an extra row here
// means the table is larger than what was asked for. Refusing beats writing
// what came back: a fixture is a curated starting point, and a truncated one
// seeds a partial world while looking whole — the same shape as a dump that
// skipped a table nobody asked about.
func overflow(model string, limit int64, got int) error {
	if limit <= 0 || int64(got) <= limit {
		return nil
	}
	return fmt.Errorf(
		"fixtures dump: %s has more than %d row(s)\n"+
			"  why: a fixture is a curated starting point, not a backup — writing the first %d would seed a partial world that looks whole\n"+
			"  fix: raise --limit, or narrow what you are dumping (a fixture somebody has to read is usually far smaller than a table)",
		model, limit, limit)
}

// fixtureDoc renders the rows as the on-disk fixture document.
//
// The rows go in as raw JSON the console shaped — decoding them into Go
// values here and re-encoding them would be this client forming an opinion
// about their contents, which is the thing the split exists to prevent.
// MarshalIndent still lays the whole document out, embedded rows included,
// so the file stays something a person can edit.
func fixtureDoc(rows []json.RawMessage) ([]byte, error) {
	if rows == nil {
		rows = []json.RawMessage{}
	}
	b, err := json.MarshalIndent(struct {
		Rows []json.RawMessage `json:"rows"`
	}{Rows: rows}, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// write puts the document where `fixtures render` will find it.
func (c *DumpCmd) write(rows []json.RawMessage) error {
	// No default, and no guess.
	//
	// It used to fall back to the CONNECTION name, which is right only when
	// the two happen to be spelled alike. `--connection core-postgres` wrote
	// `fixtures/core-postgres/`, `render` read the domain from that directory,
	// resolved none of the models under it, and reported 0 statement(s) with
	// exit 0 — 642 rows dumped, nothing seeded, no word said (deinvo,
	// 2026-09-21).
	//
	// Deriving it is not available: the dumped rows carry `<module>.<Message>`
	// and a module is a PLUGIN ACTIVATION as often as a domain (a fixture
	// under `fixtures/app/` legitimately holds `auth.Role`), and one dump of
	// one database mixes both. The lock view the client can see carries a
	// connection's name and whether it is the default — not its domain. So
	// the honest answer is to ask rather than to pick, and `render` now
	// refuses the silent-zero case as well.
	domain := c.Domain
	if domain == "" {
		return fmt.Errorf(
			"fixtures dump: --domain is required\n" +
				"  why: `fixtures render` takes a fixture's domain from the DIRECTORY it sits\n" +
				"       in, so the wrong one renders zero statements and reports success\n" +
				"  fix: pass the domain that declares these models, e.g. `--domain core`\n" +
				"       (it is NOT the connection name unless the two are spelled alike)")
	}
	dir := filepath.Join(c.Out, domain)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("fixtures dump: %w", err)
	}
	path := filepath.Join(dir, c.Name+".json")
	doc, err := fixtureDoc(rows)
	if err != nil {
		return fmt.Errorf("fixtures dump: %w", err)
	}
	if err := os.WriteFile(path, doc, 0o644); err != nil {
		return fmt.Errorf("fixtures dump: %w", err)
	}
	fmt.Fprintf(core.Stdout, "fixtures dump: %d row(s) → %s\n", len(rows), path)
	return nil
}

// resolveDSN mirrors `fixtures apply`: an explicit W17_TARGET_<CONN> wins (a
// remote DSN is a secret and must not ride a flag), otherwise the local dev
// store resolved the same way `stack build` and `db snapshot` resolve it.
func (c *DumpCmd) resolveDSN() (string, error) {
	if v := os.Getenv(envVarName(c.Connection)); v != "" {
		return v, nil
	}
	root, err := core.FindProjectRoot()
	if err != nil {
		return "", fmt.Errorf("fixtures dump: %w", err)
	}
	cfg, err := core.LoadDevConfigFn()
	if err != nil {
		return "", fmt.Errorf("fixtures dump: load dev config: %w", err)
	}
	_, p := cfg.FindByPath(root)
	dsn, skip := localtarget.ResolveDSN(c.Connection, p)
	if dsn == "" {
		return "", fmt.Errorf("fixtures dump: no DSN for connection %q — %s (or set %s for a remote target)",
			c.Connection, skip, envVarName(c.Connection))
	}
	return dsn, nil
}
