//go:build windows

package tunnel

import (
	"os"
	"syscall"
)

// Windows process-creation flags (not exported by the syscall package).
const (
	createNewProcessGroup = 0x00000200
	detachedProcess       = 0x00000008
)

// detachAttrs starts the ssh forward child detached from this console so
// it keeps running after the launching shell closes.
func detachAttrs() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: createNewProcessGroup | detachedProcess}
}

// signalPID stops the forward child. Windows has no SIGTERM delivery to
// another process, so this is a forceful kill.
func signalPID(p *os.Process) error {
	return p.Kill()
}
