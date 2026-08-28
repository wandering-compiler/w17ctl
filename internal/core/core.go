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
	SrcgoModuleBase = "github.com/MrS1lentcz/wandering-compiler"
)

// ClientCtx is the default per-call deadline for console RPCs.
func ClientCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 30*time.Second)
}
