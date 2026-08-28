// Package codegen wires `w17ctl codegen` — a thin kong adapter over the
// codegen orchestration in internal/codegen.
package codegen

import (
	codegenimpl "github.com/wandering-compiler/w17ctl/internal/codegen"
)

// Cmd generates ALL derived code: the Storage-layer Go gRPC handlers +
// the ACL / eventbus / MCP bundles + the FE clients + deploy artifacts.
// It walks parent dirs to the project root, reads w17/lock.yaml, uploads
// every .proto under proto/ to the console, and writes the returned
// files under <root>/<gen_dir>/.
type Cmd struct {
	Console string `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console CodegenService. Optional — falls back to console_addr in w17/lock.yaml, then to the binary's compile-time default."`
	Force   bool   `name:"force" help:"Overwrite existing files at the target paths. Default: error if a target file already exists."`
}

func (c *Cmd) Run() error {
	return codegenimpl.Run(c.Console, c.Force)
}
