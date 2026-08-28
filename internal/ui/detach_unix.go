//go:build !windows

package ui

import (
	"os"
	"syscall"
)

// detachAttrs starts the background UI in its own session (setsid) so it
// survives the launching terminal closing — no SIGHUP when the shell
// exits.
func detachAttrs() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

// terminate asks the server to shut down gracefully (SIGTERM → the
// server's signal context cancels and it drains, removing its state file).
func terminate(p *os.Process) error {
	return p.Signal(syscall.SIGTERM)
}
