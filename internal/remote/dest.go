// Package remote holds the small, shared primitives for w17ctl's remote
// dev-stack mode (docs/experiments/remote-stack.md): parsing an SSH
// destination and turning it into the argv fragments that rsync, the
// remote docker-compose runner, and the tunnel supervisor all need.
//
// It carries NO w17 know-how — it is pure transport plumbing, in keeping
// with the dumb-client boundary (D4): the remote is merely the docker leg
// reached over SSH.
package remote

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// Dest is a parsed SSH destination — a registered Remote.SSH value like
// "user@host", "user@host:2222", or a bare ssh_config alias "beast".
type Dest struct {
	// Target is the ssh destination argument (everything except an
	// explicit ":port"): "user@host", "host", or an alias.
	Target string
	// Port is the explicit TCP port, or 0 when none was given (ssh then
	// uses its own default / ssh_config).
	Port int
}

// ParseDest splits an SSH destination into its target and optional port.
// A trailing ":<digits>" is taken as the port (validated to 1..65535);
// any other trailing ":..." is left as part of the target (so an unusual
// alias is not mangled). An empty string is an error.
func ParseDest(s string) (Dest, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Dest{}, fmt.Errorf("remote: empty SSH destination")
	}
	if i := strings.LastIndex(s, ":"); i >= 0 {
		host, portStr := s[:i], s[i+1:]
		if n, err := strconv.Atoi(portStr); err == nil {
			// Numeric suffix → a port. Validate the range.
			if n < 1 || n > 65535 {
				return Dest{}, fmt.Errorf("remote: port %d out of range in %q", n, s)
			}
			if host == "" {
				return Dest{}, fmt.Errorf("remote: no host before port in %q", s)
			}
			return Dest{Target: host, Port: n}, nil
		}
		// Non-numeric suffix (e.g. an odd alias) → keep the whole string.
	}
	return Dest{Target: s}, nil
}

// SSHPortArgs returns the ssh CLI port flag ("-p N") when a port is set,
// else an empty slice — spliceable straight into an ssh argv.
func (d Dest) SSHPortArgs() []string {
	if d.Port == 0 {
		return nil
	}
	return []string{"-p", strconv.Itoa(d.Port)}
}

// RsyncShell returns the value for rsync's `-e` remote-shell flag when a
// non-default port is set ("ssh -p N"), else "" (rsync then uses plain
// ssh). Kept as one string because that is rsync's `-e` contract.
func (d Dest) RsyncShell() string {
	if d.Port == 0 {
		return ""
	}
	return "ssh -p " + strconv.Itoa(d.Port)
}

// ExecArgs builds the ssh argv (after the program name) that runs a single
// remote shell command string: [portFlag...] target command.
func (d Dest) ExecArgs(command string) []string {
	return append(d.SSHPortArgs(), d.Target, command)
}

// ExecFn is the seam tests stub; production runs realExec. It executes a
// single command string on the remote host over ssh, streaming stdin from
// `stdin` (nil = none) and returning captured stdout. stderr goes to the
// process stderr so remote errors are visible.
var ExecFn = realExec

func realExec(d Dest, command string, stdin io.Reader) ([]byte, error) {
	cmd := exec.Command("ssh", d.ExecArgs(command)...)
	cmd.Stdin = stdin
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	return out.Bytes(), err
}

// Exec runs a raw command on the remote host via the ExecFn seam.
func Exec(d Dest, command string, stdin io.Reader) ([]byte, error) {
	return ExecFn(d, command, stdin)
}

// ShellQuote wraps s in single quotes, escaping embedded single quotes with
// the standard POSIX idiom — close the quote, emit an escaped one, reopen:
//
//	'\''
//
// Safe for arbitrary values in a POSIX remote command string.
//
// The idiom sits in an indented block rather than inline because gofmt's
// doc-comment reformatter turns a doubled apostrophe in prose into a
// typographic quote pair, which silently rewrites the documented escape into
// nonsense. Indented lines are a code block and are left alone.
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
