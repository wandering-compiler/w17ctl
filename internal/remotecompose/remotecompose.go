// Package remotecompose runs `docker compose` on a remote docker host
// over SSH in remote-stack mode (docs/experiments/remote-stack.md §2.3).
//
// It uses remote-exec compose — `ssh host 'cd <dir> && <env> docker
// compose ...'` — deliberately, NOT DOCKER_HOST=ssh://: this way the
// build context, compose file, and relative paths all resolve
// remote-local (fast incremental builds) instead of the laptop tarring
// and shipping the context on every build.
//
// The remote command string is a pure function of the Runner + args
// (unit-tested, with POSIX shell-quoting); the exec is behind seams.
package remotecompose

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/remote"
)

// Runner targets one project's compose stack on a remote host.
type Runner struct {
	// Dest is the parsed SSH destination.
	Dest remote.Dest
	// RemoteDir is the absolute project dir on the server
	// (<RemotePath>/<Project>) that compose runs in.
	RemoteDir string
}

// RemoteCommand builds the single shell command string ssh hands the
// remote shell: `cd '<dir>' && KEY='v' ... docker compose <args>`. Env is
// a slice of "KEY=VALUE" strings (the same shape docker.RunComposeEnvFn
// takes); each VALUE is POSIX single-quoted. Only these explicit
// overrides are exported remote — never the laptop's os.Environ().
func (r Runner) RemoteCommand(env []string, composeArgs []string) string {
	var b strings.Builder
	b.WriteString("cd ")
	b.WriteString(shellQuote(r.RemoteDir))
	b.WriteString(" && ")
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			k = kv // a bare name with no '=': export empty
		}
		b.WriteString(k)
		b.WriteString("=")
		b.WriteString(shellQuote(v))
		b.WriteString(" ")
	}
	b.WriteString("docker compose")
	for _, a := range composeArgs {
		b.WriteString(" ")
		b.WriteString(a)
	}
	return b.String()
}

// SSHArgs returns the full ssh argv (everything after the program name):
// the port flag (when set), the target, then the remote command as the
// final single argument.
func (r Runner) SSHArgs(env []string, composeArgs []string) []string {
	args := r.Dest.SSHPortArgs()
	args = append(args, r.Dest.Target, r.RemoteCommand(env, composeArgs))
	return args
}

// RunFn is the seam tests stub; production runs realRun. It streams the
// remote compose output to the terminal, like docker.RunComposeEnvFn.
var RunFn = realRun

func realRun(r Runner, env []string, composeArgs ...string) error {
	cmd := exec.Command("ssh", r.SSHArgs(env, composeArgs)...)
	cmd.Stdout = core.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// Run streams a remote compose invocation via the RunFn seam.
func Run(r Runner, env []string, composeArgs ...string) error {
	return RunFn(r, env, composeArgs...)
}

// CaptureFn is the seam tests stub; production runs realCapture. It
// captures remote stdout (e.g. `ps --format json`), discarding stderr —
// mirrors docker.CaptureComposeFn.
var CaptureFn = realCapture

func realCapture(r Runner, composeArgs ...string) ([]byte, error) {
	cmd := exec.Command("ssh", r.SSHArgs(nil, composeArgs)...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = io.Discard
	err := cmd.Run()
	return buf.Bytes(), err
}

// Capture runs a remote compose invocation and returns its stdout via the
// CaptureFn seam.
func Capture(r Runner, composeArgs ...string) ([]byte, error) {
	return CaptureFn(r, composeArgs...)
}

// shellQuote wraps s in single quotes, escaping any embedded single quote
// with the standard POSIX idiom — close the quote, emit an escaped one,
// reopen:
//
//	'\''
//
// Safe for arbitrary values passed to a POSIX remote shell. Indented rather
// than inline for the reason [remote.ShellQuote] gives: gofmt rewrites a
// doubled apostrophe in prose into a typographic quote pair.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
