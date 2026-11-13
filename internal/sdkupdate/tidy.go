package sdkupdate

import (
	"bytes"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/mod/modfile"
)

// tidyModules runs `go mod tidy` in each updated module, so a requirement the
// proxy could not answer lands too: a module the project does not list yet
// but now needs, because a package it imports pulls it in (a new SDK
// dependency — golang.org/x/image arriving with image variants). Which modules
// that is depends on what the project imports, and only `go` reads that graph.
//
// Through the module's own Go minor: a local `go` when it is that version, or
// the golang:<minor> container — the toolchain choice gofmtc makes for the
// same reason, so the result does not depend on who ran the update. GOWORK is
// off, because a module's own go.mod is what a standalone build (a Dockerfile,
// a test suite) reads.
//
// A module that imports another module of the workspace without requiring it
// — the hand-written srcgo importing the stubs module's pb packages, which
// only go.work ties together — is lent the edge for the tidy alone: a require
// and a `replace` to the workspace module's directory, the pair codegen gives
// a generated bundle, dropped again afterwards (lendWorkspaceEdges). Without
// it the tidy went to the network for the stubs module's import path and
// reported a 404, every update, on a module that builds fine.
//
// A module whose tidy fails keeps exactly what the update wrote. It is
// reported, never fatal, because the update itself succeeded.
func tidyModules(stdout io.Writer, root string, mods []string) {
	var unrun []string
	workspace := workspaceModules(root)
	for _, d := range mods {
		dir := filepath.Join(root, d)
		minor := moduleGoMinor(dir)
		argv, how, ok := tidyCommand(root, dir, minor)
		if !ok {
			unrun = append(unrun, filepath.ToSlash(d))
			continue
		}
		before, err := snapshotModFiles(dir)
		if err != nil {
			fmt.Fprintf(stdout, "  tidy %s: skipped: %v\n", filepath.ToSlash(d), err)
			continue
		}
		lent, err := lendWorkspaceEdges(dir, workspace)
		if err != nil {
			before.restore()
			fmt.Fprintf(stdout, "  tidy %s: skipped: %v\n", filepath.ToSlash(d), err)
			continue
		}
		cmd := exec.Command(argv[0], argv[1:]...) //nolint:gosec // argv is built here, not from input
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GOWORK=off", "GOTOOLCHAIN=local")
		var errBuf bytes.Buffer
		cmd.Stderr = &errBuf
		if err := cmd.Run(); err != nil {
			before.restore()
			fmt.Fprintf(stdout, "  tidy %s: did not tidy on its own (%s); it keeps the update: %s\n",
				filepath.ToSlash(d), how, causeLine(errBuf.String(), err))
			continue
		}
		if err := dropLentEdges(dir, lent); err != nil {
			before.restore()
			fmt.Fprintf(stdout, "  tidy %s: tidied, but the workspace edges it was lent did not come off (%v); it keeps the update\n", filepath.ToSlash(d), err)
			continue
		}
		fmt.Fprintf(stdout, "  tidy %s: done (%s)\n", filepath.ToSlash(d), how)
	}
	if len(unrun) > 0 {
		fmt.Fprintf(stdout,
			"sdk update: no Go toolchain of the module's version and no docker here, so these were not tidied: %s\n"+
				"  why: a module the new SDK needs that the project does not list yet is added by `go mod tidy`, which reads the imports\n"+
				"  fix: run `go mod tidy` in each, with the Go version its go.mod declares\n",
			strings.Join(unrun, ", "))
	}
}

// lentVersion is the version a lent require names: a `replace` to a directory
// decides what it resolves to, and codegen writes the same one for a bundle's
// stubs module.
const lentVersion = "v0.0.0-00010101000000-000000000000"

// workspaceModules is root's go.work `use` list: each module's path → its
// directory. Empty without a go.work, or for one that does not parse.
func workspaceModules(root string) map[string]string {
	b, err := os.ReadFile(filepath.Join(root, "go.work"))
	if err != nil {
		return nil
	}
	wf, err := modfile.ParseWork("go.work", b, nil)
	if err != nil {
		return nil
	}
	mods := map[string]string{}
	for _, u := range wf.Use {
		dir := filepath.Join(root, filepath.FromSlash(u.Path))
		gm, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err != nil {
			continue
		}
		if p := modfile.ModulePath(gm); p != "" {
			mods[p] = dir
		}
	}
	return mods
}

