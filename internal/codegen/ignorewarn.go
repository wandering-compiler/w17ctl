package codegen

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/wandering-compiler/w17ctl/internal/scaffold"
)

// warnUningnoredGeneratedRoots tells the author when codegen has just written a
// generated tree their git will COMMIT.
//
// The gap it closes, measured on a consumer rather than reasoned: they pointed
// a React client at a path under their own front-end tree — an ordinary thing to want, the
// generated client next to the front-end source — and got 91 generated files
// committed to git and absent from every artefact. Their stub root escaped only
// because they had hand-written a second `.gitignore` for it; nothing told them
// the client root needed the same. On their project it is 7,597 lines per run.
//
// Why here and not in the compiler: a `.gitignore` governs its own directory and
// below, so the one codegen writes into `w17/` CANNOT reach a root outside it —
// there is no file for the console to fix. What is left is to say so, and only
// the client can, because the console never sees the consumer's repo.
//
// ⚠️ `git check-ignore` rather than reading the files: gitignore has negation,
// precedence between nested files, and per-directory scope, and a
// half-implementation that concluded "already ignored" would go SILENT on
// exactly the tree it exists to catch. Asking git is asking the authority that
// will actually decide. When git cannot answer — not installed, not a repo —
// this says nothing at all: there is no ignore question in a directory git does
// not track, and inventing one would be noise a reader cannot act on.
func warnUningnoredGeneratedRoots(root string, roots []string, stdout io.Writer) {
	if len(roots) == 0 || !gitCanAnswer(root) {
		return
	}
	// ⚠️ A checkout can COMMIT its generated trees on purpose, and this repo
	// does: `examples/*` are golden fixtures whose regenerated diff is how a
	// compiler change proves it reached anything. Their `stubs` root is
	// `srcgo/gen` — outside `w17/` and deliberately tracked — so without this
	// the warning would fire for every example on every regen and advise
	// ignoring the very trees under review. Caught before it shipped by
	// checking what the corpus actually declares rather than assuming.
	//
	// The signal already exists for exactly this distinction and is set by this
	// repo's regen, never by a consumer, so it needs no new knob.
	if os.Getenv(scaffold.TrackGeneratedEnv) == "1" {
		return
	}
	// Two groups, because they need DIFFERENT remedies and telling the second
	// one to add an ignore rule would be advice they have already followed.
	//
	// `git check-ignore` reports a TRACKED path as not-ignored even when a rule
	// matches it — measured, not assumed — and git does keep committing changes
	// to it, so warning is right. But the fix there is to UNTRACK, and a reader
	// who added the rule and is told to add the rule reads it as noise and
	// stops reading. `--no-index` answers the other half: is there a rule at
	// all, index aside.
	var needRule, needUntrack []string
	for _, r := range roots {
		r = strings.Trim(strings.TrimSpace(r), "/")
		if r == "" || r == "." || strings.HasPrefix(r, "..") {
			continue
		}
		r = path.Clean(r)
		// Inside `w17/` is the compiler's own tree — it writes the ignore there
		// itself, and a warning about it would be a warning about a file the
		// reader does not own.
		if r == "w17" || strings.HasPrefix(r, "w17/") {
			continue
		}
		// A root the lock DECLARES but this run did not write is not a tree git
		// will commit, and the first line of this warning says it is. A lock
		// can carry a root for a generator the project has since stopped
		// asking for; advising an ignore rule for a directory that is not
		// there names a path the reader will not recognise, and the claim
		// above it is simply false. Runs after generation (see the call site),
		// so absent here means absent on disk.
		//
		// ABSENCE only. Any other stat failure — a parent directory this
		// process cannot traverse, say — means the check could not tell, and
		// the rule for that in this file is to warn rather than go quiet.
		if _, err := os.Stat(filepath.Join(root, r)); errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if gitIgnores(root, r) {
			continue
		}
		if gitHasIgnoreRule(root, r) {
			needUntrack = append(needUntrack, r)
			continue
		}
		needRule = append(needRule, r)
	}
	if len(needRule) == 0 && len(needUntrack) == 0 {
		return
	}
	sort.Strings(needRule)
	sort.Strings(needUntrack)

	fmt.Fprintf(stdout,
		"codegen: warning: %d generated root(s) your git will COMMIT\n"+
			"  why: codegen rewrites these on every run, and a `.gitignore` reaches only its own\n"+
			"       directory and below, so the one in `w17/` cannot cover a root outside it\n",
		len(needRule)+len(needUntrack))
	if len(needRule) > 0 {
		fmt.Fprintf(stdout, "  no ignore rule covers: %s\n"+
			"  fix: add a line per root to the project's top-level `.gitignore`, e.g.\n",
			strings.Join(needRule, ", "))
		for _, r := range needRule {
			fmt.Fprintf(stdout, "         %s/\n", r)
		}
	}
	if len(needUntrack) > 0 {
		// The rule is already there and does nothing, because git keeps
		// committing a path it already tracks. Saying "add a rule" here would
		// be telling them to do what they did.
		fmt.Fprintf(stdout, "  a rule EXISTS but the tree is already tracked, so the rule does nothing: %s\n"+
			"  fix: untrack it once, then the rule takes effect —\n",
			strings.Join(needUntrack, ", "))
		for _, r := range needUntrack {
			fmt.Fprintf(stdout, "         git rm -r --cached %s\n", r)
		}
	}
	fmt.Fprintf(stdout,
		"  keep it if you MEANT to commit generated code — this says what git will do, not what you should want\n")
}

