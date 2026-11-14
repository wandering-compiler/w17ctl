package schema

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/migdecisions"
	w17registrypb "github.com/wandering-compiler/sdk/go/pb/w17registry"
)

// DecisionState is w17/migrate-decisions/ measured against the console now.
type DecisionState struct {
	// Base is the console's base for this plan ("" = no schema stored yet).
	Base string
	// Console is the console address this state was planned on.
	Console string
	// Elsewhere were decided on another console (`console:` names a
	// different one). This console cannot tell whether they were consumed:
	// never sent, never deleted, reported.
	Elsewhere []migdecisions.File
	// Files is every decision file on disk.
	Files []migdecisions.File
	// Consumed were decided against another base — an earlier release used
	// them (or the schema moved under them). Inert: never sent.
	Consumed []migdecisions.File
	// Sent were applied to Plan: current base, well-formed, unambiguous.
	Sent []migdecisions.File
	// Stale are Sent files the console says decide nothing (their change was
	// undone before release, or the column changed again — see
	// ForAnotherChange). Plan was made without them.
	Stale []migdecisions.File
	// Plan is the final dry run, with Sent minus Stale.
	Plan *w17registrypb.PushSchemaResponse
}

// ForAnotherChange reports whether f — a stale or id-less file — has a key
// the plan still needs decided, under a different change: the column
// changed again after the decision was written, so the open question is
// a new one. Matching the console's key string is all it takes; what the
// change IS stays the console's.
func (d *DecisionState) ForAnotherChange(f migdecisions.File) bool {
	for _, o := range d.Plan.GetFindings() {
		if o.GetDecideKey() == f.Key && migdecisions.FindingIdent(o) != f.Ident() {
			return true
		}
	}
	return false
}

// Current reports whether f was written against this state's base, on this
// state's console.
func (d *DecisionState) Current(f migdecisions.File) bool {
	return !d.OnAnotherConsole(f) && f.Base == migdecisions.BaseName(d.Base)
}

// OnAnotherConsole reports whether f records a console other than the one
// this state was planned on. Its base is that console's; this one cannot
// judge it. A file that records none (written before
// `console:`) is judged here, as before. The addresses are compared in their
// normalised form (core.NormalizeConsoleAddr): `grpcs://host:port`,
// `host:port`, any case and every loopback spelling name one console (review
// R1-5 — a CI mint on `host:port` ignored a decision written as
// `grpcs://host:port`).
func (d *DecisionState) OnAnotherConsole(f migdecisions.File) bool {
	return f.Console != "" && core.NormalizeConsoleAddr(f.Console) != core.NormalizeConsoleAddr(d.Console)
}

// RefusedDecisionsError is a dry run the console refused because of a
// decision a file sent (a choice the finding does not offer, a custom: body
// with no SQL). It is the FILE that needs fixing, not the console.
type RefusedDecisionsError struct {
	// Files are the sent files the refusal names (all sent files when it
	// names none of them).
	Files []migdecisions.File
	Err   error
}

func (e *RefusedDecisionsError) Error() string { return e.Err.Error() }
func (e *RefusedDecisionsError) Unwrap() error { return e.Err }

// Live is what a real push should send: Sent minus Stale.
func (d *DecisionState) Live() []migdecisions.File {
	stale := map[string]bool{}
	for _, f := range d.Stale {
		stale[f.Path] = true
	}
	var out []migdecisions.File
	for _, f := range d.Sent {
		if !stale[f.Path] {
			out = append(out, f)
		}
	}
	return out
}

// ResolveDecisions plans args on the console — stores nothing — and measures
// the decision files against it.
//
// Two dry runs at most. The first carries no file decisions and only learns
// the BASE: a file written against another base must not be sent at all,
// because a consumed decision whose column changes AGAIN would otherwise
// silently decide the new change. The second sends the current-base files
// and learns which of them decide nothing.
func ResolveDecisions(root string, args SchemaPushArgs) (*DecisionState, error) {
	files, err := migdecisions.Load(filepath.Join(root, migdecisions.Dir))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", migdecisions.Dir, err)
	}
	return ResolveDecisionFiles(root, args, files)
}

// ResolveDecisionFiles is ResolveDecisions over files instead of what is on
// disk — nil for a plan that ignores them (`migrate check --rewrite`, which
// deletes them only once the console has answered).
func ResolveDecisionFiles(root string, args SchemaPushArgs, files []migdecisions.File) (*DecisionState, error) {
	args.FileDecisions, args.FileCustomSQL = nil, nil
	probe, err := PlanPush(args)
	if err != nil {
		return nil, err
	}
	// PlanPush dialed the same address.
	addr, err := core.ResolveConsoleAddr(args.Console)
	if err != nil {
		return nil, err
	}
	st := &DecisionState{Base: probe.GetBase(), Console: addr, Files: files, Plan: probe}
	var current []migdecisions.File
	for _, f := range files {
		switch {
		case st.OnAnotherConsole(f):
			st.Elsewhere = append(st.Elsewhere, f)
		case f.Base != "" && !st.Current(f):
			st.Consumed = append(st.Consumed, f)
		case st.Current(f):
			current = append(current, f)
		}
	}
	st.Sent = migdecisions.Applicable(current)
	if len(st.Sent) == 0 {
		return st, nil
	}
	if args.FileDecisions, args.FileCustomSQL, err = migdecisions.Payload(root, st.Sent); err != nil {
		return nil, err
	}
	plan, err := PlanPush(args)
	if status.Code(err) == codes.InvalidArgument {
		// The first dry run, without these decisions, was accepted: the
		// console refuses one of the files.
		return nil, &RefusedDecisionsError{Files: namedIn(status.Convert(err).Message(), st.Sent), Err: err}
	}
	if err != nil {
		return nil, err
	}
	st.Plan = plan
	unused := map[string]bool{}
	for _, k := range plan.GetUnusedDecisions() {
		unused[k] = true
	}
	for _, f := range st.Sent {
		if unused[f.Ident()] {
			st.Stale = append(st.Stale, f)
		}
	}
	return st, nil
}

