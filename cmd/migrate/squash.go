package migrate

import (
	"context"
	"fmt"
	"time"

	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/schema"
	"github.com/wandering-compiler/w17ctl/internal/storageclient"
	w17registrypb "github.com/wandering-compiler/sdk/go/pb/w17registry"
)

// ====================================================================
// `w17ctl migrate squash` — freeze a change request together with its
// migrations (docs/decisions/squash-supersede-and-adopt.md)
// ====================================================================
//
// The client half of `ResetHistory(SUPERSEDE)`. The server half shipped
// with docs/decisions/squash-supersede-and-adopt.md and had no command
// in front of it, so the feature existed and was unreachable.
//
// Two calls, exactly like reset: the console marks WHAT is collapsing,
// and the ORDINARY push derives the baseline that replaces it. The
// difference from reset is what becomes of the rows — squash keeps
// them, pointed at the baseline, so a live deployment can recognise the
// baseline as work it has already done and RECORD it without running
// it. That is the whole reason squash and recreate are different
// commands rather than a flag.
//
// # Why this names a change request
//
// "Squash the last N migrations" is not a thing anyone can defend — a
// range chosen by count or by date has no edges, so it silently takes
// in whatever else happened to land in the window. A change request
// does have edges. `--initiative` carries them to the console, which
// refuses unless every live migration really is part of that change
// request and NAMES the ones that are not.
//
// It is an assertion, not a narrowing: what collapses is still the
// whole live history. A baseline is derived from the schema as it is
// NOW and appended at the end, so it correctly replaces a range only
// when nothing live is left after it. Collapsing a middle range needs a
// baseline built from the schema as it stood at that point and inserted
// there — a different mechanism, tracked in the todo above.

// SquashCmd implements `w17ctl migrate squash`.
type SquashCmd struct {
	Force      bool     `name:"force" short:"f" help:"Acknowledge the collapse. Required: without it the command prints what would happen and refuses."`
	Initiative string   `name:"initiative" placeholder:"ID" help:"Change request whose migrations this freezes. Empty = the initiative of the current git branch (what 'w17ctl initiative current' shows). The console refuses unless every live migration belongs to it, and names the ones that do not."`
	All        bool     `name:"all" help:"Collapse the whole history WITHOUT naming a change request — no assertion about what is in it. The pre-initiative behaviour; use it for a project whose pushes were never stamped."`
	ProjectID  string   `name:"project" placeholder:"ID" help:"Project whose history is collapsed. Empty = read project_id from the lock."`
	Console    string   `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console ProjectRegistry. Optional — falls back to console_addr in w17/lock.yaml, then to the binary's compile-time default."`
	Protos     []string `name:"proto" short:"p" placeholder:"PROTO" help:"Path to a .proto schema. Repeatable. Supplied = the baseline is derived for you; omitted = the command stops after the collapse and the project stays without one until you run 'migrate generate'."`
	Imports    []string `name:"import" short:"I" placeholder:"DIR" help:"Additional proto import path for the baseline push. Repeatable. Only read when --proto is given."`
	LockPath   string   `name:"lock" placeholder:"PATH" default:"w17/lock.yaml" help:"Path to the lock file, re-pinned by the baseline push. Only read when --proto is given."`
	NoLock     bool     `name:"no-lock" help:"Skip the lock-file write on the baseline push. Only read when --proto is given."`
}

