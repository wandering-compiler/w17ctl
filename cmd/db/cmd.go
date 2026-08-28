package db

import (
	"context"
	"fmt"

	stack "github.com/wandering-compiler/w17ctl/cmd/stack"
	"github.com/wandering-compiler/w17ctl/internal/autosync"
	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/snapstore"
	"github.com/wandering-compiler/w17ctl/internal/storageclient"
)

// Cmd is `w17ctl db` — local dev-DB tooling. Today: manual snapshots.
type Cmd struct {
	Snapshot SnapshotCmd `cmd:"" help:"Manual DB savepoints scoped to the current initiative — save the live stores, list, activate (restore), delete."`
}

// SnapshotCmd is `w17ctl db snapshot` — point-in-time savepoints of
// the local stores, scoped to an initiative (the dev DB lifecycle's
// manual layer, docs/specs/storage/dev-db-lifecycle.md §workflow modes).
// CRUD the developer drives; the currently-live stores are saved/
// restored. Orthogonal to the branch-switch reconcile (which only
// touches the initiative's main state). Activation guards that the
// savepoint's schema lineage matches the initiative's current
// checkpoint.
type SnapshotCmd struct {
	Save     SnapshotSaveCmd     `cmd:"" help:"Save the live stores to a named savepoint under the current initiative."`
	List     SnapshotListCmd     `cmd:"" help:"List the current initiative's savepoints."`
	Activate SnapshotActivateCmd `cmd:"" help:"Restore a named savepoint into the live stores."`
	Delete   SnapshotDeleteCmd   `cmd:"" help:"Delete a named savepoint."`
}

// dbSnapshotCommon is the shared flag set + resolution for every
// db-snapshot subcommand. The initiative is NOT a flag — it follows the
// same resolution as `stack build` (autosync ⇒ git branch; manual ⇒ the
// active initiative or "default"), so a savepoint is always scoped to
// whatever lineage the dev is currently working on.
type dbSnapshotCommon struct {
	Project string   `name:"project" placeholder:"ID" help:"Project id. Empty = read from the lock."`
	Targets []string `name:"target" short:"t" placeholder:"CONN=DSN" help:"Local store target(s) <connection>=<dsn>. Empty = auto-resolved from the lock + the host ports 'stack up' published."`
	Console string   `name:"console" placeholder:"HOST:PORT" env:"CONSOLE_STORAGE_ADDR" help:"Console storage endpoint (holds the checkpoints, for the activate consistency guard)."`
}

// resolveDb resolves the project root, the current initiative (via the
// shared workflow-mode resolution), and the mode-appropriate snapshot
// backend (local snapstore or remote-side remotesnap) — the common
// preamble. Store targets are resolved lazily by the subcommands that move
// data (save/activate).
func (c *dbSnapshotCommon) resolveDb() (root, initiative string, be snapBackend, err error) {
	root, err = core.FindProjectRoot()
	if err != nil {
		return "", "", nil, err
	}
	initiative, _, err = autosync.ResolveActiveInitiative(root)
	if err != nil {
		return "", "", nil, err
	}
	be, err = resolveBackend(root, c.Targets)
	if err != nil {
		return "", "", nil, err
	}
	return root, initiative, be, nil
}

// resolveConns auto-resolves (or parses --target) the local store targets
// into snapstore conns (the local backend's dump inputs).
func resolveConns(root string, targets []string) ([]snapstore.Conn, error) {
	specs, err := (&stack.BuildCmd{Targets: targets}).ResolveTargets(root)
	if err != nil {
		return nil, err
	}
	if len(specs) == 0 {
		return nil, fmt.Errorf("no resolvable local store targets — run 'stack up' first, or pass --target")
	}
	conns, skipped, err := stack.SnapshotConns(specs)
	if err != nil {
		return nil, err
	}
	for _, s := range skipped {
		fmt.Fprintf(core.Stdout, "db snapshot: skipping store %s\n", s)
	}
	return conns, nil
}

// currentSchemaHash returns the initiative's current checkpoint schema
// hash (the consistency-guard reference) — "" when no checkpoint exists
// yet (a never-built initiative; the guard then no-ops).
func (c *dbSnapshotCommon) currentSchemaHash(project, initiative string) (string, error) {
	sc, err := storageclient.DialStorageFn(c.Console)
	if err != nil {
		return "", err
	}
	defer sc.Close()
	ckpt, err := sc.GetCheckpoint(project, storageclient.SelfActor(), initiative)
	if err != nil {
		return "", err
	}
	if ckpt == nil {
		return "", nil
	}
	return ckpt.GetLockHash(), nil
}

// --- save ---------------------------------------------------------------

type SnapshotSaveCmd struct {
	dbSnapshotCommon
	Name string `arg:"" help:"Savepoint name."`
}

