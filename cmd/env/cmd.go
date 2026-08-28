package env

import (
	"fmt"
	"os"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	compat "github.com/wandering-compiler/w17ctl/internal/compat"
	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/storageclient"

	reviewpb "github.com/wandering-compiler/sdk/go/pb/consoleapi/review"
)

// Cmd is `w17ctl env` — deployment environments + their mode. A
// prod-mode environment turns the compat engine's BREAKING verdict into
// a hard block at review-merge (see `review merge`). Client orchestration
// over the generated storage; no facade.
type Cmd struct {
	Console string `name:"console" placeholder:"HOST:PORT" env:"CONSOLE_STORAGE_ADDR" help:"Console storage endpoint. Defaults to the logged-in console (w17ctl login), else the compiled-in default."`

	Create      CreateCmd      `cmd:"" help:"Create an environment (dev|prod)."`
	List        ListCmd        `cmd:"" help:"List a project's environments."`
	SetMode     SetModeCmd     `cmd:"" name:"set-mode" help:"Flip an environment between dev and prod."`
	SetDeployed SetDeployedCmd `cmd:"" name:"set-deployed" help:"Pin what snapshot is live in an environment (the prod-gate baseline)."`
	CheckDB     CheckDBCmd     `cmd:"" name:"check-db" help:"Dev wipe-DB warning: can this environment's DB forward-migrate to a target snapshot, or must it be reset?"`
}

func parseMode(s string) (reviewpb.Environment_Mode, error) {
	switch s {
	case "dev":
		return reviewpb.Environment_DEV, nil
	case "prod":
		return reviewpb.Environment_PROD, nil
	default:
		return reviewpb.Environment_MODE_UNSPECIFIED, fmt.Errorf("mode must be dev or prod (got %q)", s)
	}
}

func modeString(m reviewpb.Environment_Mode) string {
	if m == reviewpb.Environment_PROD {
		return "prod"
	}
	return "dev"
}

// --- create -------------------------------------------------------

type CreateCmd struct {
	Project string `name:"project" placeholder:"ID" help:"Project id. Empty = read from the current project's lock."`
	Name    string `name:"name" placeholder:"NAME" required:"" help:"Environment name (dev/staging/prod/…)."`
	Mode    string `name:"mode" placeholder:"dev|prod" default:"dev" help:"Environment mode."`
}

func (c *CreateCmd) Run(parent *Cmd) error {
	project, err := storageclient.ResolveProjectID(c.Project)
	if err != nil {
		return err
	}
	mode, err := parseMode(c.Mode)
	if err != nil {
		return err
	}
	sc, err := storageclient.DialStorageFn(parent.Console)
	if err != nil {
		return err
	}
	defer sc.Close()

	ctx, cancel := core.ClientCtx()
	defer cancel()
	resp, err := sc.EM.Create(ctx, &reviewpb.CreateEnvReq{ProjectId: project, Name: c.Name, Mode: mode})
	if err != nil {
		return fmt.Errorf("create env: %w", err)
	}
	fmt.Fprintf(core.Stdout, "created environment %q (%s, id=%s)\n", c.Name, c.Mode, resp.GetId())
	return nil
}

// --- list ---------------------------------------------------------

type ListCmd struct {
	Project string `name:"project" placeholder:"ID" help:"Project id. Empty = read from the current project's lock."`
}

func (c *ListCmd) Run(parent *Cmd) error {
	project, err := storageclient.ResolveProjectID(c.Project)
	if err != nil {
		return err
	}
	sc, err := storageclient.DialStorageFn(parent.Console)
	if err != nil {
		return err
	}
	defer sc.Close()

	ctx, cancel := core.ClientCtx()
	defer cancel()
	resp, err := sc.EQ.List(ctx, &reviewpb.ListEnvironmentsReq{ProjectId: project})
	if err != nil {
		return fmt.Errorf("list envs: %w", err)
	}
	if len(resp.GetRows()) == 0 {
		fmt.Fprintf(core.Stdout, "no environments in project %q\n", project)
		return nil
	}
	for _, e := range resp.GetRows() {
		deployed := e.GetDeployedSnapshotId()
		if deployed == "" {
			deployed = "(none)"
		}
		fmt.Fprintf(core.Stdout, "%s\tmode=%s\tdeployed=%s\n", e.GetName(), modeString(e.GetMode()), deployed)
	}
	return nil
}

// --- set-mode -----------------------------------------------------

type SetModeCmd struct {
	Project string `name:"project" placeholder:"ID" help:"Project id. Empty = read from the current project's lock."`
	Name    string `name:"name" placeholder:"NAME" required:"" help:"Environment name."`
	Mode    string `name:"mode" placeholder:"dev|prod" required:"" help:"New mode."`
}

// --- check-db (dev wipe-DB warning) -------------------------------

type CheckDBCmd struct {
	Project string `name:"project" placeholder:"ID" help:"Project id. Empty = read from the current project's lock."`
	Name    string `name:"name" placeholder:"NAME" required:"" help:"Environment whose DB you are about to migrate."`
	Target  string `name:"target" placeholder:"INIT" help:"Target initiative whose head you are migrating to. Empty = current git branch (main/master → trunk)."`
}