// lendWorkspaceEdges gives dir's go.mod a require + `replace => <relative
// dir>` for each workspace module its packages import but it neither requires
// nor replaces, and returns the paths it lent. Only the imported ones: a lent
// module's own requirements join the graph the tidy resolves, and a workspace
// module dir never imports must not be able to fail its tidy.
func lendWorkspaceEdges(dir string, workspace map[string]string) ([]string, error) {
	if len(workspace) == 0 {
		return nil, nil
	}
	gomod := filepath.Join(dir, "go.mod")
	b, err := os.ReadFile(gomod)
	if err != nil {
		return nil, err
	}
	mf, err := modfile.Parse(gomod, b, nil)
	if err != nil {
		return nil, err
	}
	has := map[string]bool{}
	if mf.Module != nil {
		has[mf.Module.Mod.Path] = true
	}
	for _, r := range mf.Require {
		has[r.Mod.Path] = true
	}
	for _, r := range mf.Replace {
		has[r.Old.Path] = true
	}
	var paths []string
	for p := range importedModules(dir, workspace) {
		if !has[p] {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	for _, p := range paths {
		rel, err := filepath.Rel(dir, workspace[p])
		if err != nil {
			return nil, err
		}
		rel = filepath.ToSlash(rel)
		if !strings.HasPrefix(rel, ".") {
			rel = "./" + rel
		}
		if err := mf.AddRequire(p, lentVersion); err != nil {
			return nil, err
		}
		if err := mf.AddReplace(p, "", rel, ""); err != nil {
			return nil, err
		}
	}
	if len(paths) == 0 {
		return nil, nil
	}
	out, err := mf.Format()
	if err != nil {
		return nil, err
	}
	return paths, os.WriteFile(gomod, out, 0o644)
}

// importedModules is the set of workspace modules dir's packages import,
// tests included (the tidy reads them too). Nested modules, vendor/, testdata/
// and hidden directories are not dir's packages and are not read.
func importedModules(dir string, workspace map[string]string) map[string]bool {
	found := map[string]bool{}
	fset := token.NewFileSet()
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p == dir {
				return nil
			}
			n := d.Name()
			if n == "vendor" || n == "testdata" || strings.HasPrefix(n, ".") || strings.HasPrefix(n, "_") {
				return filepath.SkipDir
			}
			if _, err := os.Stat(filepath.Join(p, "go.mod")); err == nil {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") {
			return nil
		}
		f, err := parser.ParseFile(fset, p, nil, parser.ImportsOnly)
		if err != nil {
			return nil
		}
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				continue
			}
			// The longest match: srcgo's `example.com/p` is a prefix of the
			// stubs module's `example.com/p/w17/stubs`, and an import of the
			// latter's packages belongs to it alone.
			best := ""
			for mod := range workspace {
				if (path == mod || strings.HasPrefix(path, mod+"/")) && len(mod) > len(best) {
					best = mod
				}
			}
			if best != "" {
				found[best] = true
			}
		}
		return nil
	})
	return found
}

// dropLentEdges takes the lent requires and replaces off dir's go.mod again,
// keeping everything the tidy wrote besides them: the author's go.mod names
// no workspace module, and a standalone build should not find one there.
func dropLentEdges(dir string, lent []string) error {
	if len(lent) == 0 {
		return nil
	}
	gomod := filepath.Join(dir, "go.mod")
	b, err := os.ReadFile(gomod)
	if err != nil {
		return err
	}
	mf, err := modfile.Parse(gomod, b, nil)
	if err != nil {
		return err
	}
	for _, p := range lent {
		if err := mf.DropRequire(p); err != nil {
			return err
		}
		if err := mf.DropReplace(p, ""); err != nil {
			return err
		}
	}
	mf.Cleanup()
	out, err := mf.Format()
	if err != nil {
		return err
	}
	return os.WriteFile(gomod, out, 0o644)
}

var goDirective = regexp.MustCompile(`(?m)^go\s+(\d+\.\d+)`)

