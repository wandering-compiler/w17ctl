package migrate

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/schema"
	"github.com/wandering-compiler/w17ctl/internal/storageclient"
	w17registrypb "github.com/wandering-compiler/sdk/go/pb/w17registry"
)

// ====================================================================
// `w17ctl migrate replace` — repair a stored migration that cannot apply
// ====================================================================
//
// The client half of `ResetHistory(REPLACE)`. A minted migration whose body
// fails on a real database stops every deploy of the lock that pins it, and
// none of the other history commands gets past it: push / generate / push-raw
// APPEND (the broken body still runs first), squash adopts only on a database
// at the collapsed head, and reset forgets what deployed databases recorded.
//
// The console supersedes the named migration and every live migration after
// it on its connection — KEEPING them, listed with what replaced them — and
// stores ONE replacement planned from the schema before the named migration
// to the schema stored now, by the console's current planner, with the
// decisions passed here. Then this command re-pins the lock to it.
//
// It is a history rewrite, so it is authorised as one: it rides on the
// ResetHistory RPC, and whoever may run `migrate squash` / `reset` on the
// project may run it — never the ci-push or deploy-fetch roles. A project
// locked to CI (`ci_only`) refuses it from a person like any other mint.
//
// The premise is that NO environment applied the migration. The console
// refuses when a database's latest report says it applied it (every `<binary> migrate apply
// --fetch` and `migrate status --fetch` reports its database's applied head),
// but absence of a report proves nothing, so `--confirm-not-applied` is the
// operator's statement, required every time. And a database that applied it
// regardless refuses to deploy the replacement on top of it.

// ReplaceCmd implements `w17ctl migrate replace`.
type ReplaceCmd struct {
	MigrationID       string   `arg:"" name:"migration-id" help:"The stored migration that cannot apply — the id 'w17ctl migrate list' prints. It and every later migration on its connection are superseded (kept, never served again) and replaced by one migration planned from the schema before it."`
	Connection        string   `name:"connection" short:"c" placeholder:"NAME" help:"Connection the migration is on. Empty = the one connection that holds the id."`
	ConfirmNotApplied bool     `name:"confirm-not-applied" help:"State that NO environment applied the migration — run '<binary> migrate status --fetch' in each one first; it must list the migration as pending. Required: without it the command prints what would happen and refuses."`
	Decide            []string `name:"decide" placeholder:"DECISION" help:"Resolve a NEEDS_CONFIRM finding of the replacement plan, exactly as 'migrate generate --decide' does: <table>.<col>[:<axis>]=<strategy>, or =custom:<sql-file>. Repeatable. The usual repair for a body that failed on production data is a different strategy for the change it made."`
	Initiative        string   `name:"initiative" placeholder:"ID" help:"Change request the replacement belongs to. Empty = the initiative of the current git branch."`
	ProjectID         string   `name:"project" placeholder:"ID" help:"Project whose migration is replaced. Empty = project_id from w17/lock.yaml."`
	Console           string   `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console ProjectRegistry. Optional — falls back to console_addr in w17/lock.yaml, then to the binary's compile-time default."`
	LockPath          string   `name:"lock" placeholder:"PATH" default:"w17/lock.yaml" help:"Path to the lock file, re-pinned to the replacement afterwards."`
	NoLock            bool     `name:"no-lock" help:"Skip the lock re-pin (then run 'w17ctl migrate generate' with the proto unchanged — it stores nothing and pins every connection to its head)."`
}

