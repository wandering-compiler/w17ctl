package stack

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/docker"
)

// buildStageRe finds the Go build stage a generated Dockerfile names `build`
// (`FROM w17-sdk AS build`): the stage --compile-only stops at.
var buildStageRe = regexp.MustCompile(`(?mi)^FROM\s+.*\s+AS\s+build\s*$`)

// compileBuildFn compiles one bundle: its Dockerfile up to the build stage,
// nothing exported (type=cacheonly) — the deploy's own build, minus the
// image. Combined output, for the failure report; tests stub it.
var compileBuildFn = func(root, builder, context, dockerfile string, buildArgs []string) ([]byte, error) {
	args := []string{"buildx", "build"}
	if builder != "" {
		args = append(args, "--builder", builder)
	}
	for _, a := range buildArgs {
		args = append(args, "--build-arg", a)
	}
	args = append(args, "--target", "build", "--output", "type=cacheonly", "--progress", "plain", "-f", dockerfile, context)
	cmd := exec.Command("docker", args...) //nolint:gosec // fixed verb; paths come from the project's compose config
	cmd.Dir = root
	return cmd.CombinedOutput()
}

// compileOnly is `stack build --compile-only`: does every generated bundle
// still compile? Each through its own Dockerfile, up to its Go build stage —
// on the SDK build image the deploy builds on, pinned by digest, with the
// cache mounts seeded from it — and no further: no image is produced, nothing
// is pushed, no store is touched. One build path: a check on the machine's own
// Go can pass where the image build fails (another Go patch, other flags,
// GOWORK), and the reverse.
//
// A bundle deploy/images.custom.txt builds your way is compiled the way the
// deploy builds it: that line's context, Dockerfile and build arguments, up to
// its `build` stage (a Dockerfile without one is said, not compiled). The
// generated Dockerfile of such a bundle is not what ships, and on its own it
// may not build at all: the console's builds against a checkout the deploy
// lays out (dev/console-app.Dockerfile).
//
// A service whose Dockerfile has no `build` stage (a store, a hand-written
// service) is not a generated Go bundle and is skipped. Every bundle is
// compiled, failing or not, and the failures are reported together, each with
// the compiler's own lines.
func (c *BuildCmd) compileOnly(root string) error {
	services, err := composeConfigFn(root, nil)
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			err = fmt.Errorf("%w: %s", err, strings.TrimSpace(string(ee.Stderr)))
		}
		return fmt.Errorf("stack build --compile-only: read the compose config: %w\n"+
			"  fix: a missing bundle .env is written by `w17ctl codegen` (drop --no-codegen)", err)
	}
	want := map[string]bool{}
	for _, s := range c.Services {
		want[s] = true
	}
	names := make([]string, 0, len(services))
	for name := range services {
		if len(want) == 0 || want[name] {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	var builder string
	if !c.NoReclaim {
		builder = docker.EnsureBuilder(root, projectForBuilder(root), c.cacheCap())
	}
	custom, err := readCustomBuilds(root)
	if err != nil {
		return fmt.Errorf("stack build --compile-only: %w", err)
	}
	var compiled, failed []string
	for _, name := range names {
		ctx, df := services[name].buildSpec(root)
		if df == "" {
			continue
		}
		body, err := os.ReadFile(df) // #nosec G304 -- the project's own compose config names it
		if err != nil || !buildStageRe.Match(body) {
			continue
		}
		var buildArgs []string
		if cb, ok := custom[name]; ok {
			ctx, df, buildArgs = cb.context, cb.dockerfile, cb.args
			body, err := os.ReadFile(df) // #nosec G304 -- the project's own deploy/images.custom.txt names it
			if err != nil {
				return fmt.Errorf("stack build --compile-only: %s: deploy/images.custom.txt builds it from %s: %w", name, rel(root, df), err)
			}
			if !buildStageRe.Match(body) {
				fmt.Fprintf(core.Stdout, "compile-only: %s is built your way (deploy/images.custom.txt: %s has no `build` stage) — not compiled here\n", name, rel(root, df))
				continue
			}
		}
		fmt.Fprintf(core.Stdout, "compile-only: %s …\n", name)
		out, err := compileBuildFn(root, builder, ctx, df, buildArgs)
		if err != nil {
			failed = append(failed, fmt.Sprintf("  - %s (%s):\n%s", name, rel(root, df), indent(compilerLines(string(out)), "      ")))
			continue
		}
		compiled = append(compiled, name)
	}
	if len(compiled)+len(failed) == 0 {
		return fmt.Errorf("stack build --compile-only: no generated bundle to compile — no service's Dockerfile has a `build` stage (run `w17ctl codegen` first?)")
	}
	if len(failed) > 0 {
		return fmt.Errorf("stack build --compile-only: %d of %d bundle(s) do not compile:\n%s\n"+
			"  why: the generated code no longer builds against the SDK and the compiler the lock pins — "+
			"the same build the deploy runs, stopped before the image",
			len(failed), len(failed)+len(compiled), strings.Join(failed, "\n"))
	}
	fmt.Fprintf(core.Stdout, "compile-only: %d bundle(s) compile (%s)\n", len(compiled), strings.Join(compiled, ", "))
	return nil
}

// buildkitPrefix is what buildkit's plain progress puts before a step's
// output line: the step number and the elapsed seconds (`#<step> 0.212 `),
// and only the seconds (`0.212 `) in the failed step's summary.
var buildkitPrefix = regexp.MustCompile(`^(#\d+ )?\d+\.\d+ `)

// compilerLines keeps what a person reads in a failed build: the Go
// toolchain's own lines (`file.go:12:2: …`, `go: …`) without buildkit's
// prefix, each once (buildkit repeats the failing step's output), and its
// ERROR line; the last lines of the log when there are none.
func compilerLines(out string) string {
	var keep []string
	seen := map[string]bool{}
	for _, l := range strings.Split(out, "\n") {
		t := strings.TrimSpace(buildkitPrefix.ReplaceAllString(strings.TrimSpace(l), ""))
		if seen[t] {
			continue
		}
		if strings.Contains(t, ".go:") || strings.HasPrefix(t, "go: ") || strings.HasPrefix(t, "ERROR: failed to build") {
			seen[t] = true
			keep = append(keep, t)
		}
	}
	if len(keep) == 0 {
		lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
		if len(lines) > 20 {
			lines = lines[len(lines)-20:]
		}
		keep = lines
	}
	if len(keep) > 40 {
		keep = append(keep[:40], fmt.Sprintf("… %d more", len(keep)-40))
	}
	return strings.Join(keep, "\n")
}

func indent(s, pre string) string {
	return pre + strings.ReplaceAll(s, "\n", "\n"+pre)
}

func rel(root, p string) string {
	if r, err := filepath.Rel(root, p); err == nil {
		return filepath.ToSlash(r)
	}
	return p
}

// customBuild is a line of deploy/images.custom.txt that rebuilds a bundle
// your way: `<image> <context> <Dockerfile> [KEY=VALUE …]`, paths relative to
// the project root, ${VAR} in a value taken from the environment (the deploy's
// workflow reads the same line the same way).
type customBuild struct {
	context, dockerfile string
	args                []string
}

func readCustomBuilds(root string) (map[string]customBuild, error) {
	path := filepath.Join(root, "deploy", "images.custom.txt")
	f, err := os.Open(path) // #nosec G304 -- the project's own file
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	out := map[string]customBuild{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 0 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		if len(fields) < 3 {
			return nil, fmt.Errorf("deploy/images.custom.txt: %q is not <image> <context> <Dockerfile> [KEY=VALUE …]", sc.Text())
		}
		cb := customBuild{context: filepath.Join(root, fields[1]), dockerfile: filepath.Join(root, fields[2])}
		for _, a := range fields[3:] {
			cb.args = append(cb.args, os.ExpandEnv(a))
		}
		out[fields[0]] = cb
	}
	return out, sc.Err()
}
