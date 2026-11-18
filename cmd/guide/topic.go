package guide

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/wandering-compiler/w17ctl/internal/core"
)

// The navigation half of `w17ctl guide`: print ONE topic of the platform
// reference, or search the index. Both are offline — they read the w17/specs/
// a previous `guide` / `codegen` wrote — so an agent can look things up without
// a console and without loading a whole spec file into its context.
//
// The INDEX.md format is the console's guide generator's; it is parsed here
// rather than imported, because w17ctl imports no compiler code:
//
//	<id> | <file>:<first>-<last> | <size>KB | <read when>
//
// Header lines start with '#'; anything that does not parse is skipped, so a
// header that grows a line never breaks a client.

// TopicCmd is `w17ctl guide topic <id>`.
type TopicCmd struct {
	ID    string `arg:"" name:"id" help:"Topic id, e.g. dql.cte-materialization ('w17ctl guide topics <keyword>' finds ids)."`
	Specs string `name:"specs" placeholder:"DIR" help:"The specs directory to read (default: w17/specs/ of the project around the working directory)."`
}

// TopicsCmd is `w17ctl guide topics [query]`.
type TopicsCmd struct {
	Query string `arg:"" optional:"" name:"query" help:"Case-insensitive substring of the INDEX line (id, file or 'read when'); empty lists every topic."`
	Specs string `name:"specs" placeholder:"DIR" help:"The specs directory to read (default: w17/specs/ of the project around the working directory)."`
}

const indexFile = "INDEX.md"

// indexEntry is one parsed INDEX.md line.
type indexEntry struct {
	ID          string
	File        string
	First, Last int
	Line        string // the raw line, printed by `topics`
}

// parseIndex reads INDEX.md, keeping every line that parses as a topic.
func parseIndex(b []byte) []indexEntry {
	var out []indexEntry
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if e, ok := parseIndexLine(line); ok {
			out = append(out, e)
		}
	}
	return out
}

func parseIndexLine(line string) (indexEntry, bool) {
	t := strings.TrimSpace(line)
	if t == "" || strings.HasPrefix(t, "#") {
		return indexEntry{}, false
	}
	parts := strings.SplitN(t, "|", 4)
	if len(parts) < 2 {
		return indexEntry{}, false
	}
	id := strings.TrimSpace(parts[0])
	loc := strings.TrimSpace(parts[1])
	colon := strings.LastIndex(loc, ":")
	if id == "" || strings.ContainsAny(id, " \t") || colon <= 0 {
		return indexEntry{}, false
	}
	file, rng := loc[:colon], loc[colon+1:]
	a, b, ok := strings.Cut(rng, "-")
	if !ok {
		return indexEntry{}, false
	}
	first, err1 := strconv.Atoi(a)
	last, err2 := strconv.Atoi(b)
	if err1 != nil || err2 != nil || first < 1 || last < first {
		return indexEntry{}, false
	}
	return indexEntry{ID: id, File: file, First: first, Last: last, Line: t}, true
}

// specsDir is --specs, else <project root>/w17/specs.
func specsDir(flag string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	root, err := core.FindProjectRoot()
	if err != nil {
		return "", fmt.Errorf("guide: %w — run inside the project, or pass --specs <dir>", err)
	}
	return filepath.Join(root, "w17", "specs"), nil
}

func loadIndex(dir string) ([]indexEntry, error) {
	path := filepath.Join(dir, indexFile)
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("guide: %s not found — `w17ctl codegen` (or `w17ctl guide --force`) fetches the platform reference", path)
		}
		return nil, fmt.Errorf("guide: read %s: %w", path, err)
	}
	entries := parseIndex(b)
	if len(entries) == 0 {
		return nil, fmt.Errorf("guide: %s lists no topics — refresh it with `w17ctl codegen`", path)
	}
	return entries, nil
}

func (c *TopicCmd) Run() error {
	dir, err := specsDir(c.Specs)
	if err != nil {
		return err
	}
	entries, err := loadIndex(dir)
	if err != nil {
		return err
	}
	want := strings.ToLower(strings.TrimSpace(c.ID))
	for _, e := range entries {
		if strings.ToLower(e.ID) == want {
			return printTopic(dir, e)
		}
	}
	msg := fmt.Sprintf("guide: no topic %q in %s", c.ID, filepath.Join(dir, indexFile))
	if near := closestIDs(want, entries, 5); len(near) > 0 {
		msg += "\n  closest: " + strings.Join(near, ", ")
	}
	msg += "\n  search: w17ctl guide topics <keyword>"
	return fmt.Errorf("%s", msg)
}

func printTopic(dir string, e indexEntry) error {
	// The file comes from a generated index, but it is still a path read from
	// disk: keep it inside the specs directory.
	clean := filepath.Clean(e.File)
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("guide: topic %s names a file outside the specs directory: %s", e.ID, e.File)
	}
	path := filepath.Join(dir, clean)
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("guide: topic %s: %w — refresh the reference with `w17ctl codegen`", e.ID, err)
	}
	lines := strings.SplitAfter(string(b), "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	if e.First > len(lines) {
		return fmt.Errorf("guide: topic %s starts at line %d but %s has %d lines — INDEX.md is out of date; refresh it with `w17ctl codegen`",
			e.ID, e.First, path, len(lines))
	}
	last := e.Last
	if last > len(lines) {
		last = len(lines)
	}
	out := strings.Join(lines[e.First-1:last], "")
	if !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	_, err = core.Stdout.Write([]byte(out))
	return err
}

func (c *TopicsCmd) Run() error {
	dir, err := specsDir(c.Specs)
	if err != nil {
		return err
	}
	entries, err := loadIndex(dir)
	if err != nil {
		return err
	}
	q := strings.ToLower(strings.TrimSpace(c.Query))
	n := 0
	for _, e := range entries {
		if q == "" || strings.Contains(strings.ToLower(e.Line), q) {
			fmt.Fprintln(core.Stdout, e.Line)
			n++
		}
	}
	if n == 0 {
		return fmt.Errorf("guide: no topic matches %q — try a shorter keyword, or `w17ctl guide topics` for all", c.Query)
	}
	return nil
}

// closestIDs ranks ids by how near they are to want: a substring match either
// way first, then by edit distance; at most limit, none implausibly far.
func closestIDs(want string, entries []indexEntry, limit int) []string {
	type cand struct {
		id    string
		score int
	}
	var cands []cand
	for _, e := range entries {
		id := strings.ToLower(e.ID)
		score := levenshtein(want, id)
		if want != "" && (strings.Contains(id, want) || strings.Contains(want, id)) {
			score = 0
		} else if score > max(len(want), len(id))/2 {
			// Also accept a shared last segment (the slug), a common slip
			// when the file prefix is guessed wrong.
			if !sameSlug(want, id) {
				continue
			}
			score = len(id)
		}
		cands = append(cands, cand{e.ID, score})
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].score < cands[j].score })
	var out []string
	for i := 0; i < len(cands) && i < limit; i++ {
		out = append(out, cands[i].id)
	}
	return out
}

func sameSlug(a, b string) bool {
	slug := func(s string) string {
		if i := strings.Index(s, "."); i >= 0 {
			return s[i+1:]
		}
		return s
	}
	return slug(a) != "" && slug(a) == slug(b)
}

func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}
