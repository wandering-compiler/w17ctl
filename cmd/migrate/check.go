package migrate

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/lockfile"
	"github.com/wandering-compiler/w17ctl/internal/migdecisions"
	"github.com/wandering-compiler/w17ctl/internal/schema"
	"github.com/wandering-compiler/w17ctl/internal/storageclient"
	w17registrypb "github.com/wandering-compiler/sdk/go/pb/w17registry"
)

// ====================================================================
// `w17ctl migrate check` — what would `migrate generate` do now?
// ====================================================================
//
// A pull request's gate for schema changes. It asks the console to plan a
// push of the current proto WITHOUT storing anything (PushSchema dry_run),
// with the decisions in w17/migrate-decisions/, and says whether a release
// could generate its migrations unattended.
//
// It never mints: no migration, no revision, no lock pin — which is also why
// a project whose migrations only CI may write (ci_only) still lets every
// member run it.
//
// # Exit codes — distinct, because a pipeline branches on them
//
//	0  nothing to decide: no change, or every change decided
//	2  a change needs a decision — `--write` creates the file to decide in
//	3  a decision file needs tidying (stale, duplicated, conflicting, or not
//	   chosen yet) — `--write` fixes what it can, `--rewrite` starts over
//	4  (--generated only) the proto has changes no stored migration covers,
//	   or the lock does not pin them — run `migrate generate` and commit
//
// The worst condition wins, in the order 3, 2, 4: tidy the files first,
// then decide, then generate.
//
// # Never a dead end
//
// Every non-zero exit names the command that gets out of it. `--write`
// repairs without losing a choice anybody made; `--rewrite` throws every
// decision file away and writes fresh ones from what the console sees now.
// Both only touch w17/migrate-decisions/, and git keeps what they removed.

const (
	exitNeedsDecision = 2
	exitTidyDecisions = 3
	exitNotGenerated  = 4
)

// checkExitError carries a check's exit code through kong (kong.ExitCoder).
type checkExitError struct {
	code int
	msg  string
}

func (e checkExitError) Error() string { return e.msg }
func (e checkExitError) ExitCode() int { return e.code }

// CheckCmd implements `w17ctl migrate check`.
type CheckCmd struct {
	Write     bool     `name:"write" help:"Tidy w17/migrate-decisions/: write a file for every change still needing a decision, delete files nothing needs any more, merge duplicates that agree. Keeps every choice already made."`
	Rewrite   bool     `name:"rewrite" help:"Start the decision files over: delete everything in w17/migrate-decisions/ and write a fresh file for every change needing a decision. Choices made so far must be made again (git still has them). The way out of any mess."`
	Generated bool     `name:"generated" help:"Release gate: also fail (exit 4) when the proto has changes no stored migration covers, or the lock does not pin them — i.e. 'migrate generate' has not run, or its lock was not committed."`
	Protos    []string `name:"proto" short:"p" placeholder:"PROTO" help:"Path to a .proto schema. Repeatable. Empty = every .proto under the project's proto root."`
	ProjectID string   `name:"project" placeholder:"ID" help:"Project identifier. Empty = project_id from w17/lock.yaml."`
	Console   string   `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console. Optional — falls back to console_addr in w17/lock.yaml, then to the binary's compile-time default."`
	LockPath  string   `name:"lock" placeholder:"PATH" default:"w17/lock.yaml" help:"Lock file whose pinned targets --generated compares against the console."`
}

func (c *CheckCmd) Run() error {
	if c.Write && c.Rewrite {
		return fmt.Errorf("migrate check: --write and --rewrite are alternatives — --rewrite already writes every file fresh")
	}
	src := &GenerateCmd{Protos: c.Protos, ProjectID: c.ProjectID, Console: c.Console}
	if err := src.deriveFromProject(); err != nil {
		return err
	}
	root, err := core.FindProjectRoot()
	if err != nil {
		return fmt.Errorf("migrate check: not inside a w17 project: %w", err)
	}
	dir := filepath.Join(root, migdecisions.Dir)
	args := schema.SchemaPushArgs{Protos: src.Protos, Imports: src.Imports, ProjectID: src.ProjectID, Console: src.Console}

	var removed []string
	if c.Rewrite {
		gone, err := removeAll(dir)
		if err != nil {
			return err
		}
		for _, p := range gone {
			removed = append(removed, rel(root, p))
		}
	}
	st, err := schema.ResolveDecisions(root, args)
	if err != nil {
		return err
	}
	r := assess(root, st)
	r.removed = append(removed, r.removed...)
	if c.Write || c.Rewrite {
		if err := r.repair(root, dir, st); err != nil {
			return err
		}
	}
	if c.Generated && len(r.open) == 0 && len(r.tidy) == 0 {
		// An unreachable console or a refused token is NOT "not generated":
		// it returns as an error (exit 1), so a pipeline never branches on a
		// network failure as if a mint were missing.
		if r.notGenerated, err = c.notGenerated(st.Plan, src); err != nil {
			return fmt.Errorf("migrate check --generated: compare the lock with the console: %w", err)
		}
	}
	r.print()
	return r.verdict()
}

