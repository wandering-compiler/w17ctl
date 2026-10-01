// Package migdecisions reads and writes the decision files under
// w17/migrate-decisions/ — how a NEEDS_CONFIRM finding is decided in a pull
// request instead of on somebody's command line.
//
// # Why files, and why one per decision
//
// A schema change the planner will not classify on its own needs a person to
// choose a strategy. `--decide` on a command line makes that choice invisible:
// it happens in whoever's shell ran the release. A file in the repository makes
// it part of the change — written in the PR that caused it, reviewed there (a
// CODEOWNERS entry on the directory routes it to a database owner), merged with
// it, and in git history afterwards.
//
// One file per decision, named by time, commit and key, so two PRs deciding
// different things never touch the same file, and a reviewer sees exactly one
// question per file. The file offers the options as a list; deciding is
// DELETING all but one, not remembering a syntax.
//
// # Lifecycle
//
//   - `w17ctl migrate check --write` writes a file for each open finding;
//   - a reviewer deletes every option but one;
//   - `w17ctl migrate generate` / `push` apply them and, on success, delete
//     them.
//
// # A decision is bound to the schema it was made against
//
// Each file records the console's BASE — the stored schema the finding was
// planned against. A decision applies only while the base is the same. Every
// mint moves the base, so once a release has consumed a decision it can
// never decide a LATER change of the same column, even when the file is
// still in the repository (a CI job that mints on a runner and does not
// commit back leaves it there). Such a file is reported as consumed and is
// inert; `migrate check --write` deletes it.
//
// The client holds no knowledge of WHAT a decision means: the key, the
// offered options and the change summary all come from the console's
// finding, and the console parses and validates the decision itself.
package migdecisions

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	w17registrypb "github.com/wandering-compiler/sdk/go/pb/w17registry"
)

// Dir is the decisions directory, relative to the project root.
const Dir = "w17/migrate-decisions"

// File is one decision file as read from disk.
type File struct {
	// Path is the file's path on disk.
	Path string
	// Key is the finding it decides (`table.column:axis`), copied from the
	// console's finding when the file was written.
	Key string
	// Choice is the single strategy left in `choose`, or "custom" with
	// CustomPath set. Empty when Problem is set.
	Choice     string
	CustomPath string
	// Base is the stored schema the decision was made against
	// ("none" = no schema yet); see the package doc.
	Base string
	// Undecided: a well-formed file still waiting for a person to keep
	// exactly one option. Problem says how many are left.
	Undecided bool
	// Problem says why the file cannot be applied as it stands; empty when
	// it can.
	Problem string
}

// Flag renders the file as the decision the console receives.
func (f File) Flag() string {
	if f.Choice == "custom" {
		return f.Key + "=custom:" + f.CustomPath
	}
	return f.Key + "=" + f.Choice
}

// onDisk is the file's YAML shape. Only `finding` and `choose` are read back;
// the rest is for the person deciding.
type onDisk struct {
	Finding string      `yaml:"finding"`
	Base    string      `yaml:"base"`
	Choose  []yaml.Node `yaml:"choose"`
}

// NoBase is the base written for a project with no stored schema yet.
const NoBase = "none"

// BaseName renders a console base for a file: "" (no schema) is NoBase.
func BaseName(consoleBase string) string {
	if consoleBase == "" {
		return NoBase
	}
	return consoleBase
}

// Load reads every *.yaml in dir. A missing directory is no decisions. A file
// that cannot be applied is returned with Problem set, never dropped: a file
// nobody can use is still a file somebody has to deal with.
func Load(dir string) ([]File, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []File
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		out = append(out, readOne(filepath.Join(dir, e.Name())))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func readOne(path string) File {
	f := File{Path: path}
	body, err := os.ReadFile(path)
	if err != nil {
		f.Problem = "unreadable: " + err.Error()
		return f
	}
	var d onDisk
	if err := yaml.Unmarshal(body, &d); err != nil {
		f.Problem = "not valid YAML: " + err.Error()
		return f
	}
	f.Key = strings.TrimSpace(d.Finding)
	f.Base = strings.TrimSpace(d.Base)
	if f.Key == "" {
		f.Problem = "no `finding:` — the file does not say what it decides"
		return f
	}
	if f.Base == "" {
		f.Problem = "no `base:` — the file does not say which schema it was decided against; rewrite it with `w17ctl migrate check --rewrite`"
		return f
	}
	switch len(d.Choose) {
	case 0:
		f.Undecided = true
		f.Problem = "no option left under `choose:` — a database owner keeps exactly one"
		return f
	case 1:
	default:
		f.Undecided = true
		f.Problem = fmt.Sprintf("%d options left under `choose:` — a database owner deletes all but the one chosen", len(d.Choose))
		return f
	}
	n := d.Choose[0]
	switch n.Kind {
	case yaml.ScalarNode:
		f.Choice = strings.TrimSpace(n.Value)
	case yaml.MappingNode:
		var m map[string]string
		if err := n.Decode(&m); err == nil && len(m) == 1 && strings.TrimSpace(m["custom"]) != "" {
			f.Choice, f.CustomPath = "custom", strings.TrimSpace(m["custom"])
		}
	}
	if f.Choice == "" {
		f.Problem = "the option under `choose:` is neither a strategy nor `custom: <file.sql>`"
	}
	return f
}

// Duplicates groups the usable files by key and returns the keys decided by
// more than one file. Same choice = redundant (keep one, delete the rest);
// different choices = a conflict only a person can settle.
func Duplicates(files []File) (redundant [][]File, conflicting [][]File) {
	byKey := map[string][]File{}
	for _, f := range files {
		if f.Problem == "" {
			byKey[f.Key] = append(byKey[f.Key], f)
		}
	}
	keys := make([]string, 0, len(byKey))
	for k := range byKey {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		group := byKey[k]
		if len(group) < 2 {
			continue
		}
		same := true
		for _, f := range group[1:] {
			if f.Flag() != group[0].Flag() {
				same = false
			}
		}
		if same {
			redundant = append(redundant, group)
		} else {
			conflicting = append(conflicting, group)
		}
	}
	return redundant, conflicting
}

// Applicable returns the decisions to send: every usable file whose key is
// decided once, or several times the same way (sent once). Keys decided in
// conflicting ways are left out — sending either would pick a winner nobody
// chose.
func Applicable(files []File) []File {
	_, conflicting := Duplicates(files)
	skip := map[string]bool{}
	for _, g := range conflicting {
		skip[g[0].Key] = true
	}
	seen := map[string]bool{}
	var out []File
	for _, f := range files {
		if f.Problem != "" || skip[f.Key] || seen[f.Key] {
			continue
		}
		seen[f.Key] = true
		out = append(out, f)
	}
	return out
}

// Payload turns decisions into what PushSchema takes: the decide strings and
// the bodies of any custom SQL they name, read relative to the project root.
//
// The path must stay INSIDE the project, symlinks resolved. A decision file
// is repository content and CI reads it on every PR; `custom: ../../etc/x`
// (or a link pointing out) would otherwise ship a file from the runner to
// the console.
func Payload(root string, files []File) ([]string, map[string]string, error) {
	var flags []string
	var custom map[string]string
	for _, f := range files {
		flags = append(flags, f.Flag())
		if f.Choice != "custom" {
			continue
		}
		path, err := insideRoot(root, f.CustomPath)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: custom SQL %s: %w", f.Path, f.CustomPath, err)
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: custom SQL %s: %w", f.Path, f.CustomPath, err)
		}
		if custom == nil {
			custom = map[string]string{}
		}
		custom[f.CustomPath] = string(body)
	}
	return flags, custom, nil
}

