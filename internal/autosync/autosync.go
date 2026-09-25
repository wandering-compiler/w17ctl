// Package autosync resolves the dev-DB workflow mode + the active
// initiative for a project (docs/specs/storage/dev-db-lifecycle.md
// §workflow modes). Autosync (Mode A, default) ties the initiative to the
// current git branch; manual (Mode B) uses an explicitly-activated
// initiative or the fixed "default" lineage. Shared by the stack cluster
// (build/reconcile/manage), initiative activate, and db snapshot.
package autosync

import (
	"fmt"
	"path/filepath"

	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/devconfig"
	"github.com/wandering-compiler/w17ctl/internal/lockfile"
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
			// Name the unborn-branch case instead of blaming the repo: the
			// two causes the generic message lists are both visibly false
			// there, so it sends the reader to check something that is fine.
			if b := storageclient.GitUnbornBranchFn(); b != "" {
				return "", nil, fmt.Errorf("autosync is on and branch %q has no commits yet — the initiative is derived from the branch, and a branch with no HEAD cannot be resolved. Make the first commit (`git commit`), or set autosync:false and use 'w17ctl initiative activate <name>'", b)
			}
			// On a CI runner neither way out below can be taken: the
			// checkout is a commit by construction (GitLab detaches every
			// job, Azure too, GitHub on pull_request), and `initiative
			// activate` writes developer state that is not in the repo.
			// The branch is not lost there, it is in a provider variable —
			// so name the one thing the reader can actually set.
			return "", nil, fmt.Errorf("autosync is on but there's no current git branch (detached HEAD or not a git repo) — checkout a branch, or set %s to the branch being built (that is the CI case: a runner's checkout has no branch, and your provider exposes it as CI_COMMIT_REF_NAME, github.head_ref, BITBUCKET_BRANCH or similar), or set autosync:false and use 'w17ctl initiative activate <name>'", storageclient.InitiativeEnv)
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
//
// Resolved by the lock's `project:` where there is one, like every other caller
// that ends up naming a store. Best-effort stays best-effort: an ambiguous or
// mismatched registry yields nil here rather than an error, because this feeds a
// MODE decision (is autosync on) and not a connection — the commands that open a
// database surface the same refusal loudly at their own call sites.
func DevProjectFor(root string) *devconfig.Project {
	cfg, err := core.LoadDevConfigFn()
	if err != nil {
		return nil
	}
	name, p, rerr := cfg.ResolveProject(lockProjectBestEffort(root), root)
	if rerr == nil && p != nil {
		return p
	}
	if rerr == nil {
		_ = name
		return nil
	}
	// ⚠️ The strict resolution refused — a lock naming a project this machine
	// has not registered, or two entries on one path. Every OTHER caller stops
	// there, and should: they are about to open a database.
	//
	// This one is not. It answers "is autosync on for this checkout", reading
	// only the mode override; the entry's PORTS are never touched here. Refusing
	// would silently flip the mode of any checkout whose lock names a project
	// registered elsewhere — a behaviour change with none of the safety, since
	// no store is reached either way. So fall back to the path, which is at
	// least deterministic now.
	_, p = cfg.FindByPath(root)
	return p
}

// lockProjectBestEffort reads `project:` from the checkout's lock, or "".
func lockProjectBestEffort(root string) string {
	lk, err := lockfile.Load(filepath.Join(root, "w17", "lock.yaml"))
	if err != nil || lk == nil {
		return ""
	}
	return lk.Project
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