// report is one check's verdict on the plan and the decision files.
type report struct {
	open      []*w17registrypb.Finding // still needing a decision
	fileFor   map[string]bool          // open keys that have a current-base file
	waiting   []migdecisions.File      // undecided files whose finding is open
	consumed  []migdecisions.File      // another base: inert, reported
	deletable []migdecisions.File      // --write removes: stale, redundant, undecided with no finding
	tidy      []string                 // what keeps the check at 3, one line each
	byHand    bool                     // some tidy item only a person (or --rewrite) can settle
	written   []string
	removed   []string
	changed   []string // connections a push would migrate
	// notGenerated lists why --generated fails.
	notGenerated []string
	base         string
	commit       string
	branch       string
	root         string
}

func assess(root string, st *schema.DecisionState) *report {
	r := &report{fileFor: map[string]bool{}, changed: st.Plan.GetChangedConnections(), base: st.Base, root: root}
	r.commit, r.branch = gitShortHead(root), storageclient.GitCurrentBranchFn()
	r.open = st.Plan.GetFindings()
	r.consumed = st.Consumed
	open := map[string]bool{}
	for _, f := range r.open {
		open[f.GetDecideKey()] = true
	}
	var current []migdecisions.File
	for _, f := range st.Files {
		if st.Current(f) {
			current = append(current, f)
			if open[f.Key] {
				r.fileFor[f.Key] = true
			}
		}
	}
	for _, f := range st.Files {
		switch {
		case f.Base != "" && !st.Current(f):
			// consumed — reported by print, never a failure
		case f.Undecided && open[f.Key]:
			r.waiting = append(r.waiting, f)
		case f.Undecided:
			r.deletable = append(r.deletable, f)
			r.tidy = append(r.tidy, fmt.Sprintf("%s: waits for a choice on %s, which no change needs any more — delete it", rel(root, f.Path), f.Key))
		case f.Problem != "":
			r.byHand = true
			r.tidy = append(r.tidy, fmt.Sprintf("%s: %s — fix it by hand, or start over with `w17ctl migrate check --rewrite`", rel(root, f.Path), f.Problem))
		}
	}
	for _, f := range st.Stale {
		r.deletable = append(r.deletable, f)
		r.tidy = append(r.tidy, fmt.Sprintf("%s: decides %s, which no change needs any more — delete it", rel(root, f.Path), f.Key))
	}
	redundant, conflicting := migdecisions.Duplicates(current)
	for _, g := range redundant {
		for _, f := range g[1:] {
			r.deletable = append(r.deletable, f)
			r.tidy = append(r.tidy, fmt.Sprintf("%s: a second file deciding %s the same way — delete it", rel(root, f.Path), f.Key))
		}
	}
	for _, g := range conflicting {
		var paths []string
		for _, f := range g {
			paths = append(paths, fmt.Sprintf("%s (%s)", rel(root, f.Path), f.Flag()))
		}
		r.byHand = true
		r.tidy = append(r.tidy, fmt.Sprintf("%s is decided differently in %d files — a database owner keeps ONE: %s", g[0].Key, len(g), strings.Join(paths, ", ")))
	}
	return r
}

// repair applies --write / --rewrite: deletes the consumed files and every
// file nothing needs, then writes a file for each open finding without one.
// Conflicts, broken files and undecided files whose question is open stay:
// they hold a person's choice, or the question waiting for one.
func (r *report) repair(root, dir string, st *schema.DecisionState) error {
	for _, f := range append(append([]migdecisions.File(nil), r.consumed...), r.deletable...) {
		if err := os.Remove(f.Path); err != nil && !os.IsNotExist(err) {
			return err
		}
		r.removed = append(r.removed, rel(root, f.Path))
	}
	r.consumed, r.deletable = nil, nil
	var keep []string
	for _, t := range r.tidy {
		if strings.Contains(t, "— delete it") {
			continue
		}
		keep = append(keep, t)
	}
	r.tidy = keep
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	now := time.Now()
	for _, f := range r.open {
		if r.fileFor[f.GetDecideKey()] {
			continue
		}
		p := filepath.Join(dir, migdecisions.FileName(now, r.commit, f.GetDecideKey()))
		if err := os.WriteFile(p, migdecisions.Render(f, st.Base, r.commit, r.branch), 0o644); err != nil {
			return err
		}
		r.fileFor[f.GetDecideKey()] = true
		r.written = append(r.written, rel(root, p))
		if written, rerr := migdecisions.Load(dir); rerr == nil {
			for _, w := range written {
				if w.Path == p {
					r.waiting = append(r.waiting, w)
				}
			}
		}
	}
	return nil
}