// moduleGoMinor is the Go minor a module's go.mod declares ("1.26"), or ""
// when it declares none.
func moduleGoMinor(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		return ""
	}
	if m := goDirective.FindSubmatch(b); m != nil {
		return string(m[1])
	}
	return ""
}

// tidyCommand is how to run `go mod tidy` in dir with Go minor: the local
// toolchain when it is that version, else the golang:<minor> container with the
// project root mounted at its own path (a `replace => ../x` must resolve) and a
// module cache kept in the user's cache directory. ok is false when neither is
// available.
func tidyCommand(root, dir, minor string) (argv []string, how string, ok bool) {
	if minor == "" {
		return nil, "", false
	}
	if goBin, err := exec.LookPath("go"); err == nil && localGoMinor(goBin) == minor {
		return []string{goBin, "mod", "tidy"}, "local go " + minor, true
	}
	if _, err := exec.LookPath("docker"); err != nil {
		return nil, "", false
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		cache = os.TempDir()
	}
	modCache := filepath.Join(cache, "w17ctl", "gomod")
	if err := os.MkdirAll(modCache, 0o755); err != nil {
		return nil, "", false
	}
	argv = []string{"docker", "run", "--rm"}
	if uid, gid := os.Getuid(), os.Getgid(); uid >= 0 && runtime.GOOS != "windows" {
		// The files tidy writes stay the user's, not root's.
		argv = append(argv, "-u", strconv.Itoa(uid)+":"+strconv.Itoa(gid))
	}
	argv = append(argv,
		"-e", "HOME=/tmp", "-e", "GOWORK=off", "-e", "GOTOOLCHAIN=local",
		"-e", "GOMODCACHE=/gomod", "-e", "GOCACHE=/tmp/gocache", "-e", "GOFLAGS=-buildvcs=false",
	)
	for _, k := range []string{"GOPROXY", "GOSUMDB", "GONOSUMDB", "GOPRIVATE", "GONOPROXY", "GODEBUG"} {
		if v, set := os.LookupEnv(k); set {
			argv = append(argv, "-e", k+"="+v)
		}
	}
	argv = append(argv,
		"-v", root+":"+root, "-v", modCache+":/gomod", "-w", dir,
		"golang:"+minor, "go", "mod", "tidy",
	)
	return argv, "docker golang:" + minor, true
}

// localGoMinor is the minor of the go binary at goBin ("1.26"), or "".
func localGoMinor(goBin string) string {
	out, err := exec.Command(goBin, "env", "GOVERSION").Output() //nolint:gosec // fixed arguments
	if err != nil {
		return ""
	}
	v := strings.TrimPrefix(strings.TrimSpace(string(out)), "go")
	parts := strings.SplitN(v, ".", 3)
	if len(parts) < 2 {
		return ""
	}
	return parts[0] + "." + parts[1]
}

// modFiles is a module's go.mod and go.sum as they were before a tidy.
type modFiles struct {
	dir      string
	mod, sum []byte
	hadSum   bool
}

func snapshotModFiles(dir string) (*modFiles, error) {
	m := &modFiles{dir: dir}
	var err error
	if m.mod, err = os.ReadFile(filepath.Join(dir, "go.mod")); err != nil {
		return nil, err
	}
	m.sum, err = os.ReadFile(filepath.Join(dir, "go.sum"))
	switch {
	case err == nil:
		m.hadSum = true
	case !os.IsNotExist(err):
		return nil, err
	}
	return m, nil
}

// restore puts the files back: a failed tidy can have written either.
func (m *modFiles) restore() {
	_ = os.WriteFile(filepath.Join(m.dir, "go.mod"), m.mod, 0o644)
	if m.hadSum {
		_ = os.WriteFile(filepath.Join(m.dir, "go.sum"), m.sum, 0o644)
	} else {
		_ = os.Remove(filepath.Join(m.dir, "go.sum"))
	}
}

// causeLine is the line of a failed command's stderr that names the cause —
// the last one: go reports a missing module as a chain ("x tested by", "x.test
// imports", then the package and why), and the first line of it says nothing
// a reader can act on. Download progress is skipped; the error itself stands
// in when stderr said nothing.
func causeLine(stderr string, err error) string {
	lines := strings.Split(stderr, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" && !strings.HasPrefix(l, "go: downloading") {
			return l
		}
	}
	return err.Error()
}
