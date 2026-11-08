package infra

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/remote"
	"github.com/wandering-compiler/w17ctl/internal/stackship"
)

// RecoverCmd is `w17ctl infra recover` — a dead server's environment rebuilt
// on a new one: setup.sh there, the stack's files and the values from the
// console, then install/recover.sh, which restores every backed-up database
// server from its latest backup. The deploy that follows (the workflow, or
// `w17ctl deploy`) starts the databases and finishes the restores; recover
// deploys nothing itself. A separate command, never a mode of setup or deploy:
// those keep ignoring the backups.
type RecoverCmd struct {
	Env            string   `name:"env" required:"" help:"The environment whose server died."`
	To             string   `name:"to" required:"" placeholder:"USER@HOST[:PORT]" help:"The new server, freshly provisioned: setup.sh runs there, then the restore."`
	ValuesOnServer bool     `name:"values-on-server" help:"The env files are already in /opt/stack/<env>/env on the new server (you delivered them): fetch none from the console."`
	Args           []string `arg:"" optional:"" passthrough:"" help:"Passed to setup.sh after --, e.g. -- --backup-device /dev/disk/by-id/<id>."`
	LockPath       string   `name:"lock" placeholder:"PATH" default:"w17/lock.yaml" help:"Path to the lock file."`
	Console        string   `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console (owns the lock). Optional — falls back to the binary's compile-time default."`
}

// The seams the tests replace: local programs (rsync, git), the values'
// fetch and this binary's path.
var (
	recoverRun = func(stdin io.Reader, stdout io.Writer, dir, name string, args ...string) error {
		cmd := exec.Command(name, args...)
		cmd.Dir, cmd.Stdin, cmd.Stdout, cmd.Stderr = dir, stdin, stdout, os.Stderr
		return cmd.Run()
	}
	recoverFetch = stackship.FetchEnv
	recoverSelf  = os.Executable
)

func (c *RecoverCmd) Run() error {
	view, err := core.DescribeInfraAt("infra recover", c.Console, c.LockPath, false, "")
	if err != nil {
		return err
	}
	env, err := FindEnv(view, c.Env)
	if err != nil {
		return fmt.Errorf("infra recover: %w", err)
	}
	if env.GetTargetKind() != "swarm" {
		return fmt.Errorf("infra recover: rebuilds a swarm environment's server; %s deploys onto %s (%s), whose databases are the cloud's", c.Env, env.GetTarget(), env.GetTargetKind())
	}
	target, err := remote.Parse(c.To)
	if err != nil {
		return fmt.Errorf("infra recover: --to: %w", err)
	}
	lockAbs, err := filepath.Abs(c.LockPath)
	if err != nil {
		return err
	}
	root := filepath.Dir(filepath.Dir(lockAbs))
	envDir := filepath.Join(root, "deploy", c.Env)
	if _, err := os.Stat(filepath.Join(envDir, "install", "setup.sh")); err != nil {
		return fmt.Errorf("infra recover: %s/install/setup.sh is missing — run `w17ctl codegen` first", envDir)
	}
	if _, err := os.Stat(filepath.Join(envDir, "install", "recover.sh")); err != nil {
		return fmt.Errorf("infra recover: environment %s backs no database up (w17/infra.yaml), so there is nothing to recover — set the new server up with `w17ctl infra setup --env %s` and deploy", c.Env, c.Env)
	}
	if !c.ValuesOnServer && os.Getenv(stackship.FetchTokenVar) == "" && os.Getenv("W17_SECRETS_CODE") == "" && !stdinTerminal() {
		return fmt.Errorf("infra recover: the values come from the console — set %s to the token of a machine account given Deploy access to %s, "+
			"or run it from a terminal (or with W17_SECRETS_CODE) to read them as yourself with a read code", stackship.FetchTokenVar, c.Env)
	}
	stack := "/opt/stack/" + c.Env

	step("the new server, " + c.To)
	if err := preflight(target, c.Env); err != nil {
		return err
	}

	step("setup.sh on " + c.To)
	// A piped stdin is the confirmation's, not setup.sh's.
	var setupIn io.Reader = os.Stdin
	if !stdinTerminal() {
		setupIn = strings.NewReader("")
	}
	if err := setupServer(setupIn, c.To, envDir, "deploy/"+c.Env, c.Args); err != nil {
		return err
	}

	step("the stack's files")
	if err := stackship.SendFiles(recoverRun, target, root, envDir, stack); err != nil {
		return fmt.Errorf("infra recover: %w", err)
	}
	if c.ValuesOnServer {
		fmt.Fprintf(core.Stdout, "env values: already on %s (--values-on-server)\n", c.To)
	} else {
		step("the env files, from the console into ssh")
		if err := c.sendValues(target, root, lockAbs, stack); err != nil {
			return err
		}
	}

	step("what the backups hold")
	if err := target.Run(nil, core.Stdout, target.Sudo(stack+"/install/recover.sh --check")); err != nil {
		return fmt.Errorf("infra recover: recover.sh --check on %s: %w — nothing was restored", c.To, err)
	}

	text := fmt.Sprintf(`
This restores environment %[1]s's databases on %[2]s from the backups listed above.

  The old server must be OFF or cut off from the backups' store. A server still running keeps
  archiving into the same backups, and its retention could delete what the new one writes.

`, c.Env, c.To)
	if err := confirm("infra recover", text, "nothing was restored", typed{"the environment's name", c.Env}); err != nil {
		return err
	}

	step("recover.sh — the restores, on " + c.To)
	if err := target.Run(nil, core.Stdout, target.Sudo(stack+"/install/recover.sh")); err != nil {
		return fmt.Errorf("infra recover: recover.sh on %s: %w", c.To, err)
	}
	fmt.Fprint(core.Stdout, nextSteps(c.Env, c.To, env.GetHost().GetSsh()))
	return nil
}