func (c *SquashCmd) Run() error {
	projectID := c.ProjectID
	if projectID == "" {
		projectID = core.LockProjectIDBestEffort()
	}
	if projectID == "" {
		return fmt.Errorf("no project id: pass --project, or run inside a project whose lock carries project_id")
	}

	// Resolve the change request BEFORE the banner, so the banner can
	// name it. --all is the deliberate opt-out; anything else that fails
	// to resolve refuses, because a squash whose scope silently became
	// "everything" is the exact accident this command exists to stop.
	initiativeID := ""
	if !c.All {
		found := storageclient.ResolveCurrentInitiative(c.Console, projectID, c.Initiative)
		if found.ID == "" {
			return fmt.Errorf("cannot tell which change request to freeze — %s. Pass --initiative ID, or --all to collapse the whole history without naming one", found.Why)
		}
		initiativeID = found.ID
		fmt.Fprintf(core.Stdout, "squash: freezing %s\n", found.Describe())
	}

	// The banner runs before the --force check on purpose: someone who
	// typed the command without the flag should still read what it would
	// have done.
	fmt.Fprintln(core.Stdout, "⚠️  w17ctl migrate squash collapses this project's migration history into one baseline.")
	fmt.Fprintln(core.Stdout, "      • the collapsed migrations are KEPT and stay listable — this is not `migrate reset`")
	fmt.Fprintln(core.Stdout, "      • they leave the apply plan: a fresh database gets the baseline instead")
	fmt.Fprintln(core.Stdout, "      • a deployment already at the collapsed head RECORDS the baseline without running it")
	fmt.Fprintln(core.Stdout, "      • a deployment PART-WAY through the collapsed range records it too, and would then")
	fmt.Fprintln(core.Stdout, "        be missing everything it had not yet applied — squash while your deployments are")
	fmt.Fprintln(core.Stdout, "        at the head, not mid-rollout")
	if c.All {
		fmt.Fprintln(core.Stdout, "      • --all: NOTHING checks what is in the range. Whatever is live is collapsed.")
	}
	fmt.Fprintln(core.Stdout)

	if !c.Force {
		return fmt.Errorf("refusing: `migrate squash` rewrites the project's migration history — pass --force to confirm")
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

	if err := squashHistory(cl, projectID, initiativeID); err != nil {
		return err
	}

	if len(c.Protos) == 0 {
		fmt.Fprintln(core.Stdout, "collapsed. The project has NO baseline yet — its apply plan is empty until you run")
		fmt.Fprintln(core.Stdout, "`w17ctl migrate generate --proto <file>`, which derives it and claims the collapsed range.")
		return nil
	}

	fmt.Fprintln(core.Stdout, "deriving the baseline")
	// The baseline belongs to the change request that produced it — the
	// same one just frozen. Passing it explicitly rather than letting the
	// push re-derive from the branch keeps the two halves of one squash
	// stamped identically even if the branch moved in between.
	if err := schema.RunSchemaPush(schema.SchemaPushArgs{
		Protos: c.Protos, Imports: c.Imports, ProjectID: projectID, Console: c.Console,
		LockPath: c.LockPath, NoLock: c.NoLock,
		// --initial rather than letting AUTO probe: the collapse dropped
		// the stored schema, so create is where the probe would land
		// anyway — asking for it explicitly buys a post-condition, since
		// the console refuses an initial push for a project that still
		// has a schema. This push failing means the collapse did not.
		Mode:         schema.SchemaPushAuto,
		ForceInitial: true,
		Initiative:   initiativeID,
	}); err != nil {
		return fmt.Errorf("baseline push after squash: %w", err)
	}

	fmt.Fprintln(core.Stdout, "done. The baseline replaces the collapsed range; deployments at the old head will")
	fmt.Fprintln(core.Stdout, "record it without running it on their next `migrate fetch` + `apply`.")
	return nil
}

// squashHistory runs the collapse and reports what it took in. It uses
// the same narrow client interface the reset path does.
//
// The hand-authored bodies are NAMED here as they are for reset, but
// they mean something different and the wording says so: a squash keeps
// them, so this is a list of what the baseline now stands in for, not a
// list of what is gone.
func squashHistory(cl projectRegistryResetter, projectID, initiativeID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := cl.ResetHistory(ctx, &w17registrypb.ResetHistoryRequest{
		ProjectId:    projectID,
		Policy:       w17registrypb.ResetHistoryPolicy_RESET_HISTORY_POLICY_SUPERSEDE,
		InitiativeId: initiativeID,
		// The console refuses without this. --force is what the operator
		// acknowledged with; this is that acknowledgement crossing the
		// wire, and the code path never reaches here without it.
		AcknowledgeDataLoss: true,
	})
	if err != nil {
		return fmt.Errorf("squash history for project %s (nothing was collapsed): %w", projectID, err)
	}

	fmt.Fprintf(core.Stdout, "history collapsed: %d migration(s) superseded, %d of them hand-authored\n",
		resp.GetMigrationsDropped(), resp.GetRawMigrationsDropped())
	for _, id := range resp.GetRawMigrationIds() {
		fmt.Fprintf(core.Stdout, "    now stood in for by the baseline: %s\n", id)
	}
	if resp.GetRawMigrationsDropped() > 0 {
		fmt.Fprintln(core.Stdout, "    ^ these bodies are author-owned and are NOT re-derived from the schema. The")
		fmt.Fprintln(core.Stdout, "      baseline reproduces the SCHEMA they left behind, not the data work they did.")
	}
	return nil
}
