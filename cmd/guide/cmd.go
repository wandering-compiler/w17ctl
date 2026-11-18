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
	"path/filepath"
	"strings"

	"github.com/wandering-compiler/w17ctl/internal/authstore"
	"github.com/wandering-compiler/w17ctl/internal/codegen"
	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/guidestamp"
)

//go:embed guide.md
var guideBody []byte

// Cmd writes AGENTS.md (the usage guide) + CLAUDE.md (which imports it) and
// fetches w17/specs/ (the platform reference). Idempotent: refuses to clobber
// AGENTS.md unless --force (the server-generated w17/specs/ is always
// refreshed), and never touches an existing CLAUDE.md.
//
// The flags live on the parent so a bare `w17ctl guide --force` keeps working;
// the write itself is the hidden default subcommand, because kong runs the Run
// of EVERY node on the selected path — a Run here would also fire under
// `guide topic`.
type Cmd struct {
	Out     string `name:"out" short:"o" default:"AGENTS.md" placeholder:"FILE" help:"File to write the usage guide to (default AGENTS.md). CLAUDE.md beside it imports it (@AGENTS.md) — Claude Code reads CLAUDE.md, other agents read AGENTS.md."`
	Stdout  bool   `name:"stdout" help:"Print the usage guide to stdout instead of writing files."`
	Force   bool   `name:"force" short:"f" help:"Overwrite FILE if it already exists (CLAUDE.md is only ever created, never overwritten)."`
	Console string `name:"console" env:"W17_CONSOLE_ADDR" help:"Console address for fetching the w17/specs/ platform reference (defaults to the logged-in console)."`
	NoSpecs bool   `name:"no-specs" help:"Write only AGENTS.md (+ CLAUDE.md); skip fetching the server-generated w17/specs/ platform reference."`

	Files  writeCmd  `cmd:"" name:"write" default:"1" hidden:"" help:"Write AGENTS.md + CLAUDE.md and fetch w17/specs/ — what a bare 'w17ctl guide' does."`
	Topic  TopicCmd  `cmd:"" help:"Print one topic of the platform reference by its id (from w17/specs/INDEX.md). Offline: reads the project's w17/specs/."`
	Topics TopicsCmd `cmd:"" help:"List the topics of the platform reference whose INDEX line matches QUERY (case-insensitive substring; all when empty). Offline."`
}

// writeCmd is the default subcommand: a bare `w17ctl guide [flags]`.
type writeCmd struct{}

// Run writes the guide with the parent's flags (kong binds ancestors).
func (writeCmd) Run(c *Cmd) error { return c.Write() }

// claudeFile is what makes Claude Code load the guide: it reads
// CLAUDE.md, not AGENTS.md, and follows `@<file>` imports.
const claudeFile = "CLAUDE.md"

