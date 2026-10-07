package infra

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/remote"
)

// SetupCmd is `w17ctl infra setup` — prepare one environment's server with
// the operator's own SSH key: copy deploy/<env>/install there and run its
// setup.sh as root. The server is the lock's `host.ssh`; nothing about it is
// decided here, the script is the console's (rendered by codegen).
type SetupCmd struct {
	Env      string   `name:"env" required:"" help:"The environment whose server to prepare."`
	Args     []string `arg:"" optional:"" passthrough:"" help:"Passed to setup.sh after --, e.g. -- --backup-device /dev/disk/by-id/<id>."`
	LockPath string   `name:"lock" placeholder:"PATH" default:"w17/lock.yaml" help:"Path to the lock file."`
	Console  string   `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console (owns the lock). Optional — falls back to the binary's compile-time default."`
}

func (c *SetupCmd) Run() error {
	view, err := core.DescribeLockAt("infra setup", c.Console, c.LockPath)
	if err != nil {
		return err
	}
	in := view.GetInfra()
	if in.GetTarget() != "swarm" {
		return fmt.Errorf("infra setup: prepares the servers of the swarm target; this project's infrastructure is %q", firstNonEmpty(in.GetTarget(), "not declared — `w17ctl infra init`"))
	}
	var target string
	found := false
	for _, e := range in.GetEnvironments() {
		if e.GetName() == c.Env {
			found, target = true, e.GetHost().GetSsh()
		}
	}
	if !found {
		return fmt.Errorf("infra setup: no environment %q (`w17ctl infra show`)", c.Env)
	}
	if target == "" {
		return fmt.Errorf("infra setup: environment %q names no server — `w17ctl infra update --ssh %s=root@<server>`", c.Env, c.Env)
	}
	tg, err := remote.Parse(target)
	if err != nil {
		return fmt.Errorf("infra setup: %w", err)
	}
	dest, port := tg.Dest(), tg.Port

	lockAbs, err := filepath.Abs(c.LockPath)
	if err != nil {
		return err
	}
	envDir := filepath.Join(filepath.Dir(filepath.Dir(lockAbs)), "deploy", c.Env)
	if _, err := os.Stat(filepath.Join(envDir, "install", "setup.sh")); err != nil {
		return fmt.Errorf("infra setup: %s/install/setup.sh is missing — run `w17ctl codegen` first", envDir)
	}
	// install/ and, when the project has one, its own hooks/ (setup.sh runs
	// hooks/setup.sh) — laid out as on the server.
	archive, err := tarDirs(envDir, "install", "hooks")
	if err != nil {
		return fmt.Errorf("infra setup: %w", err)
	}

	base := []string{"-p", port, "--", dest}
	// One connection uploads, a second one runs with a terminal: a non-root
	// user's sudo may ask for a password, which needs the tty the upload's
	// stdin (the archive) cannot be.
	var remoteDir bytes.Buffer
	fmt.Fprintf(core.Stdout, "copying deploy/%s/install to %s …\n", c.Env, target)
	if err := remote.SSH(bytes.NewReader(archive), &remoteDir, append(base,
		`d=$(mktemp -d /tmp/w17-setup.XXXXXX) && tar -C "$d" -xf - && echo "$d"`)...); err != nil {
		return fmt.Errorf("infra setup: copying to %s: %w", target, err)
	}
	d := strings.TrimSpace(remoteDir.String())
	if !strings.HasPrefix(d, "/tmp/w17-setup.") || strings.ContainsAny(d, " '\"\n;$`\\") {
		return fmt.Errorf("infra setup: the server answered %q instead of the directory it unpacked into", d)
	}
	run := "bash " + d + "/install/setup.sh"
	for _, a := range c.Args {
		run += " " + remote.ShellQuote(a)
	}
	if user, _, _ := strings.Cut(dest, "@"); user != "root" {
		run = "sudo " + run
	}
	remoteCmd := run + `; rc=$?; rm -rf ` + d + `; exit $rc`
	if err := remote.SSH(os.Stdin, core.Stdout, append([]string{"-t", "-p", port, "--", dest}, remoteCmd)...); err != nil {
		return fmt.Errorf("infra setup: setup.sh on %s failed: %w", target, err)
	}
	fmt.Fprintf(core.Stdout, "server %s is ready for environment %s\n", target, c.Env)
	return nil
}

// tarDirs archives the files under root/<sub> for each sub that exists, keeping
// each file's mode (the scripts are executable) and its root-relative path.
func tarDirs(root string, subs ...string) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, sub := range subs {
		dir := filepath.Join(root, sub)
		if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err := tarInto(tw, root, dir); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func tarInto(tw *tar.Writer, root, dir string) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		body, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if err := tw.WriteHeader(&tar.Header{Name: filepath.ToSlash(rel), Mode: int64(info.Mode().Perm()), Size: int64(len(body))}); err != nil {
			return err
		}
		_, err = tw.Write(body)
		return err
	})
}
