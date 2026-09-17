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
	"bytes"
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
		// --force replaces the file with THIS binary's copy, which can be
		// older than what is already there.
		//
		// The guide is compiled in, so the version that wrote the existing
		// file is whatever w17ctl the author last ran — possibly a newer one,
		// on another machine or for another domain. A consumer lost advice
		// that way: their AGENTS.md carried lines their local binary had never
		// heard of, and --force replaced them silently. They restored it from
		// git, which is the only reason they noticed.
		//
		// Overwriting is still what --force means, so this does not refuse.
		// It says what is happening, because a one-way replacement the user
		// cannot see is the part that cost them.
		if existing, rerr := os.ReadFile(c.Out); rerr == nil && !bytes.Equal(existing, guideBody) {
			fmt.Fprintf(core.Stdout,
				"guide: replacing %s with this binary's copy (w17ctl %s).\n"+
					"  note: the guide ships INSIDE w17ctl, so a file written by a NEWER client loses\n"+
					"        whatever that version had to say. `git diff %s` before committing.\n",
				c.Out, core.Version, c.Out)
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
	// Fetch the server-generated platform reference.
	//
	// Whether an unreachable console is a note or an error depends on WHERE
	// this runs, because the same outcome means two different things:
	//
	//   - Outside a project — the documented first run, in an empty dir before
	//     `init` — there is no console to reach yet. AGENTS.md is written,
	//     which is what was asked for, so a note and exit 0 are honest.
	//   - Inside a project, the specs ARE the point. Reporting "skipped" and
	//     exiting 0 there made a failed fetch indistinguishable from an
	//     up-to-date one: it cost us a wrong conclusion while verifying a
	//     deploy (the reference looked stale when it had simply never
	//     arrived), and a CI step that cannot fail is the class of defect this
	//     project keeps finding. Same shape as a seed step that does nothing
	//     and reports success.
	root, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("guide: cwd: %w", err)
	}
	if err := codegen.GuideViaConsole(root, c.Console); err != nil {
		if _, rerr := core.FindProjectRoot(); rerr != nil {
			fmt.Fprintf(core.Stdout, "note: skipped w17/specs/ (the platform reference): %v\n", err)
			fmt.Fprintln(core.Stdout, "      read the error above rather than repeating the step: `w17ctl whoami --verify`")
			fmt.Fprintln(core.Stdout, "      says whether the credential is the problem. A refusal that survives a verified")
			fmt.Fprintln(core.Stdout, "      login is something else, and running `login` again will not change it.")
			return nil
		}
		return fmt.Errorf("guide: w17/specs/ (the platform reference) was not written: %w\n"+
			"  fix: check the console is reachable and you are logged in (`w17ctl whoami`),\n"+
			"       or pass --no-specs to write only AGENTS.md", err)
	}
	fmt.Fprintln(core.Stdout, "wrote w17/specs/ — the w17 platform reference (server-generated; read before designing)")
	return nil
}
