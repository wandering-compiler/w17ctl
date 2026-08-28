package project

import (
	"context"
	"fmt"
	"time"

	"github.com/wandering-compiler/w17ctl/internal/core"
	schemahub "github.com/wandering-compiler/w17ctl/internal/schema"
	"github.com/wandering-compiler/w17ctl/internal/storageclient"
	w17registrypb "github.com/wandering-compiler/sdk/go/pb/w17registry"
)

// ImportFromCmd implements `w17ctl project import-from` — folding a whole
// source project into this one (docs/decisions/project-import.md).
//
// Named `import-from` because `project import` already means something
// else and older: registering an existing project in the LOCAL registry
// and allocating it host ports. Reusing that name would have made two
// unrelated operations answer to one word.
//
// # What the console does, and why this command is thin
//
// Everything that matters happens server-side, because everything that
// matters needs the signing key: a signature binds the project it was
// minted for, so importing a history means RE-SIGNING every body under
// the target's scope. The console verifies each body under its original
// scope first and refuses the whole import if any fails — that ordering
// is what separates an import from a laundering machine, and it is not
// something a client may be trusted with (D4).
//
// The console also refuses unless the source's history is CONSISTENT
// (`w17ctl migrate verify-history` asks the same question), and unless no
// connection, table or message name is claimed by both projects.
//
// # The source is never touched
//
// Import is a copy of state. A refused or regretted import leaves the
// project it came from exactly as it was.
type ImportFromCmd struct {
	Source     string `name:"from" placeholder:"PROJECT_ID" required:"" help:"Source project to import. Its history comes across verbatim, re-signed for this project; it is left untouched."`
	TargetID   string `name:"project" placeholder:"ID" help:"Project to import INTO. Empty = read project_id from the lock."`
	Initiative string `name:"initiative" placeholder:"ID" help:"Change request the imported range belongs to. Empty = the initiative of the current git branch."`
	LockPath   string `name:"lock" placeholder:"PATH" default:"w17/lock.yaml" help:"Path to this project's lock. Re-pinned after the import so the imported connections have a fetch target — without it 'migrate fetch' skips them silently."`
	Console    string `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"Console gRPC endpoint. Falls back to console_addr in the lock, then the compile-time default."`
	Force      bool   `name:"force" short:"f" help:"Acknowledge that this project's schema and migration history are about to gain another project's. Required."`
}

func (c *ImportFromCmd) Run() error {
	target := c.TargetID
	if target == "" {
		target = core.LockProjectIDBestEffort()
	}
	if target == "" {
		return fmt.Errorf("no target project: pass --project, or run inside a project whose lock carries project_id")
	}
	if c.Source == target {
		return fmt.Errorf("--from and the target are the same project (%s) — there is nothing to import", target)
	}

	fmt.Fprintf(core.Stdout, "⚠️  importing project %s INTO %s:\n", c.Source, target)
	fmt.Fprintln(core.Stdout, "      • its migration history comes across verbatim, re-signed for this project")
	fmt.Fprintln(core.Stdout, "      • its tables are added to this project's stored schema")
	fmt.Fprintln(core.Stdout, "      • the source project is NOT modified")
	fmt.Fprintln(core.Stdout, "      • the console refuses if the source's history is not consistent, or if any")
	fmt.Fprintln(core.Stdout, "        connection / table / message name is claimed by both")
	fmt.Fprintln(core.Stdout)
	if !c.Force {
		return fmt.Errorf("refusing: `project import-from` merges another project's history into this one — pass --force to confirm")
	}

	initiative := storageclient.ResolveCurrentInitiative(c.Console, target, c.Initiative)
	fmt.Fprintf(core.Stdout, "import: %s\n", initiative.Describe())

	addr, cl, closeConn, err := dialForProject(target, c.Console)
	if err != nil {
		return err
	}
	defer closeConn()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	resp, err := cl.ImportProject(ctx, &w17registrypb.ImportProjectRequest{
		SourceProjectId: c.Source,
		TargetProjectId: target,
		InitiativeId:    initiative.ID,
	})
	if err != nil {
		// Nothing was applied: the console builds the whole re-signed set
		// before it mutates anything, so a refusal leaves both projects
		// exactly as they were. Say so — it is what decides whether a
		// retry is safe.
		return fmt.Errorf("import %s into %s via %s (nothing was imported; both projects are unchanged): %w",
			c.Source, target, addr, err)
	}

	fmt.Fprintf(core.Stdout, "imported: %d migration(s), %d table(s) and %d fixture(s) from %s\n",
		resp.GetMigrationsImported(), resp.GetTablesImported(), resp.GetFixturesImported(), c.Source)

	// Re-pin the lock. The imported migrations arrive on connections this
	// project's lock has never heard of, and `migrate fetch` skips a
	// connection with no pinned target — SILENTLY, which is how an import
	// that reported success left its migrations unfetchable. Pin from the
	// console's current heads, the same way a push does.
	if c.LockPath != "" {
		listCtx, listCancel := context.WithTimeout(context.Background(), 30*time.Second)
		listResp, listErr := cl.ListMigrations(listCtx, &w17registrypb.ListMigrationsRequest{ProjectId: target})
		listCancel()
		if listErr != nil {
			return fmt.Errorf("import succeeded but the lock could not be re-pinned (list migrations): %w", listErr)
		}
		if err := schemahub.PinLockTargets(c.Console, c.LockPath, target, listResp.GetMigrations()); err != nil {
			return fmt.Errorf("import succeeded but the lock could not be re-pinned (%s): %w", c.LockPath, err)
		}
		fmt.Fprintf(core.Stdout, "lock: re-pinned %s (the imported connections now have a fetch target)\n", c.LockPath)
	}
	fmt.Fprintln(core.Stdout, "Next: bring the source's protos into this repo so the next push diffs against a")
	fmt.Fprintln(core.Stdout, "schema that matches what the console now holds, then `migrate fetch` + `apply`.")
	return nil
}
