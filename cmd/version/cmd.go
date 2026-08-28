// Package version implements `w17ctl version`.
package version

import (
	"fmt"
	"runtime"

	"github.com/wandering-compiler/w17ctl/internal/core"
)

// Cmd prints this binary's identity.
//
// It also prints the COMPILED-IN console address, which is not decoration: a
// client reaching the wrong console is one of the two ways a w17 setup goes
// wrong silently, and the resolution order (flag > logged-in console > this
// default) means the compiled value is the one nobody can see any other way.
type Cmd struct{}

func (c *Cmd) Run() error {
	fmt.Printf("w17ctl %s\n", core.VersionString())
	if core.BuildDate != "" {
		fmt.Printf("  built:   %s\n", core.BuildDate)
	}
	fmt.Printf("  go:      %s %s/%s\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)
	def := core.DefaultConsoleAddr
	if def == "" {
		def = "(none compiled in — pass --console or log in)"
	}
	fmt.Printf("  console: %s (compiled-in default; a logged-in console wins over it)\n", def)
	return nil
}