func (c *ReplaceCmd) Run() error {
	projectID := c.ProjectID
	if projectID == "" {
		projectID = core.LockProjectIDBestEffort()
	}
	if projectID == "" {
		return fmt.Errorf("no project id: pass --project, or run inside a project whose lock carries project_id")
	}

	// The banner runs before the confirmation check on purpose: someone who
	// typed the command without the flag should still read what it does.
	fmt.Fprintf(core.Stdout, "w17ctl migrate replace %s — repair a stored migration that cannot apply.\n", c.MigrationID)
	fmt.Fprintln(core.Stdout, "      • it and every later migration on its connection are SUPERSEDED: kept and listed, never served to apply again")
	fmt.Fprintln(core.Stdout, "      • ONE replacement is planned from the schema before it to the schema stored now, with --decide")
	fmt.Fprintln(core.Stdout, "      • the lock is re-pinned to the replacement; commit it and deploy")
	fmt.Fprintln(core.Stdout, "      • ONLY for a migration NO environment applied: run `<binary> migrate status --fetch` in every")
	fmt.Fprintln(core.Stdout, "        environment first — it must list the migration as pending (and that run tells the console")
	fmt.Fprintln(core.Stdout, "        what the database applied; the console refuses a migration a database last reported applied)")
	fmt.Fprintln(core.Stdout, "      • a migration an environment applied is fixed forward, with a new migration — never replaced")
	fmt.Fprintln(core.Stdout)

	if !c.ConfirmNotApplied {
		return fmt.Errorf("refusing: pass --confirm-not-applied once every environment's `migrate status --fetch` lists %s as pending", c.MigrationID)
	}

	decide, customSQL, err := schema.BuildDecidePayload(c.Decide)
	if err != nil {
		return err
	}
	initiative := storageclient.ResolveCurrentInitiative(c.Console, projectID, c.Initiative)
	fmt.Fprintf(core.Stdout, "migrate replace: %s\n", initiative.Describe())

	// Can this run pin what it is about to store? Asked first, as a mint asks:
	// a replace that stores and then cannot pin leaves the lock pointing at a
	// migration the console no longer serves.
	if !c.NoLock && c.LockPath != "" {
		if err := schema.CheckCanPinLock(c.Console, c.LockPath, projectID); err != nil {
			return fmt.Errorf("migrate replace: %w", err)
		}
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

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	resp, err := cl.ResetHistory(ctx, &w17registrypb.ResetHistoryRequest{
		ProjectId:       projectID,
		Policy:          w17registrypb.ResetHistoryPolicy_RESET_HISTORY_POLICY_REPLACE,
		Connection:      c.Connection,
		MigrationId:     c.MigrationID,
		InitiativeId:    initiative.ID,
		Decide:          decide,
		DecideCustomSql: customSQL,
		// --confirm-not-applied is what the operator stated; this is that
		// statement crossing the wire. The code never reaches here without it.
		AcknowledgeNotApplied: true,
	})
	if err != nil {
		return fmt.Errorf("replace %s in project %s (nothing was replaced): %w", c.MigrationID, projectID, err)
	}
	if findings := resp.GetFindings(); len(findings) > 0 {
		return replaceFindingsErr(findings)
	}

	rep := resp.GetReplacement()
	fmt.Fprintf(core.Stdout, "migrate replace: %d migration(s) superseded on %s, kept in the history:\n", len(resp.GetSupersededIds()), rep.GetConnection())
	for _, id := range resp.GetSupersededIds() {
		fmt.Fprintf(core.Stdout, "  - %s  → superseded by %s\n", id, rep.GetId())
	}
	fmt.Fprintf(core.Stdout, "  + %s [%s]  sha256=%s  (the replacement)\n", rep.GetId(), rep.GetConnection(), schema.ShortHash(rep.GetContentSha256()))

	if c.NoLock || c.LockPath == "" {
		fmt.Fprintln(core.Stdout, "lock: skipped — run `w17ctl migrate generate` (proto unchanged: it stores nothing and pins every connection to its head)")
		fmt.Fprintln(core.Stdout, replaceNext)
		return nil
	}
	list, err := cl.ListMigrations(ctx, &w17registrypb.ListMigrationsRequest{ProjectId: projectID})
	if err != nil {
		return fmt.Errorf("the migration WAS replaced, but listing the history to pin the lock failed — run `w17ctl migrate generate` to pin it: %w", err)
	}
	pinFrom := append(list.GetMigrations(), rep)
	if _, err := schema.PinLockTargets(c.Console, c.LockPath, projectID, pinFrom); err != nil {
		return fmt.Errorf("the migration WAS replaced, but pinning %s failed — run `w17ctl migrate generate` to pin it: %w", c.LockPath, err)
	}
	fmt.Fprintf(core.Stdout, "lock: pinned %s to the replacement\n", c.LockPath)
	fmt.Fprintln(core.Stdout, replaceNext)
	return nil
}

// replaceNext is the step after a replace, wherever the lock got pinned.
const replaceNext = "next: commit the lock, check `w17ctl migrate check --generated` exits 0, and deploy."

// replaceFindingsErr prints the decisions the replacement plan still needs,
// each with the --decide key that answers it, and fails.
func replaceFindingsErr(findings []*w17registrypb.Finding) error {
	fmt.Fprintln(core.Stdout, "migrate replace: refused — the replacement plan needs decisions (nothing was replaced):")
	for _, f := range findings {
		fmt.Fprintf(core.Stdout, "  %s.%s — %s\n", f.GetTableName(), f.GetColumnName(), f.GetAxis())
		if f.GetRationale() != "" {
			fmt.Fprintf(core.Stdout, "    why: %s\n", f.GetRationale())
		}
		if k := f.GetDecideKey(); k != "" {
			fmt.Fprintf(core.Stdout, "    decide: --decide %s=<%s>\n", k, strings.Join(f.GetOptions(), "|"))
		}
	}
	return fmt.Errorf("%d unresolved finding(s); pass --decide for each and run it again", len(findings))
}
