// Package deploy implements `w17ctl deploy` — one person's way from the
// laptop to an environment, swarm or cloud. It
// runs what the deploy workflow runs, on the same generated scripts, with the
// operator's own credentials; what it skips is the review, the release branch
// and the record a CI run leaves — which is why it says so every time.
package deploy

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"golang.org/x/term"

	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/remote"
	codegenpb "github.com/wandering-compiler/sdk/go/pb/w17compiler"
)

// Cmd is `w17ctl deploy`.
type Cmd struct {
	Env         string   `name:"env" required:"" help:"The environment to deploy (deploy/<env>/)."`
	Expect      string   `name:"expect" placeholder:"FINGERPRINT" help:"swarm: deploy only if the server still holds the state w17ctl infra plan printed (its fingerprint) — refuses when someone deployed in between."`
	StateBucket string   `name:"state-bucket" env:"TF_STATE_BUCKET" help:"aws-ecs / gcp-cloudrun: the Terraform state bucket (deploy/<env>/terraform/bootstrap created it)."`
	WithTests   bool     `name:"with-tests" help:"Run the e2e suite (w17ctl test) first; a red suite stops the deploy."`
	AllowDirty  bool     `name:"allow-dirty" help:"Deploy a tree with uncommitted changes (the images are then tagged as dirty)."`
	BuildFlags  []string `name:"build-flag" placeholder:"FLAG" help:"Passed to every docker build, e.g. --build-flag=--build-arg=X=1. Repeatable."`
	LockPath    string   `name:"lock" placeholder:"PATH" default:"w17/lock.yaml" help:"Path to the lock file."`
	Console     string   `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console. Optional — falls back to the binary's compile-time default."`
}

const warning = `
⚠  w17ctl deploy is a SHORTCUT for one person's MVP or proof of concept, not the recommended
   way to deploy. The migrations it pins and the images it ships come from this laptop, not from
   a reviewed commit on a release branch, and no CI run records what went out. For anything
   other people rely on: ` + "`w17ctl ci init`" + ` and the deploy workflow.
`

// run is the seam the tests replace: every local program (git, docker,
// rsync, this binary) goes through it, stdio attached.
var run = func(stdin io.Reader, stdout io.Writer, dir, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir, cmd.Stdin, cmd.Stdout, cmd.Stderr = dir, stdin, stdout, os.Stderr
	return cmd.Run()
}

// self is the path of this binary, for the steps that are other w17ctl commands.
var self = os.Executable

// fetchTokenVar names the token the values are fetched with: a machine
// account's, given Deploy access to the environment on the project's page.
// Not W17_TOKEN — the rest of a deploy (the lock, the migrations, codegen)
// runs as you, and the deploy-fetch role may do nothing but fetch.
const fetchTokenVar = "W17_FETCH_TOKEN" //nolint:gosec // G101: the variable's name, not a credential.

// fetchEnv is the seam the tests replace: the deploy bot's fetch, run as
// `w17ctl secrets fetch` in a child whose W17_TOKEN is $W17_FETCH_TOKEN (and
// with no device key: that one belongs to your login). It writes one file per
// service into dir, checked against the rendered stack — or nothing.
//
// The console is resolved here and handed to the child both as --console and
// as W17_CONSOLE_ADDR: an env token is presented only to the console it is
// bound to, and a child left to resolve it on its own could bind it to
// another one and send the stored login instead.
var fetchEnv = func(root, selfPath, lockPath, console, env, ref, dir string, asJSON bool) error {
	addr, err := core.ResolveConsoleAddr(console)
	if err != nil {
		return err
	}
	args := []string{"secrets", "fetch", "--env", env, "--ref", ref, "--out", dir, "--lock", lockPath, "--console", addr}
	if asJSON {
		args = append(args, "--json")
	}
	// No deploy bot: the values are read as you, under a read code from the
	// project's page (secrets fetch --read) — a deploy from your own machine
	// needs no bot. Your login and key stay; the code is asked on the terminal
	// unless $W17_SECRETS_CODE carries it.
	if os.Getenv(fetchTokenVar) == "" {
		fmt.Fprintf(core.Stdout, "no %s: reading %s's values as you, with a read code\n", fetchTokenVar, env)
		cmd := exec.Command(selfPath, append(args, "--read")...) // #nosec G204 -- this binary
		cmd.Dir, cmd.Stdin, cmd.Stdout, cmd.Stderr = root, os.Stdin, core.Stdout, os.Stderr
		for _, kv := range os.Environ() {
			if !strings.HasPrefix(kv, core.EnvConsoleAddrVar+"=") {
				cmd.Env = append(cmd.Env, kv)
			}
		}
		cmd.Env = append(cmd.Env, core.EnvConsoleAddrVar+"="+addr)
		return cmd.Run()
	}
	cmd := exec.Command(selfPath, args...) // #nosec G204 -- this binary
	cmd.Dir, cmd.Stdout, cmd.Stderr = root, core.Stdout, os.Stderr
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "W17_TOKEN=") && !strings.HasPrefix(kv, core.EnvDPoPKeyVar+"=") && !strings.HasPrefix(kv, core.EnvConsoleAddrVar+"=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, "W17_TOKEN="+os.Getenv(fetchTokenVar), core.EnvConsoleAddrVar+"="+addr)
	return cmd.Run()
}

