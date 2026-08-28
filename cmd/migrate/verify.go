package migrate

import (
	"context"
	"fmt"
	"time"

	"github.com/wandering-compiler/w17ctl/internal/core"
	w17registrypb "github.com/wandering-compiler/sdk/go/pb/w17registry"
)

// VerifyHistoryCmd implements `w17ctl migrate verify-history` — the
// clean-gate, asked out loud.
//
// The predicate has existed server-side since revisions landed and had NO
// client surface: it ran only as `UpdateSchema`'s own drift guard, so the
// only way an operator ever met the answer was a push refused with it
// quoted in the error. That is late and back-to-front — the question
// "does my recorded history actually reproduce my stored schema" is one
// you want answered BEFORE deciding to squash, reset or import, not as
// the reason a push you already meant to make was rejected.
//
// Import (docs/decisions/project-import.md) is what forced it: only a
// history that provably reproduces its schema may be re-signed under a
// new scope, because after the re-signing nobody can tell the difference.
type VerifyHistoryCmd struct {
	ProjectID string `name:"project" placeholder:"ID" help:"Project to check. Empty = read project_id from the lock."`
	Console   string `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"Console gRPC endpoint. Falls back to console_addr in the lock, then the compile-time default."`
	Strict    bool   `name:"strict" help:"Exit non-zero on UNKNOWN too. Off by default: a project pushed before revisions were recorded reads UNKNOWN through no fault of its own, so failing on it would condemn every legacy project. Turn it on in a pipeline that has decided it only accepts PROVEN histories."`
}

func (c *VerifyHistoryCmd) Run() error {
	projectID := c.ProjectID
	if projectID == "" {
		projectID = core.LockProjectIDBestEffort()
	}
	if projectID == "" {
		return fmt.Errorf("no project id: pass --project, or run inside a project whose lock carries project_id")
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

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := cl.VerifyHistory(ctx, &w17registrypb.VerifyHistoryRequest{ProjectId: projectID})
	if err != nil {
		return fmt.Errorf("verify-history %s: %w", projectID, err)
	}

	switch resp.GetVerdict() {
	case w17registrypb.HistoryVerdict_HISTORY_VERDICT_CONSISTENT:
		fmt.Fprintf(core.Stdout, "history: consistent — %d recorded push(es) account for exactly the stored migrations, and the last one produced the stored schema\n",
			resp.GetRecordedRevisions())
		return nil

	case w17registrypb.HistoryVerdict_HISTORY_VERDICT_DRIFTED:
		// A refusal, because it is the answer that changes what you may do
		// next: the stored schema is not the one the history produced, so
		// the next diff would be planned from a state nothing created.
		fmt.Fprintf(core.Stdout, "history: DRIFTED — %s\n", resp.GetDetail())
		fmt.Fprintln(core.Stdout, "  The stored schema is not the one the recorded pushes produced. Every migration")
		fmt.Fprintln(core.Stdout, "  planned from here diffs against a state nothing created, and it looks healthy")
		fmt.Fprintln(core.Stdout, "  because a differ cannot tell a wrong starting point from a right one.")
		fmt.Fprintln(core.Stdout, "  `w17ctl migrate reset` re-derives a baseline from the proto (DEV-only, drops data).")
		return fmt.Errorf("project %s has a drifted migration history", projectID)

	default:
		// UNKNOWN: migrations exist, revisions do not. Every project pushed
		// before revisions were recorded reads this way, so it is reported
		// and — unless --strict — not failed. It says "no evidence", never
		// "bad evidence".
		fmt.Fprintf(core.Stdout, "history: unknown — the project has migrations but no revision records, so the question cannot be answered\n")
		fmt.Fprintln(core.Stdout, "  This is what every project pushed before revisions existed looks like. It is not")
		fmt.Fprintln(core.Stdout, "  drift; nothing says the history is wrong, only that nothing vouches for it. The")
		fmt.Fprintln(core.Stdout, "  next ordinary push starts recording, and from then on the answer is real.")
		if c.Strict {
			return fmt.Errorf("project %s has no history evidence and --strict was set", projectID)
		}
		return nil
	}
}
