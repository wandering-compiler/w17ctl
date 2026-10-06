// Package codegen wires `w17ctl codegen` — a thin kong adapter over the
// codegen orchestration in internal/codegen.
package codegen

import (
	codegenimpl "github.com/wandering-compiler/w17ctl/internal/codegen"
	"github.com/wandering-compiler/w17ctl/internal/core"
)

// Cmd generates ALL derived code: the Storage-layer Go gRPC handlers +
// the ACL / eventbus / MCP bundles + the FE clients + deploy artifacts.
// It walks parent dirs to the project root, reads w17/lock.yaml, has the
// console place the run on the codegen cluster, uploads every .proto under
// proto/ to the granted worker, and writes the returned files under
// <root>/<gen_dir>/ plus the lock the console signed.
type Cmd struct {
	Console        string `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console CodegenService. Optional — falls back to console_addr in w17/lock.yaml, then to the binary's compile-time default."`
	Force          bool   `name:"force" help:"Overwrite existing files at the target paths. Default: error if a target file already exists."`
	Gofmt          string `name:"gofmt" enum:"embedded,pinned" default:"embedded" help:"Which formatter shapes the generated Go. embedded (default): the one built into w17ctl — no dependencies, identical for everyone on the same w17ctl. pinned: the Go version this project's go.mod declares, via a local toolchain of exactly that version or a docker image of it; refuses if neither is available rather than quietly using a different one."`
	AdoptGitignore bool   `name:"adopt-gitignore" help:"Create w17/.gitignore if this project has none, so the compiler output stops showing up in your diffs. One-time and explicit: codegen never creates it on its own, because a project may be tracking some of the generated tree on purpose."`
	Retries        int    `name:"retries" default:"1" help:"How many times a run that loses its connection to the codegen worker mid-run is placed and run again. A refusal of the input is never retried, and nothing is retried after the run completes; 0 disables re-runs."`
	NoUpdateCheck  bool   `name:"no-update-check" help:"Skip the check for a newer w17ctl / sdk/go / plugin that runs first at a terminal. CI never runs it; this is for a terminal session that should not be asked."`
}

// PreGenerate is the update check run before generating — `w17ctl update`'s
// (cmd/update.PreGenerate), wired by the root command. A seam because the
// update command reaches the plugin commands, which already import this one.
var PreGenerate func(console, root string) error

func (c *Cmd) Run() error {
	if !c.NoUpdateCheck && PreGenerate != nil {
		if root, err := core.FindProjectRoot(); err == nil {
			if err := PreGenerate(c.Console, root); err != nil {
				return err
			}
		}
	}
	return codegenimpl.Run(c.Console, c.Force, c.AdoptGitignore, c.Gofmt, c.Retries)
}
