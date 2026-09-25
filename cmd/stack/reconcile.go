package stack

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	plan "github.com/wandering-compiler/w17ctl/internal/plan"

	codegen "github.com/wandering-compiler/w17ctl/internal/codegen"
	"github.com/wandering-compiler/w17ctl/internal/containerdump"
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
//
// Goes through docker.CaptureComposeFn — which prepends the explicit `-f` —
// rather than shelling out itself. It used to shell out, and that one call
// was enough to undo the scoping everywhere else: a consumer's `stack build`
// still read the compose file at their repo root, because THIS is the call
// that discovers services and it never saw the flag the other nine got.
//
// The rule is worth stating because the bypass is so easy to write: nothing
// in this client runs `docker compose` except through internal/docker, and
// the reason is that the file selection lives there and nowhere else.
// composeRunningFn lists the compose services that are RUNNING right now.
//
// Separate from composeServicesFn because the two answer different questions and
// the difference is load-bearing: `config --services` is every service the file
// DECLARES, and a one-shot seed is declared, ran once and exited. Resuming it
// would run it again — against the database the switch has just restored, which
// is how a reconcile could reapply fixture data over a branch's own.
var composeRunningFn = func(root string) ([]string, error) {
	out, err := docker.CaptureComposeFn(root, append(docker.FileArgs(root), "ps", "--services", "--status", "running")...)
	if err != nil {
		return nil, err
	}
	return splitComposeLines(string(out)), nil
}

// splitComposeLines turns compose's newline-separated service list into names.
func splitComposeLines(out string) []string {
	var svcs []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if s := strings.TrimSpace(line); s != "" {
			svcs = append(svcs, s)
		}
	}
	return svcs
}