func (c *Cmd) Run() error {
	fmt.Fprint(core.Stdout, warning)
	abs, err := filepath.Abs(c.LockPath)
	if err != nil {
		return err
	}
	root := filepath.Dir(filepath.Dir(abs))
	view, err := core.DescribeInfraAt("deploy", c.Console, c.LockPath, false, "")
	if err != nil {
		return err
	}
	var env *codegenpb.LockInfraEnvironment
	for _, e := range view.GetInfra().GetEnvironments() {
		if e.GetName() == c.Env {
			env = e
		}
	}
	if env == nil {
		return fmt.Errorf("deploy: no environment %q (`w17ctl infra show`)", c.Env)
	}
	kind := env.GetTargetKind()
	var target remote.Target
	ssh := env.GetHost().GetSsh()
	switch kind {
	case "swarm":
		if ssh == "" {
			return fmt.Errorf("deploy: environment %q names no server — `w17ctl infra set targets.%s.ssh=root@<server>`, then `w17ctl infra setup --env %s`", c.Env, env.GetTarget(), c.Env)
		}
		if target, err = remote.Parse(ssh); err != nil {
			return fmt.Errorf("deploy: %w", err)
		}
	case "aws-ecs", "gcp-cloudrun":
		if c.Expect != "" {
			return fmt.Errorf("deploy: --expect is a swarm fingerprint; on %s the Terraform state is the lock (`w17ctl infra plan` shows its plan)", kind)
		}
		if c.StateBucket == "" {
			return fmt.Errorf("deploy: the Terraform state bucket — --state-bucket or TF_STATE_BUCKET (deploy/%s/terraform/bootstrap created it)", c.Env)
		}
	default:
		return fmt.Errorf("deploy: environment %s has no known target kind (%q)", c.Env, kind)
	}
	// The values come with the deploy bot's token, or — a person deploying
	// from their own machine — under a read code they type or pass in
	// $W17_SECRETS_CODE. Checked before anything runs: a run with neither
	// (CI without its secret) would mint migrations and then stop.
	if os.Getenv(fetchTokenVar) == "" && os.Getenv("W17_SECRETS_CODE") == "" && !term.IsTerminal(int(os.Stdin.Fd())) {
		return fmt.Errorf("deploy: the values come from the console — set %s to the token of a machine account given Deploy access to %s on the project's page, "+
			"or deploy from a terminal (or with W17_SECRETS_CODE) to read them as yourself with a read code from that page", fetchTokenVar, c.Env)
	}

	dirty, sha, err := gitState(root)
	if err != nil {
		return err
	}
	if dirty && !c.AllowDirty {
		return errors.New("deploy: the tree has uncommitted changes — commit them (the images and the pinned migrations should match a commit), or pass --allow-dirty")
	}

	selfPath, err := self()
	if err != nil {
		return err
	}
	// The server's machine is asked before anything runs: one no image can be built
	// for is refused before the suite, the pinned migrations and the codegen, not
	// after them.
	var platform string
	if kind == "swarm" {
		if platform, err = serverPlatform(target); err != nil {
			return fmt.Errorf("deploy: %w", err)
		}
	}
	w17 := func(args ...string) error {
		if c.Console != "" {
			args = append(args, "--console", c.Console)
		}
		return run(os.Stdin, core.Stdout, root, selfPath, args...)
	}
	if c.WithTests {
		step("the e2e suite")
		if err := w17("test"); err != nil {
			return fmt.Errorf("deploy: the e2e suite failed — nothing was deployed: %w", err)
		}
	}
	step("mint and pin the migrations")
	if err := w17("migrate", "generate"); err != nil {
		return fmt.Errorf("deploy: migrate generate: %w", err)
	}
	step("render the code and deploy/" + c.Env + " for the pinned lock")
	// --force as the workflows run it: the images are built from what the console
	// generates for this lock, whatever an older run left on disk.
	if err := w17("codegen", "--force"); err != nil {
		return fmt.Errorf("deploy: codegen: %w", err)
	}

	// The values are fetched once the tree is known to be deployable and is
	// rendered for this lock: the check against each service's .env.example
	// must see the services and keys this deploy runs, not the ones an older
	// render left on disk; and a fetch is a hand-out the console records, so a
	// deploy that was never going to run should not make one.
	fmt.Fprintf(core.Stdout, "env values: from the console\n")
	ref := sha
	if dirty {
		ref += "-dirty"
	}
	valuesDir, err := os.MkdirTemp("", "w17-values-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(valuesDir) }()
	if err := fetchEnv(root, selfPath, abs, c.Console, c.Env, ref, valuesDir, kind != "swarm"); err != nil {
		return fmt.Errorf("deploy: fetching %s's values: %w", c.Env, err)
	}

	envDir := filepath.Join(root, "deploy", c.Env)
	if kind != "swarm" {
		return c.deployCloud(root, envDir, kind, env, view, valuesDir, sha, dirty)
	}
	if c.Expect != "" {
		fp, err := target.Fingerprint("/opt/stack/" + c.Env)
		if err != nil {
			return fmt.Errorf("deploy: %w", err)
		}
		if fp != c.Expect {
			return fmt.Errorf("deploy: the server holds %s, not the %s the plan was made against — someone deployed in between; run `w17ctl infra plan --env %s` again", fp, c.Expect, c.Env)
		}
	}
	images, err := readImages(filepath.Join(envDir, "install", "images.txt"))
	if err != nil {
		return fmt.Errorf("deploy: %w", err)
	}
	images, custom, err := withCustomImages(images, filepath.Join(root, "deploy", "images.custom.txt"))
	if err != nil {
		return fmt.Errorf("deploy: %w", err)
	}
	tag := "sha-" + sha
	if dirty {
		tag += "-dirty"
	}
	pgTag, err := dirHash(filepath.Join(envDir, "services", "postgres"))
	if err != nil {
		return fmt.Errorf("deploy: %w", err)
	}
	pgTag = "pg-" + pgTag
	prefix := "w17local/" + imageName(view.GetProject())

	step("build the images for " + platform + ", the server's platform")
	var refs []string
	for _, im := range images {
		ref := prefix + "/" + im.name + ":" + tag
		if im.name == "stack-postgres" {
			ref = prefix + "/stack-postgres:" + pgTag
		}
		args := []string{"build", "-q"}
		if !ownPlatform(c.BuildFlags) {
			args = append(args, "--platform", platform)
		}
		args = append(args, c.BuildFlags...)
		for _, a := range im.buildArgs {
			args = append(args, "--build-arg", a)
		}
		if im.target != "" {
			args = append(args, "--target", im.target)
		}
		args = append(args, "-t", ref, "-f", im.dockerfile, im.context)
		if err := run(nil, core.Stdout, root, "docker", args...); err != nil {
			return fmt.Errorf("deploy: build %s: %w", im.name, err)
		}
		refs = append(refs, ref)
	}

	step("ship the images to " + ssh + " (no registry: docker save | docker load)")
	pr, pw := io.Pipe()
	go func() {
		pw.CloseWithError(run(nil, pw, root, "docker", append([]string{"save"}, refs...)...))
	}()
	if err := target.Run(pr, io.Discard, target.Sudo("docker load -q")); err != nil {
		return fmt.Errorf("deploy: loading the images on %s: %w", ssh, err)
	}

	stack := "/opt/stack/" + c.Env
	step("the stack's files")
	rsync := append([]string{"-az", "--delete-after", "--chown=root:root", "--chmod=go-w"}, target.RsyncArgs()...)
	srcs := append([]string(nil), remote.StackFiles...)
	if st, err := os.Stat(filepath.Join(envDir, "hooks")); err == nil && st.IsDir() {
		srcs = append(srcs, "hooks")
	} else if err := target.Run(nil, io.Discard, target.Sudo("rm -rf "+stack+"/hooks")); err != nil {
		// A hook deleted here must not keep running there.
		return fmt.Errorf("deploy: removing %s/hooks: %w", stack, err)
	}
	if err := run(nil, core.Stdout, envDir, "rsync", append(append(rsync, "--exclude", ".env*"), append(srcs, target.Dest()+":"+stack+"/")...)...); err != nil {
		return fmt.Errorf("deploy: rsync: %w", err)
	}
	files := []string{"w17/lock.yaml"}
	if st, err := os.Stat(filepath.Join(root, "w17", "fixtures")); err == nil && st.IsDir() {
		files = append(files, "w17/fixtures")
	}
	if err := run(nil, core.Stdout, root, "rsync", append(append(rsync, "-R"), append(files, target.Dest()+":"+stack+"/")...)...); err != nil {
		return fmt.Errorf("deploy: rsync: %w", err)
	}

	step("the env files, from the console into ssh")
	envFiles, _ := filepath.Glob(filepath.Join(valuesDir, "*.env"))
	sort.Strings(envFiles)
	for _, p := range envFiles {
		svc := strings.TrimSuffix(filepath.Base(p), ".env")
		body, err := os.ReadFile(p) // #nosec G304 -- the fetch's own output
		if err != nil {
			return err
		}
		dst := remote.ShellQuote(stack + "/env/" + svc + ".env")
		write := fmt.Sprintf("umask 077; mkdir -p %s && cat > %s.new && mv %s.new %s", remote.ShellQuote(stack+"/env"), dst, dst, dst)
		if err := target.Run(bytes.NewReader(body), io.Discard, target.Sudo("sh -c "+remote.ShellQuote(write))); err != nil {
			return fmt.Errorf("deploy: delivering %s.env: %w", svc, err)
		}
	}

	step("deploy-server.sh — blue/green on " + ssh)
	exports := fmt.Sprintf("export DEPLOY_TAG=%s PG_TAG=%s IMAGE_PREFIX=%s CUSTOM_IMAGES=%s REGISTRY=\n",
		remote.ShellQuote(tag), remote.ShellQuote(pgTag), remote.ShellQuote(prefix), remote.ShellQuote(strings.Join(custom, " ")))
	if err := target.Run(strings.NewReader(exports), core.Stdout,
		target.Sudo("bash -c "+remote.ShellQuote("set -a; . /dev/stdin; set +a; exec "+stack+"/install/deploy-server.sh"))); err != nil {
		return fmt.Errorf("deploy: deploy-server.sh: %w", err)
	}
	fmt.Fprintf(core.Stdout, "\ndeployed %s (%s) to %s — commit the lock: migrate generate pinned new migrations in it.\n", tag, c.Env, ssh)
	fmt.Fprint(core.Stdout, warning)
	return nil
}

// serverPlatform is the docker platform of the server's machine: the images
// are built for it, whatever this one is. Without `--platform` docker builds
// for the laptop, and an amd64 image on an arm64 server starts and dies with
// "exec format error" on every task — a restart loop that names no cause.
func serverPlatform(target remote.Target) (string, error) {
	var out strings.Builder
	if err := target.Run(nil, &out, "uname -m"); err != nil {
		return "", fmt.Errorf("asking %s for its machine type: %w", target.Dest(), err)
	}
	// The last line: a shell profile that prints on a non-interactive login
	// comes before the answer, never after it.
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	switch m := strings.TrimSpace(lines[len(lines)-1]); m {
	case "x86_64", "amd64":
		return "linux/amd64", nil
	case "aarch64", "arm64":
		return "linux/arm64", nil
	default:
		return "", fmt.Errorf("%s is a %q machine; the images build for linux/amd64 and linux/arm64 only", target.Dest(), m)
	}
}

// ownPlatform reports whether the build flags already choose the platform
// (`--build-flag=--platform=…`, the way to reach another machine before the
// server was asked): docker's --platform is a list, and a second one would
// turn every build into a multi-platform build the default driver refuses.
func ownPlatform(flags []string) bool {
	for _, f := range flags {
		if f == "--platform" || strings.HasPrefix(f, "--platform=") {
			return true
		}
	}
	return false
}

// imageName turns the lock's project name into a docker repository component
// (lower-case alphanumerics separated by single . _ or -).
func imageName(project string) string {
	n := strings.Trim(regexp.MustCompile(`[^a-z0-9]+`).ReplaceAllString(strings.ToLower(project), "-"), "-")
	if n == "" {
		return "project"
	}
	return n
}

func step(s string) { fmt.Fprintf(core.Stdout, "\n── %s\n", s) }

// gitState: whether the tree has uncommitted changes, and HEAD's short sha.
func gitState(root string) (dirty bool, sha string, err error) {
	var out bytes.Buffer
	if err := run(nil, &out, root, "git", "rev-parse", "--short=7", "HEAD"); err != nil {
		return false, "", errors.New("deploy: not a git repository with a commit — the images are tagged by the commit they are built from")
	}
	sha = strings.TrimSpace(out.String())
	out.Reset()
	if err := run(nil, &out, root, "git", "status", "--porcelain"); err != nil {
		return false, "", fmt.Errorf("deploy: git status: %w", err)
	}
	return strings.TrimSpace(out.String()) != "", sha, nil
}

// image is one line of images.txt / images.custom.txt: <image> <context>
// <Dockerfile> [+target=<stage>] [+lock] [KEY=VALUE …] — the trailing pairs
// are build arguments; +lock (a cloud bundle) layers the lock and the
// fixtures on top.
type image struct {
	name, context, dockerfile string
	target                    string
	lock                      bool
	buildArgs                 []string
}

var buildArg = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=\S*$`)

func readImages(path string) ([]image, error) {
	f, err := os.Open(path) // #nosec G304 -- the project's own generated file
	if err != nil {
		return nil, fmt.Errorf("%s: %w — run `w17ctl codegen`", path, err)
	}
	defer func() { _ = f.Close() }()
	var out []image
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < 3 {
			return nil, fmt.Errorf("%s: %q is not <image> <context> <Dockerfile> [KEY=VALUE …]", path, line)
		}
		im := image{name: parts[0], context: parts[1], dockerfile: parts[2]}
		for _, a := range parts[3:] {
			switch {
			case strings.HasPrefix(a, "+target="):
				im.target = strings.TrimPrefix(a, "+target=")
			case a == "+lock":
				im.lock = true
			case buildArg.MatchString(a):
				im.buildArgs = append(im.buildArgs, a)
			default:
				return nil, fmt.Errorf("%s: %q — %q is not a KEY=VALUE build argument", path, line, a)
			}
		}
		out = append(out, im)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s lists no image", path)
	}
	return out, sc.Err()
}

// withCustomImages applies deploy/images.custom.txt (yours, optional; same
// format as images.txt): a name images.txt has is built your way instead, a new
// name is an extra image, built and shipped like the bundles — for your extra
// services. Returns the merged list and the new names.
func withCustomImages(images []image, path string) ([]image, []string, error) {
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return images, nil, nil
	}
	custom, err := readImages(path)
	if err != nil {
		return nil, nil, err
	}
	var added []string
	for _, c := range custom {
		if c.name == "stack-postgres" {
			return nil, nil, fmt.Errorf("%s: stack-postgres is the stack's own (content-hashed from deploy/<env>/services/postgres) and cannot be replaced", path)
		}
		if !imageRef.MatchString(c.name) {
			return nil, nil, fmt.Errorf("%s: %q is not an image name", path, c.name)
		}
		if i := slices.IndexFunc(images, func(im image) bool { return im.name == c.name }); i >= 0 {
			images[i] = c
			continue
		}
		images = append(images, c)
		added = append(added, c.name)
	}
	return images, added, nil
}

var imageRef = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// dirHash is the content hash of a directory's files (names and bytes, .env*
// excluded): the stack-postgres tag changes exactly when its files do.
func dirHash(dir string) (string, error) {
	var paths []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasPrefix(d.Name(), ".env") {
			return err
		}
		paths = append(paths, p)
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(paths)
	h := sha256.New()
	for _, p := range paths {
		b, err := os.ReadFile(p) // #nosec G304 -- files of the project's own deploy tree
		if err != nil {
			return "", err
		}
		rel, _ := filepath.Rel(dir, p)
		fmt.Fprintf(h, "%x  ./%s\n", sha256.Sum256(b), filepath.ToSlash(rel))
	}
	return fmt.Sprintf("%x", h.Sum(nil))[:12], nil
}
