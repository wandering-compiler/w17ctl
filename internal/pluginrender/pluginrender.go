// Package pluginrender turns a plugin's AUTHOR tree into its PUBLISHED form.
//
// # Three surfaces, one shape
//
// A plugin exists in three shapes and only the first is written by a person:
//
//	author      plugins/<name>/            live .go, tests, src/gen (generated)
//	published   <registry>/<name>/         .go.src, tests, no src/gen
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
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// SrcSuffix marks a Go file carried as data rather than as code.
//
// Named here rather than spelled in each place that appends or strips it: it is
// the whole contract between the render and the unpack, and a literal in two
// files is a contract in neither.
const SrcSuffix = ".src"

// skipUnderSrc are paths under a plugin's `src/` the published form omits.
//
// `src/gen` is GENERATED, all of it — the pb, the contract and the feature
// constants — by `w17ctl plugin gen` for the author and by staging inside a
// consumer's project, so publishing it would ship bytes that are replaced on
// first use; `plugindigest` leaves `src/gen` out for the same reason. What
// ships is everything a person wrote.
var skipUnderSrc = []string{"gen"}

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

	// sandboxes/ ships too: it is the source a project builds a plugin's
	// local-build sandbox image from (`sandboxes[].build` in the manifest), and
	// that image is published by no registry — without the source a consumer
	// can only try to pull it, and the pull is refused.
	sandboxDir := filepath.Join(src, "sandboxes")
	if fi, err := os.Stat(sandboxDir); err == nil && fi.IsDir() {
		if err := renderSandboxes(sandboxDir, filepath.Join(dest, "sandboxes"), &st); err != nil {
			return st, err
		}
	}
	if err := checkSandboxBuilds(dest); err != nil {
		return st, err
	}
	st.Plugins = 1
	return st, nil
}

// renderSandboxes copies a plugin's sandbox source into the published form.
//
// Unlike src/, EVERYTHING ships — a Dockerfile, a config file, a fixture the
// image bakes in are all build input, and a build is the only thing this tree
// is for. The Go goes inert all the same (`.src`, tests carried), by the rule
// every published Go file follows: a sandbox's `go.mod` live in a consumer's
// proto dir before the unpack strips the suffix is the module-inside-their-
// build the suffix exists to prevent. Nothing compiles it in place there
// either: it is a docker build context, and its own go.mod keeps it a module of
// its own once unpacked.
func renderSandboxes(srcDir, destDir string, st *Stats) error {
	return filepath.WalkDir(srcDir, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(srcDir, p)
		if rerr != nil {
			return rerr
		}
		base := filepath.Base(p)
		if strings.HasSuffix(base, SrcSuffix) {
			return fmt.Errorf("plugin render: sandboxes/%s is already in the published form — render takes an "+
				"author tree; a published tree is installed as it is", filepath.ToSlash(rel))
		}
		out := filepath.Join(destDir, rel)
		switch {
		case strings.HasSuffix(base, ".go"):
			out += SrcSuffix
			st.GoFiles++
			if strings.HasSuffix(base, "_test.go") {
				st.Tests++
			}
		case suffixedUnderSrc[base]:
			out += SrcSuffix
		}
		if err := copyFile(p, out); err != nil {
			return err
		}
		st.Files++
		return nil
	})
}

// sandboxBuildManifest is the part of plugin.yaml checkSandboxBuilds reads.
// The console owns the manifest's schema and validates it strictly; this
// reads only the build paths, tolerantly, so a field it does not know is the
// console's to judge rather than a render failure here.
type sandboxBuildManifest struct {
	Sandboxes []struct {
		Name  string `yaml:"name"`
		Build *struct {
			Context    string `yaml:"context"`
			Dockerfile string `yaml:"dockerfile"`
		} `yaml:"build"`
	} `yaml:"sandboxes"`
}

