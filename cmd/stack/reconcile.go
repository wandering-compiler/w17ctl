package stack

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	plan "github.com/wandering-compiler/w17ctl/internal/plan"

	codegen "github.com/wandering-compiler/w17ctl/internal/codegen"
	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/docker"
	"github.com/wandering-compiler/w17ctl/internal/remotecompose"

	"github.com/jackc/pgx/v5"

	"github.com/wandering-compiler/w17ctl/internal/reconcile"
	"github.com/wandering-compiler/w17ctl/internal/snapstore"
	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	"github.com/wandering-compiler/sdk/go/tooling/migrate"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/factory"
)

// composeServicesFn captures `docker compose config --services` (the
// full service inventory). A package var so tests can stub it.
var composeServicesFn = func(root string) ([]string, error) {
	cmd := exec.Command("docker", "compose", "config", "--services")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var svcs []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if s := strings.TrimSpace(line); s != "" {
			svcs = append(svcs, s)
		}
	}
	return svcs, nil
}

// nonStoreServices returns the compose services that are NOT stateful
// stores. The store services are named after the connections
// (composegen uses the connection name as the service name), so quiesce
// stops everything else — leaving the stores up for a consistent dump.
func nonStoreServices(all []string, storeNames map[string]bool) []string {
	var out []string
	for _, s := range all {
		if !storeNames[s] {
			out = append(out, s)
		}
	}
	return out
}

// buildReconcileDeps assembles the branch-switch reconcile effects from
// the resolved build inputs. Dump/Restore reuse the snapstore +
// Snapshotters; Quiesce stops the non-store services; BuildFresh wipes
// each store in place (migrate.Wiper) then applies nil→current;
// SeedFixtures seeds the project's fixtures into the freshly-built stores.
// composeCtl abstracts the docker-compose operations reconcile's Quiesce
// needs (list services + stop non-store ones) so they run against the
// LOCAL daemon or a REMOTE host per the build's resolved mode. build
// constructs the right one; a nil cc falls back to local (the historical
// path), so callers that don't set it keep working.
type composeCtl struct {
	listServices func() ([]string, error)
	stop         func(services []string) error
}

// localComposeCtl is the default (local daemon) compose control.
func localComposeCtl(root string) composeCtl {
	return composeCtl{
		listServices: func() ([]string, error) { return composeServicesFn(root) },
		stop: func(services []string) error {
			return docker.RunComposeFn(root, append([]string{"stop"}, services...)...)
		},
	}
}

// remoteComposeCtl runs the same operations on the remote host over SSH —
// `docker compose config --services` (parsed from stdout) + `stop`.
func remoteComposeCtl(r remotecompose.Runner) composeCtl {
	return composeCtl{
		listServices: func() ([]string, error) {
			out, err := remotecompose.Capture(r, "config", "--services")
			if err != nil {
				return nil, err
			}
			var svcs []string
			for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
				if s := strings.TrimSpace(line); s != "" {
					svcs = append(svcs, s)
				}
			}
			return svcs, nil
		},
		stop: func(services []string) error {
			return remotecompose.Run(r, nil, append([]string{"stop"}, services...)...)
		},
	}
}

