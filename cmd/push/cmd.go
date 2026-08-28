package push

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/wandering-compiler/w17ctl/internal/core"
	schemahub "github.com/wandering-compiler/w17ctl/internal/schema"
	"github.com/wandering-compiler/w17ctl/internal/storageclient"
	w17registrypb "github.com/wandering-compiler/sdk/go/pb/w17registry"
)

// Cmd implements `w17ctl push` — the **single** unified
// command that ships every artifact the project has to console
// in one call. Today: compiled IR schema + every
// fixtures/<domain>/<name>.json. Tomorrow: any other artifact
// types we add (initial_data once it lands, …).
//
// Idempotent: calling repeatedly with the same inputs is safe.
// Server-side stores latest body; in the future the
// PR/approval workflow gates promotion, but the wire contract
// stays the same — caller always sends "everything I have."
//
// Per docs/conventions-global/structure.md the operator never
// has to think about "which subcommand"; one command, server
// figures out what changed.
type Cmd struct {
	Protos      []string `name:"proto" short:"p" placeholder:"PROTO" required:"" help:"Path to a .proto schema. Repeatable for multi-file schemas. Routing-style flag (operator decides what's in scope), so flag is fine."`
	Imports     []string `name:"import" short:"I" placeholder:"DIR" help:"Additional proto import path. Repeatable."`
	ProjectID   string   `name:"project" placeholder:"ID" env:"W17_PROJECT_ID" help:"Project identifier. Falls back to W17_PROJECT_ID env var; then to w17/lock.yaml project_id."`
	Console     string   `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of console. Optional — falls back to console_addr in w17/lock.yaml, then the binary's compile-time default."`
	LockPath    string   `name:"lock" placeholder:"PATH" default:"w17/lock.yaml" help:"Path to the lock file. Updated to pin target_migration_id per connection after a successful schema push."`
	NoLock      bool     `name:"no-lock" help:"Skip lock-file write."`
	FixturesDir string   `name:"fixtures-dir" placeholder:"DIR" default:"fixtures" help:"Root of the fixtures tree. Files are read from <dir>/<domain>/<name>.json (default group) or <dir>/<domain>/<group>/<name>.json (named group). Every group is pushed."`
	Decide      []string `name:"decide" placeholder:"DECISION" help:"Resolve a NEEDS_CONFIRM finding so the push isn't blocked. Form: <table>.<col>[:<axis>]=<strategy> or =custom:<sql-file>. Repeatable; parsed + applied by the console."`
	Initiative  string   `name:"initiative" placeholder:"ID" help:"Change-request (initiative) id these migrations belong to. Empty = the initiative of the current git branch, the same one 'w17ctl initiative current' shows. What 'w17ctl migrate squash --initiative' later collapses."`
}

func (c *Cmd) Run() error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	projectID := c.ProjectID
	if projectID == "" {
		projectID = core.LockProjectIDBestEffort()
	}
	if projectID == "" {
		return fmt.Errorf("push: --project / W17_PROJECT_ID / lock project_id not set")
	}

	ir, err := schemahub.LoadIRBytes(ctx, c.Protos, c.Imports, c.Console)
	if err != nil {
		return fmt.Errorf("load schema: %w", err)
	}

	addr, err := core.ResolveConsoleAddr(c.Console)
	if err != nil {
		return err
	}
	cl, conn, err := core.DialProjectRegistry(addr)
	if err != nil {
		return fmt.Errorf("connect %s: %w", addr, err)
	}
	defer func() { _ = conn.Close() }()

	if err := pushSchemaIdempotent(ctx, cl, c.Console, projectID, ir, c.LockPath, c.NoLock, c.Decide, c.Initiative); err != nil {
		return err
	}

	if err := pushFixtures(ctx, cl, projectID, c.FixturesDir); err != nil {
		return err
	}
	return nil
}

