// Package autosync resolves the dev-DB workflow mode + the active
// initiative for a project (docs/specs/storage/dev-db-lifecycle.md
// §workflow modes). Autosync (Mode A, default) ties the initiative to the
// current git branch; manual (Mode B) uses an explicitly-activated
// initiative or the fixed "default" lineage. Shared by the stack cluster
// (build/reconcile/manage), initiative activate, and db snapshot.
package autosync

import (
	"fmt"

	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/devconfig"
	"github.com/wandering-compiler/w17ctl/internal/snapstore"
	"github.com/wandering-compiler/w17ctl/internal/storageclient"
)

// DefaultInitiative is the single implicit initiative used in manual
// (autosync-off) mode when no active initiative is set — the "always one
// working copy" workflow.
const DefaultInitiative = "default"

// ResolveActiveInitiative resolves the dev-DB initiative (the checkpoint +
// snapshot scope) and the reconcile branch-source for the project, from
// the workflow mode + state — there is NO per-command flag:
//
//   - autosync ON (default) → the git-derived initiative (main/master →
//     trunk); reconcile against that same name;
//   - autosync OFF, an active initiative set (`initiative activate`) →
//     that name; reconcile against it;
//   - autosync OFF, no active initiative → the fixed "default" initiative,
//     NO reconcile.
func ResolveActiveInitiative(root string) (initiative string, branchFn func() string, err error) {
	p := DevProjectFor(root)
	if EffectiveAutosync(lockAutosyncBestEffort(root), p) {
		name, _, terr := storageclient.ResolveInitiativeTarget("")
		if terr != nil {
			// Autosync needs a git branch; on detached HEAD / no repo,
			// point at the two ways out (not the removed --name flag).
			return "", nil, fmt.Errorf("autosync is on but there's no current git branch (detached HEAD or not a git repo) — checkout a branch, or set autosync:false and use 'w17ctl initiative activate <name>'")
		}
		return name, func() string { return name }, nil
	}
	active := ""
	if p != nil {
		active = p.ActiveInitiative
	}
	if active == "" {
		return DefaultInitiative, nil, nil
	}
	return active, func() string { return active }, nil
}

// DevProjectFor returns the devconfig project for a root, best-effort (nil
// on any error — callers fall back to the lock / defaults).
func DevProjectFor(root string) *devconfig.Project {
	cfg, err := core.LoadDevConfigFn()
	if err != nil {
		return nil
	}
	_, p := cfg.FindByPath(root)
	return p
}

// On reports whether the project's autosync (branch-driven) mode is in
// effect (lock default + devconfig override).
func On(root string) bool {
	return EffectiveAutosync(lockAutosyncBestEffort(root), DevProjectFor(root))
}

// lockAutosyncBestEffort asks the console for the lock's autosync tri-state
// (§8.2 — the client holds no lock types), returning nil when the lock is
// absent / unreadable / the console is unreachable. A nil result makes
// EffectiveAutosync fall back to the devconfig override or the default-on. The
// console address is resolved from W17_CONSOLE_ADDR / the compiled default (these
// dev-DB reads carry no per-command --console flag).
func lockAutosyncBestEffort(root string) *bool {
	view, err := core.DescribeLockFromRoot("", root)
	if err != nil {
		return nil
	}
	switch view.GetAutosync() {
	case "true":
		b := true
		return &b
	case "false":
		b := false
		return &b
	default:
		return nil
	}
}

// StaleDBHint returns a one-line hint (or "") when the local DB was last
// built for a different initiative than the current one — i.e. a branch
// switch happened but no `stack build` synced the DB yet. Best-effort +
// non-blocking.
func StaleDBHint(root string) string {
	if !On(root) {
		return ""
	}
	cur, _, err := ResolveActiveInitiative(root)
	if err != nil {
		return ""
	}
	last, err := snapstore.New(root).LastLive()
	if err != nil || last == "" || last == cur {
		return ""
	}
	return fmt.Sprintf("note: the local DB was last built for initiative %q but you're on %q now — run 'w17ctl stack build' to sync the schema/data", last, cur)
}

// EffectiveAutosync resolves the dev-DB workflow mode for the project:
// autosync (Mode A, default) vs manual (Mode B). Precedence: the
// dev-machine-local devconfig override wins; else the lock's project
// default; else default-on. So a team sets the default in the signed lock
// and a dev opts out locally in ~/.w17/config.yaml.
func EffectiveAutosync(lockAutosync *bool, p *devconfig.Project) bool {
	if p != nil && p.Autosync != nil {
		return *p.Autosync
	}
	if lockAutosync != nil {
		return *lockAutosync
	}
	return true
}
