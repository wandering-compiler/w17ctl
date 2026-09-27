// Package guidestamp records and reads the w17ctl version that wrote a
// project's AGENTS.md.
//
// A LEAF package on purpose. Two callers need it and they sit on opposite sides
// of an existing dependency: `cmd/guide` writes the file, and
// `internal/codegen` is where a stale one has to be NOTICED (it is the command
// the adoption loop runs). `cmd/guide` already imports `internal/codegen` for
// the specs fetch, so putting this in either of them would close a cycle.
package guidestamp

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/pluginfetch"
)

// The version marker AGENTS.md carries, and the only way anything can tell a
// stale guide from a current one.
//
// # Why this is needed at all
//
// A w17 project has THREE guide artefacts and only two of them refresh
// themselves:
//
//	w17/specs/*      the platform reference   server-generated → every codegen
//	w17/AGENTS.md    the project map          server-generated → every codegen
//	AGENTS.md        w17ctl's own commands    COMPILED IN      → nothing
//
// The third is the one that documents what an upgrade actually changes — the
// CLI surface — and it is the one nothing refreshed. `w17ctl update` swaps the
// binary and says nothing about it; `w17ctl guide` refuses to overwrite an
// existing file without --force; and `codegen`, which contacts the console and
// refreshes the other two anyway, had no way to ask how old this one was.
//
// So an adopter upgrades, reads AGENTS.md, and is told how a client they no
// longer have used to work. Nothing anywhere says so.
//
// # Why a stamp rather than a byte-compare
//
// --force already compared bytes, and a byte-compare can only say "these
// differ". DIRECTION is what matters: replacing a NEWER file with an older
// binary's copy destroys advice, and that has happened — a consumer's AGENTS.md
// carried lines their local binary had never heard of, --force replaced them
// silently, and they noticed only because the file was in git.
//
// # Shape
//
// An HTML comment on the FIRST line: invisible in every rendered view, inert to
// the coding agents that read this file, and `head -1` answers "which client
// wrote this" without parsing anything. First rather than last so a truncated
// file still carries it.
const (
	prefix = "<!-- w17ctl-guide "
	suffix = " -->"
)

// Stamp returns the guide body with the writing client's version recorded
// on its first line.
//
// A dev build stamps "dev", honestly, rather than inventing a number — the same
// rule core.VersionString already applies, and for the same reason: a made-up
// version reads as a release nobody can find. "dev" is deliberately NOT
// comparable, which Staleness below reports rather than guesses at.
func Stamp(body []byte) []byte {
	v := core.Version
	if v == "" {
		v = "dev"
	}
	return append([]byte(prefix+v+suffix+"\n"), body...)
}

// VersionOf reads the stamp back, or "" when the file carries none —
// which is every AGENTS.md written before this existed.
func VersionOf(body []byte) string {
	line := body
	if i := bytes.IndexByte(body, '\n'); i >= 0 {
		line = body[:i]
	}
	s := strings.TrimSpace(string(line))
	if !strings.HasPrefix(s, prefix) || !strings.HasSuffix(s, suffix) {
		return ""
	}
	return strings.TrimSpace(s[len(prefix) : len(s)-len(suffix)])
}

// Relation is how the AGENTS.md on disk stands to the running binary's
// copy.
type Relation int

const (
	// Unknown — no stamp, or either side is a dev build. Nothing can be
	// concluded, and saying so is the point: the alternative is a confident
	// wrong answer about whether somebody is about to lose advice.
	Unknown Relation = iota
	// Current — written by this exact version.
	Current
	// Stale — written by an OLDER client. This is the case the adopter
	// needs told: the file describes a client they no longer have.
	Stale
	// Newer — written by a NEWER client. Refreshing from here is a
	// DOWNGRADE that loses whatever that version had to say.
	Newer
)

// Staleness compares the stamp on disk with the running binary.
//
// Version ordering comes from pluginfetch.CompareVersions rather than a second
// implementation here. It is not a plugin concern despite where it lives: it is
// an ordering over OUR OWN tag format with prereleases SELECTED, which is what
// w17ctl's own versions are (every release is an -rc.N) and what semver's rule
// would get backwards. A second comparator is how two answers drift without
// ever disagreeing loudly.
func Staleness(onDisk []byte) (Relation, string, string) {
	was := VersionOf(onDisk)
	mine := core.Version
	if mine == "" {
		mine = "dev"
	}
	if was == "" || was == "dev" || mine == "dev" {
		return Unknown, was, mine
	}
	switch c := pluginfetch.CompareVersions(was, mine); {
	case c == 0:
		return Current, was, mine
	case c < 0:
		return Stale, was, mine
	default:
		return Newer, was, mine
	}
}

// StaleNotice returns the one line to print when a project's AGENTS.md was
// written by an older client, or "" when there is nothing to say.
//
// The place this is SAID is not the place the file is WRITTEN: `codegen`
// is the command the adoption loop runs, it already refreshes the other two
// guide artefacts, and it is therefore where a stale third one is noticed at no
// extra cost. `update` cannot do the comparison at all — it IS the old binary,
// so the newer guide it is about to install is not in it.
//
// Silent on Newer: that is not the adopter's problem to act on, and
// `guide --force` is where it matters because that is the command that would
// destroy it.
func StaleNotice(agentsMD []byte) string {
	rel, was, mine := Staleness(agentsMD)
	if rel != Stale {
		return ""
	}
	return fmt.Sprintf(
		"note: AGENTS.md was written by w17ctl %s and you are running %s — "+
			"`w17ctl guide --force` to refresh it.\n"+
			"      It ships inside the client, so it is the one guide a codegen "+
			"cannot bring up to date.", was, mine)
}
