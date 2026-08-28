package initiative

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/schema"
	"github.com/wandering-compiler/w17ctl/internal/storageclient"
	initiativespb "github.com/wandering-compiler/sdk/go/pb/consoleapi/initiatives"
)

// Cmd is `w17ctl initiative` — the thin client for console v2
// initiatives + snapshots. It calls the GENERATED storage services
// (InitiativeQuery/Mutation, SnapshotQuery/Mutation) DIRECTLY — there is
// no hand-written server facade. The lifecycle glue (get-or-create,
// lazy materialize, head advance) is client-side orchestration here;
// "one trunk per project" is a DB partial-unique index (not app logic).
//
// GIT-SYNC (part A): the active initiative is derived fresh from the
// current git branch on every command (main/master → reserved `trunk`).
// Read-only commands never create anything (lazy materialization — the
// initiative is materialized only on the first WRITE: `push`).
type Cmd struct {
	Console string `name:"console" placeholder:"HOST:PORT" env:"CONSOLE_STORAGE_ADDR" help:"Console storage endpoint. Defaults to the logged-in console (w17ctl login), else the compiled-in default."`

	Current     CurrentCmd     `cmd:"" help:"Show the initiative for the current git branch (read-only — never materializes)."`
	List        ListCmd        `cmd:"" help:"List every initiative in the project."`
	Show        ShowCmd        `cmd:"" help:"Show one initiative (by --name, or the current branch)."`
	Materialize MaterializeCmd `cmd:"" help:"Explicitly create (get-or-create) the initiative for a branch."`
	Push        PushCmd        `cmd:"" help:"Push a snapshot for the current branch — lazily materializes the initiative on first write and advances its head."`
	Create      CreateCmd      `cmd:"" help:"Manual mode only — create + switch to a new active initiative (a fresh local DB lineage)."`
	Activate    ActivateCmd    `cmd:"" help:"Manual mode only — switch the active initiative; subsequent 'stack build' / 'db snapshot' scope to it."`
}

// --- current ------------------------------------------------------

type CurrentCmd struct {
	Project string `name:"project" placeholder:"ID" help:"Project id. Empty = read from the current project's lock."`
}

func (c *CurrentCmd) Run(parent *Cmd) error {
	project, err := storageclient.ResolveProjectID(c.Project)
	if err != nil {
		return err
	}
	name, isTrunk, err := storageclient.ResolveInitiativeTarget("")
	if err != nil {
		return err
	}
	sc, err := storageclient.DialStorageFn(parent.Console)
	if err != nil {
		return err
	}
	defer sc.Close()

	found, err := sc.FindInitiative(project, name)
	if err != nil {
		return fmt.Errorf("current: %w", err)
	}
	if found == nil {
		trunk := ""
		if isTrunk {
			trunk = " (trunk)"
		}
		fmt.Fprintf(core.Stdout, "%s%s — not yet materialized (no writes on this branch yet)\n", name, trunk)
		return nil
	}
	printInitiative(found)
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
	resp, err := sc.IQ.List(ctx, &initiativespb.ListInitiativesReq{ProjectId: project})
	if err != nil {
		return fmt.Errorf("list: %w", err)
	}
	if len(resp.GetRows()) == 0 {
		fmt.Fprintf(core.Stdout, "no initiatives in project %q\n", project)
		return nil
	}
	for _, i := range resp.GetRows() {
		printInitiative(i)
	}
	return nil
}

// --- show ---------------------------------------------------------

type ShowCmd struct {
	Project string `name:"project" placeholder:"ID" help:"Project id. Empty = read from the current project's lock."`
	Name    string `name:"name" placeholder:"NAME" help:"Initiative name. Empty = current git branch (main/master → trunk)."`
}

func (c *ShowCmd) Run(parent *Cmd) error {
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

	found, err := sc.FindInitiative(project, name)
	if err != nil {
		return fmt.Errorf("show: %w", err)
	}
	if found == nil {
		return fmt.Errorf("no initiative %q in project %q", name, project)
	}
	printInitiative(found)
	return nil
}

// --- materialize --------------------------------------------------

type MaterializeCmd struct {
	Project string `name:"project" placeholder:"ID" help:"Project id. Empty = read from the current project's lock."`
	Name    string `name:"name" placeholder:"NAME" help:"Initiative name. Empty = current git branch (main/master → trunk)."`
	By      string `name:"by" placeholder:"ACTOR" help:"Actor stamp (created_by). Empty = local OS user (v1 self-only)."`
}

func (c *MaterializeCmd) Run(parent *Cmd) error {
	project, err := storageclient.ResolveProjectID(c.Project)
	if err != nil {
		return err
	}
	name, isTrunk, err := storageclient.ResolveInitiativeTarget(c.Name)
	if err != nil {
		return err
	}
	actor := c.By
	if actor == "" {
		actor = storageclient.SelfActor()
	}
	sc, err := storageclient.DialStorageFn(parent.Console)
	if err != nil {
		return err
	}
	defer sc.Close()

	got, _, err := sc.GetOrCreate(project, name, isTrunk, actor)
	if err != nil {
		return fmt.Errorf("materialize: %w", err)
	}
	printInitiative(got)
	return nil
}

