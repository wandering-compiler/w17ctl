package gofmtc

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// Mode selects which formatter shapes the generated Go.
type Mode string

const (
	// ModeEmbedded formats with the go/format compiled into this binary.
	// Zero dependencies — a CI runner needs w17ctl and nothing else — and
	// byte-identical for everyone on the same w17ctl.
	ModeEmbedded Mode = "embedded"

	// ModePinned formats with the Go version the project's go.mod declares,
	// via a local toolchain of exactly that version or a docker image of it.
	//
	// The selection is driven by a COMMITTED file, so it is the same for
	// everyone on the team — which is the whole reason it is allowed to
	// exist. A mode that used "whatever Go is installed" would hand two
	// colleagues different bytes for the same project.
	ModePinned Mode = "pinned"
)

// ParseMode validates the --gofmt flag.
func ParseMode(s string) (Mode, error) {
	switch Mode(s) {
	case "", ModeEmbedded:
		return ModeEmbedded, nil
	case ModePinned:
		return ModePinned, nil
	default:
		return "", fmt.Errorf("--gofmt=%s: expected %q or %q", s, ModeEmbedded, ModePinned)
	}
}

var goDirectiveRe = regexp.MustCompile(`(?m)^go\s+(\d+\.\d+(?:\.\d+)?)\s*$`)

// ProjectGoVersion reads the Go version the project declares.
//
// Order matters, and it is not the obvious one. A w17 project has NO root
// go.mod: it carries a `go.work` at the root and a go.mod per module
// (srcgo/, w17/stubs/, each bundle). So the workspace file is checked first —
// it is the root-level, committed, single declaration for the whole tree,
// which is exactly the property this needs.
//
// The root go.mod comes second for a plain Go project that grew a w17 tree
// inside it, and srcgo/go.mod last because that is where a w17 project keeps
// its own hand-written module.
func ProjectGoVersion(root string) (string, error) {
	for _, rel := range []string{"go.work", "go.mod", filepath.Join("srcgo", "go.mod")} {
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			continue
		}
		if m := goDirectiveRe.FindSubmatch(b); m != nil {
			return string(m[1]), nil
		}
	}
	return "", fmt.Errorf(
		"--gofmt=pinned: no `go` directive found in go.work, go.mod or srcgo/go.mod under %s\n"+
			"  why: the mode formats with the version the PROJECT declares, and nothing here declares one\n"+
			"  fix: declare one, or drop --gofmt=pinned to use the formatter built into w17ctl", root)
}

// minorOf reduces 1.26.3 to 1.26. gofmt's behaviour is a property of the
// minor release; a patch never changes formatting, and requiring an exact
// patch match would send every project to docker for nothing.
func minorOf(v string) string {
	parts := strings.SplitN(v, ".", 3)
	if len(parts) < 2 {
		return v
	}
	return parts[0] + "." + parts[1]
}

// pinnedFormatter returns a Source-shaped function bound to the project's
// declared Go version.
//
// It NEVER falls back to the embedded formatter. A silent fallback is the one
// thing this mode exists to prevent: the caller asked for the project's Go,
// and quietly giving them a different one produces a tree that differs by who
// ran it.
func pinnedFormatter(root string) (func([]byte) ([]byte, error), string, error) {
	want, err := ProjectGoVersion(root)
	if err != nil {
		return nil, "", err
	}
	wantMinor := minorOf(want)

	if gofmt, ok := localGofmt(wantMinor); ok {
		return func(src []byte) ([]byte, error) { return runFormatter(src, gofmt) }, "local go " + wantMinor, nil
	}
	if _, derr := exec.LookPath("docker"); derr == nil {
		img := "golang:" + wantMinor
		return func(src []byte) ([]byte, error) {
			return runFormatter(src, []string{"docker", "run", "--rm", "-i", img, "gofmt"})
		}, "docker " + img, nil
	}
	return nil, "", fmt.Errorf(
		"--gofmt=pinned: this project declares go %s, and neither a local go %s nor docker is available\n"+
			"  why: the mode exists to format with the project's OWN Go; falling back to another one would\n"+
			"       produce a tree that differs depending on who generated it\n"+
			"  fix: install go %s, install docker, or drop --gofmt=pinned to use the formatter built into w17ctl",
		want, wantMinor, wantMinor)
}

// localGofmt reports the local toolchain's gofmt when its version matches.
func localGofmt(wantMinor string) ([]string, bool) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		return nil, false
	}
	out, err := exec.Command(goBin, "version").Output()
	if err != nil {
		return nil, false
	}
	// `go version go1.26.3 linux/amd64`
	fields := strings.Fields(string(out))
	if len(fields) < 3 || !strings.HasPrefix(fields[2], "go") {
		return nil, false
	}
	if minorOf(strings.TrimPrefix(fields[2], "go")) != wantMinor {
		return nil, false
	}
	return []string{goBin, "fmt"}, true
}

// runFormatter pipes src through an external formatter.
//
// `gofmt` reads stdin and writes stdout, so one process handles one file. The
// cost only lands on files that actually need formatting — an unchanged file
// is skipped on its emit hash long before it reaches here.
func runFormatter(src []byte, argv []string) ([]byte, error) {
	cmd := exec.Command(argv[0], argv[1:]...) //nolint:gosec // argv is built here, not from input
	cmd.Stdin = bytes.NewReader(src)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%s: %w\n%s", strings.Join(argv, " "), err, errBuf.String())
	}
	return out.Bytes(), nil
}

// EmbeddedGoMinor is the Go minor this binary's formatter comes from, for
// telling the user which one they are getting.
func EmbeddedGoMinor() string { return minorOf(strings.TrimPrefix(runtime.Version(), "go")) }
