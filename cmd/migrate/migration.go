package migrate

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/storageclient"
	w17registrypb "github.com/wandering-compiler/sdk/go/pb/w17registry"
)

// PushRawCmd backs `w17ctl migrate push-raw` — a single-
// migration escape hatch (D-iter3-26) for hand-authored YAML bodies
// the schema-driven planner can't auto-emit, typically TRANSFORM_FIELD
// ops carrying author-supplied Starlark scripts.
// Reads the YAML body from a file, ships it to console via the
// PushRawMigration RPC. Console validates + signs + stores;
// returns the assigned migration id.
//
// Use case: TRANSFORM_FIELD with a Starlark script the auto-emit
// path can't produce from a schema diff. Operator authors the
// YAML by hand; this command pushes it as the next migration on
// (project, connection).
//
// **Escape hatch caveat:** the supplied body's effect must be
// consistent with the project's stored schema. Console doesn't
// verify the YAML's ops align with column / table state — that's
// the author's responsibility.
type PushRawCmd struct {
	ProjectID  string `name:"project" placeholder:"ID" required:"" help:"Project ID the migration targets. Must already have a stored schema (run 'migrate generate --initial' first)."`
	Connection string `name:"connection" placeholder:"NAME" required:"" help:"Connection name within the project's schema. Must match a connection declared by the schema."`
	BodyPath   string `name:"body" placeholder:"FILE" required:"" help:"Path to a YAML file containing the lib/datamigrate.Migration shape (version: 1; encoding: json|protobuf; operations: [...])."`
	Console    string `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console MigrationRegistry. Optional — falls back to console_addr in w17/lock.yaml, then to the binary's compile-time default."`
	Initiative string `name:"initiative" placeholder:"ID" help:"Change-request (initiative) id this migration belongs to. Empty = the initiative of the current git branch. A hand-authored body is inside its CR's range like any other migration — an unstamped one survives a freeze that collapses the rest."`
}

func (c *PushRawCmd) Run() error {
	body, err := os.ReadFile(c.BodyPath)
	if err != nil {
		return fmt.Errorf("read --body %s: %w", c.BodyPath, err)
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

	initiative := storageclient.ResolveCurrentInitiative(c.Console, c.ProjectID, c.Initiative)
	fmt.Fprintf(core.Stdout, "migration push-raw: %s\n", initiative.Describe())

	resp, err := cl.PushRawMigration(ctx, &w17registrypb.PushRawMigrationRequest{
		ProjectId:    c.ProjectID,
		Connection:   c.Connection,
		YamlBody:     string(body),
		InitiativeId: initiative.ID,
	})
	if err != nil {
		return fmt.Errorf("push-raw: %w", err)
	}
	mig := resp.GetMigration()
	fmt.Fprintf(core.Stdout, "migration push-raw: stored id=%s connection=%s sha256=%s\n",
		mig.GetId(), mig.GetConnection(), mig.GetContentSha256())
	return nil
}
