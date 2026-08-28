// Package tunnel opens the SSH local-forwards that make a remote stack
// feel local (docs/experiments/remote-stack.md §2.1): docker publishes
// each port on the REMOTE host, and w17ctl runs `ssh -N -L
// <local>:127.0.0.1:<remote>` so `localhost:<port>` behaves identically.
//
// The forwards serve two masters (§6): user-facing app ports AND w17ctl's
// own control-plane connections (the remote DB for migrate/reconcile).
// Both are just Forward entries.
//
// Spec derivation is a pure, unit-tested function of the port map; the
// long-lived ssh child is behind the StartFn seam. `stack up` opens the
// supervisor, `stack down` closes it.
package tunnel

import (
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"

	"github.com/wandering-compiler/w17ctl/internal/remote"
)

// Forward is one local-forward: bind LocalPort on the laptop, forward to
// RemotePort on the server's loopback. In the common case they are equal
// (the port allocator already hands out unique host ports, so the same
// number is free locally), but control forwards (e.g. the remote Postgres
// port) may differ.
type Forward struct {
	LocalPort  int
	RemotePort int
}

// Spec renders the ssh `-L` argument: "<local>:127.0.0.1:<remote>". The
// remote side is pinned to 127.0.0.1 (the interface docker publishes to),
// not the wildcard.
func (f Forward) Spec() string {
	return strconv.Itoa(f.LocalPort) + ":127.0.0.1:" + strconv.Itoa(f.RemotePort)
}

// ForwardsFromPorts turns a project's host-port map (env-var → port, as
// devconfig assigns) into forwards where local==remote, merges any extra
// control forwards, drops zero ports, de-duplicates by local port, and
// returns them sorted by local port (deterministic argv). The allocator
// guarantees unique local ports across ALL projects, so two projects'
// tunnels never collide — the core 10-project promise.
func ForwardsFromPorts(ports map[string]int, extra ...Forward) []Forward {
	byLocal := map[int]Forward{}
	for _, p := range ports {
		if p == 0 {
			continue
		}
		byLocal[p] = Forward{LocalPort: p, RemotePort: p}
	}
	for _, f := range extra {
		if f.LocalPort == 0 {
			continue
		}
		byLocal[f.LocalPort] = f
	}
	out := make([]Forward, 0, len(byLocal))
	for _, f := range byLocal {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LocalPort < out[j].LocalPort })
	return out
}

// Supervisor owns the ssh child that holds a project's forwards open for
// the life of a `stack up`.
type Supervisor struct {
	Dest     remote.Dest
	Forwards []Forward

	proc Process
}

// SSHArgs builds the ssh argv (after the program name): -N (forward only,
// no remote command), resilience options, the port flag, one -L per
// forward, then the target last.
func (s *Supervisor) SSHArgs() []string {
	args := []string{
		"-N",
		"-o", "ExitOnForwardFailure=yes",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=3",
	}
	args = append(args, s.Dest.SSHPortArgs()...)
	for _, f := range s.Forwards {
		args = append(args, "-L", f.Spec())
	}
	args = append(args, s.Dest.Target)
	return args
}

// Open launches the ssh forward child. A supervisor with no forwards is a
// no-op (nothing to tunnel). Opening an already-running supervisor errors.
func (s *Supervisor) Open() error {
	if len(s.Forwards) == 0 {
		return nil
	}
	if s.proc != nil {
		return fmt.Errorf("tunnel: already open")
	}
	p, err := StartFn(s.SSHArgs())
	if err != nil {
		return fmt.Errorf("tunnel: start ssh forwards: %w", err)
	}
	s.proc = p
	return nil
}

// Close tears the forwards down. Safe to call when never opened or
// already closed (idempotent).
func (s *Supervisor) Close() error {
	if s.proc == nil {
		return nil
	}
	err := s.proc.Close()
	s.proc = nil
	return err
}

// PID returns the OS pid of the running forward child (0 when closed or
// never opened) — recorded in the state file so a later `stack down` in a
// fresh w17ctl process can kill it.
func (s *Supervisor) PID() int {
	if s.proc == nil {
		return 0
	}
	return s.proc.PID()
}

// Process is the handle Open holds — a running ssh child. Because the
// child is detached (it must outlive the one-shot `stack up`), Close
// signals it by pid rather than reaping a still-owned process.
type Process interface {
	PID() int
	Close() error
}

// StartFn is the seam tests stub; production runs realStart.
var StartFn = realStart

func realStart(argv []string) (Process, error) {
	cmd := exec.Command("ssh", argv...)
	cmd.Stdout = os.Stderr // ssh -N is quiet; surface diagnostics on stderr
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = detachAttrs() // outlive this w17ctl invocation
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release() // hand the child to init; down kills it by pid
	return &procHandle{pid: pid}, nil
}

// procHandle wraps the detached ssh child. Close SIGTERMs it by pid (the
// process was Release()d, so there is nothing to Wait on here).
type procHandle struct {
	pid int
}

func (h *procHandle) PID() int     { return h.pid }
func (h *procHandle) Close() error { return KillPID(h.pid) }
