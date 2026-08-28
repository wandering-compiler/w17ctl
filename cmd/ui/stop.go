package ui

import (
	"github.com/wandering-compiler/w17ctl/internal/core"
	uiimpl "github.com/wandering-compiler/w17ctl/internal/ui"
)

// StopCmd stops the background UI.
type StopCmd struct{}

func (c *StopCmd) Run() error {
	return uiimpl.Stop(core.Stdout)
}
