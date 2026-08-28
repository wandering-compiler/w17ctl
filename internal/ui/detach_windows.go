//go:build windows

package ui

import (
	"os"
	"syscall"
)

// Windows process-creation flags (not exported by the syscall package).
const (
	createNewProcessGroup = 0x00000200
	detachedProcess       = 0x00000008
)

// detachAttrs starts the background UI detached from this console so it
// keeps running after the launching shell closes.
func detachAttrs() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: createNewProcessGroup | detachedProcess}
}

// terminate stops the server. Windows has no SIGTERM delivery to another
// process, so this is a forceful kill; the next `ui start` rewrites the
// (now stale) state file.
func terminate(p *os.Process) error {
	return p.Kill()
}