// notGenerated is the release gate: a change no stored migration covers, or
// a lock that does not pin the console's latest migration — id AND content
// digest — per connection. Errors are infrastructure, not drift.
func (c *CheckCmd) notGenerated(plan *w17registrypb.PushSchemaResponse, src *GenerateCmd) ([]string, error) {
	var out []string
	for _, conn := range plan.GetChangedConnections() {
		out = append(out, fmt.Sprintf("%s: the proto has changes no stored migration covers — run `w17ctl migrate generate`", conn))
	}
	if len(out) > 0 {
		return out, nil
	}
	return lockBehindConsole(src.Console, src.ProjectID, c.LockPath)
}

func (r *report) print() {
	w := core.Stdout
	for _, p := range r.removed {
		fmt.Fprintf(w, "removed %s\n", p)
	}
	for _, p := range r.written {
		fmt.Fprintf(w, "wrote   %s\n", p)
	}
	for _, f := range r.consumed {
		fmt.Fprintf(w, "decision consumed by an earlier release (inert): %s — `w17ctl migrate check --write` deletes it\n", rel(r.root, f.Path))
	}
	if len(r.changed) > 0 && len(r.open) == 0 {
		fmt.Fprintf(w, "migrate check: a release would generate migrations for: %s\n", strings.Join(r.changed, ", "))
	}
	if len(r.open) == 0 && len(r.tidy) == 0 && len(r.notGenerated) == 0 {
		if len(r.changed) == 0 {
			fmt.Fprintln(w, "migrate check: ok — the proto matches the stored schema; nothing to generate")
		} else {
			fmt.Fprintln(w, "migrate check: ok — nothing needs a decision")
		}
	}
	for _, f := range r.open {
		where := "no decision file yet — `w17ctl migrate check --write`"
		if r.fileFor[f.GetDecideKey()] {
			where = "decision file waiting for a database owner to keep ONE option"
		}
		fmt.Fprintf(w, "needs a decision: %s — %s (%s)\n", f.GetDecideKey(), f.GetRationale(), where)
	}
	for _, t := range r.tidy {
		fmt.Fprintf(w, "decision file: %s\n", t)
	}
	for _, n := range r.notGenerated {
		fmt.Fprintf(w, "not generated: %s\n", n)
	}
	writeStepSummary(r)
}

// tidyAdvice names what gets out of exit 3 for THIS state: --write when it
// can settle everything left, otherwise the person (or --rewrite).
func (r *report) tidyAdvice() string {
	if r.byHand {
		return "a database owner settles them (keep ONE decision per change, fix broken files), or start over with `w17ctl migrate check --rewrite`; then commit " + migdecisions.Dir
	}
	return "run `w17ctl migrate check --write` and commit " + migdecisions.Dir
}

func (r *report) verdict() error {
	switch {
	case len(r.tidy) > 0:
		return checkExitError{exitTidyDecisions, "decision files in " + migdecisions.Dir + " need tidying — " + r.tidyAdvice()}
	case len(r.open) > 0:
		for _, f := range r.open {
			if !r.fileFor[f.GetDecideKey()] {
				return checkExitError{exitNeedsDecision, fmt.Sprintf("%d schema change(s) need a decision — run `w17ctl migrate check --write`, commit %s, and have a database owner keep ONE option in each file", len(r.open), migdecisions.Dir)}
			}
		}
		return checkExitError{exitNeedsDecision, fmt.Sprintf("%d decision file(s) in %s are waiting for a database owner to keep ONE option each", len(r.open), migdecisions.Dir)}
	case len(r.notGenerated) > 0:
		return checkExitError{exitNotGenerated, "migrations are not generated for this proto — run `w17ctl migrate generate` and commit w17/lock.yaml"}
	}
	return nil
}