func buildReconcileDeps(root string, cc composeCtl, currentBranch func() string, currentBytes []byte, applierFor migrate.ApplierFor, specs []factory.TargetSpec, console string) (reconcile.Deps, error) {
	if cc.listServices == nil || cc.stop == nil {
		cc = localComposeCtl(root)
	}
	st := snapstore.New(root)
	conns, skipped, err := SnapshotConns(specs)
	if err != nil {
		return reconcile.Deps{}, err
	}
	for _, s := range skipped {
		fmt.Fprintf(core.Stdout, "stack build: reconcile skipping store %s\n", s)
	}

	storeNames := map[string]bool{}
	connNames := make([]string, 0, len(specs))
	for _, s := range specs {
		storeNames[s.Connection] = true
		connNames = append(connNames, s.Connection)
	}

	logf := func(format string, args ...any) { fmt.Fprintf(core.Stdout, format+"\n", args...) }

	return reconcile.Deps{
		CurrentBranch: currentBranch,
		LastLive:      st.LastLive,
		SetLastLive:   st.SetLastLive,
		HasSnapshot:   st.Has,
		Quiesce: func(context.Context) error {
			all, err := cc.listServices()
			if err != nil {
				return fmt.Errorf("list services: %w", err)
			}
			stop := nonStoreServices(all, storeNames)
			if len(stop) == 0 {
				return nil
			}
			return cc.stop(stop)
		},
		Dump:    func(ctx context.Context, branch string) error { return st.Save(ctx, branch, conns) },
		Restore: func(ctx context.Context, branch string) error { return st.Load(ctx, branch, conns) },
		BuildFresh: func(ctx context.Context) error {
			// Fresh build: wipe each store IN PLACE (narrow — only the
			// connected store's data, via migrate.Wiper; no `compose down -v`
			// that would nuke unrelated volumes, and no container teardown /
			// readiness dance) then apply the whole current schema. The
			// outgoing branch was already snapshotted, so the wipe is
			// recoverable. currentBytes is the opaque compiled IR (the plan
			// RPC takes it verbatim — the client never decodes it).
			if err := wipeStores(ctx, applierFor, connNames, logf); err != nil {
				return err
			}
			_, err := plan.DevPlanAndApply(ctx, nil, currentBytes, applierFor, logf)
			return err
		},
		SeedFixtures: func(ctx context.Context) error {
			// The console renders each fixture server-side (schema-aware) from
			// the uploaded IR bytes; the client only executes the returned
			// statements.
			return seedFixtures(ctx, root, currentBytes, specs, console, logf)
		},
		Logf: logf,
	}, nil
}

// wipeStores empties each store in place via its migrate.Wiper (PG drops
// the public schema; Redis FLUSHDBs; SQLite/MySQL drop their tables).
// A store whose Applier has no Wiper (schemaless NATS/S3) is logged +
// skipped — the relational fresh-build doesn't target it. Only the
// connected store is touched; unrelated docker volumes are untouched.
func wipeStores(ctx context.Context, applierFor migrate.ApplierFor, conns []string, logf func(string, ...any)) error {
	for _, c := range conns {
		a, err := applierFor(c)
		if err != nil {
			return fmt.Errorf("wipe %s: %w", c, err)
		}
		w, ok := a.(migrate.Wiper)
		if !ok {
			_ = a.Close()
			logf("reconcile: store %q has no wipe support — skipping (fresh build won't reset it)", c)
			continue
		}
		err = w.Wipe(ctx)
		_ = a.Close()
		if err != nil {
			return fmt.Errorf("wipe %s: %w", c, err)
		}
	}
	return nil
}