// pushSchemaIdempotent pushes the compiled IR in AUTO mode — the server
// resolves first-push (create) vs revision (update) itself, so from the
// operator's POV `push` is one command. The client ships the IR bytes; the
// create/update split is the server's implementation detail.
func pushSchemaIdempotent(ctx context.Context, cl w17registrypb.ProjectRegistryClient, console, projectID string, ir []byte, lockPath string, noLock bool, decide []string, explicitInitiative string) error {
	decideFlags, customSQL, err := schemahub.BuildDecidePayload(decide)
	if err != nil {
		return err
	}
	// Which change request these migrations belong to. Reported either way
	// (see CurrentInitiative.Why) — an unstamped push is one no freeze can
	// ever collapse, so it must not pass unnoticed.
	initiative := storageclient.ResolveCurrentInitiative(console, projectID, explicitInitiative)
	fmt.Fprintf(core.Stdout, "push: %s\n", initiative.Describe())

	resp, err := cl.PushSchema(ctx, &w17registrypb.PushSchemaRequest{
		ProjectId:       projectID,
		Ir:              ir,
		Mode:            w17registrypb.PushMode_PUSH_MODE_AUTO,
		Decide:          decideFlags,
		DecideCustomSql: customSQL,
		InitiativeId:    initiative.ID,
	})
	if err != nil {
		return err
	}
	if findings := resp.GetFindings(); len(findings) > 0 {
		return schemahub.PrintFindingsErr(findings)
	}
	migrations := resp.GetMigrations()

	fmt.Fprintf(core.Stdout, "push: schema — %d migration(s) stored\n", len(migrations))
	for _, m := range migrations {
		fmt.Fprintf(core.Stdout, "  - %s [%s]  sha256=%s\n",
			m.GetId(), m.GetConnection(), schemahub.ShortHash(m.GetContentSha256()))
	}

	if noLock || lockPath == "" {
		return nil
	}
	// Pin the lock to each connection's CURRENT head. When this push
	// stored new migrations, `migrations` already carries them. When
	// the schema was unchanged (0 stored), pin from the console's
	// existing heads instead — otherwise a lock that lost its pin
	// (committed unpinned, hand-reverted, or regenerated) never
	// recovers without a schema change, and `w17ctl migrate fetch` then
	// skips every connection with "no target pinned". A no-op re-push
	// should leave the lock correctly pinned, not stuck unpinned.
	pinFrom := migrations
	if len(pinFrom) == 0 {
		listResp, listErr := cl.ListMigrations(ctx, &w17registrypb.ListMigrationsRequest{ProjectId: projectID})
		if listErr != nil {
			return fmt.Errorf("list migrations to pin lock: %w", listErr)
		}
		pinFrom = listResp.GetMigrations()
	}
	if err := schemahub.PinLockTargets(console, lockPath, projectID, pinFrom); err != nil {
		return fmt.Errorf("pin lock %s: %w", lockPath, err)
	}
	if len(pinFrom) > 0 {
		fmt.Fprintf(core.Stdout, "push: lock pinned %s\n", lockPath)
	}
	return nil
}

// pushFixtures scans the fixtures tree and ships each file's raw JSON to the
// console, which validates it against the project's stored schema server-side
// (table / column / pk-arity) before storing — the client holds no schema /
// fixtures logic. Output is sorted (domain ASC, name ASC) for deterministic
// per-run logs across reruns.
//
// No filters. The unified push is "everything I have"; if
// you want partial scoping, use git or a sparser local tree.
func pushFixtures(ctx context.Context, cl w17registrypb.ProjectRegistryClient, projectID, fixturesDir string) error {
	files, err := discoverFixtureFiles(fixturesDir)
	if err != nil {
		return fmt.Errorf("scan %s: %w", fixturesDir, err)
	}
	if len(files) == 0 {
		fmt.Fprintf(core.Stdout, "push: no fixtures under %s\n", fixturesDir)
		return nil
	}
	for _, f := range files {
		body, err := os.ReadFile(f.path)
		if err != nil {
			return fmt.Errorf("%s: read: %w", f.path, err)
		}
		resp, err := cl.PushFixture(ctx, &w17registrypb.PushFixtureRequest{
			ProjectId: projectID,
			Domain:    f.domain,
			Name:      f.name,
			Json:      body,
		})
		if err != nil {
			return fmt.Errorf("%s: push: %w", f.path, err)
		}
		fmt.Fprintf(core.Stdout, "push: fixture %s/%s (%d row(s))  sha256=%s\n",
			f.domain, f.name, resp.GetRowCount(), schemahub.ShortHash(resp.GetContentSha256()))
	}
	fmt.Fprintf(core.Stdout, "push: %d fixture file(s)\n", len(files))
	return nil
}

// fixtureFile is one discovered file ready to push.
type fixtureFile struct {
	path   string
	domain string
	name   string
}

// discoverFixtureFiles walks `dir` for `<dir>/<domain>/<group...>/<name>.json`.
// A file directly under `<domain>/` is the DEFAULT group; a file nested in
// subdirectories belongs to the named group formed by the subdirectory path.
// The registry `name` is the domain-relative path without `.json` (slash-
// separated), so `app/dev/user.json` pushes as (domain=app, name="dev/user")
// and `fixtures fetch` writes it straight back to the same path — the group is
// carried in the flat name, no wire-level group field. Every group is pushed
// (no dev exclusion): what a target actually applies is the deploy's call, not
// a structural gate. Missing dir is non-fatal — projects without fixtures get
// an empty list, push proceeds with schema-only.
func discoverFixtureFiles(dir string) ([]fixtureFile, error) {
	root, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("absolute path: %w", err)
	}
	if _, err := os.Stat(root); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("stat %s: %w", root, err)
	}

	var out []fixtureFile
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", root, err)
	}
	for _, dirEntry := range entries {
		if !dirEntry.IsDir() {
			continue
		}
		domain := dirEntry.Name()
		domainDir := filepath.Join(root, domain)
		walkErr := filepath.WalkDir(domainDir, func(p string, d os.DirEntry, werr error) error {
			if werr != nil {
				return fmt.Errorf("read %s: %w", p, werr)
			}
			if d.IsDir() || !strings.HasSuffix(d.Name(), ".json") {
				return nil
			}
			rel, err := filepath.Rel(domainDir, p)
			if err != nil {
				return err
			}
			out = append(out, fixtureFile{
				path:   p,
				domain: domain,
				name:   filepath.ToSlash(strings.TrimSuffix(rel, ".json")),
			})
			return nil
		})
		if walkErr != nil {
			return nil, walkErr
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].domain != out[j].domain {
			return out[i].domain < out[j].domain
		}
		return out[i].name < out[j].name
	})
	return out, nil
}
