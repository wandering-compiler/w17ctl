package plugin

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/wandering-compiler/w17ctl/internal/core"
	codegenpb "github.com/wandering-compiler/sdk/go/pb/w17compiler"
	"github.com/wandering-compiler/sdk/go/tooling/pathguard"
)

// GenPbCmd implements `w17ctl plugin gen-pb [dir]`.
//
// It regenerates a plugin's standalone `src/gen/pb/*.pb.go` from its
// `proto/` tree — the committed pb files that exist ONLY for the plugin
// author's local dev/test loop (`go test ./...` inside `src/`). The
// project codegen pipeline generates its OWN per-activation pb at
// staging time and never uses these files.
//
// Thin-client model: the compile is a COMPILER concern, so it runs on
// the console. The client only uploads the raw proto/ tree + plugin.yaml
// to the GeneratePluginPb RPC, and writes the pb.go files the server
// returns — no buf / loader / manifest / placeholder logic client-side.
type GenPbCmd struct {
	Dir     string `arg:"" optional:"" name:"dir" default:"." help:"Plugin source directory (holds plugin.yaml + proto/ + src/). Default: current directory."`
	Console string `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console CodegenService. Optional — falls back to the binary's compile-time default."`
}

func (c *GenPbCmd) Run() error {
	dir, err := filepath.Abs(c.Dir)
	if err != nil {
		return fmt.Errorf("plugin gen-pb: resolve dir: %w", err)
	}

	pluginYaml, err := os.ReadFile(filepath.Join(dir, "plugin.yaml"))
	if err != nil {
		return fmt.Errorf("plugin gen-pb: read plugin.yaml: %w", err)
	}
	protoFiles, err := readPluginProto(filepath.Join(dir, "proto"))
	if err != nil {
		return fmt.Errorf("plugin gen-pb: %w", err)
	}
	if len(protoFiles) == 0 {
		return fmt.Errorf("plugin gen-pb: no .proto files under %s", filepath.Join(dir, "proto"))
	}

	addr, err := core.ResolveConsoleAddr(c.Console)
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
	stream, err := cl.GeneratePluginPb(ctx, &codegenpb.GeneratePluginPbRequest{
		Files:      protoFiles,
		PluginYaml: pluginYaml,
	})
	if err != nil {
		return err
	}

	outDir := filepath.Join(dir, "src", "gen", "pb")
	if err := clearGeneratedPb(outDir); err != nil {
		return fmt.Errorf("plugin gen-pb: %w", err)
	}
	// The server prefixes each file with the "gen/pb" output root; the
	// plugin's pb dir is src/gen/pb, so land each under src/.
	//
	// The pb files arrive one per stream message (a batched response would
	// cap the whole set at gRPC's default 4 MiB on the gateway→backend hop),
	// so they are written as they land — the wipe above happens FIRST, and a
	// stream that breaks mid-run surfaces as an error, never as a quietly
	// half-regenerated gen/pb.
	srcDir := filepath.Join(dir, "src")
	writeOne := func(f *codegenpb.GeneratedFile) error {
		// SERVER-SUPPLIED path: contain it under src/ so a buggy/compromised
		// console cannot escape the plugin dir via `..`/absolute.
		dst, err := pathguard.Join(srcDir, f.GetRelativePath())
		if err != nil {
			return fmt.Errorf("plugin gen-pb: server file path %q escapes the plugin dir: %w", f.GetRelativePath(), err)
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return fmt.Errorf("plugin gen-pb: mkdir %s: %w", filepath.Dir(dst), err)
		}
		if err := os.WriteFile(dst, f.GetContents(), 0o644); err != nil {
			return fmt.Errorf("plugin gen-pb: write %s: %w", dst, err)
		}
		return nil
	}
	written, err := core.RecvGeneratedFiles(stream, writeOne)
	if err != nil {
		return err
	}
	if written == 0 {
		return fmt.Errorf("plugin gen-pb: server produced no output (check proto under %s)", filepath.Join(dir, "proto"))
	}
	fmt.Fprintf(core.Stdout, "plugin gen-pb: wrote %d files to %s\n", written, outDir)
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
