package initiative

import (
	"fmt"

	project "github.com/wandering-compiler/w17ctl/cmd/project"

	"github.com/wandering-compiler/w17ctl/internal/autosync"
	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/snapstore"
)

// Manual-mode initiative pointer (docs/specs/storage/dev-db-lifecycle.md
// §workflow modes). When autosync is OFF, the active initiative is a
// persistent per-project pointer in `~/.w17/config.yaml` that
// `stack build` + `db snapshot` scope to — so the dev switches DB
// lineages with `initiative activate <name>` instead of a per-command
// flag. In autosync mode these commands refuse: the git branch IS the
// initiative there.

// errAutosyncInitiative is the refusal when these manual-mode commands
// run with autosync on.
func errAutosyncInitiative() error {
	return fmt.Errorf("autosync is on — the initiative follows your git branch automatically; switch git branches instead, or set autosync:false (lock or ~/.w17/config.yaml) to manage initiatives manually")
}

// setActiveInitiative registers the project (if needed) and persists the
// active-initiative pointer. Refuses in autosync mode.
func setActiveInitiative(root, name string) (string, error) {
	if autosync.On(root) {
		return "", errAutosyncInitiative()
	}
	cfg, err := core.LoadDevConfigFn()
	if err != nil {
		return "", err
	}
	pname, err := project.EnsureRegistered(cfg, root)
	if err != nil {
		return "", err
	}
	cfg.Projects[pname].ActiveInitiative = name
	if err := core.SaveDevConfigFn(cfg); err != nil {
		return "", err
	}
	return pname, nil
}

// CreateCmd — `w17ctl initiative create <name>`.
type CreateCmd struct {
	Name string `arg:"" help:"New initiative name (a fresh local DB lineage)."`
}

func (c *CreateCmd) Run() error {
	root, err := core.FindProjectRoot()
	if err != nil {
		return err
	}
	if autosync.On(root) {
		return errAutosyncInitiative()
	}
	// Guard: don't "create" over an initiative that already has local
	// state — switch to it with `activate` instead.
	if snapstore.New(root).Has(c.Name) {
		return fmt.Errorf("initiative %q already has local DB state — use 'w17ctl initiative activate %s' to switch to it", c.Name, c.Name)
	}
	if _, err := setActiveInitiative(root, c.Name); err != nil {
		return err
	}
	fmt.Fprintf(core.Stdout, "initiative: created + activated %q — 'stack build' will create its DB lineage on the next build\n", c.Name)
	return nil
}

// ActivateCmd — `w17ctl initiative activate <name>`.
type ActivateCmd struct {
	Name string `arg:"" help:"Initiative to switch to."`
}

func (c *ActivateCmd) Run() error {
	root, err := core.FindProjectRoot()
	if err != nil {
		return err
	}
	if _, err := setActiveInitiative(root, c.Name); err != nil {
		return err
	}
	fmt.Fprintf(core.Stdout, "initiative: active initiative is now %q — the next 'stack build' snapshots the outgoing one and restores/builds %q\n", c.Name, c.Name)
	return nil
}