// seedFixtures applies the project's hand-authored fixtures into the
// freshly-built stores after a fresh branch build, so dev has working seed
// data. It seeds the DEFAULT group (`fixtures/<domain>/<name>.json`) plus the
// conventional `dev` group (`fixtures/<domain>/dev/<name>.json`) — see
// localSeedGroups; other named groups are on-demand (`fixtures apply
// --group`). PG-only (the fixtures engine is relational/Postgres in F-1).
//
// Public-split (Block 3): the schema-aware render runs SERVER-side — the
// client uploads the compiled IR bytes + each local fixture's raw JSON to the
// console's FixtureFetch.RenderFixtureSeed and executes the returned
// parameterized upserts against its local postgres store. So the client holds
// no fixtures package / irpb. No fixtures dir / no postgres store ⇒ a logged
// no-op.
func seedFixtures(ctx context.Context, root string, schemaBytes []byte, specs []factory.TargetSpec, console string, logf func(string, ...any)) error {
	fixturesDir := filepath.Join(root, "fixtures")
	seeds, err := collectFixtureSeeds(fixturesDir)
	if err != nil {
		return fmt.Errorf("seed fixtures: %w", err)
	}
	if len(seeds) == 0 {
		return nil // no fixtures to seed (incl. a missing fixtures/ dir)
	}
	pgDSN := firstPostgresDSN(specs)
	if pgDSN == "" {
		logf("reconcile: no postgres store among targets — skipping fixtures (PG-only)")
		return nil
	}

	// Connect the local store first (the dev-side thing most likely
	// misconfigured), then the console renderer.
	conn, err := pgx.Connect(ctx, pgDSN)
	if err != nil {
		return fmt.Errorf("seed fixtures: dial postgres: %w", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	addr, err := core.ResolveConsoleAddr(console)
	if err != nil {
		return fmt.Errorf("seed fixtures: %w", err)
	}
	cl, fconn, err := core.DialFixtureFetch(addr)
	if err != nil {
		return fmt.Errorf("seed fixtures: connect %s: %w", addr, err)
	}
	defer func() { _ = fconn.Close() }()

	rows := 0
	for _, s := range seeds {
		if !localSeedGroups[s.group] {
			// On-demand group (e.g. `demo`) — not auto-seeded by `stack up`.
			// Apply it explicitly with `w17ctl fixtures apply --group <g>`.
			continue
		}
		disp := filepath.Base(s.path)
		if s.group != "" {
			disp = s.group + "/" + disp
		}
		body, err := os.ReadFile(s.path)
		if err != nil {
			return fmt.Errorf("seed fixtures %s/%s: %w", s.domain, disp, err)
		}
		// Render server-side: the console filters the schema to the domain
		// (empty → no statements, so a domain not in the current schema is
		// skipped) and emits parameterized upserts.
		resp, err := cl.RenderFixtureSeed(ctx, &applyfetchpb.RenderFixtureSeedRequest{
			Ir:     schemaBytes,
			Domain: s.domain,
			Json:   body,
		})
		if err != nil {
			return fmt.Errorf("seed fixtures %s/%s: render: %w", s.domain, disp, err)
		}
		stmts := resp.GetStatements()
		if len(stmts) == 0 {
			continue
		}
		if err := applySeedStmts(ctx, conn, stmts); err != nil {
			return fmt.Errorf("seed fixtures %s/%s: apply: %w", s.domain, disp, err)
		}
		rows += len(stmts)
	}
	if rows > 0 {
		logf("reconcile: seeded %d fixture row(s)", rows)
	}
	return nil
}

// localSeedGroups are the fixture groups `stack up` seeds into the local dev
// stores automatically: the DEFAULT group ("") plus the conventional "dev"
// group (everyday local dev data). Other named groups are on-demand
// (`w17ctl fixtures apply --group <g>`) so one-off/demo groups don't land in a
// plain `stack up`. This is a LOCAL-convenience convention only — NOT a
// prod-safety gate: `push` ships every group (incl. dev) to the registry, and
// what a deployed environment applies is the deploy's call.
var localSeedGroups = map[string]bool{"": true, "dev": true}

// fixtureSeed is one fixture file to seed: its domain, its group ("" = default
// group), and the file path.
type fixtureSeed struct {
	domain string
	group  string
	path   string
}

// collectFixtureSeeds walks fixtures/<domain>/<group...>/<name>.json, tagging
// each seed with its domain + group (a file directly under <domain>/ is the
// default group ""). A missing fixturesDir yields nil (no fixtures, not an
// error). It returns EVERY group; filtering to the auto-seeded set
// (localSeedGroups) is the caller's job.
func collectFixtureSeeds(fixturesDir string) ([]fixtureSeed, error) {
	entries, err := os.ReadDir(fixturesDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", fixturesDir, err)
	}
	var out []fixtureSeed
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		domain := e.Name()
		domainDir := filepath.Join(fixturesDir, domain)
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
			group := filepath.ToSlash(filepath.Dir(rel))
			if group == "." {
				group = ""
			}
			out = append(out, fixtureSeed{domain: domain, group: group, path: p})
			return nil
		})
		if walkErr != nil {
			return nil, walkErr
		}
	}
	return out, nil
}

// applySeedStmts runs every rendered SeedStmt in ONE transaction, args bound as
// PARAMETERS ($1..$N the server already wrote) — never interpolated into SQL.
func applySeedStmts(ctx context.Context, conn *pgx.Conn, stmts []*applyfetchpb.SeedStmt) error {
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	for _, st := range stmts {
		args := make([]any, len(st.GetArgs()))
		for j, a := range st.GetArgs() {
			args[j] = a.AsInterface()
		}
		if _, err := tx.Exec(ctx, st.GetSql(), args...); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
	}
	return tx.Commit(ctx)
}

// firstPostgresDSN returns the DSN of the first postgres target (the
// fixtures engine is PG-only in F-1), or "" when none.
func firstPostgresDSN(specs []factory.TargetSpec) string {
	for _, s := range specs {
		if d, ok := codegen.DialectFromConnectionName(s.Connection); ok && d == "postgres" {
			return s.DSN
		}
	}
	return ""
}
