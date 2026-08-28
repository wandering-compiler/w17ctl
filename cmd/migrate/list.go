package migrate

import (
	"context"
	"fmt"
	"time"

	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/schema"
	w17registrypb "github.com/wandering-compiler/sdk/go/pb/w17registry"
)

// ListCmd backs `w17ctl migrate list` — it queries the
// console's stored migration history.
type ListCmd struct {
	ProjectID  string `name:"project" short:"p" placeholder:"ID" required:"" help:"Project identifier."`
	Connection string `name:"connection" short:"c" placeholder:"NAME" help:"Optional connection-name filter. Empty = every connection's migrations interleaved by stored order."`
	From       string `name:"from" placeholder:"MIGRATION_ID" help:"Optional pagination cursor. Returned list starts strictly after this ID."`
	UpTo       string `name:"up-to" placeholder:"MIGRATION_ID" help:"Optional upper bound. Stream stops after emitting this ID."`
	Console    string `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console MigrationRegistry. Optional — falls back to console_addr in w17/lock.yaml, then to the binary's compile-time default."`
}

func (c *ListCmd) Run() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	addr, err := core.ResolveConsoleAddr(c.Console)
	if err != nil {
		return err
	}
	cl, conn, err := core.DialProjectRegistry(addr)
	if err != nil {
		return fmt.Errorf("connect %s: %w", addr, err)
	}
	defer func() { _ = conn.Close() }()

	resp, err := cl.ListMigrations(ctx, &w17registrypb.ListMigrationsRequest{
		ProjectId:       c.ProjectID,
		Connection:      c.Connection,
		FromMigrationId: c.From,
		UpToMigrationId: c.UpTo,
	})
	if err != nil {
		return err
	}
	migrations := resp.GetMigrations()

	if len(migrations) == 0 {
		fmt.Fprintf(core.Stdout, "no migrations for project %s", c.ProjectID)
		if c.Connection != "" {
			fmt.Fprintf(core.Stdout, " (connection=%s)", c.Connection)
		}
		fmt.Fprintln(core.Stdout)
		return nil
	}

	fmt.Fprintf(core.Stdout, "project %s — %d migration(s):\n", c.ProjectID, len(migrations))
	for _, m := range migrations {
		fmt.Fprintf(core.Stdout, "  - %s [%s]  sha256=%s\n",
			m.GetId(), m.GetConnection(), schema.ShortHash(m.GetContentSha256()))
	}
	return nil
}