func (c *SnapshotSaveCmd) Run() error {
	_, initiative, be, err := c.resolveDb()
	if err != nil {
		return err
	}
	project, err := storageclient.ResolveProjectID(c.Project)
	if err != nil {
		return err
	}
	// Schema-hash pin for the activate-time consistency guard. A
	// never-built initiative legitimately has no checkpoint (empty hash,
	// guard later no-ops). A console error is different — we cannot pin
	// the lineage, so the savepoint is recorded WITHOUT a guard reference;
	// surface that loudly rather than silently pinning an empty hash.
	schemaHash, err := c.currentSchemaHash(project, initiative)
	if err != nil {
		fmt.Fprintf(core.Stdout, "db snapshot: warning — could not read current schema from console (%v); saving %q without a lineage pin (activate's consistency guard won't protect it)\n", err, c.Name)
		schemaHash = ""
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120e9)
	defer cancel()
	if err := be.save(ctx, initiative, c.Name, schemaHash); err != nil {
		return err
	}
	fmt.Fprintf(core.Stdout, "db snapshot: saved %q for initiative %q\n", c.Name, initiative)
	return nil
}

// --- list ---------------------------------------------------------------

type SnapshotListCmd struct {
	dbSnapshotCommon
}

func (c *SnapshotListCmd) Run() error {
	_, initiative, be, err := c.resolveDb()
	if err != nil {
		return err
	}
	names, err := be.list(initiative)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		fmt.Fprintf(core.Stdout, "no savepoints for initiative %q\n", initiative)
		return nil
	}
	fmt.Fprintf(core.Stdout, "savepoints for initiative %q:\n", initiative)
	for _, n := range names {
		hash, _ := be.schemaHash(initiative, n)
		short := hash
		if len(short) > 12 {
			short = short[:12]
		}
		fmt.Fprintf(core.Stdout, "  %s\t(schema %s)\n", n, short)
	}
	return nil
}

// --- activate -----------------------------------------------------------

type SnapshotActivateCmd struct {
	dbSnapshotCommon
	Name  string `arg:"" help:"Savepoint name to restore."`
	Force bool   `name:"force" help:"Restore even when the savepoint's schema lineage differs from the initiative's current checkpoint (the consistency guard would otherwise refuse)."`
}

func (c *SnapshotActivateCmd) Run() error {
	_, initiative, be, err := c.resolveDb()
	if err != nil {
		return err
	}
	if ok, herr := be.has(initiative, c.Name); herr != nil {
		return herr
	} else if !ok {
		return fmt.Errorf("no savepoint %q for initiative %q", c.Name, initiative)
	}
	project, err := storageclient.ResolveProjectID(c.Project)
	if err != nil {
		return err
	}

	// Consistency guard: the savepoint pins the schema hash it was taken
	// at; refuse to restore it into a diverged schema unless --force.
	// Crucially the guard FAILS CLOSED — if we can't read the pinned hash
	// (disk error) or the current hash (console unreachable), we cannot
	// verify lineage, so we refuse rather than silently restoring possibly
	// incompatible data. --force is the explicit "skip the guard" override.
	pinned, err := be.schemaHash(initiative, c.Name)
	if err != nil && !c.Force {
		return fmt.Errorf("savepoint %q: cannot read its pinned schema: %w (restoring blindly could put old-shaped data into the live stores; pass --force to override)", c.Name, err)
	}
	current, err := c.currentSchemaHash(project, initiative)
	if err != nil && !c.Force {
		return fmt.Errorf("cannot reach the console to read the current schema (the consistency guard's reference): %w (pass --force to restore without the guard)", err)
	}
	if schemaGuardBlocks(pinned, current, c.Force) {
		return fmt.Errorf("savepoint %q was taken at schema %.12s but the initiative is now at %.12s — restoring would put old-shaped data into the new schema; pass --force to override", c.Name, pinned, current)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120e9)
	defer cancel()
	if err := be.load(ctx, initiative, c.Name); err != nil {
		return err
	}
	fmt.Fprintf(core.Stdout, "db snapshot: activated %q for initiative %q\n", c.Name, initiative)
	return nil
}

// schemaGuardBlocks reports whether activating a savepoint must be
// refused: both the savepoint's pinned schema hash and the initiative's
// current one are known AND they differ AND --force was not passed. An
// unknown hash on either side (never-built initiative / unpinned
// savepoint) disables the guard — there's nothing to compare.
func schemaGuardBlocks(pinned, current string, force bool) bool {
	if force || pinned == "" || current == "" {
		return false
	}
	return pinned != current
}

// --- delete -------------------------------------------------------------

type SnapshotDeleteCmd struct {
	dbSnapshotCommon
	Name string `arg:"" help:"Savepoint name to delete."`
}

func (c *SnapshotDeleteCmd) Run() error {
	_, initiative, be, err := c.resolveDb()
	if err != nil {
		return err
	}
	if ok, herr := be.has(initiative, c.Name); herr != nil {
		return herr
	} else if !ok {
		return fmt.Errorf("no savepoint %q for initiative %q", c.Name, initiative)
	}
	if err := be.remove(initiative, c.Name); err != nil {
		return err
	}
	fmt.Fprintf(core.Stdout, "db snapshot: deleted %q for initiative %q\n", c.Name, initiative)
	return nil
}