// Write is `w17ctl guide`: AGENTS.md, CLAUDE.md, then w17/specs/.
func (c *Cmd) Write() error {
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
		// on another machine or for another domain. Advice is lost that way:
		// an AGENTS.md carrying lines the local binary has never heard of,
		// replaced silently by --force.
		//
		// Overwriting is still what --force means, so this does not refuse.
		// It says what is happening, because a one-way replacement the user
		// cannot see is the part that cost them.
		//
		// Now by STAMP rather than by byte-compare. The compare could only say
		// "these differ", which is true of an upgrade and of a downgrade alike
		// — so the warning fired on every refresh and named neither case. The
		// stamp gives direction, so a downgrade can be called a downgrade and
		// an ordinary refresh can stop crying wolf.
		if existing, rerr := os.ReadFile(c.Out); rerr == nil {
			switch rel, was, mine := guidestamp.Staleness(existing); rel {
			case guidestamp.Newer:
				fmt.Fprintf(core.Stdout,
					"guide: ⚠ %s was written by w17ctl %s and you are running %s — this is a "+
						"DOWNGRADE.\n"+
						"  The guide ships INSIDE w17ctl, so whatever %s had to say is lost and "+
						"cannot be fetched back.\n"+
						"  `git diff %s` before committing, or upgrade first (`w17ctl update`).\n",
					c.Out, was, mine, was, c.Out)
			case guidestamp.Unknown:
				// No stamp (written before stamps existed), or a dev build on
				// one side. Say only what is true: something is being replaced
				// and the direction is not knowable.
				if !bytes.Equal(existing, guidestamp.Stamp(guideBody)) {
					fmt.Fprintf(core.Stdout,
						"guide: replacing %s with this binary's copy (w17ctl %s).\n"+
							"  note: the existing file carries no version stamp, so whether this is a "+
							"refresh or a\n"+
							"        downgrade cannot be told. `git diff %s` before committing.\n",
						c.Out, mine, c.Out)
				}
			}
		}
	} else if !os.IsNotExist(statErr) {
		return fmt.Errorf("guide: stat %s: %w", c.Out, statErr)
	}
	if err := os.WriteFile(c.Out, guidestamp.Stamp(guideBody), 0o644); err != nil {
		return fmt.Errorf("guide: write %s: %w", c.Out, err)
	}
	fmt.Fprintf(core.Stdout, "wrote %s — the w17ctl usage guide for coding agents\n", c.Out)
	if err := c.ensureClaudeImport(); err != nil {
		return err
	}

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
		loggedIn := hasCredential(c.Console)
		if _, rerr := core.FindProjectRoot(); rerr != nil {
			fmt.Fprintf(core.Stdout, "note: skipped w17/specs/ (the platform reference): %v\n", err)
			if !loggedIn {
				fmt.Fprintln(core.Stdout, "      not logged in — ask the human to run `w17ctl login`; an agent cannot complete it.")
				return nil
			}
			fmt.Fprintln(core.Stdout, "      read the error above rather than repeating the step: `w17ctl whoami --verify`")
			fmt.Fprintln(core.Stdout, "      says whether the credential is the problem. A refusal that survives a verified")
			fmt.Fprintln(core.Stdout, "      login is something else, and running `login` again will not change it.")
			return nil
		}
		if !loggedIn {
			return fmt.Errorf("guide: w17/specs/ (the platform reference) was not written: %w\n"+
				"  not logged in — ask the human to run `w17ctl login`; an agent cannot complete it\n"+
				"  (`w17ctl whoami` shows the stored login), or pass --no-specs to write only AGENTS.md", err)
		}
		return fmt.Errorf("guide: w17/specs/ (the platform reference) was not written: %w\n"+
			"  fix: check the console is reachable and you are logged in (`w17ctl whoami`),\n"+
			"       or pass --no-specs to write only AGENTS.md", err)
	}
	fmt.Fprintln(core.Stdout, "wrote w17/specs/ — the w17 platform reference (server-generated; read before designing)")
	return nil
}

// ensureClaudeImport makes Claude Code load the guide. It reads CLAUDE.md, not
// AGENTS.md, so without this the file `guide` writes is invisible to it. An
// existing CLAUDE.md is the author's and is never rewritten; when it does not
// import the guide, one line says what to add.
func (c *Cmd) ensureClaudeImport() error {
	base := filepath.Base(c.Out)
	path := filepath.Join(filepath.Dir(c.Out), claudeFile)
	importLine := "@" + base
	existing, err := os.ReadFile(path)
	switch {
	case err == nil:
		if !strings.Contains(string(existing), importLine) {
			fmt.Fprintf(core.Stdout, "note: %s exists and does not import the guide — add a line `%s` so Claude Code loads it\n", path, importLine)
		}
		return nil
	case !os.IsNotExist(err):
		return fmt.Errorf("guide: read %s: %w", path, err)
	}
	body := "<!-- Claude Code reads this file; the project guide is " + base + " (written by `w17ctl guide`). -->\n" + importLine + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return fmt.Errorf("guide: write %s: %w", path, err)
	}
	fmt.Fprintf(core.Stdout, "wrote %s — imports %s for Claude Code\n", path, base)
	return nil
}

// hasCredential reports whether a command would present any credential to the
// console: a machine token, or a stored login for the console it resolves to.
// It decides the wording of a failed specs fetch — "log in" is the wrong
// advice for a refused login and the only advice when there is none.
func hasCredential(console string) bool {
	if core.EnvTokenIsSet() {
		return true
	}
	addr, err := core.ResolveConsoleAddr(console)
	if err != nil || addr == "" {
		return false
	}
	st, err := authstore.LoadDefault()
	if err != nil {
		return false
	}
	inst := core.LookupInstance(st, addr)
	return inst != nil && inst.Token != ""
}
