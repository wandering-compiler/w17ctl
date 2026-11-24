// Package i18n is `w17ctl i18n`: the state of the project's translation
// catalogs (`<languages_dir>/<bundle>/<lang>.po`).
package i18n

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/wandering-compiler/w17ctl/internal/core"
	sdki18n "github.com/wandering-compiler/sdk/go/lib/i18n"
)

// Cmd is `w17ctl i18n`.
type Cmd struct {
	Status StatusCmd `cmd:"" help:"What is translated, per catalog: translated, untranslated, fuzzy (a changed source sentence whose old translation waits for review — not served) and obsolete (kept, never served). --strict fails on anything left to do."`
}

// StatusCmd is `w17ctl i18n status`.
type StatusCmd struct {
	Dir     string `name:"dir" placeholder:"PATH" help:"The catalogs' directory. Default: the lock's languages_dir, else w17/languages."`
	Strict  bool   `name:"strict" help:"Exit 1 when a catalog has an untranslated or fuzzy entry, or a problem — for CI."`
	Console string `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"Console endpoint, asked for the lock's languages_dir. Defaults to the logged-in console."`
}

func (c *StatusCmd) Run() error {
	root, err := core.FindProjectRoot()
	if err != nil {
		return err
	}
	dir := c.Dir
	if dir == "" {
		dir = "w17/languages"
		if view, err := core.DescribeLockFromRoot(c.Console, root); err == nil && view.GetLanguagesDir() != "" {
			dir = view.GetLanguagesDir()
		}
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(root, dir)
	}
	rows, err := collect(dir)
	if err != nil {
		return err
	}
	open := report(core.Stdout, rows)
	if c.Strict && open {
		return errors.New("i18n: catalogs have untranslated or fuzzy entries, or problems")
	}
	return nil
}

type row struct {
	file  string // relative to the catalogs' directory
	stats sdki18n.CatalogStats
}

func collect(dir string) ([]row, error) {
	var rows []row
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(p, ".po") {
			return nil
		}
		b, err := os.ReadFile(p) // #nosec G304 -- a catalog under the project's languages_dir
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		rows = append(rows, row{file: filepath.ToSlash(rel), stats: sdki18n.Stats(b)})
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("i18n: no catalogs at %s (run `w17ctl codegen`, or pass --dir)", dir)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].file < rows[j].file })
	return rows, err
}

// report prints one line per catalog and the problems under it; it returns
// whether anything is left to do.
func report(w io.Writer, rows []row) bool {
	if len(rows) == 0 {
		fmt.Fprintln(w, "no catalogs")
		return false
	}
	width := 0
	for _, r := range rows {
		width = max(width, len(r.file))
	}
	open := false
	for _, r := range rows {
		s := r.stats
		pct := 100
		if s.Total > 0 {
			pct = s.Translated * 100 / s.Total
		}
		fmt.Fprintf(w, "%-*s  %3d%%  %d/%d translated", width, r.file, pct, s.Translated, s.Total)
		if s.Untranslated > 0 {
			fmt.Fprintf(w, ", %d untranslated", s.Untranslated)
		}
		if s.Fuzzy > 0 {
			fmt.Fprintf(w, ", %d fuzzy (review)", s.Fuzzy)
		}
		if s.Obsolete > 0 {
			fmt.Fprintf(w, ", %d obsolete", s.Obsolete)
		}
		fmt.Fprintln(w)
		for _, p := range s.Problems {
			fmt.Fprintf(w, "  ! %v\n", p)
		}
		if s.Untranslated > 0 || s.Fuzzy > 0 || len(s.Problems) > 0 {
			open = true
		}
	}
	return open
}