// sendValues fetches the environment's values from the console and writes
// them into the new server's stack/env.
func (c *RecoverCmd) sendValues(target remote.Target, root, lockAbs, stack string) error {
	selfPath, err := recoverSelf()
	if err != nil {
		return err
	}
	var sha bytes.Buffer
	ref := "recover"
	if recoverRun(nil, &sha, root, "git", "rev-parse", "--short=7", "HEAD") == nil {
		ref = strings.TrimSpace(sha.String())
	}
	dir, err := os.MkdirTemp("", "w17-values-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := recoverFetch(root, selfPath, lockAbs, c.Console, c.Env, ref, dir, false); err != nil {
		return fmt.Errorf("infra recover: fetching %s's values: %w", c.Env, err)
	}
	if err := stackship.SendEnv(target, dir, stack); err != nil {
		return fmt.Errorf("infra recover: %w", err)
	}
	return nil
}

// preflight refuses a server that runs the environment: recover is for a new
// one. Nothing is written.
func preflight(target remote.Target, env string) error {
	var out strings.Builder
	probe := "if command -v docker >/dev/null 2>&1; then docker service ls -q --filter label=com.docker.stack.namespace=" + remote.ShellQuote(env) + " 2>/dev/null | head -n 1; fi; echo reachable"
	if err := target.Run(nil, &out, target.Sudo("sh -c "+remote.ShellQuote(probe))); err != nil {
		return fmt.Errorf("infra recover: reaching %s: %w", target.Dest(), err)
	}
	lines := strings.Fields(out.String())
	if len(lines) == 0 || lines[len(lines)-1] != "reachable" {
		return fmt.Errorf("infra recover: %s answered %q", target.Dest(), out.String())
	}
	if len(lines) > 1 {
		return fmt.Errorf("infra recover: environment %s runs on %s — recover rebuilds a dead server on a NEW one; on a running server, install/restore.sh brings a database server back", env, target.Dest())
	}
	fmt.Fprintf(core.Stdout, "%s is reachable and runs no %s stack\n", target.Dest(), env)
	return nil
}

// nextSteps is what the operator does after recover.sh: the deploy, and what
// CI needs to reach the new server.
func nextSteps(env, to, declared string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\nrecover: %s's databases are prepared on %s. Next:\n", env, to)
	fmt.Fprintf(&b, "  1. The deploy workflow's environment %s: set its variable STACK_HOST_KEY to the line setup.sh printed above.\n", env)
	if declared != to {
		fmt.Fprintf(&b, "  2. w17/infra.yaml names %s: `w17ctl infra update --ssh %s=%s`, commit it, and run the deploy workflow.\n", orNone(declared), env, to)
	} else {
		b.WriteString("  2. Run the deploy workflow again — re-running the last run's deploy job is enough, its images are built.\n")
	}
	fmt.Fprintf(&b, "     (Without CI: `w17ctl deploy --env %s`.) The deploy starts each database, replays what was archived and\n", env)
	b.WriteString("     takes a new base backup; until it ran, nothing is served and nothing is backed up.\n")
	return b.String()
}

func orNone(s string) string {
	if s == "" {
		return "no server"
	}
	return s
}

func step(s string) { fmt.Fprintf(core.Stdout, "\n── %s\n", s) }
