// Package sdk wires the `w17ctl sdk` command. It is a thin kong adapter:
// it parses flags and delegates to internal/sdkupdate, which holds the
// implementation. (cmd/<command>/cmd.go is the conventional home of a
// command package's root command.)
package sdk

import (
	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/sdkupdate"
)

// Cmd groups sdk subcommands. Today just `update`.
type Cmd struct {
	Update UpdateCmd `cmd:"" help:"Move this project onto a new public sdk/go release — rewrites the require + go.sum hashes in every module, including the hand-written one codegen won't touch."`
}

// UpdateCmd is `w17ctl sdk update`.
//
// Codegen pins the GENERATED modules, but it never rewrites the project's
// hand-written module (author-owned) and it resolves from the version the
// project already knows — so a project could not move onto a newer SDK at all.
// Doing it by hand needed go.sum hashes, i.e. a local Go toolchain, which a
// consumer is not required to have. This closes that loop over the module
// proxy alone.
type UpdateCmd struct {
	ProjectRoot string `arg:"" optional:"" default:"." help:"Project root. Defaults to cwd."`
	Version     string `name:"version" placeholder:"VERSION" help:"Pin this exact version (e.g. v0.0.0-20260716201145-36e33cc8168a). Default: the proxy's @latest."`
}

// Run delegates to the implementation, rendering progress to the shared
// output writer.
func (c *UpdateCmd) Run() error {
	return sdkupdate.Run(core.Stdout, c.ProjectRoot, c.Version)
}
