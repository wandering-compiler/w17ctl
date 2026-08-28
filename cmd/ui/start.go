package ui

import (
	"github.com/wandering-compiler/w17ctl/internal/core"
	uiimpl "github.com/wandering-compiler/w17ctl/internal/ui"
)

// StartCmd starts (or re-opens) the background UI.
type StartCmd struct {
	Port   int  `name:"port" default:"7917" help:"Loopback port to serve the UI on (default 7917)."`
	NoOpen bool `name:"no-open" help:"Do not open a browser; just ensure the server is running."`

	// Foreground is the internal flag the launcher passes to the spawned
	// child: it runs the blocking server in this process instead of
	// daemonising again. Not for direct use.
	Foreground bool `name:"foreground" hidden:"" help:"Run the server in the foreground (internal: used by the background launcher)."`
}

func (c *StartCmd) Run() error {
	return uiimpl.Start(core.Stdout, c.Port, c.NoOpen, c.Foreground)
}
