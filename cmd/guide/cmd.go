// Package guide writes AGENTS.md — the AI-agent USAGE guide for driving w17ctl
// — and fetches the w17 PLATFORM reference (w17/specs/*) from the console.
//
// The split is deliberate: AGENTS.md describes w17ctl's OWN commands, so it is
// client-embedded (only the client authoritatively knows its command surface,
// and it works offline before login). The platform reference (types,
// annotations, generation model, event system) describes the COMPILER's output,
// so it is generated server-side and fetched — an old client never ships an
// outdated design; when codegen changes, w17/specs/ changes with it.
package guide

import (
	_ "embed"
	"fmt"
	"os"

	"github.com/wandering-compiler/w17ctl/internal/codegen"
	"github.com/wandering-compiler/w17ctl/internal/core"
)

//go:embed guide.md
var guideBody []byte

// Cmd writes AGENTS.md (the usage guide) + fetches w17/specs/ (the platform
// reference). AGENTS.md is the cross-tool convention coding agents read into
// context automatically. Idempotent: refuses to clobber AGENTS.md unless
// --force (the server-generated w17/specs/ is always refreshed).
type Cmd struct {
	Out     string `name:"out" short:"o" default:"AGENTS.md" placeholder:"FILE" help:"File to write the usage guide to (default AGENTS.md — the convention coding agents read automatically)."`
	Stdout  bool   `name:"stdout" help:"Print the usage guide to stdout instead of writing files."`
	Force   bool   `name:"force" short:"f" help:"Overwrite FILE if it already exists."`
	Console string `name:"console" env:"W17_CONSOLE_ADDR" help:"Console address for fetching the w17/specs/ platform reference (defaults to the compiled-in console)."`
	NoSpecs bool   `name:"no-specs" help:"Write only AGENTS.md; skip fetching the server-generated w17/specs/ platform reference."`
}

func (c *Cmd) Run() error {
	if c.Stdout {
		_, err := core.Stdout.Write(guideBody)
		return err
	}
	if _, statErr := os.Stat(c.Out); statErr == nil {
		if !c.Force {
			return fmt.Errorf("guide: %s already exists — pass --force to refresh, or --stdout to print", c.Out)
		}
	} else if !os.IsNotExist(statErr) {
		return fmt.Errorf("guide: stat %s: %w", c.Out, statErr)
	}
	if err := os.WriteFile(c.Out, guideBody, 0o644); err != nil {
		return fmt.Errorf("guide: write %s: %w", c.Out, err)
	}
	fmt.Fprintf(core.Stdout, "wrote %s — the w17ctl usage guide for coding agents\n", c.Out)

	if c.NoSpecs {
		return nil
	}
	// Fetch the server-generated platform reference. Best-effort: AGENTS.md is
	// already written, and the specs need a reachable console (+ login), which
	// a fresh empty-dir first run may not have yet.
	root, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("guide: cwd: %w", err)
	}
	if err := codegen.GuideViaConsole(root, c.Console); err != nil {
		fmt.Fprintf(core.Stdout, "note: skipped w17/specs/ (the platform reference): %v\n", err)
		fmt.Fprintln(core.Stdout, "      run `w17ctl guide` again with a reachable console (after `w17ctl login`) to write it.")
		return nil
	}
	fmt.Fprintln(core.Stdout, "wrote w17/specs/ — the w17 platform reference (server-generated; read before designing)")
	return nil
}
