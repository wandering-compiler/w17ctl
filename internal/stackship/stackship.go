// Package stackship delivers a swarm environment's rendered tree and its
// values to the server: what `w17ctl deploy` and `w17ctl infra recover` both
// send before a script runs there. The values come from the console only
// (`w17ctl secrets fetch`), never from a file on this machine.
package stackship

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/remote"
)

// Runner runs a local program in dir, stdio attached — the callers' own seam.
type Runner func(stdin io.Reader, stdout io.Writer, dir, name string, args ...string) error

// FetchTokenVar names the token the values are fetched with: a machine
// account's, given Deploy access to the environment on the project's page.
// Not W17_TOKEN — the rest of a deploy (the lock, the migrations, codegen)
// runs as you, and the deploy-fetch role may do nothing but fetch.
const FetchTokenVar = "W17_FETCH_TOKEN" //nolint:gosec // G101: the variable's name, not a credential.

// FetchEnv runs `w17ctl secrets fetch` (selfPath) in a child whose W17_TOKEN
// is $W17_FETCH_TOKEN (and with no device key: that one belongs to your
// login). It writes one file per service into dir, checked against the
// rendered stack — or nothing.
//
// The console is resolved here and handed to the child both as --console and
// as W17_CONSOLE_ADDR: an env token is presented only to the console it is
// bound to, and a child left to resolve it on its own could bind it to
// another one and send the stored login instead.
func FetchEnv(root, selfPath, lockPath, console, env, ref, dir string, asJSON bool) error {
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
	if os.Getenv(FetchTokenVar) == "" {
		fmt.Fprintf(core.Stdout, "no %s: reading %s's values as you, with a read code\n", FetchTokenVar, env)
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
	cmd.Env = append(cmd.Env, "W17_TOKEN="+os.Getenv(FetchTokenVar), core.EnvConsoleAddrVar+"="+addr)
	return cmd.Run()
}

// RsyncFlags copy a stack's files to a server as root's, read-only to others.
var RsyncFlags = []string{"-az", "--delete-after", "--chown=root:root", "--chmod=go-w"}

// SendFiles rsyncs the environment's rendered tree (envDir: services/,
// install/, stack-limits.sh and the project's hooks/) and the project's lock
// and fixtures (under root) into stack on t. A hooks/ the project no longer
// has is removed there: a hook deleted here must not keep running there.
func SendFiles(run Runner, t remote.Target, root, envDir, stack string) error {
	rsync := append(append([]string(nil), RsyncFlags...), t.RsyncArgs()...)
	srcs := append([]string(nil), remote.StackFiles...)
	if st, err := os.Stat(filepath.Join(envDir, "hooks")); err == nil && st.IsDir() {
		srcs = append(srcs, "hooks")
	} else if err := t.Run(nil, io.Discard, t.Sudo("rm -rf "+stack+"/hooks")); err != nil {
		return fmt.Errorf("removing %s/hooks: %w", stack, err)
	}
	if err := run(nil, core.Stdout, envDir, "rsync", append(append(rsync, "--exclude", ".env*"), append(srcs, t.Dest()+":"+stack+"/")...)...); err != nil {
		return fmt.Errorf("rsync: %w", err)
	}
	files := []string{"w17/lock.yaml"}
	if st, err := os.Stat(filepath.Join(root, "w17", "fixtures")); err == nil && st.IsDir() {
		files = append(files, "w17/fixtures")
	}
	if err := run(nil, core.Stdout, root, "rsync", append(append(rsync, "-R"), append(files, t.Dest()+":"+stack+"/")...)...); err != nil {
		return fmt.Errorf("rsync: %w", err)
	}
	return nil
}

// SendEnv writes every <service>.env FetchEnv left in valuesDir into
// stack/env on t, over ssh's stdin (never a command line), each beside its
// target and moved over it, mode 0600.
func SendEnv(t remote.Target, valuesDir, stack string) error {
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
		if err := t.Run(bytes.NewReader(body), io.Discard, t.Sudo("sh -c "+remote.ShellQuote(write))); err != nil {
			return fmt.Errorf("delivering %s.env: %w", svc, err)
		}
	}
	return nil
}
