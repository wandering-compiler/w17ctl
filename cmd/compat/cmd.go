package compat

import (
	"fmt"

	engine "github.com/wandering-compiler/w17ctl/internal/compat"
	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/storageclient"

	initiativespb "github.com/wandering-compiler/sdk/go/pb/consoleapi/initiatives"
)

// Cmd is `w17ctl compat` — the compatibility report between two
// compiled snapshots, with the dev/prod policy gate. w17ctl is a thin
// client: it fetches the two snapshots' compiled IR from console storage
// and ships them to the console's ClassifyIR RPC, which classifies +
// applies the gate (thin-client refactor Step 3d — the engine moved
// behind the API). CI can fail a build on a prod-mode block (non-zero
// exit).
type Cmd struct {
	Console string `name:"console" placeholder:"HOST:PORT" env:"CONSOLE_STORAGE_ADDR" help:"Console storage endpoint. Defaults to the logged-in console (w17ctl login), else the compiled-in default."`

	Report       ReportCmd       `cmd:"" help:"Diff two snapshots by id (base → head)."`
	AgainstTrunk AgainstTrunkCmd `cmd:"" name:"against-trunk" help:"Diff the current branch's initiative head against trunk (the review semantic diff)."`
}

// --- report -------------------------------------------------------

type ReportCmd struct {
	Base string `name:"base" placeholder:"SNAP" help:"Base snapshot id (empty = initial state)."`
	Head string `name:"head" placeholder:"SNAP" required:"" help:"Head snapshot id."`
	Mode string `name:"mode" placeholder:"dev|prod" default:"dev" help:"Environment mode for the gate (dev=warn, prod=block breaking)."`
}

func (c *ReportCmd) Run(parent *Cmd) error {
	sc, err := storageclient.DialStorageFn(parent.Console)
	if err != nil {
		return err
	}
	defer sc.Close()

	base, err := sc.LoadIRBytes(c.Base)
	if err != nil {
		return err
	}
	head, err := sc.LoadIRBytes(c.Head)
	if err != nil {
		return err
	}
	return classifyAndGate(base, head, c.Mode)
}

// classifyAndGate classifies base→head in mode, prints the report, and
// returns a non-nil error when the gate blocks a BREAKING change (prod
// mode blocks; dev warns). Shared by report + against-trunk so the gate
// decision + its message live in one place.
func classifyAndGate(base, head []byte, mode string) error {
	resp, err := engine.ClassifyCompat(base, head, mode)
	if err != nil {
		return err
	}
	if engine.RenderReportPB(resp, mode) {
		return fmt.Errorf("compat gate: BREAKING change blocked in %s mode", mode)
	}
	return nil
}

// --- against-trunk ------------------------------------------------

type AgainstTrunkCmd struct {
	Project string `name:"project" placeholder:"ID" help:"Project id. Empty = read from the current project's lock."`
	Name    string `name:"name" placeholder:"NAME" help:"Initiative name. Empty = current git branch (main/master → trunk)."`
	Mode    string `name:"mode" placeholder:"dev|prod" default:"dev" help:"Environment mode for the gate decision."`
}

func (c *AgainstTrunkCmd) Run(parent *Cmd) error {
	project, err := storageclient.ResolveProjectID(c.Project)
	if err != nil {
		return err
	}
	name, _, err := storageclient.ResolveInitiativeTarget(c.Name)
	if err != nil {
		return err
	}
	sc, err := storageclient.DialStorageFn(parent.Console)
	if err != nil {
		return err
	}
	defer sc.Close()

	ctx, cancel := core.ClientCtx()
	list, err := sc.IQ.List(ctx, &initiativespb.ListInitiativesReq{ProjectId: project})
	cancel()
	if err != nil {
		return fmt.Errorf("list initiatives: %w", err)
	}
	var trunkHead, initHead string
	for _, i := range list.GetRows() {
		switch {
		case i.GetIsTrunk():
			trunkHead = i.GetHeadSnapshotId()
		case i.GetName() == name:
			initHead = i.GetHeadSnapshotId()
		}
	}
	if initHead == "" {
		return fmt.Errorf("initiative %q has no snapshot yet (push first)", name)
	}

	base, err := sc.LoadIRBytes(trunkHead) // may be "" → nil (no trunk snapshot yet)
	if err != nil {
		return err
	}
	head, err := sc.LoadIRBytes(initHead)
	if err != nil {
		return err
	}
	fmt.Fprintf(core.Stdout, "compat: initiative %q vs trunk\n", name)
	return classifyAndGate(base, head, c.Mode)
}
