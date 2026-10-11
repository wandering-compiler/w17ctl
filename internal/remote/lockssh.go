package remote

// The server of an infrastructure environment (`w17ctl infra setup`,
// `w17ctl deploy`), reached with the operator's own ssh — their agent, keys,
// known_hosts and config. The target is the lock's `host.ssh`
// (`user@host[:port]`, validated by the console), which unlike a dev-stack
// Dest may carry a bracketed IPv6 address; and these calls stream (docker
// save, decrypted env files) rather than capture.

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// Target is a parsed `user@host[:port]`.
type Target struct {
	User, Host, Port string
}

// Parse splits the lock's ssh target; a bracketed IPv6 host loses its
// brackets, which ssh does not take.
func Parse(s string) (Target, error) {
	user, host, ok := strings.Cut(s, "@")
	if !ok || user == "" || host == "" {
		return Target{}, fmt.Errorf("ssh target %q is not user@host[:port]", s)
	}
	port := "22"
	if strings.HasPrefix(host, "[") {
		end := strings.Index(host, "]")
		if end < 0 {
			return Target{}, fmt.Errorf("ssh target %q: unclosed [", s)
		}
		if rest := host[end+1:]; rest != "" {
			port = strings.TrimPrefix(rest, ":")
		}
		host = host[1:end]
	} else if h, p, found := strings.Cut(host, ":"); found {
		host, port = h, p
	}
	return Target{User: user, Host: host, Port: port}, nil
}

// Dest is what ssh dials: user@host.
func (t Target) Dest() string { return t.User + "@" + t.Host }

// Root reports whether commands run as root without sudo.
func (t Target) Root() bool { return t.User == "root" }

// Sudo prefixes a remote command with sudo for a non-root user.
func (t Target) Sudo(cmd string) string {
	if t.Root() {
		return cmd
	}
	return "sudo " + cmd
}

// SSH is the seam the tests replace: it runs the system ssh.
var SSH = func(stdin io.Reader, stdout io.Writer, args ...string) error {
	cmd := exec.Command("ssh", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, os.Stderr
	return cmd.Run()
}

// Run runs one remote command line, stdin and stdout attached.
func (t Target) Run(stdin io.Reader, stdout io.Writer, remote string) error {
	return SSH(stdin, stdout, "-p", t.Port, "--", t.Dest(), remote)
}

// RunTTY runs one remote command line with a terminal (a sudo password prompt).
func (t Target) RunTTY(remote string) error {
	return SSH(os.Stdin, os.Stdout, "-t", "-p", t.Port, "--", t.Dest(), remote)
}

// RsyncArgs are the rsync options that reach this target (port, sudo).
func (t Target) RsyncArgs() []string {
	args := []string{"-e", "ssh -p " + t.Port}
	if !t.Root() {
		args = append(args, "--rsync-path=sudo rsync")
	}
	return args
}

// StackFiles are the parts of a swarm environment's rendered tree a deploy
// copies to the server (deploy/<env>/… → /opt/stack/<env>/…), besides the
// project's hooks/ when it has one.
var StackFiles = []string{"services", "install", "stack-limits.sh"}

// Fingerprint is a hash of what the last deploy left in stackDir on the
// server — the files it copies, no .env — so a plan can name the state it
// was made against and a deploy can refuse when that state moved
// (`w17ctl deploy --expect`). "none" when nothing was deployed yet.
func (t Target) Fingerprint(stackDir string) (string, error) {
	var out strings.Builder
	script := "cd " + ShellQuote(stackDir) + " 2>/dev/null || { echo none; exit 0; }; " +
		"find " + strings.Join(StackFiles, " ") + " hooks -type f ! -name '.env*' 2>/dev/null | LC_ALL=C sort | " +
		"xargs -r sha256sum | sha256sum | cut -c1-16"
	if err := t.Run(nil, &out, t.Sudo("sh -c "+ShellQuote(script))); err != nil {
		return "", fmt.Errorf("fingerprint of %s on %s: %w", stackDir, t.Dest(), err)
	}
	fp := strings.TrimSpace(out.String())
	if fp == "" {
		fp = "none"
	}
	return fp, nil
}
