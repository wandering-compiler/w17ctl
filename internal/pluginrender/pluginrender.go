// Package pluginrender turns a plugin's AUTHOR tree into its PUBLISHED form.
//
// # Three surfaces, one shape
//
// A plugin exists in three shapes and only the first is written by a person:
//
//	author      plugins/<name>/            live .go, tests, src/gen/pb
//	published   <registry>/<name>/         .go.src, tests, no src/gen/pb
//	consumer    <proto_dir>/plugins/<name>/ live .go, NO tests
//
// This package owns the author → published step. `pluginfetch.stripSrcSuffix`
// owns published → consumer. They are inverses over everything but the tests,
// and nothing checked that until this package existed: the render lived as a
// shell loop in the Makefile and the strip lived in Go, so the two halves of
// one fact were written in two languages by two people at two times.
//
// ⚠️ A mismatch between them is not a broken build here. It is a CONSUMER's
// tree that does not compile — a `.go.src` nobody unpacked, or a `.go` that
// should have been inert and turns the plugin into a module inside somebody
// else's build. `pluginrender_test.go` renders and then strips and requires the
// round trip.
//
// # Why the Go files go inert
//
// A live `.go` (let alone a `go.mod`) under `<proto_dir>/plugins/<name>/src`
// would make the plugin a module inside the consumer's own build, which it is
// not: its sources are STAGED into a service bundle with their import paths
// rewritten. The `.src` suffix is how the published form says "data, not code".
//
// # Why the tests are published and then dropped
//
// They belong in the public registry: a plugin is the one part of this system
// somebody outside the team reads, and a plugin whose tests are invisible asks
// to be trusted on its word.
//
// They do not belong in a consumer's tree. Nothing there runs them — the
// sources are staged with rewritten import paths, and a staged test would
// reference symbols the staging never placed. Measured on this repo, tests are
// +121% on auth's published Go, +89% on payment, +76% on agent: ten thousand
// lines committed into somebody's repository for code they cannot run.
package pluginrender

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// SrcSuffix marks a Go file carried as data rather than as code.
//
// Named here rather than spelled in each place that appends or strips it: it is
// the whole contract between the render and the unpack, and a literal in two
// files is a contract in neither.
const SrcSuffix = ".src"

// skipUnderSrc are paths under a plugin's `src/` the published form omits.
//
// `src/gen/pb` is REGENERATED — by `w17ctl plugin gen-pb` for the author and by
// codegen inside a consumer's project — so publishing it would ship bytes that
// are replaced on first use, and `plugindigest` skips `src/gen` for the same
// reason. What ships is everything a person wrote.
var skipUnderSrc = []string{"gen/pb"}

// verbatimUnderSrc keep their name in the published form.
//
// `.gitignore` is not Go and is not staged; it is there so an author cloning
// the registry gets the same ignores.
var verbatimUnderSrc = map[string]bool{".gitignore": true}

// suffixedUnderSrc go inert under their own name plus SrcSuffix, like the Go
// files, because a live `go.mod` under a consumer's proto dir is the exact
// thing that would make the plugin a module in their build.
var suffixedUnderSrc = map[string]bool{"go.mod": true, "go.sum": true}

// Stats is what a render produced, so a caller can refuse a render that
// produced nothing rather than reporting success for an empty tree.
type Stats struct {
	Plugins int
	Files   int
	GoFiles int
	Tests   int
}

// Plugin renders one author tree at src into dest.
//
// dest is created and REPLACED: a render that merged into a previous one's
// leftovers would publish a file the author has since deleted.
func Plugin(src, dest string) (Stats, error) {
	var st Stats
	manifest := filepath.Join(src, "plugin.yaml")
	if _, err := os.Stat(manifest); err != nil {
		return st, fmt.Errorf("plugin render: %s carries no plugin.yaml — that file is what "+
			"makes a directory a plugin: %w", src, err)
	}
	if err := os.RemoveAll(dest); err != nil {
		return st, fmt.Errorf("plugin render: clearing %s: %w", dest, err)
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return st, fmt.Errorf("plugin render: %w", err)
	}

	if err := copyFile(manifest, filepath.Join(dest, "plugin.yaml")); err != nil {
		return st, err
	}
	st.Files++
	// A README is the only optional root file, and its absence is not a
	// finding: a plugin is documented by its manifest's own prose too.
	if err := copyFile(filepath.Join(src, "README.md"), filepath.Join(dest, "README.md")); err == nil {
		st.Files++
	} else if !os.IsNotExist(err) {
		return st, err
	}

	// proto/ ships VERBATIM. It is the contract, it is what codegen reads, and
	// nothing about it is code in the consumer's build.
	if fi, err := os.Stat(filepath.Join(src, "proto")); err == nil && fi.IsDir() {
		n, cerr := copyTree(filepath.Join(src, "proto"), filepath.Join(dest, "proto"), nil)
		if cerr != nil {
			return st, cerr
		}
		st.Files += n
	}

	srcDir := filepath.Join(src, "src")
	if fi, err := os.Stat(srcDir); err == nil && fi.IsDir() {
		if err := renderSrc(srcDir, filepath.Join(dest, "src"), &st); err != nil {
			return st, err
		}
	}
	st.Plugins = 1
	return st, nil
}