var composeServicesFn = func(root string) ([]string, error) {
	out, err := docker.CaptureComposeFn(root, append(docker.FileArgs(root), "config", "--services")...)
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

// runningAmong narrows a list to the services that are running right now.
//
// A ctl with no listRunning (a test's hand-built one) falls back to the whole
// list, which is the pre-existing behaviour: this narrowing removes a resume
// that should not happen, and failing to narrow is the old bug rather than a new
// one. An ERROR is different — the daemon answered something unexpected, and
// resuming a set we could not verify is how a seed gets re-run — so it
// propagates and the switch stops before anything is stopped.
func (c composeCtl) runningAmong(services []string) ([]string, error) {
	if c.listRunning == nil {
		return services, nil
	}
	running, err := c.listRunning()
	if err != nil {
		return nil, fmt.Errorf("list running services: %w", err)
	}
	live := make(map[string]bool, len(running))
	for _, r := range running {
		live[r] = true
	}
	var out []string
	for _, s := range services {
		if live[s] {
			out = append(out, s)
		}
	}
	return out, nil
}

// startArgs is the compose incantation that brings quiesced services back.
//
// NOT `compose start`, which is the symmetric counterpart to the `stop` that
// quiesced them and was the obvious choice. `start` resolves `depends_on` and
// refuses when a dependency has no container — and a one-shot seed service is
// exactly that: it ran, exited, and in an ephemeral stack was never created at
// all. Every example in this repo has one, so the symmetric command failed on
// the common case:
//
//	catalog-storage is missing dependency catalog-storage-seed
//	reconcile: could not restart the services it stopped: exit status 1
//
// `up -d --no-deps --no-recreate` starts the containers that exist and touches
// nothing else: --no-deps because the dependencies were never stopped, so there
// is nothing to bring back, and --no-recreate because recreating a store mid
// reconcile would discard the state the snapshot was just taken from.
// --no-build keeps a missing image a loud failure rather than a silent rebuild
// in the middle of a branch switch.
func startArgs(run func(args []string) error) func(services []string) error {
	return func(services []string) error {
		if len(services) == 0 {
			return nil
		}
		return run(append([]string{"up", "-d", "--no-deps", "--no-recreate", "--no-build"}, services...))
	}
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
	// listRunning is what Resume is built from — see composeRunningFn.
	listRunning func() ([]string, error)
	stop        func(services []string) error
	start       func(services []string) error
}

// composeServiceForFn is the seam the tests substitute: resolving a store's
// compose service goes through the docker daemon, and the thing worth pinning
// is what the quiesce list does with the answer — not the daemon.
var composeServiceForFn = containerdump.ComposeServiceFor

// localComposeCtl is the default (local daemon) compose control.
func localComposeCtl(root string) composeCtl {
	return composeCtl{
		listServices: func() ([]string, error) { return composeServicesFn(root) },
		listRunning:  func() ([]string, error) { return composeRunningFn(root) },
		stop: func(services []string) error {
			return docker.RunComposeFn(root, append(append(docker.FileArgs(root), "stop"), services...)...)
		},
		start: startArgs(func(args []string) error {
			return docker.RunComposeFn(root, append(docker.FileArgs(root), args...)...)
		}),
	}
}

// remoteComposeCtl runs the same operations on the remote host over SSH —
// `docker compose config --services` (parsed from stdout) + `stop`.
func remoteComposeCtl(r remotecompose.Runner) composeCtl {
	return composeCtl{
		listRunning: func() ([]string, error) {
			out, err := remotecompose.Capture(r, "ps", "--services", "--status", "running")
			if err != nil {
				return nil, err
			}
			return splitComposeLines(string(out)), nil
		},
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
		start: startArgs(func(args []string) error {
			return remotecompose.Run(r, nil, args...)
		}),
	}
}

func buildReconcileDeps(root string, cc composeCtl, currentBranch func() string, currentBytes []byte, applierFor migrate.ApplierFor, specs []factory.TargetSpec, console string) (reconcile.Deps, error) {
	if cc.listServices == nil || cc.stop == nil || cc.start == nil {
		cc = localComposeCtl(root)
	}
	quiesced := &[]string{}
	st := snapstore.New(root)
	conns, skipped, err := SnapshotConns(specs)
	if err != nil {
		return reconcile.Deps{}, err
	}
	for _, s := range skipped {
		fmt.Fprintf(core.Stdout, "stack build: reconcile skipping store %s\n", s)
	}

	// Two names per store, and nothing makes them agree: the CONNECTION name
	// the author chose in their targets, and the compose SERVICE name in
	// their stack file. Excluding only the first stopped the store wherever
	// they differed, and the snapshot then failed inside a container that was
	// no longer running (marb #57). Both are excluded now; the service is
	// resolved through the container that publishes the store's port, which
	// is the one comparison that does not assume the names match.
	storeNames := map[string]bool{}
	connNames := make([]string, 0, len(specs))
	svcCtx, svcCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer svcCancel()
	for _, s := range specs {
		storeNames[s.Connection] = true
		connNames = append(connNames, s.Connection)
		if svc := composeServiceForFn(svcCtx, s.DSN); svc != "" {
			storeNames[svc] = true
		}
	}

	logf := func(format string, args ...any) { fmt.Fprintf(core.Stdout, format+"\n", args...) }

	return reconcile.Deps{
		CurrentBranch: currentBranch,
		LastLive:      st.LastLive,
		SetLastLive:   st.SetLastLive,
		HasSnapshot:   st.Has,
		// quiesced records what Quiesce actually stopped, so Resume can put
		// back exactly that and nothing else.
		Quiesce: func(context.Context) error {
			all, err := cc.listServices()
			if err != nil {
				return fmt.Errorf("list services: %w", err)
			}
			stop := nonStoreServices(all, storeNames)
			if len(stop) == 0 {
				return nil
			}
			// What to bring back afterwards is decided BEFORE stopping
			// anything, and it is what was actually RUNNING — not what the
			// compose file declares. A one-shot seed service is declared,
			// ran once and exited; resuming it runs it AGAIN, against the
			// database this switch has just restored. `stop` still covers
			// everything non-store, because stopping an already-stopped
			// service costs nothing and a service that starts between the
			// two calls must not keep writing during the dump.
			resume, rerr := cc.runningAmong(stop)
			if rerr != nil {
				return rerr
			}
			if err := cc.stop(stop); err != nil {
				return err
			}
			*quiesced = append((*quiesced)[:0], resume...)
			return nil
		},
		// A compose `stop` is EXPLICIT, and `restart: unless-stopped` is
		// defined not to undo one — so before this existed, every branch
		// switch left the project's gateway and business services down until
		// someone noticed and ran `stack up`. It read as an infrastructure
		// outage with no cause, because the command that caused it had
		// already reported success (marb #57).
		Resume: func(context.Context) error {
			if len(*quiesced) == 0 {
				return nil
			}
			svcs := append([]string(nil), *quiesced...)
			*quiesced = (*quiesced)[:0]
			return cc.start(svcs)
		},
		// A snapshot that does not cover every store is not a snapshot of this
		// project, and until 2026-09-24 a skipped store only printed a line
		// (above) before the switch went ahead anyway.
		//
		// What that produced: a branch directory holding one of two stores,
		// which `Has` — it stats the DIRECTORY — reports as a snapshot. The
		// switch proceeds, the uncovered store is overwritten with nothing
		// standing behind it, and coming back to that branch fails at restore
		// with `open …/authplatform-postgres.sql: no such file or directory`.
		// marb had six of those directories (#68).
		//
		// Refused HERE rather than inside snapstore because this is the only
		// place that knows a store was left out — snapstore is handed the conns
		// that survived and cannot tell a two-store project from a one-store
		// one. The refusal reaches the caller as a failed Dump, which stops the
		// switch before anything changes and prints the two ways on.
		Dump: func(ctx context.Context, branch string) error {
			if len(skipped) > 0 {
				return fmt.Errorf("%d of this project's stores could not be snapshotted (%s), so the snapshot would cover only %d of them — a partial snapshot is not one, and switching on it loses whatever the uncovered store holds",
					len(skipped), strings.Join(skipped, "; "), len(conns))
			}
			return st.Save(ctx, branch, conns)
		},
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
			// No connection list here: a reconcile builds from NIL base
			// (a full create), so there is no checkpoint claim to verify
			// against — the comparison exists to catch a base that lies,
			// and an empty base cannot.
			_, err := plan.DevPlanAndApply(ctx, nil, currentBytes, applierFor, nil, logf)
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
// declaredRolesFixture is the file codegen writes from `(w17.acl_roles)`. It
// is a DECLARATION, not data: the author wrote it beside their models, and it
// is regenerated from them on every codegen.
const declaredRolesFixture = "acl-roles.json"

// seedDeclaredRoles applies ONLY the generated role fixtures, and is called on
// every `stack build` — the way the schema is.
//
// Roles used to reach a local database by exactly one route: a fixture seed
// inside the reconcile's FRESH arm, which runs only when a branch switch finds
// no snapshot. A first build, and every same-branch build, returns from
// reconcile before that. So a project that declared a bootstrap role and ran
// `codegen && stack build` got a schema with `select count(*) from auth_role`
// = 0, and nothing said so.
//
// What that costs is not "a missing row". `first_user` has nothing to grant,
// so the first account registers with NO roles — and the flag is spent, so the
// one chance is gone. The result is a sign-in that cannot invite anybody and a
// database effectively closed, reached through a SignUp that returned 200 and
// a token (marb, 2026-09-23).
//
// Only the generated role file, deliberately. Hand-authored data fixtures keep
// the behaviour they have: their seeds are upserts, and re-applying them on
// every build would reset rows a developer had edited between builds. Roles
// carry no such expectation — they are regenerated from the proto anyway.
func seedDeclaredRoles(ctx context.Context, root string, schemaBytes []byte, specs []factory.TargetSpec, console string, logf func(string, ...any)) error {
	return seedFixturesFiltered(ctx, root, schemaBytes, specs, console, logf, isDeclaredRolesSeed)
}

// isDeclaredRolesSeed is THE predicate — named so the test can call it rather
// than restate it. A test that re-implements a filter agrees with its own copy
// and goes on passing while the real one changes underneath (the first version
// of the test here did exactly that, and let a break through).
func isDeclaredRolesSeed(s fixtureSeed) bool {
	return filepath.Base(s.path) == declaredRolesFixture
}

func seedFixtures(ctx context.Context, root string, schemaBytes []byte, specs []factory.TargetSpec, console string, logf func(string, ...any)) error {
	return seedFixturesFiltered(ctx, root, schemaBytes, specs, console, logf, nil)
}

func seedFixturesFiltered(ctx context.Context, root string, schemaBytes []byte, specs []factory.TargetSpec, console string, logf func(string, ...any), keep func(fixtureSeed) bool) error {
	fixturesDir := filepath.Join(root, "fixtures")
	seeds, err := collectFixtureSeeds(fixturesDir)
	if keep != nil && err == nil {
		var filtered []fixtureSeed
		for _, s := range seeds {
			if keep(s) {
				filtered = append(filtered, s)
			}
		}
		seeds = filtered
	}
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

	// Connect the FIRST local store first (the dev-side thing most likely
	// misconfigured), then the console renderer. The others are opened on
	// demand below — a project whose second store is only used by a domain
	// with no fixtures should not need it reachable.
	conn, err := pgx.Connect(ctx, pgDSN)
	if err != nil {
		return fmt.Errorf("seed fixtures: dial postgres: %w", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	// One connection per store, and the store is named by the RENDERER.
	//
	// Every domain's fixtures used to go through `pgDSN` — whichever postgres
	// target came first. With two postgres stores that seeded the second
	// domain's rows into the first domain's database: `relation "auth_role"
	// does not exist` where the table was absent, and a silent write into the
	// wrong store where a same-named table was there (marb #67, marbai-04 §3).
	//
	// The client cannot work out the mapping — it uploads the IR as opaque
	// bytes and never decodes it — so the console returns the connection name
	// alongside the statements, read from the domain-filtered schema.
	dsnByConn := map[string]string{}
	for _, sp := range specs {
		if d, ok := codegen.DialectFromConnectionName(sp.Connection); ok && d == "postgres" {
			dsnByConn[sp.Connection] = sp.DSN
		}
	}
	open := map[string]*pgx.Conn{pgDSN: conn}
	defer func() {
		for dsn, c := range open {
			if dsn != pgDSN {
				_ = c.Close(ctx)
			}
		}
	}()
	warnedUnnamed := false

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
		// Where these rows go. An empty name is an OLD console: keep today's
		// behaviour rather than change what it does under the operator, but
		// say so — on a multi-store project that silence is the defect.
		target := resp.GetConnection()
		dsn := pgDSN
		switch target {
		case "":
			if !warnedUnnamed && len(dsnByConn) > 1 {
				warnedUnnamed = true
				logf("reconcile: this console does not say which store a fixture belongs to, so all %d domains are seeded through %q — "+
					"on a project with more than one postgres store that is how a domain's rows land in another domain's database. Deploy a console built from this version.",
					len(dsnByConn), firstPostgresConnName(specs))
			}
		default:
			d, rerr := seedTargetDSN(target, dsnByConn)
			if rerr != nil {
				return fmt.Errorf("seed fixtures %s/%s: %w", s.domain, disp, rerr)
			}
			dsn = d
		}
		c, ok := open[dsn]
		if !ok {
			c, err = pgx.Connect(ctx, dsn)
			if err != nil {
				return fmt.Errorf("seed fixtures %s/%s: dial %s: %w", s.domain, disp, target, err)
			}
			open[dsn] = c
		}
		if err := applySeedStmts(ctx, c, stmts); err != nil {
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

// firstPostgresConnName is firstPostgresDSN's companion, for a message that
// names the store rather than printing a DSN (which carries a password).
func firstPostgresConnName(specs []factory.TargetSpec) string {
	for _, s := range specs {
		if d, ok := codegen.DialectFromConnectionName(s.Connection); ok && d == "postgres" {
			return s.Connection
		}
	}
	return ""
}

// sortedKeys renders a map's keys in a stable order, so a refusal naming the
// available targets does not shuffle between runs.
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// seedTargetDSN resolves the store a rendered fixture belongs to.
//
// Separated from the loop so the decision can be driven without a live
// postgres: it is the whole of marb #67, and a fix nothing exercises is the
// shape that let the defect ship.
//
// A named connection with no matching target is a REFUSAL, not a fallback to
// the default store. Falling back is what the old code did implicitly, and it
// is how a domain's rows reached another domain's database — silently, where a
// table of the same name happened to exist.
func seedTargetDSN(target string, dsnByConn map[string]string) (string, error) {
	if dsn, ok := dsnByConn[target]; ok {
		return dsn, nil
	}
	return "", fmt.Errorf("the schema puts this domain on connection %q, and no postgres target names it (targets: %s) — "+
		"pass it with `--target %s=<dsn>`, or the rows would go to a store that is not this domain's",
		target, strings.Join(sortedKeys(dsnByConn), ", "), target)
}