// gitHasIgnoreRule reports whether a rule covers rel, INDEX ASIDE.
//
// `--no-index` is the whole point: without it a tracked path answers
// not-ignored no matter what the rules say, so the two states "nobody wrote a
// rule" and "a rule exists and is defeated by tracking" are indistinguishable —
// and they need opposite remedies.
//
// One query, on the plain path, for the reason written out over gitIgnores:
// with the directory present — and the caller only asks about roots that are —
// a `dir/` rule already matches the plain path, and the trailing-slash form
// lies about a file.
func gitHasIgnoreRule(root, rel string) bool {
	cmd := exec.Command("git", "check-ignore", "-q", "--no-index", "--", rel)
	cmd.Dir = root
	return cmd.Run() == nil
}

// gitCanAnswer reports whether `git check-ignore` is worth asking here: git on
// PATH and root inside a work tree. Both failures mean the same thing — nobody
// is going to commit anything — so neither is reported.
func gitCanAnswer(root string) bool {
	if _, err := exec.LookPath("git"); err != nil {
		return false
	}
	cmd := exec.Command("git", "rev-parse", "--is-inside-work-tree")
	cmd.Dir = root
	out, err := cmd.Output()
	return err == nil && strings.TrimSpace(string(out)) == "true"
}

// gitIgnores asks git whether rel is ignored.
//
// ⚠️ Exit code 1 means NOT ignored and is not an error; any other failure is
// read as "not ignored" too, which is the direction that warns rather than the
// one that goes quiet. A check that cannot tell must not conclude the safe-
// looking answer.
//
// ⚠️ ONE query, on the plain path. Both this and gitHasIgnoreRule used to retry
// with a trailing slash, for a rule written `dir/`. Measured, and it is worse
// than unnecessary:
//
//	rule `web/gen/`, directory present (empty or not) → plain path: IGNORED
//	rule `web/gen/`, nothing on disk                  → plain path: not ignored
//	rule `web/gen/`, a FILE at web/gen                → plain: not ignored,
//	                                                    with slash: IGNORED
//
// The caller only asks about roots that EXIST, so the first line is the only
// one that can be reached and the retry answers nothing new. The third line is
// why it had to go rather than stay as insurance: git reads the trailing slash
// as the caller ASSERTING a directory, so the retry reported a plain file as
// ignored by a directory rule that does not cover it — silence about a path
// git will commit, which is the one direction this file refuses.
func gitIgnores(root, rel string) bool {
	cmd := exec.Command("git", "check-ignore", "-q", "--", rel)
	cmd.Dir = root
	return cmd.Run() == nil
}