// --- push ---------------------------------------------------------

type PushCmd struct {
	Project         string   `name:"project" placeholder:"ID" help:"Project id. Empty = read from the current project's lock."`
	Name            string   `name:"name" placeholder:"NAME" help:"Initiative name. Empty = current git branch (main/master → trunk)."`
	Protos          []string `name:"proto" short:"p" placeholder:"PROTO" help:"Proto schema file(s) to compile into the snapshot's real IR (enables compat diffs). Repeatable. Empty = store a lock-bytes stand-in."`
	Imports         []string `name:"import" short:"I" placeholder:"DIR" help:"Additional proto import path(s) (w17 vocab + shared trees). Repeatable."`
	CompilerVersion string   `name:"compiler-version" placeholder:"VER" default:"dev" help:"Compiler version pinned into the snapshot."`
	By              string   `name:"by" placeholder:"ACTOR" help:"Actor stamp (created_by). Empty = local OS user (v1 self-only)."`
}

func (c *PushCmd) Run(parent *Cmd) error {
	project, err := storageclient.ResolveProjectID(c.Project)
	if err != nil {
		return err
	}
	name, isTrunk, err := storageclient.ResolveInitiativeTarget(c.Name)
	if err != nil {
		return err
	}
	actor := c.By
	if actor == "" {
		actor = storageclient.SelfActor()
	}

	// ir_schema: when --proto is given, compile the REAL IR (the same
	// client-side loader + ir.Build pipeline `migrate generate` uses) and
	// store it — that is what makes `w17ctl compat` produce real findings.
	// Without --proto, fall back to a deterministic lock-bytes stand-in.
	lockBytes, lockHash, err := lockSnapshotBytes()
	if err != nil {
		return err
	}
	irSchema := lockBytes
	if len(c.Protos) > 0 {
		ctx, cancel := core.ClientCtx()
		// Snapshot IR carries the descriptor set so compat WIRE/API fire
		// for relational schemas too (G6) — distinct from `migrate
		// generate`, whose IR stays descriptor-free. The IR is opaque bytes
		// (the client never decodes it); the proto dir + default connection
		// resolve server-side via DescribeLock inside the compile.
		irSchema, err = schema.LoadIRBytesWithDescriptors(ctx, c.Protos, c.Imports, parent.Console)
		cancel()
		if err != nil {
			return fmt.Errorf("compile IR: %w", err)
		}
	}

	sc, err := storageclient.DialStorageFn(parent.Console)
	if err != nil {
		return err
	}
	defer sc.Close()

	// Lazy materialize on first write.
	initiative, created, err := sc.GetOrCreate(project, name, isTrunk, actor)
	if err != nil {
		return fmt.Errorf("push: %w", err)
	}

	// Atomic: store the snapshot + advance the head in one generated
	// multi-op handler (one transaction) — no window where a snapshot is
	// stored but the head is stale (G7).
	pushCtx, pushCancel := core.ClientCtx()
	pushed, err := sc.SM.PushSnapshot(pushCtx, &initiativespb.PushSnapshotReq{
		ProjectId: project, InitiativeId: initiative.GetId(), IrSchema: irSchema,
		LockHash: lockHash, CompilerVersion: c.CompilerVersion, CreatedBy: actor,
	})
	pushCancel()
	if err != nil {
		return fmt.Errorf("push snapshot: %w", err)
	}

	if created {
		// A mutating command whose target initiative was just created is
		// unmissable (part A: never silently push onto a new target).
		fmt.Fprintf(core.Stdout, "⚠ materialized initiative %q (first write on this branch)\n", name)
	}
	fmt.Fprintf(core.Stdout, "pushed snapshot %s → initiative %q (head advanced, by %s)\n", pushed.GetSnapshotId(), name, actor)
	return nil
}

// lockSnapshotBytes reads the project's lock file and returns its bytes
// + sha256 hex — the substrate stand-in for the compiled IR + the
// snapshot's lock_hash.
func lockSnapshotBytes() ([]byte, string, error) {
	root, err := core.FindProjectRootFn()
	if err != nil {
		return nil, "", fmt.Errorf("locate project root: %w", err)
	}
	body, err := os.ReadFile(filepath.Join(root, "w17", "lock.yaml"))
	if err != nil {
		return nil, "", fmt.Errorf("read lock: %w", err)
	}
	sum := sha256.Sum256(body)
	return body, hex.EncodeToString(sum[:]), nil
}

func printInitiative(i *initiativespb.Initiative) {
	trunk := ""
	if i.GetIsTrunk() {
		trunk = " trunk"
	}
	head := i.GetHeadSnapshotId()
	if head == "" {
		head = "(none)"
	}
	fmt.Fprintf(core.Stdout, "%s\t%s%s\tstatus=%s\thead=%s\t(by %s)\n",
		i.GetName(), i.GetId(), trunk, i.GetStatus().String(), head, i.GetCreatedBy())
}