// renderSrc walks a plugin's authored Go and writes its inert form.
func renderSrc(srcDir, destDir string, st *Stats) error {
	return filepath.WalkDir(srcDir, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		rel, rerr := filepath.Rel(srcDir, p)
		if rerr != nil {
			return rerr
		}
		slash := filepath.ToSlash(rel)
		for _, skip := range skipUnderSrc {
			if slash == skip || strings.HasPrefix(slash, skip+"/") {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
		}
		if d.IsDir() {
			return nil
		}
		base := filepath.Base(p)
		switch {
		case verbatimUnderSrc[base]:
			if err := copyFile(p, filepath.Join(destDir, rel)); err != nil {
				return err
			}
			st.Files++
		case suffixedUnderSrc[base]:
			if err := copyFile(p, filepath.Join(destDir, rel+SrcSuffix)); err != nil {
				return err
			}
			st.Files++
		case strings.HasSuffix(base, ".go"):
			if err := copyFile(p, filepath.Join(destDir, rel+SrcSuffix)); err != nil {
				return err
			}
			st.Files++
			st.GoFiles++
			if strings.HasSuffix(base, "_test.go") {
				st.Tests++
			}
		}
		// Anything else under src/ is deliberately NOT published. A fixture, a
		// stray binary or an editor dropping is not part of what a consumer
		// compiles, and publishing it would put it in their repository.
		return nil
	})
}

func copyTree(src, dst string, skip func(rel string) bool) (int, error) {
	var n int
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		rel, rerr := filepath.Rel(src, p)
		if rerr != nil {
			return rerr
		}
		if skip != nil && skip(filepath.ToSlash(rel)) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if err := copyFile(p, filepath.Join(dst, rel)); err != nil {
			return err
		}
		n++
		return nil
	})
	return n, err
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	fi, err := in.Stat()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, fi.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// Catalogue renders every plugin under pluginsDir into dest — the whole public
// registry, as the publish step rsyncs it.
//
// ⚠️ RENDERING NOTHING IS NOT A SUCCESS, and that guard is why this function
// exists rather than a loop at each call site. The shell it replaces was ONE
// recipe line, so a `cp` that failed inside it set that command's status and
// the loop carried on — the recipe's result was the LAST command's, and with
// `plugins/` empty that was a successful `done`. Measured: over an empty tree it
// printed `cp: cannot stat 'plugins/*//plugin.yaml'` and exited 0, producing a
// catalogue holding only TESTING.md.
//
// That matters because the publish rsyncs this output into the public registry
// with `--delete`. The empty catalogue was caught downstream by a zero-guard
// over published TESTS — which fires because no test was carried, not because
// no PLUGIN was, and which sits behind a documented skip flag. With that flag
// set, an empty render would have wiped every plugin from the registry and
// reported success.
func Catalogue(pluginsDir, dest string, notes []string) (Stats, error) {
	var total Stats
	if err := os.RemoveAll(dest); err != nil {
		return total, fmt.Errorf("plugin render: clearing %s: %w", dest, err)
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return total, fmt.Errorf("plugin render: %w", err)
	}

	// The registry's root is otherwise bare, and the rsync runs with --delete:
	// anything not rendered here does not exist there. These are how a reader
	// learns why the Go files end in `.src` and how to run the tests beside them.
	for _, n := range notes {
		if err := copyFile(filepath.Join(pluginsDir, n), filepath.Join(dest, n)); err != nil {
			return total, fmt.Errorf("plugin render: %s is declared as a registry note and "+
				"is not there — the registry root would ship without it: %w", n, err)
		}
		total.Files++
	}

	entries, err := os.ReadDir(pluginsDir)
	if err != nil {
		return total, fmt.Errorf("plugin render: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		src := filepath.Join(pluginsDir, e.Name())
		// A directory with no manifest is not a plugin. `plugins/` also holds
		// prose and an unshipped list, and counting entries rather than
		// MANIFESTS is how a guard over this directory once passed with every
		// plugin moved out.
		if _, serr := os.Stat(filepath.Join(src, "plugin.yaml")); serr != nil {
			continue
		}
		st, rerr := Plugin(src, filepath.Join(dest, e.Name()))
		if rerr != nil {
			return total, rerr
		}
		total.Plugins += st.Plugins
		total.Files += st.Files
		total.GoFiles += st.GoFiles
		total.Tests += st.Tests
	}

	if total.Plugins == 0 {
		return total, fmt.Errorf(
			"plugin render: %s holds no plugin (a directory with a plugin.yaml) — "+
				"rendering nothing is not a success, and this output is rsynced into the "+
				"public registry with --delete", pluginsDir)
	}
	return total, nil
}