// Run is the dev "wipe your DB" warning: it asks the compat engine
// whether the environment's currently-deployed schema can forward-
// migrate to the target snapshot. v1 uses the env's deployed_snapshot
// as the proxy for the live DB's applied schema (real live-DB schema
// introspection is a richer follow-up). Only DB-domain findings matter
// — a wire/API break does not require a DB reset. A BREAKING DB finding
// means the DB cannot be safely forward-migrated → reset it.
func (c *CheckDBCmd) Run(parent *Cmd) error {
	project, err := storageclient.ResolveProjectID(c.Project)
	if err != nil {
		return err
	}
	targetName, _, err := storageclient.ResolveInitiativeTarget(c.Target)
	if err != nil {
		return err
	}
	sc, err := storageclient.DialStorageFn(parent.Console)
	if err != nil {
		return err
	}
	defer sc.Close()

	env, err := sc.FindEnv(project, c.Name)
	if err != nil {
		return err
	}
	if env == nil {
		return fmt.Errorf("no environment %q", c.Name)
	}
	target, err := sc.FindInitiative(project, targetName)
	if err != nil {
		return err
	}
	if target == nil || target.GetHeadSnapshotId() == "" {
		return fmt.Errorf("target initiative %q has no snapshot", targetName)
	}

	base, err := sc.LoadIRBytes(env.GetDeployedSnapshotId()) // "" → nil = empty DB
	if err != nil {
		return err
	}
	head, err := sc.LoadIRBytes(target.GetHeadSnapshotId())
	if err != nil {
		return err
	}

	// Only DB-domain findings decide whether the DB must be wiped — mode
	// is irrelevant to the findings (it only drives the gate), so "dev".
	resp, err := compat.ClassifyCompat(base, head, "dev")
	if err != nil {
		return err
	}
	breaking := compat.DBBreakingFindings(resp)
	if len(breaking) == 0 {
		fmt.Fprintf(core.Stdout, "env %q DB can forward-migrate to %q safely\n", c.Name, targetName)
		return nil
	}
	fmt.Fprintf(os.Stderr, "⚠ env %q DB is inconsistent with %q — it cannot be safely forward-migrated:\n", c.Name, targetName)
	for _, f := range breaking {
		fmt.Fprintf(os.Stderr, "  DB BREAKING %s  %s\n", f.GetChange(), f.GetMessage())
	}
	fmt.Fprintf(os.Stderr, "  reset it:  w17ctl migrate reset --force\n")
	return fmt.Errorf("dev DB reset required before migrating env %q to %q", c.Name, targetName)
}

// --- set-deployed -------------------------------------------------

type SetDeployedCmd struct {
	Project  string `name:"project" placeholder:"ID" help:"Project id. Empty = read from the current project's lock."`
	Name     string `name:"name" placeholder:"NAME" required:"" help:"Environment name."`
	Snapshot string `name:"snapshot" placeholder:"SNAP" required:"" help:"Snapshot id that is now live in this environment."`
}

func (c *SetDeployedCmd) Run(parent *Cmd) error {
	project, err := storageclient.ResolveProjectID(c.Project)
	if err != nil {
		return err
	}
	sc, err := storageclient.DialStorageFn(parent.Console)
	if err != nil {
		return err
	}
	defer sc.Close()

	env, err := sc.FindEnv(project, c.Name)
	if err != nil {
		return err
	}
	if env == nil {
		return fmt.Errorf("no environment %q", c.Name)
	}
	ctx, cancel := core.ClientCtx()
	defer cancel()
	// Carries the baseline read a line above: SetDeployed is a compare-and-set
	// since T3-7 D-F1, so a `review merge --env` that lands in between makes
	// this pin fail loudly rather than silently reverting the env baseline the
	// prod compat gate diffs against.
	if _, err := sc.EM.SetDeployed(ctx, &reviewpb.SetEnvDeployedReq{
		Id:                         env.GetId(),
		DeployedSnapshotId:         c.Snapshot,
		ExpectedDeployedSnapshotId: env.GetDeployedSnapshotId(),
	}); err != nil {
		if status.Code(err) == codes.NotFound {
			return fmt.Errorf("set deployed: environment %q moved to another snapshot while this ran (it was %q when read) — re-run to pin over the new value", c.Name, env.GetDeployedSnapshotId())
		}
		return fmt.Errorf("set deployed: %w", err)
	}
	fmt.Fprintf(core.Stdout, "environment %q deployed snapshot → %s\n", c.Name, c.Snapshot)
	return nil
}

func (c *SetModeCmd) Run(parent *Cmd) error {
	project, err := storageclient.ResolveProjectID(c.Project)
	if err != nil {
		return err
	}
	mode, err := parseMode(c.Mode)
	if err != nil {
		return err
	}
	sc, err := storageclient.DialStorageFn(parent.Console)
	if err != nil {
		return err
	}
	defer sc.Close()

	env, err := sc.FindEnv(project, c.Name)
	if err != nil {
		return err
	}
	if env == nil {
		return fmt.Errorf("no environment %q", c.Name)
	}
	ctx, cancel := core.ClientCtx()
	defer cancel()
	if _, err := sc.EM.SetMode(ctx, &reviewpb.SetEnvModeReq{Id: env.GetId(), Mode: mode}); err != nil {
		return fmt.Errorf("set mode: %w", err)
	}
	fmt.Fprintf(core.Stdout, "environment %q → %s\n", c.Name, c.Mode)
	return nil
}
