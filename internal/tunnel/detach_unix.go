//go:build !windows

package tunnel

import (
	"os"
	"syscall"
)

// detachAttrs starts the ssh forward child in its own session (setsid) so
// it survives the one-shot `stack up` returning and the launching shell
// closing — the tunnels must live for the whole dev session.
func detachAttrs() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

// signalPID asks the forward child to stop (SIGTERM → ssh exits, dropping
// its forwards). Best-effort: a missing process is not an error.
func signalPID(p *os.Process) error {
	return p.Signal(syscall.SIGTERM)
}
