package project

import (
	"context"
	"fmt"
	"time"

	"github.com/wandering-compiler/w17ctl/internal/core"
	w17registrypb "github.com/wandering-compiler/sdk/go/pb/w17registry"
)

// Moving a project between organizations is TWO commands with TWO logins,
// and that is not ceremony — it is the only way the console can check both
// sides.
//
// One auth token proves one thing: membership in the org it was issued for.
// The console sees the caller's ACTIVE org and nothing else — there is no
// list of the caller's organizations anywhere it can read. So a single
// command naming the target org would be asking the console to take the
// caller's word for it, and anyone holding their own org's token could move
// any project they can name to themselves. `project_id` is not a secret; it
// sits in every consuming repo's lock.
//
// Hence: `release-to-org` runs as the SOURCE org (proving the project may be
// given away), `claim` runs as the TARGET org (proving the caller may receive
// it). Log in as the other org between them.

// ReleaseToOrgCmd backs `w17ctl project release-to-org`.
type ReleaseToOrgCmd struct {
	ProjectID string `name:"project" placeholder:"ID" help:"Project to offer. Empty = read project_id from the lock."`
	TargetOrg string `name:"to" placeholder:"ORG_ID" help:"Organization to offer the project to. Empty WITHDRAWS a pending offer."`
	Console   string `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"Console gRPC endpoint. Falls back to console_addr in the lock, then the compile-time default."`
}

func (c *ReleaseToOrgCmd) Run() error {
	projectID, cl, closeConn, err := dialForProject(c.ProjectID, c.Console)
	if err != nil {
		return err
	}
	defer closeConn()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := cl.ReleaseProjectToOrg(ctx, &w17registrypb.ReleaseProjectToOrgRequest{
		ProjectId: projectID, TargetOrgId: c.TargetOrg,
	})
	if err != nil {
		return fmt.Errorf("release project %s: %w", projectID, err)
	}
	if resp.GetTargetOrgId() == "" {
		fmt.Fprintf(core.Stdout, "offer withdrawn: project %s is not up for transfer\n", projectID)
		return nil
	}
	fmt.Fprintf(core.Stdout, "project %s offered to org %s\n", projectID, resp.GetTargetOrgId())
	fmt.Fprintln(core.Stdout, "Nothing has moved yet. Log in as that organization and run:")
	fmt.Fprintf(core.Stdout, "    w17ctl project claim --project %s\n", projectID)
	return nil
}

// ClaimCmd backs `w17ctl project claim`.
type ClaimCmd struct {
	ProjectID string `name:"project" required:"" placeholder:"ID" help:"Project to claim. Must have been offered to the organization you are logged in as."`
	NewName   string `name:"rename" placeholder:"NAME" help:"Name the project takes here. Needed only when this organization already has a project of that name — the refusal says so."`
	Console   string `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"Console gRPC endpoint. Falls back to console_addr in the lock, then the compile-time default."`
}

func (c *ClaimCmd) Run() error {
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

	resp, err := cl.ClaimProject(ctx, &w17registrypb.ClaimProjectRequest{
		ProjectId: c.ProjectID, NewProjectName: c.NewName,
	})
	if err != nil {
		// The console refuses a name collision by naming it, because the
		// consuming repo's lock carries the project name and is signed —
		// renaming behind the operator's back would invalidate it. Steer at
		// the flag rather than making them find it.
		return fmt.Errorf("claim project %s (on a name collision, retry with --rename <name>): %w", c.ProjectID, err)
	}

	fmt.Fprintf(core.Stdout, "project %s is now in this organization, named %q\n", c.ProjectID, resp.GetProjectName())
	fmt.Fprintln(core.Stdout, "Deployments still holding the old organization's token will start failing on")
	fmt.Fprintln(core.Stdout, "permissions — that is expected, the same as a revoked token. Re-issue theirs.")
	if c.NewName != "" {
		fmt.Fprintln(core.Stdout, "The name changed, so update project_name in the consuming repo's lock.")
	}
	return nil
}

// dialForProject resolves the project id (flag, else the lock) and opens the
// registry connection. Shared so both halves of a move report a missing
// project id the same way.
func dialForProject(projectID, console string) (string, w17registrypb.ProjectRegistryClient, func(), error) {
	if projectID == "" {
		projectID = core.LockProjectIDBestEffort()
	}
	if projectID == "" {
		return "", nil, nil, fmt.Errorf("no project id: pass --project, or run inside a project whose lock carries project_id")
	}
	addr, err := core.ResolveConsoleAddr(console)
	if err != nil {
		return "", nil, nil, err
	}
	cl, conn, err := core.DialProjectRegistry(addr)
	if err != nil {
		return "", nil, nil, fmt.Errorf("connect %s: %w", addr, err)
	}
	return projectID, cl, func() { _ = conn.Close() }, nil
}