// insideRoot resolves rel against root and refuses anything that lands
// outside it — an absolute path, `..`, or a symlink pointing out.
func insideRoot(root, rel string) (string, error) {
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("must be relative to the project root, not absolute")
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(filepath.Join(root, rel))
	if err != nil {
		return "", err
	}
	if r, err := filepath.Rel(realRoot, real); err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("resolves outside the project root (%s)", real)
	}
	return real, nil
}

// optionHelp is the one-line gloss written beside each option. Wording for
// the person deciding — the console decides what each strategy DOES.
var optionHelp = map[string]string{
	"safe":            "apply as planned; existing data is not at risk",
	"lossless_using":  "convert existing values in place — the migration fails if a value does not convert",
	"needs_confirm":   "apply the planned change as is — you confirm the existing data fits it",
	"drop_and_create": "⚠ DROPS the column's existing data and recreates it",
}

var unsafeName = regexp.MustCompile(`[^a-z0-9]+`)

// FileName is `<UTC time>-<commit>-<key>-<hash>.yaml`; commit may be empty.
// The slug is for people and is not injective (`a_b.c` and `a.b_c` slug
// alike), so a short hash of the exact key keeps two questions written in
// one run from landing on one file.
func FileName(now time.Time, commit, key string) string {
	slug := strings.Trim(unsafeName.ReplaceAllString(strings.ToLower(key), "-"), "-")
	sum := sha256.Sum256([]byte(key))
	parts := []string{now.UTC().Format("20060102T1504Z")}
	if commit != "" {
		parts = append(parts, commit)
	}
	parts = append(parts, slug, hex.EncodeToString(sum[:3]))
	return strings.Join(parts, "-") + ".yaml"
}

// Render writes the file a reviewer edits for one finding, against base (the
// console's, "" for no schema yet).
func Render(f *w17registrypb.Finding, base, commit, branch string) []byte {
	var b bytes.Buffer
	b.WriteString("# A database owner decides this — delete every option under `choose:`\n")
	b.WriteString("# but ONE, then approve. `w17ctl migrate generate` applies it at release\n")
	b.WriteString("# and deletes this file.\n")
	where := []string{}
	if commit != "" {
		where = append(where, "commit "+commit)
	}
	if branch != "" {
		where = append(where, "branch "+branch)
	}
	if len(where) > 0 {
		fmt.Fprintf(&b, "# Found by `w17ctl migrate check` on %s.\n", strings.Join(where, ", "))
	}
	fmt.Fprintf(&b, "finding: %s\n", f.GetDecideKey())
	fmt.Fprintf(&b, "base: %s   # the stored schema this is decided against — a release consumes it\n", BaseName(base))
	if p, c := f.GetPrevSummary(), f.GetCurrSummary(); p != "" || c != "" {
		fmt.Fprintf(&b, "change: %s\n", quote(strings.TrimSpace(p+" → "+c)))
	}
	if r := f.GetRationale(); r != "" {
		fmt.Fprintf(&b, "why: %s\n", quote(r))
	}
	if p := f.GetProposed(); p != "" {
		fmt.Fprintf(&b, "proposed: %s\n", p)
	}
	b.WriteString("choose:\n")
	for _, o := range f.GetOptions() {
		if h := optionHelp[o]; h != "" {
			fmt.Fprintf(&b, "  - %-16s # %s\n", o, h)
		} else {
			fmt.Fprintf(&b, "  - %s\n", o)
		}
	}
	b.WriteString("  # - custom: path/to/your.sql   # your own SQL instead (path from the project root)\n")
	return b.Bytes()
}

func quote(s string) string {
	out, err := yaml.Marshal(s)
	if err != nil {
		return fmt.Sprintf("%q", s)
	}
	return strings.TrimSpace(string(out))
}
