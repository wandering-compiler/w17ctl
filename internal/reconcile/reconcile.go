// Package reconcile is the branch-switch reconcile of the dev DB
// lifecycle (docs/specs/storage/dev-db-lifecycle.md S7). When a
// DB-touching command (`stack build`, `stack up --build`) notices the
// current git branch differs from the branch w17 last built live, it
// snapshots the outgoing branch's stores and swaps in the incoming
// branch's state — so the local DB always matches the branch you are
// on, with no hand reconciliation.
//
// The flow (only on a detected switch):
//
//  1. Quiesce — stop every container EXCEPT the stateful stores, so a
//     plain read-dump is consistent (no writers).
//  2. Dump the OUTGOING branch's stores to w17/tmp/<old>/db/ (snapstore).
//  3. Restore or build fresh the INCOMING branch:
//     - snapshot exists  → restore every store from it;
//     - no snapshot      → build the stores fresh from the new branch's
//     IR and re-seed fixtures.
//  4. Dev diff-apply — bring the restored/fresh state up to the new
//     branch's current proto (snapshot-restore + diff-apply compose
//     self-healingly: the snapshot need not be current; the diff
//     converges it).
//  5. Record the incoming branch as the new live branch.
//
// This package is pure orchestration over injected effects (Deps) so it
// stays testable without docker / real stores; the w17ctl command wires
// the real implementations.
package reconcile

import (
	"context"
	"fmt"
)

// Deps are the injected effects the reconcile drives. The w17ctl
// command supplies real implementations (git, snapstore, docker,
// devapply); tests supply fakes. Every func may be nil ONLY if the flow
// provably never calls it for the path under test — Run guards the
// required ones.
type Deps struct {
	// CurrentBranch returns the current git branch ("" = detached HEAD /
	// no repo → reconcile is skipped, the caller falls back to --name).
	CurrentBranch func() string
	// SkipSnapshot switches WITHOUT snapshotting the outgoing branch.
	//
	// It is not a shortcut, it is a choice with a price: the outgoing
	// branch's dev data stops being recoverable, so switching back to it
	// later finds no snapshot and builds fresh. The default refuses, because
	// the alternative is a command that silently destroys the database of
	// the branch you just left (marb #62).
	SkipSnapshot bool

	// LastLive / SetLastLive read + persist the branch w17 last built
	// live (snapstore).
	LastLive    func() (string, error)
	SetLastLive func(branch string) error
	// HasSnapshot reports whether a stored snapshot exists for a branch.
	HasSnapshot func(branch string) bool

	// Quiesce stops every container except the stateful stores.
	Quiesce func(ctx context.Context) error

	// Resume starts back exactly what Quiesce stopped. It runs on EVERY
	// path out of the reconcile, the failing ones included: the services
	// are down because this function stopped them, so leaving them down
	// after a failed snapshot or a failed apply hands the operator a second
	// problem on top of the one they already have. `restart: unless-stopped`
	// does not cover it — a compose `stop` is explicit and that policy is
	// defined not to undo one.
	Resume func(ctx context.Context) error
	// Dump snapshots the given (outgoing) branch's stores to disk.
	Dump func(ctx context.Context, branch string) error
	// Restore loads the given (incoming) branch's stores from disk.
	Restore func(ctx context.Context, branch string) error
	// BuildFresh builds the stores fresh from the current branch's IR
	// (bootstrap-plan on empty stores) — the no-snapshot path.
	BuildFresh func(ctx context.Context) error
	// SeedFixtures auto-applies the branch's fixtures after a fresh
	// build, so dev has working seed data.
	SeedFixtures func(ctx context.Context) error
	// DiffApply runs the dev diff-apply (checkpoint → current proto)
	// onto the now-restored/fresh stores.
	DiffApply func(ctx context.Context) error

	// Logf emits progress (nil → discarded).
	Logf func(string, ...any)
}

// Outcome reports what Run did, for the caller's messaging + tests.
type Outcome struct {
	// Switched is true when a branch change was detected and reconciled.
	Switched bool
	// From / To are the outgoing / incoming branches (To set whenever a
	// branch resolved, even with no switch).
	From, To string
	// Fresh is true when the incoming branch had no snapshot and was
	// built fresh + fixture-seeded (only meaningful when Switched).
	Fresh bool
	// Unsnapshotted names the outgoing branch that was left WITHOUT a
	// snapshot because --no-snapshot was given. Empty on every ordinary
	// run. The caller reports it in the line that announces success, so
	// the hatch leaves a trace where the result is read.
	Unsnapshotted string
}

