// Package codegenversion wires `w17ctl codegen-version` — the lock's
// `codegen_version`, the codegen engine the project generates with.
package codegenversion

import (
	"fmt"

	"github.com/wandering-compiler/w17ctl/internal/core"
	codegenpb "github.com/wandering-compiler/sdk/go/pb/w17compiler"
)

// Cmd implements `w17ctl codegen-version <version>`.
//
// Like `sdk pin` it ships a LockEditIntent and writes back what the console
// returns: the lock is signed, and the pin's shape is checked server-side.
// `w17ctl update --all` sets it for you, to the minor of the SDK it pins; this
// is for the other two cases — an exact release candidate, and going back.
type Cmd struct {
	Version  string `arg:"" optional:"" name:"version" help:"A release minor (v0.1) — the project then gets that minor's newest patch — or an exact release candidate (v0.2.0-rc.3), which the console serves only to a project opted in on its page in the console (Codegen). Omit it with --clear."`
	Clear    bool   `name:"clear" help:"Remove the pin: the console generates with its newest release minor and says so on every run."`
	LockPath string `name:"lock" placeholder:"PATH" default:"w17/lock.yaml" help:"Path to the lock file."`
	Console  string `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console (owns the lock). Optional — falls back to the binary's compile-time default."`
}

func (c *Cmd) Run() error {
	switch {
	case c.Clear && c.Version != "":
		return fmt.Errorf("codegen-version: pass a version or --clear, not both")
	case !c.Clear && c.Version == "":
		return fmt.Errorf("codegen-version: name a version (v0.1, or v0.2.0-rc.3) — or --clear to remove the pin")
	}
	if err := core.EditLockOnDisk("codegen-version", c.Console, c.LockPath, &codegenpb.LockEditIntent{
		Intent: &codegenpb.LockEditIntent_SetCodegenVersion{
			SetCodegenVersion: &codegenpb.SetCodegenVersionIntent{Version: c.Version},
		},
	}); err != nil {
		return err
	}
	if c.Clear {
		fmt.Fprintf(core.Stdout, "codegen-version: cleared (lock re-signed — the console generates with its newest minor)\n")
		return nil
	}
	fmt.Fprintf(core.Stdout, "codegen-version: %s (lock re-signed — the next codegen generates with it)\n", c.Version)
	return nil
}
