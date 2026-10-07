// Package deploy implements `w17ctl deploy` — one person's shortcut from the
// laptop to a swarm environment, for an MVP or a proof of concept
// (docs/decisions/infra-targets.md §4.5). It runs what the deploy workflow
// runs, on the same generated scripts, with the operator's own credentials;
// what it skips is the review, the release branch and the record a CI run
// leaves — which is why it says so every time.
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

	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/remote"
	"github.com/wandering-compiler/sdk/go/service/secret/sopsenv"
)

// Cmd is `w17ctl deploy`.
type Cmd struct {
	Env        string   `name:"env" required:"" help:"The environment to deploy (deploy/<env>/)."`
	WithTests  bool     `name:"with-tests" help:"Run the e2e suite (w17ctl test) first; a red suite stops the deploy."`
	AllowDirty bool     `name:"allow-dirty" help:"Deploy a tree with uncommitted changes (the images are then tagged as dirty)."`
	BuildFlags []string `name:"build-flag" placeholder:"FLAG" help:"Passed to every docker build, e.g. --build-flag=--build-arg=X=1. Repeatable."`
	Key        string   `name:"key" placeholder:"FILE" help:"The environment's age key file. Default: $SOPS_AGE_KEY, then $SOPS_AGE_KEY_FILE."`
	LockPath   string   `name:"lock" placeholder:"PATH" default:"w17/lock.yaml" help:"Path to the lock file."`
	Console    string   `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console. Optional — falls back to the binary's compile-time default."`
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

func (c *Cmd) Run() error {
	fmt.Fprint(core.Stdout, warning)
	abs, err := filepath.Abs(c.LockPath)
	if err != nil {
		return err
	}
	root := filepath.Dir(filepath.Dir(abs))
	view, err := core.DescribeLockAt("deploy", c.Console, c.LockPath)
	if err != nil {
		return err
	}
	if view.GetInfra().GetTarget() != "swarm" {
		return fmt.Errorf("deploy: deploys a swarm environment; this project's infrastructure is %q", view.GetInfra().GetTarget())
	}
	var ssh string
	found := false
	for _, e := range view.GetInfra().GetEnvironments() {
		if e.GetName() == c.Env {
			found, ssh = true, e.GetHost().GetSsh()
		}
	}
	if !found {
		return fmt.Errorf("deploy: no environment %q (`w17ctl infra show`)", c.Env)
	}
	if ssh == "" {
		return fmt.Errorf("deploy: environment %q names no server — `w17ctl infra update --ssh %s=root@<server>`, then `w17ctl infra setup --env %s`", c.Env, c.Env, c.Env)
	}
	target, err := remote.Parse(ssh)
	if err != nil {
		return fmt.Errorf("deploy: %w", err)
	}
	ids, err := sopsenv.LoadIdentities(os.Getenv, nonEmpty(c.Key)...)
	if err != nil {
		return fmt.Errorf("deploy: %w", err)
	}
	if len(ids) == 0 {
		return fmt.Errorf("deploy: no age key for the env files — set SOPS_AGE_KEY (or SOPS_AGE_KEY_FILE), or pass --key")
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

	envDir := filepath.Join(root, "deploy", c.Env)
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

	step("build the images")
	var refs []string
	for _, im := range images {
		ref := prefix + "/" + im.name + ":" + tag
		if im.name == "stack-postgres" {
			ref = prefix + "/stack-postgres:" + pgTag
		}
		args := append([]string{"build", "-q"}, c.BuildFlags...)
		for _, a := range im.buildArgs {
			args = append(args, "--build-arg", a)
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
	srcs := []string{"services", "install", "stack-limits.sh"}
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

	step("the env files, decrypted into ssh")
	encs, _ := filepath.Glob(filepath.Join(envDir, "services", "*", ".env.enc"))
	sort.Strings(encs)
	for _, enc := range encs {
		svc := filepath.Base(filepath.Dir(enc))
		b, err := os.ReadFile(enc)
		if err != nil {
			return err
		}
		lines, err := sopsenv.Decrypt(b, ids)
		if err != nil {
			return fmt.Errorf("deploy: deploy/%s/services/%s/.env.enc: %w", c.Env, svc, err)
		}
		dst := stack + "/env/" + svc + ".env"
		write := fmt.Sprintf("umask 077; mkdir -p %s/env && cat > %s.new && [ -s %s.new ] && mv %s.new %s", stack, dst, dst, dst, dst)
		if err := target.Run(bytes.NewReader(sopsenv.Format(lines)), io.Discard, target.Sudo("sh -c "+remote.ShellQuote(write))); err != nil {
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

func nonEmpty(s string) []string {
	if s == "" {
		return nil
	}
	return []string{s}
}

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
// <Dockerfile> [KEY=VALUE …] — the trailing pairs are build arguments.
type image struct {
	name, context, dockerfile string
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
		for _, a := range parts[3:] {
			if !buildArg.MatchString(a) {
				return nil, fmt.Errorf("%s: %q — %q is not a KEY=VALUE build argument", path, line, a)
			}
		}
		out = append(out, image{parts[0], parts[1], parts[2], parts[3:]})
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