// namedIn picks the files whose decision key a console message names; all of
// them when it names none (the console's wording is its own).
func namedIn(msg string, files []migdecisions.File) []migdecisions.File {
	var out []migdecisions.File
	for _, f := range files {
		if strings.Contains(msg, f.Ident()) || strings.Contains(msg, f.Key) {
			out = append(out, f)
		}
	}
	if len(out) == 0 {
		return files
	}
	return out
}

// ApplyDecisionFiles adds the decisions a release should apply to a push —
// what `migrate generate` and `push` (the CI command) both do, so a decision
// committed in a PR is applied whichever of them mints.
//
// Nothing a release cannot use blocks it: a consumed file (another base) and
// a stale one (its change was undone) are reported and left out — the
// console would refuse a stale one outright — and an undecided or
// conflicting one only matters if its finding is still open, in which case
// the push stops on that finding and says so.
func ApplyDecisionFiles(root string, args *SchemaPushArgs) error {
	// No files, no round trip: most pushes decide nothing.
	if files, err := migdecisions.Load(filepath.Join(root, migdecisions.Dir)); err != nil || len(files) == 0 {
		return err
	}
	st, err := ResolveDecisions(root, *args)
	if err != nil {
		return err
	}
	for _, f := range st.Files {
		if f.Problem != "" && (f.Base == "" || st.Current(f)) {
			fmt.Fprintf(core.Stdout, "decision file skipped: %s: %s\n", relTo(root, f.Path), f.Problem)
		}
	}
	for _, f := range st.Consumed {
		fmt.Fprintf(core.Stdout, "decision consumed by an earlier release, ignored: %s (%s)\n", relTo(root, f.Path), f.Key)
	}
	for _, f := range st.Elsewhere {
		fmt.Fprintf(core.Stdout, "decision made on console %s, ignored on %s: %s (%s)\n", f.Console, st.Console, relTo(root, f.Path), f.Key)
	}
	for _, f := range st.Stale {
		if st.ForAnotherChange(f) {
			fmt.Fprintf(core.Stdout, "decision made for a different change of %s, ignored: %s — `w17ctl migrate check --write` asks again\n", f.Key, relTo(root, f.Path))
			continue
		}
		fmt.Fprintf(core.Stdout, "decision not needed any more, ignored: %s (%s)\n", relTo(root, f.Path), f.Key)
	}
	var current []migdecisions.File
	for _, f := range st.Files {
		if st.Current(f) {
			current = append(current, f)
		}
	}
	_, conflicting := migdecisions.Duplicates(current)
	for _, g := range conflicting {
		fmt.Fprintf(core.Stdout, "decision skipped: %s is decided differently in %d files\n", g[0].Key, len(g))
	}
	args.FileDecisions, args.FileCustomSQL, err = migdecisions.Payload(root, st.Live())
	return err
}

// ConsumeDecisionFiles deletes every decision file after a successful push.
// A decision belongs to the release it decided: applied, stale, consumed or
// never chosen, none of them answers an open question once the push
// succeeded. (Were one left behind it would be inert anyway — its base is
// gone — but the directory should not grow.)
//
// A file decided on ANOTHER console (`console:`) is not this push's: it is
// kept. console is the --console flag value, resolved
// the way the push resolved it.
func ConsumeDecisionFiles(root, lockPath, console string) error {
	files, err := migdecisions.Load(filepath.Join(root, migdecisions.Dir))
	if err != nil {
		return err
	}
	addr, err := core.ResolveConsoleAddr(console)
	if err != nil {
		return err
	}
	kept := 0
	for _, f := range files {
		if f.Console != "" && core.NormalizeConsoleAddr(f.Console) != core.NormalizeConsoleAddr(addr) {
			kept++
			continue
		}
		if err := os.Remove(f.Path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("migrations stored, but removing the consumed decision %s failed: %w", relTo(root, f.Path), err)
		}
		fmt.Fprintf(core.Stdout, "decision consumed: %s\n", relTo(root, f.Path))
	}
	if len(files) > kept {
		fmt.Fprintf(core.Stdout, "commit %s and the removed %s files together — a pipeline that does not leaves them inert, and `w17ctl migrate check --write` deletes them\n", lockPath, migdecisions.Dir)
	}
	return nil
}

func relTo(root, p string) string {
	if r, err := filepath.Rel(root, p); err == nil {
		return r
	}
	return p
}
