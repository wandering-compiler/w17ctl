package codegen

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/wandering-compiler/w17ctl/internal/core"
	codegenpb "github.com/wandering-compiler/sdk/go/pb/w17compiler"
	"github.com/wandering-compiler/sdk/go/tooling/pathguard"
)

// generatedFileWriter returns the per-file sink for a generator RPC's server
// stream: it writes each file under the project root as it arrives (their
// relative paths are project-rooted, e.g. `w17/services/...` for a bundle,
// `proto/domains/.../w17.lock.events.proto` for a lock). The executable bit is
// inferred from a .sh suffix. With `announce` each path is printed as it lands;
// without it the caller summarises the set itself (the specs refresh that rides
// along with codegen, whose own output is already long).
//
// It is a SINK, not a collector, on purpose: the generator RPCs stream so that
// no single gRPC message carries a project's whole output (core.RecvGeneratedFiles
// explains the cap this removes). Buffering the stream here to write it
// afterwards would put that limit straight back, on the client's side of the
// wire.
func generatedFileWriter(root string, announce bool) func(*codegenpb.GeneratedFile) error {
	return func(f *codegenpb.GeneratedFile) error {
		// The relative path is SERVER-SUPPLIED. Route it through the shared
		// containment guard (same boundary applyWriteOps enforces) so a
		// compromised/buggy console cannot escape the project root via a `..`
		// traversal or an absolute path — which, combined with the .sh→0755
		// bit below, would otherwise let it drop an executable anywhere.
		dst, err := pathguard.Join(root, f.GetRelativePath())
		if err != nil {
			return fmt.Errorf("server file path %q escapes the project root: %w", f.GetRelativePath(), err)
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", filepath.Dir(dst), err)
		}
		mode := os.FileMode(0o644)
		if filepath.Ext(dst) == ".sh" {
			mode = 0o755
		}
		if err := os.WriteFile(dst, f.GetContents(), mode); err != nil {
			return fmt.Errorf("write %s: %w", dst, err)
		}
		if announce {
			fmt.Fprintf(core.Stdout, "wrote %s\n", f.GetRelativePath())
		}
		return nil
	}
}

// GenerateClientViaConsole renders the FE client tree(s) on the console's
// GenerateClient RPC (seam-D, thin-client refactor Step 3a) and writes
// them under each client's output_root. Uploads the proto tree + the
// signed lock (the generator reads generated_code.clients[] from it).
func GenerateClientViaConsole(root, console, protoDir, lockPath string) error {
	files, err := core.ReadProtoTree(root, protoDir)
	if err != nil {
		return err
	}
	lockBytes, err := os.ReadFile(lockPath)
	if err != nil {
		return fmt.Errorf("read lock: %w", err)
	}
	addr, err := core.ResolveConsoleAddr(console)
	if err != nil {
		return err
	}
	cl, conn, err := core.DialCodegen(addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	// The languages dir comes from the console's lock projection (§8.2 — the
	// client holds no lock types).
	dctx, dcancel := core.ClientCtx()
	view, err := cl.DescribeLock(dctx, &codegenpb.DescribeLockRequest{Lock: lockBytes})
	dcancel()
	if err != nil {
		return fmt.Errorf("describe lock: %w", err)
	}

	// Plugin-activated projects need the module path server-side so the FE
	// generator can expand plugin proto placeholders (the server stages
	// only protos, not the project go.mod).
	goModule := core.ReadGoModule(root, core.DefaultGenDir)
	// The on-disk .po catalogs (already merged) ride along so the FE generator
	// bakes a populated i18n.ts — it stages only protos+lock otherwise.
	poFiles := readExistingPo(root, view.GetLanguagesDir())
	ctx, cancel := core.ClientCtx()
	defer cancel()
	stream, err := cl.GenerateClient(ctx, &codegenpb.GenerateClientRequest{Files: files, Lock: lockBytes, GoModule: goModule, PoFiles: poFiles})
	if err != nil {
		return err
	}
	_, err = core.RecvGeneratedFiles(stream, generatedFileWriter(root, true))
	return err
}

// GuideViaConsole fetches the w17 PLATFORM self-description (w17/specs/*) from
// the console's Guide RPC and writes it under the project root. The content is
// generated SERVER-SIDE from the compiler's own vocabulary, so it always
// matches the running compiler — a thin client of any version gets a current
// description. Takes no project input.
func GuideViaConsole(root, console string) error {
	addr, err := core.ResolveConsoleAddr(console)
	if err != nil {
		return err
	}
	cl, conn, err := core.DialCodegen(addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	ctx, cancel := core.ClientCtx()
	defer cancel()
	stream, err := cl.Guide(ctx, &codegenpb.GuideRequest{})
	if err != nil {
		return err
	}
	_, err = core.RecvGeneratedFiles(stream, generatedFileWriter(root, true))
	return err
}

// refreshPlatformSpecs re-fetches w17/specs/* over an ALREADY-DIALED codegen
// client and returns how many files it wrote.
//
// It exists so `w17ctl codegen` can carry the refresh, not just
// `w17ctl guide`. The reference describes the COMPILER's output, so it is
// stale exactly when the compiler has moved — and the loop that moves with
// the compiler is the adoption loop, which runs codegen and has no reason to
// re-run guide. A consumer read an old copy, concluded a shipped annotation
// did not exist, and went looking for a gap that had already been closed
// (deinvo, 2026-07-28). Refreshing where the compiler is contacted anyway
// costs one RPC and removes the drift entirely.
func refreshPlatformSpecs(cl codegenpb.CodegenServiceClient, root string) (int, error) {
	ctx, cancel := core.ClientCtx()
	defer cancel()
	stream, err := cl.Guide(ctx, &codegenpb.GuideRequest{})
	if err != nil {
		return 0, err
	}
	return core.RecvGeneratedFiles(stream, generatedFileWriter(root, false))
}

// readGenGoFiles gathers the generated Go files under
// `<root>/<genDir>/domains/` (excluding tests) as upload payload for the
// project-map's grpc-client edge derivation. Best-effort: a missing tree
// yields nil (the map degrades to proto-only edges). Filenames are
// project-root-relative (the server stages them at that path).
func readGenGoFiles(root, genDir string) []*codegenpb.ProtoFile {
	base := filepath.Join(root, genDir, "domains")
	var out []*codegenpb.ProtoFile
	_ = filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		body, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return nil
		}
		out = append(out, &codegenpb.ProtoFile{Filename: filepath.ToSlash(rel), Contents: body})
		return nil
	})
	return out
}
