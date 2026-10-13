package plugin

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/wandering-compiler/w17ctl/internal/codegen"
	"github.com/wandering-compiler/w17ctl/internal/core"
	codegenpb "github.com/wandering-compiler/sdk/go/pb/w17compiler"
	"github.com/wandering-compiler/sdk/go/tooling/pathguard"

	"github.com/wandering-compiler/w17ctl/internal/gofmtc"
)

// GenPbCmd implements `w17ctl plugin gen [dir]` (`gen-pb` is its old name).
//
// It generates a plugin's `src/gen` from `plugin.yaml` and `proto/`: the pb
// (`gen/pb`), the contract (`gen/interfaces.go`: EnvConfig, ClientSet,
// HandlerRegistry) and the feature constants (`gen/features.go`, at the
// manifest defaults). Staging generates the same package for every
// activation, from the same functions, so the author's `go build` and
// `go test` inside `src/` see what a bundle will give the plugin. Nothing in
// `src/gen` is written by hand or published.
//
// Thin-client model: the compile is a COMPILER concern, so it runs on a
// codegen worker the console places (PLACEMENT_OP_PLUGIN_PB). The client
// only uploads the raw proto/ tree + plugin.yaml to the worker's
// GeneratePluginPb, and writes the pb.go files it returns — no buf / loader /
// manifest / placeholder logic client-side.
type GenPbCmd struct {
	Dir     string `arg:"" optional:"" name:"dir" default:"." help:"Plugin source directory (holds plugin.yaml + proto/ + src/). Default: current directory."`
	Console string `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console CodegenService. Optional — falls back to the binary's compile-time default."`
}