// checkSandboxBuilds refuses a published form that declares a sandbox build
// whose Dockerfile it does not carry.
//
// Only sandboxes/ and src/ reach the published form, so a build context
// anywhere else renders to nothing — and the failure would otherwise surface
// as a `docker compose build` error in somebody else's project, against a
// release that can no longer be changed.
func checkSandboxBuilds(dest string) error {
	body, err := os.ReadFile(filepath.Join(dest, "plugin.yaml"))
	if err != nil {
		return fmt.Errorf("plugin render: %w", err)
	}
	var m sandboxBuildManifest
	if yaml.Unmarshal(body, &m) != nil {
		// Not this check's finding: the console parses the manifest and says
		// what is wrong with it.
		return nil
	}
	for _, sb := range m.Sandboxes {
		if sb.Build == nil {
			continue
		}
		file := sb.Build.Dockerfile
		if file == "" {
			file = "Dockerfile"
		}
		rel := path.Join(sb.Build.Context, file)
		if !filepath.IsLocal(filepath.FromSlash(rel)) {
			// A path that leaves the plugin is the console's refusal to make.
			continue
		}
		if fi, serr := os.Stat(filepath.Join(dest, filepath.FromSlash(rel))); serr != nil || fi.IsDir() {
			return fmt.Errorf("plugin render: sandbox %s builds from %s, which the published form does not carry — "+
				"a sandbox's build source lives under sandboxes/ (or the plugin's src/), the only places a release ships",
				sb.Name, rel)
		}
	}
	return nil
}

// privateDocRef matches a path into the platform's private docs. The
// platform's plugins are published to a PUBLIC registry and authored in a
// private repository, so a comment pointing at its internal docs points its
// reader at nothing — and names internal documents in public. Say the reason
// in the comment instead.
//
// Checked by Catalogue — the platform's own publish — and NOT by Plugin: an
// author outside this repository renders, installs (`plugin update --from`)
// and develops (`plugin dev`) through Plugin, and their own repository may
// well have docs of its own to cite.
var privateDocRef = regexp.MustCompile(`docs/(todos|decisions|specs|runbooks|audit|archive)/`)

// refusePrivateDocRefs refuses a rendered catalogue that cites the private docs,
// naming every place so the author can fix them in one pass.
func refusePrivateDocRefs(root string) error {
	var hits []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		body, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		rel, _ := filepath.Rel(root, path)
		for i, line := range strings.Split(string(body), "\n") {
			if privateDocRef.MatchString(line) {
				hits = append(hits, fmt.Sprintf("%s:%d", filepath.ToSlash(rel), i+1))
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("plugin render: %w", err)
	}
	if len(hits) > 0 {
		return fmt.Errorf("plugin render: the published tree cites the platform's private docs, which no reader of the "+
			"registry can open — state the reason in place instead (%d): %s", len(hits), strings.Join(hits, ", "))
	}
	return nil
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
		// An already-RENDERED tree is refused, not rendered again: its Go is
		// `.go.src`, which none of the cases below takes, so a second render
		// dropped every line of it and reported success. `plugin dev` handed a
		// release's tree did exactly that — a throwaway project with the
		// plugin's code silently missing, which "generates" proves nothing about.
		if strings.HasSuffix(base, ".go"+SrcSuffix) || base == "go.mod"+SrcSuffix || base == "go.sum"+SrcSuffix {
			return fmt.Errorf("plugin render: %s is already in the published form (%s) — render takes an "+
				"author tree; a published tree is installed as it is", srcDir, filepath.ToSlash(rel))
		}
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

	// The whole tree the registry receives: every plugin and the root notes.
	if err := refusePrivateDocRefs(dest); err != nil {
		return total, err
	}
	if total.Plugins == 0 {
		return total, fmt.Errorf(
			"plugin render: %s holds no plugin (a directory with a plugin.yaml) — "+
				"rendering nothing is not a success, and this output is rsynced into the "+
				"public registry with --delete", pluginsDir)
	}
	return total, nil
}