// writeStepSummary appends a markdown report to $GITHUB_STEP_SUMMARY when a
// GitHub Actions job provides one — the job page is where a PR author looks.
func writeStepSummary(r *report) {
	path := os.Getenv("GITHUB_STEP_SUMMARY")
	if path == "" {
		return
	}
	var b strings.Builder
	b.WriteString("### w17 migrate check\n\n")
	switch {
	case len(r.tidy) > 0:
		b.WriteString("❌ **Decision files need tidying** — " + r.tidyAdvice() + ".\n\n")
		for _, t := range r.tidy {
			b.WriteString("- " + t + "\n")
		}
	case len(r.open) > 0:
		b.WriteString("❌ **Schema changes need a database owner's decision.**\n\n")
		for _, f := range r.open {
			next := "no decision file yet — run `w17ctl migrate check --write` and commit `" + migdecisions.Dir + "`"
			if r.fileFor[f.GetDecideKey()] {
				next = "its decision file waits for a database owner to keep ONE option"
			}
			fmt.Fprintf(&b, "- `%s` — %s (%s)\n", f.GetDecideKey(), f.GetRationale(), next)
		}
	case len(r.notGenerated) > 0:
		b.WriteString("❌ **Migrations are not generated for this proto.** Run `w17ctl migrate generate` and commit `w17/lock.yaml`.\n\n")
		for _, n := range r.notGenerated {
			b.WriteString("- " + n + "\n")
		}
	default:
		b.WriteString("✅ Nothing needs a decision.")
		if len(r.changed) > 0 {
			b.WriteString(" A release generates migrations for: " + strings.Join(r.changed, ", ") + ".")
		}
		b.WriteString("\n")
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	_, _ = f.WriteString(b.String() + "\n")
}

// removeAll deletes every decision file (not the directory, and nothing that
// is not a .yaml decision).
func removeAll(dir string) ([]string, error) {
	files, err := migdecisions.Load(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, f := range files {
		if err := os.Remove(f.Path); err != nil && !os.IsNotExist(err) {
			return out, err
		}
		out = append(out, f.Path)
	}
	sort.Strings(out)
	return out, nil
}

func gitShortHead(root string) string {
	cmd := exec.Command("git", "rev-parse", "--short", "HEAD")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func rel(root, p string) string {
	if r, err := filepath.Rel(root, p); err == nil {
		return r
	}
	return p
}

// lockBehindConsole names every connection whose lock target is not the
// console's latest migration for it — generated, but the lock carrying the
// pin was not committed (or was overwritten). Migrations come back in stored
// order, so the last one per connection is its head; a squash appends its
// baseline at the end, so that holds after one too.
func lockBehindConsole(console, projectID, lockPath string) ([]string, error) {
	lk, err := lockfile.Load(lockPath)
	if err != nil {
		return nil, err
	}
	pinned := map[string]lockfile.Connection{}
	for _, c := range lk.Connections {
		pinned[c.Name] = c
	}
	addr, err := core.ResolveConsoleAddr(console)
	if err != nil {
		return nil, err
	}
	cl, conn, err := core.DialProjectRegistry(addr)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := cl.ListMigrations(ctx, &w17registrypb.ListMigrationsRequest{ProjectId: projectID})
	if err != nil {
		return nil, err
	}
	head := map[string]*w17registrypb.Migration{}
	for _, m := range resp.GetMigrations() {
		head[m.GetConnection()] = m
	}
	conns := make([]string, 0, len(head))
	for c := range head {
		conns = append(conns, c)
	}
	sort.Strings(conns)
	var out []string
	// Id AND digest: the deploy verifies the artefact against the pinned
	// digest, so a right id with an empty or stale digest pins nothing.
	for _, c := range conns {
		got, h := pinned[c], head[c]
		switch {
		case got.TargetMigrationID == "":
			out = append(out, fmt.Sprintf("%s: the lock pins no migration, the console's latest is %s — commit the lock `migrate generate` wrote", c, h.GetId()))
		case got.TargetMigrationID != h.GetId():
			out = append(out, fmt.Sprintf("%s: the lock pins %s, the console's latest is %s — commit the lock `migrate generate` wrote", c, got.TargetMigrationID, h.GetId()))
		case got.TargetContentSha256 != h.GetContentSha256():
			out = append(out, fmt.Sprintf("%s: the lock pins %s with digest %q, the console's is %q — commit the lock `migrate generate` wrote", c, h.GetId(), got.TargetContentSha256, h.GetContentSha256()))
		}
	}
	return out, nil
}