func (c *GenPbCmd) Run() error {
	dir, err := filepath.Abs(c.Dir)
	if err != nil {
		return fmt.Errorf("plugin gen: resolve dir: %w", err)
	}

	pluginYaml, err := os.ReadFile(filepath.Join(dir, "plugin.yaml"))
	if err != nil {
		return fmt.Errorf("plugin gen: read plugin.yaml: %w", err)
	}
	protoFiles, err := readPluginProto(filepath.Join(dir, "proto"))
	if err != nil {
		return fmt.Errorf("plugin gen: %w", err)
	}
	if len(protoFiles) == 0 {
		return fmt.Errorf("plugin gen: no .proto files under %s", filepath.Join(dir, "proto"))
	}

	addr, err := core.ResolveConsoleAddr(c.Console)
	if err != nil {
		return err
	}
	_, conn, err := core.DialCodegen(addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	outDir := filepath.Join(dir, "src", "gen", "pb")
	// The generation runs on a codegen worker: the console only PLACES it
	// (PLACEMENT_OP_PLUGIN_PB — no project, so no lock and nothing signed).
	//
	// The pb files arrive one per stream message (a batched response would cap
	// the whole set at gRPC's default 4 MiB on the relay hop) and are only
	// COLLECTED while the run is live. Nothing on disk changes until the run
	// has finished cleanly: a compiler refusal, a worker lost mid-run (and
	// re-run on a new placement) or a stream that breaks leaves the existing
	// gen/pb exactly as it was. It used to be wiped before the first file
	// arrived, so any of those left the plugin without its stubs.
	var got []*codegenpb.GeneratedFile
	start := &codegenpb.PlaceGenerateStart{ProtoLines: codegen.ProtoLines(protoFiles), Op: codegenpb.PlacementOp_PLACEMENT_OP_PLUGIN_PB}
	err = codegen.RunOnWorker(core.PlacerFn(conn), start, core.Stdout,
		func(ctx context.Context, worker codegenpb.CodegenServiceClient, _ []byte) error {
			got = nil // a re-run starts afresh
			stream, err := worker.GeneratePluginPb(ctx)
			if err != nil {
				return err
			}
			// Header alone, then the tree in chunks (see core.SendProtoChunks).
			if err := stream.Send(&codegenpb.GeneratePluginPbRequest{PluginYaml: pluginYaml}); err != nil {
				return err
			}
			if err := core.SendProtoChunks(protoFiles,
				func(b []*codegenpb.ProtoFile) *codegenpb.GeneratePluginPbRequest {
					return &codegenpb.GeneratePluginPbRequest{Files: b}
				}, stream.Send); err != nil {
				return err
			}
			if err := stream.CloseSend(); err != nil {
				return err
			}
			_, err = core.RecvGeneratedFiles(stream, func(f *codegenpb.GeneratedFile) error {
				got = append(got, f)
				return nil
			})
			return err
		})
	if err != nil {
		return err
	}
	// Every file is checked BEFORE the old stubs go: a path that escapes, or a
	// body that does not format, refuses the whole set with gen/pb untouched.
	//
	// The server prefixes each file with the "gen/pb" output root; the
	// plugin's pb dir is src/gen/pb, so each lands under src/.
	srcDir := filepath.Join(dir, "src")
	type pending struct {
		dst  string
		body []byte
	}
	var todo []pending
	for _, f := range got {
		// SERVER-SUPPLIED path: contain it under src/ so a buggy/compromised
		// worker cannot escape the plugin dir via `..`/absolute.
		dst, err := pathguard.Join(srcDir, f.GetRelativePath())
		if err != nil {
			return fmt.Errorf("plugin gen: server file path %q escapes the plugin dir: %w", f.GetRelativePath(), err)
		}
		// Belt and braces: these come from protoc-gen-go, whose output is
		// already gofmt-clean, so this is a no-op today. It is here because
		// "the server does not format" is a property of every path, and a
		// writer that assumes its source is tidy is how the next one gets
		// missed.
		body, ferr := gofmtc.SourceIfGo(f.GetRelativePath(), f.GetContents())
		if ferr != nil {
			return ferr
		}
		todo = append(todo, pending{dst: dst, body: body})
	}
	// After the path checks, so a set that is refused for where it would
	// write says that, not that it is empty. The contract and the feature
	// constants come back whatever the protos say; it is the pb that shows
	// the protos compiled into anything.
	pbCount := 0
	for _, f := range got {
		if strings.HasPrefix(f.GetRelativePath(), "gen/pb/") {
			pbCount++
		}
	}
	if pbCount == 0 {
		return fmt.Errorf("plugin gen: server produced no output (check proto under %s) — the existing gen/pb is left as it was", filepath.Join(dir, "proto"))
	}
	if err := clearGeneratedPb(outDir); err != nil {
		return fmt.Errorf("plugin gen: %w", err)
	}
	for _, w := range todo {
		if err := os.MkdirAll(filepath.Dir(w.dst), 0o755); err != nil {
			return fmt.Errorf("plugin gen: mkdir %s: %w", filepath.Dir(w.dst), err)
		}
		if err := os.WriteFile(w.dst, w.body, 0o644); err != nil {
			return fmt.Errorf("plugin gen: write %s: %w", w.dst, err)
		}
	}
	fmt.Fprintf(core.Stdout, "plugin gen: wrote %d files under %s (%d pb)\n", len(todo), filepath.Join(srcDir, "gen"), pbCount)
	return nil
}

// readPluginProto walks protoDir for *.proto and returns them as
// proto-root-relative ProtoFiles ("types/models.proto") — raw bytes,
// placeholders unexpanded (the server expands them).
func readPluginProto(protoDir string) ([]*codegenpb.ProtoFile, error) {
	var files []*codegenpb.ProtoFile
	err := filepath.WalkDir(protoDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() || !strings.HasSuffix(path, ".proto") {
			return nil
		}
		rel, err := filepath.Rel(protoDir, path)
		if err != nil {
			return err
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		files = append(files, &codegenpb.ProtoFile{Filename: filepath.ToSlash(rel), Contents: body})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read proto under %s: %w", protoDir, err)
	}
	return files, nil
}

// clearGeneratedPb removes existing *.pb.go from outDir so a renamed/
// deleted proto doesn't leave a stale stub. A missing dir is fine.
func clearGeneratedPb(outDir string) error {
	matches, err := filepath.Glob(filepath.Join(outDir, "*.pb.go"))
	if err != nil {
		return err
	}
	sort.Strings(matches)
	for _, m := range matches {
		if err := os.Remove(m); err != nil {
			return fmt.Errorf("remove stale %s: %w", m, err)
		}
	}
	return nil
}
