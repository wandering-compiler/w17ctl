// Package verify wires the `w17ctl verify` command. It is a thin
// kong adapter: it parses flags and delegates to internal/verify, which
// holds the implementation. (cmd/<command>/cmd.go is the conventional
// home of a command package's root command.)
package verify

import (
	"github.com/wandering-compiler/w17ctl/internal/core"
	verifyimpl "github.com/wandering-compiler/w17ctl/internal/verify"
)

// Cmd is `w17ctl verify` — the unified drift hook paired with
// `w17ctl codegen`. It recomputes the committed generated locks (ACL +
// eventbus) from the current proto and fails (non-zero exit) on any
// drift, without running a full codegen. Intended as a CI step
// asserting nobody changed a .proto without regenerating + committing
// the locks.
type Cmd struct {
	Console string `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console CodegenService. Optional — falls back to console_addr in w17/lock.yaml, then the binary's compile-time default."`
}

// Run delegates to the implementation, rendering progress to the shared
// output writer.
func (c *Cmd) Run() error {
	return verifyimpl.Run(core.Stdout, c.Console)
}