// snapshotRefusal explains a failed outgoing snapshot.
//
// The bare wrapped error named a missing `pg_dump` and stopped, which left
// three things unsaid that a person standing in front of it needs: what state
// their project is in, that EVERY later build will stop in the same place
// (last-live still names the outgoing branch, correctly — the stores do hold
// its data), and that there is a way on at all. marb ran it twice to find out
// the second one.
func snapshotRefusal(last string, err error) error {
	return fmt.Errorf(`reconcile: snapshot %q: %w

  Nothing was changed: the stores still hold %[1]q's data, and the services
  this stopped have been restarted. Every `+"`stack build`"+` in this checkout will
  stop here until the snapshot can be taken — the switch is not reconciled,
  and recording it as if it were would leave the database and the tree
  describing different branches.

  Two ways on:
    - give this machine the store's client (pg_dump / mysqldump), or leave the
      store's container running so the dump can be taken inside it; or
    - `+"`w17ctl stack build --no-snapshot`"+` to switch WITHOUT snapshotting %[1]q.
      Its dev data stops being recoverable: coming back to that branch later
      finds no snapshot and builds fresh.`, last, err)
}

// Run detects a branch switch and, if any, reconciles the local stores.
// It is a no-op (Switched=false) when the branch is unchanged since the
// last live build, or on the very first build (no prior live branch) —
// in both cases the caller's normal diff-apply handles the DB. The live
// branch is recorded on every successful call so the next command can
// detect the next switch.
func Run(ctx context.Context, d Deps) (Outcome, error) {
	logf := d.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if d.CurrentBranch == nil || d.LastLive == nil || d.SetLastLive == nil {
		return Outcome{}, fmt.Errorf("reconcile: CurrentBranch/LastLive/SetLastLive are required")
	}

	current := d.CurrentBranch()
	if current == "" {
		// Detached HEAD / no repo — can't branch-scope; skip silently.
		return Outcome{}, nil
	}
	out := Outcome{To: current}

	last, err := d.LastLive()
	if err != nil {
		return out, fmt.Errorf("reconcile: read last-live: %w", err)
	}
	out.From = last

	// No switch: first build (last=="") or same branch. Just (re)record
	// the live branch; the caller's diff-apply brings the DB current.
	if last == "" || last == current {
		if err := d.SetLastLive(current); err != nil {
			return out, fmt.Errorf("reconcile: record live branch: %w", err)
		}
		return out, nil
	}

	// Switch detected: last != current.
	out.Switched = true
	logf("branch switch detected: %s → %s — reconciling local stores", last, current)

	if err := d.Quiesce(ctx); err != nil {
		return out, fmt.Errorf("reconcile: quiesce: %w", err)
	}
	if d.Resume != nil {
		defer func() {
			if err := d.Resume(ctx); err != nil {
				logf("reconcile: could not restart the services it stopped: %v", err)
				logf("  run `w17ctl stack up` to bring them back")
			}
		}()
	}
	if d.SkipSnapshot {
		// Said on the line that reports the switch, not buried: an escape
		// hatch whose output looks like a clean run is how people take one
		// without knowing they did.
		out.Unsnapshotted = last
		logf("NOT snapshotting %q (--no-snapshot) — switching back to it will find no snapshot and build fresh, losing its dev data", last)
	} else {
		logf("snapshotting outgoing branch %q", last)
		if err := d.Dump(ctx, last); err != nil {
			return out, snapshotRefusal(last, err)
		}
	}

	if d.HasSnapshot != nil && d.HasSnapshot(current) {
		logf("restoring snapshot for %q", current)
		if err := d.Restore(ctx, current); err != nil {
			return out, fmt.Errorf("reconcile: restore %q: %w", current, err)
		}
	} else {
		out.Fresh = true
		logf("no snapshot for %q — building fresh + seeding fixtures", current)
		if err := d.BuildFresh(ctx); err != nil {
			return out, fmt.Errorf("reconcile: build fresh: %w", err)
		}
		if d.SeedFixtures != nil {
			if err := d.SeedFixtures(ctx); err != nil {
				return out, fmt.Errorf("reconcile: seed fixtures: %w", err)
			}
		}
	}

	// Converge restored/fresh state to the current proto.
	if d.DiffApply != nil {
		if err := d.DiffApply(ctx); err != nil {
			return out, fmt.Errorf("reconcile: diff-apply: %w", err)
		}
	}

	if err := d.SetLastLive(current); err != nil {
		return out, fmt.Errorf("reconcile: record live branch: %w", err)
	}
	logf("reconcile complete — local stores now match %q", current)
	return out, nil
}
