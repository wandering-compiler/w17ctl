package sdkupdate

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
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
// A module whose tidy fails keeps exactly what the update wrote. That is the
// normal answer for a module that only builds inside a workspace (it imports a
// module it does not require), whose requirements the workspace supplies; it
// is reported, never fatal, because the update itself succeeded.
func tidyModules(stdout io.Writer, root string, mods []string) {
	var unrun []string
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
