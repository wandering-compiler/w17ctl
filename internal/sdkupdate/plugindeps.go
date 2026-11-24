package sdkupdate

import (
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/mod/modfile"

	"github.com/wandering-compiler/w17ctl/internal/codegen"
)

// pluginDependents returns the project's own modules (repo-relative) that
// reach a vendored plugin tree through a local `replace` — a worker module
// building against the cluster plugin's library, say.
//
// Such a module need not require the SDK itself: it reaches it only through
// the plugin tree, so the SDK walk (findSdkModules) never lists it. But when
// the plugin tree moves (plugin update) or the SDK does (sdk update), the
// tree can import SDK packages the module's go.sum has no entry for, and its
// standalone build — a Dockerfile, CI with GOWORK=off — fails with
// "missing go.sum entry". The tidy that fixes it is the update's to run.
//
// The plugin trees themselves are skipped, as in the SDK walk: they carry
// their own pin and the lock holds a digest over them.
func pluginDependents(root string, plugins map[string]bool) []string {
	if len(plugins) == 0 {
		return nil
	}
	var out []string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// Unreadable: the SDK walk already said so; nothing here to act on.
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel, rErr := filepath.Rel(root, p)
		if rErr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if p != root && (skipDirs[d.Name()] || plugins[rel]) {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() != "go.mod" {
			return nil
		}
		data, rdErr := os.ReadFile(p)
		if rdErr != nil {
			return nil
		}
		f, pErr := modfile.Parse(p, data, nil)
		if pErr != nil {
			return nil
		}
		dir := filepath.Dir(p)
		for _, r := range f.Replace {
			if r.New.Version != "" || !modfile.IsDirectoryPath(r.New.Path) {
				continue
			}
			target, tErr := filepath.Rel(root, filepath.Join(dir, filepath.FromSlash(r.New.Path)))
			if tErr != nil {
				continue
			}
			if inPluginTree(filepath.ToSlash(target), plugins) {
				modDir, _ := filepath.Rel(root, dir)
				out = append(out, modDir)
				return nil
			}
		}
		return nil
	})
	sort.Strings(out)
	return out
}

// inPluginTree reports whether the repo-relative path rel is a plugin tree or
// lies under one.
func inPluginTree(rel string, plugins map[string]bool) bool {
	for tree := range plugins {
		if rel == tree || strings.HasPrefix(rel, tree+"/") {
			return true
		}
	}
	return false
}

// without returns mods minus every entry of already.
func without(mods, already []string) []string {
	seen := make(map[string]bool, len(already))
	for _, m := range already {
		seen[filepath.ToSlash(m)] = true
	}
	var out []string
	for _, m := range mods {
		if !seen[filepath.ToSlash(m)] {
			out = append(out, m)
		}
	}
	return out
}

// TidyPluginDependents tidies the project's own modules that build against a
// vendored plugin tree (pluginDependents) — what `plugin update` runs once the
// new trees are in place, so a module importing a plugin's library keeps
// building on its own. Reported, never fatal, like the SDK update's tidy.
func TidyPluginDependents(stdout io.Writer, root string) {
	root, err := filepath.Abs(root)
	if err != nil {
		return
	}
	if deps := pluginDependents(root, pluginTrees(root)); len(deps) > 0 {
		retakeAndTidy(stdout, root, deps)
	}
}

// retakeAndTidy tidies mods and keeps codegen's claim on the generated ones
// among them (codegen.Retake): the tidy is w17ctl's change, so the next
// codegen rewrites those go.mod files instead of refusing them as edited.
func retakeAndTidy(stdout io.Writer, root string, mods []string) {
	commit := codegen.Retake(root, modFilePaths(mods))
	defer commit()
	tidyModules(stdout, root, mods)
}

// modFilePaths is each module directory's go.mod and go.sum, relative to the
// project root as the module directories are.
func modFilePaths(mods []string) []string {
	out := make([]string, 0, 2*len(mods))
	for _, d := range mods {
		out = append(out, filepath.Join(d, "go.mod"), filepath.Join(d, "go.sum"))
	}
	return out
}
