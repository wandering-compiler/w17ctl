// Package core holds the cross-cutting infrastructure every w17ctl
// command relies on: project-root discovery, lock loading, the proto
// tree reader, console-address resolution + dialing, and the shared
// output writer. It is the bottom layer of the w17ctl architecture —
// the thin `cmd/<command>` packages and the `internal/<command>`
// implementation packages both import it, and it imports NONE of them
// (so the dependency graph stays acyclic).
//
// Test seams are exported package vars (e.g. FindProjectRootFn) that
// tests override to inject fakes without touching disk or the network.
package core

import (
	"context"
	"io"
	"os"
	"time"
)

// Stdout is the writer every command renders user-facing output to.
// Tests point it at a buffer to assert on output. The implementation
// packages take an io.Writer explicitly rather than reaching for this
// global; it exists for the thin cmd layer + transitional callers.
var Stdout io.Writer = os.Stdout

// Stderr is the twin for lines that are NOT the command's result — warnings a
// person should see without them landing in a pipe somebody is parsing. Same
// contract as Stdout: tests point it at a buffer.
var Stderr io.Writer = os.Stderr

// DefaultGenDir is the conventions-global default generated-code
// directory (`structure.md`): go.mod + generated stubs live under
// <root>/srcgo/ unless the lock overrides it.
const DefaultGenDir = "srcgo"

// SdkModuleBase / SrcgoModuleBase are the module-path PREFIXES of the two
// runtime modules a scaffolded w17 project touches. They diverge because
// the SDK is PUBLIC and srcgo is PRIVATE:
//
//   - SdkModuleBase + "/sdk/go" → the public SDK a generated project
//     REQUIRES (`github.com/wandering-compiler/sdk/go`). A published w17ctl
//     can retarget this via ldflags; the monorepo default already points at
//     the public path since sdk/go carries it.
//   - SrcgoModuleBase + "/srcgo" → the PRIVATE compiler monorepo. A
//     scaffolded project never requires it — this base only feeds the inert
//     co-dev `replace`/`go.work use` (and mergeGoModReplaces' stale-replace
//     filter), so it stays on the private org path forever.
var (
	SdkModuleBase   = "github.com/wandering-compiler"
	SrcgoModuleBase = "github.com/wandering-compiler/platform"
)

// SdkFloor is the sdk/go snapshot that went out with THIS w17ctl — stamped at
// publish time by scripts/publish-w17ctl.sh as a generated file, derived from
// the published sdk repo's HEAD commit. Empty in a local or co-dev build,
// which disables the check below rather than guessing.
//
// ⚠️ Derived from the COMMIT, not from the proxy's `@latest`. The first release
// to use this floor caught why: publish-sdk had already pushed the new
// snapshot, the proxy served that exact version's .info, and `@latest` still
// answered the previous day's — so a floor read from there would have been one
// version behind on the day it had to bite, and silently, because a floor that
// is too low refuses nothing and reads as working.
//
// It exists because a project's sdk/go pin and the code codegen writes are two
// independently moving things, and nothing compared them. A consumer upgraded
// to rc.52, kept a pin from five days earlier, and the generated gateway called
// `restgw.WriteGRPCErrorCtx` — a symbol that pin predates. `codegen` exited 0,
// `stack build` failed inside the build image with `undefined:`, and nothing
// tied the two together. It stayed silent for days because their CI compiles
// the authored tree, not the generated bundles (marb #81).
//
// ⚠️ A FLOOR, not a ceiling. The code being generated comes from the CONSOLE,
// which can be newer than the client; this only catches a pin older than the
// client itself. That is the case that actually bit, but a console ahead of
// w17ctl can still emit a symbol no pin this check accepts would carry —
// closing THAT needs the console to declare its own sdk/go version, which it
// does not today.
var SdkFloor = ""

// ClientCtx is the default per-call deadline for console RPCs.
func ClientCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 30*time.Second)
}
