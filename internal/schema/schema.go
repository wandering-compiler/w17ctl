// Package schema is the thin client's IR-compile seam: it gathers a
// project's proto file set + entry roots and calls the console's CompileIR
// RPC (thin-client refactor Step 3b — the loader + ir.Build pipeline +
// plugin staging all run server-side). The client only reads local files;
// the ACL permission catalogue is resolved server-side from the uploaded
// committed ACL lock.
//
// Shared by the schema/migrate/push commands + `stack build` (dev diff
// base) + initiative push (snapshot IR). The IR is handled as OPAQUE bytes
// throughout — the client never decodes it (public-split Block 3: it drops
// srcgo/pb/common/ir). LoadIRBytes is a package var so tests inject a fake IR
// blob without a live console.
package schema

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/wandering-compiler/w17ctl/internal/core"
	codegenpb "github.com/wandering-compiler/sdk/go/pb/w17compiler"
)

// LoadIRBytes is the package var production leaves at DefaultLoadIRBytes; tests
// override it to inject a fake IR blob (no live console). It returns the
// compiled IR as OPAQUE bytes — the shape the push path ships straight to the
// public ProjectRegistry.PushSchema (it never decodes the IR client-side).
//
// console is the caller's explicit --console value (empty = fall back to the
// login/compiled-in default via ResolveConsoleAddr). It MUST be threaded: the
// IR compile dials the console just like the push that follows it, so a caller
// that only passes --console (no login, no W17_CONSOLE_ADDR) — e.g. the
// pipeline e2e — would otherwise fail this step with "no console configured"
// while the push step, which does honour --console, was fine.
var LoadIRBytes = DefaultLoadIRBytes

// LoadIRBytesWithDescriptors is the package var production leaves at
// DefaultLoadIRBytesWithDescriptors; tests override it. Same as LoadIRBytes
// but the IR carries a FileDescriptorSet even for relational schemas — the
// compat engine's WIRE + API domains need it (snapshot push, the compat input).
var LoadIRBytesWithDescriptors = DefaultLoadIRBytesWithDescriptors

// DefaultLoadIRBytes builds the project IR via CompileIR (no descriptors) and
// returns the raw marshalled bytes — the client never decodes them. Consumers
// forward the bytes verbatim (push → ProjectRegistry.PushSchema; stack dev
// diff → CodegenService.Plan + checkpoint; fixtures → RenderFixtureSeed). The
// proto dir + default connection are resolved server-side via DescribeLock —
// no client-side lock parsing.
func DefaultLoadIRBytes(ctx context.Context, paths, imports []string, console string) ([]byte, error) {
	return compileIRBytesViaConsole(ctx, paths, imports, console, false)
}

// DefaultLoadIRBytesWithDescriptors is DefaultLoadIRBytes with a
// FileDescriptorSet attached (snapshot push / compat input).
func DefaultLoadIRBytesWithDescriptors(ctx context.Context, paths, imports []string, console string) ([]byte, error) {
	return compileIRBytesViaConsole(ctx, paths, imports, console, true)
}

// compileIRBytesViaConsole gathers the project's proto file set + the entry
// roots and calls the console's CompileIR, returning the raw marshalled IR
// bytes. The loader + ir.Build + plugin staging run server-side; the client
// only reads local files. The console endpoint is the same one codegen dials
// (--console / W17_CONSOLE_ADDR, else the logged-in console, else the
// compiled-in default — core.ResolveConsoleAddr; the lock's w17_url is not
// a source).
func compileIRBytesViaConsole(ctx context.Context, paths, imports []string, console string, includeDescriptors bool) ([]byte, error) {
	if len(paths) == 0 {
		return nil, fmt.Errorf("no --proto paths given")
	}
	root, err := core.FindProjectRoot()
	if err != nil {
		return nil, err
	}

	addr, err := core.ResolveConsoleAddr(console)
	if err != nil {
		return nil, err
	}
	cl, conn, err := core.DialCodegen(addr)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()

	// Resolve the proto dir + default connection server-side (DescribeLock) —
	// no client-side lock parsing. The lock is read from disk as opaque bytes
	// and handed to the console, which applies the Effective* defaults. Done on
	// the same CodegenService conn the CompileIR below uses.
	lockBytes, err := os.ReadFile(filepath.Join(root, "w17", "lock.yaml"))
	if err != nil {
		return nil, fmt.Errorf("read lock: %w", err)
	}
	view, err := cl.DescribeLock(ctx, &codegenpb.DescribeLockRequest{Lock: lockBytes})
	if err != nil {
		return nil, fmt.Errorf("describe lock: %w", err)
	}
	protoDir := view.GetProtoDir()
	protoRoot := filepath.Join(root, protoDir)
	// Resolve symlinks so the entry-path → proto-root relativization below
	// is robust to a symlinked checkout (e.g. macOS /var → /private/var).
	if resolved, rerr := filepath.EvalSymlinks(protoRoot); rerr == nil {
		protoRoot = resolved
	}

	// Upload the whole project proto tree (model + service + types + plugin
	// trees). The server resolves the w17 vocabulary from its own proto
	// root, so it's deliberately not sent.
	files, err := core.ReadProtoTree(root, protoDir)
	if err != nil {
		return nil, err
	}

	// roots = the entry --proto files as wire names (relative to the proto
	// root) — the server loads only these + their transitive imports, so a
	// subset push builds exactly its reachable tables.
	roots := make([]string, 0, len(paths))
	for _, p := range paths {
		abs, aerr := filepath.Abs(p)
		if aerr != nil {
			return nil, fmt.Errorf("resolve %s: %w", p, aerr)
		}
		if _, serr := os.Stat(abs); serr != nil {
			return nil, fmt.Errorf("%s: %w", p, serr)
		}
		if resolved, rerr := filepath.EvalSymlinks(abs); rerr == nil {
			abs = resolved
		}
		rel, rerr := filepath.Rel(protoRoot, abs)
		if rerr != nil || strings.HasPrefix(rel, "..") {
			return nil, fmt.Errorf("proto %s is not under the project proto dir %s — keep model protos under the proto tree", p, protoRoot)
		}
		roots = append(roots, filepath.ToSlash(rel))
	}

	// Plugin-activation staging needs a Go pb output root (to expand the
	// plugin protos' `@project`/`@pkg` placeholders) + the consuming
	// project's go_module. The full `codegen` path derives stub_targets from
	// the lock server-side; this IR-compile path threads the equivalent: the
	// lock-derived plugin pb root from the view (the same value goPbRoot picks
	// over codegen's stub_targets) + go_module read from the local go.mod (as
	// the codegen path does), under the conventions-global gen dir. Harmless
	// for plugin-free projects — the server only consults these when a domain
	// actually activates a plugin.
	goModule := core.ReadGoModule(root, core.DefaultGenDir)
	req := &codegenpb.CompileIRRequest{
		Files:              files,
		Roots:              roots,
		DefaultConnection:  view.GetDefaultConnection(),
		IncludeDescriptors: includeDescriptors,
		GoModule:           goModule,
		GenDir:             core.DefaultGenDir,
	}
	if pbRoot := view.GetPluginPbRoot(); pbRoot != "" {
		req.StubTargets = []*codegenpb.StubTarget{{Language: "go", OutputRoot: pbRoot}}
	}
	// The ACL permission catalogue is resolved server-side from the
	// uploaded committed ACL lock proto (the client just ships the proto
	// tree — no acllock parsing client-side).
	resp, err := cl.CompileIR(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("compile IR: %w", err)
	}
	return resp.GetSchema(), nil
}
