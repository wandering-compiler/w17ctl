// Package fixtureschema answers, for the rendered fixture seeds of a project,
// what each would be rendered against NOW — the "current" half of the schema
// staleness check (sdk migrate.CurrentSchema).
//
// A rendered seed depends on its fixture's bytes, on the schema of the tables
// the render reads, and on the console's renderer. The render manifest
// records all three; the first is compared against the fixture on disk, the
// other two can only be compared against what the console says about the
// schema as it is now — the client holds the IR as opaque bytes and cannot
// digest a table itself. So this re-renders each manifest seed's fixture
// against the current IR and keeps only the provenance the console returns.
// The statements are discarded: deciding whether a seed is stale is not the
// moment to rewrite it.
//
// Shared by `codegen` (which writes the answer to w17/fixtures, where the
// binary reads it), `verify` (which checks the committed answer and the seeds
// against it), and `fixtures render` (which discovers the protos the same way).
package fixtureschema

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/protoscan"
	"github.com/wandering-compiler/w17ctl/internal/schema"
	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	"github.com/wandering-compiler/sdk/go/tooling/migrate"
)

// ModelProtos is the schema file set a fixture renders against: the model
// protos under <root>/<protoDir>, plus the connection-declaring modules (a KV
// or LOCAL_FS module declares no table, so the model walk alone would leave
// its connection undeclared to the IR build). Empty when the project has no
// model protos — appended AFTER that check, so the modules can never make a
// table-less project look non-empty.
func ModelProtos(root, protoDir string) ([]string, error) {
	models, modules, err := protoscan.DiscoverModelProtos(filepath.Join(root, protoDir))
	if err != nil {
		return nil, fmt.Errorf("discover model protos: %w", err)
	}
	if len(models) == 0 {
		return nil, nil
	}
	return append(models, modules...), nil
}

// Current computes the current schema provenance of every seed in the render
// manifest under rendered (`w17/fixtures`). It returns nil — nothing to
// compare — when there is no manifest, no authoring tree beside it, no model
// proto, or when the console answers without schema provenance (it predates
// it): each is "cannot tell", which must not read as stale.
//
// A fixture that no longer renders against the current schema is recorded as
// such (CurrentSeedSchema.Error), not returned as an error: it is the finding.
// A seed whose fixture is gone is left out — the fixture axis reports it.
func Current(ctx context.Context, rendered, root, protoDir, console string) (*migrate.CurrentSchema, error) {
	m, ok, err := migrate.ReadRenderManifest(rendered)
	if err != nil || !ok || len(m.Seeds) == 0 || m.SourceRoot == "" {
		return nil, err
	}
	srcRoot := filepath.Join(rendered, filepath.FromSlash(m.SourceRoot))
	if _, err := os.Stat(srcRoot); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	protos, err := ModelProtos(root, protoDir)
	if err != nil || len(protos) == 0 {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	ir, err := schema.LoadIRBytes(ctx, protos, nil, console)
	if err != nil {
		return nil, fmt.Errorf("load schema: %w", err)
	}
	addr, err := core.ResolveConsoleAddr(console)
	if err != nil {
		return nil, err
	}
	cl, conn, err := core.DialFixtureFetch(addr)
	if err != nil {
		return nil, fmt.Errorf("connect %s: %w", addr, err)
	}
	defer func() { _ = conn.Close() }()

	cur := &migrate.CurrentSchema{Seeds: map[string]migrate.CurrentSeedSchema{}}
	known := false
	for key, rs := range m.Seeds {
		body, err := os.ReadFile(filepath.Join(srcRoot, filepath.FromSlash(rs.Source)))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("read fixture %s: %w", rs.Source, err)
		}
		domain, _, _ := strings.Cut(key, "/")
		resp, err := cl.RenderFixtureSeed(ctx, &applyfetchpb.RenderFixtureSeedRequest{Ir: ir, Domain: domain, Json: body})
		if err != nil {
			if msg, isFinding := renderFinding(err); isFinding {
				cur.Seeds[key] = migrate.CurrentSeedSchema{Error: msg}
				known = true
				continue
			}
			return nil, fmt.Errorf("render %s: %w", key, err)
		}
		s := migrate.RenderedSchemaOf(resp.GetSchema())
		if s != nil {
			known = true
		}
		cur.Seeds[key] = migrate.CurrentSeedSchema{Schema: s}
	}
	if !known {
		return nil, nil
	}
	return cur, nil
}

// renderFinding separates "this fixture does not render against this schema"
// — a property of the project, recorded in the committed .current-schema.json
// and reported as a stale seed — from everything else. Only FailedPrecondition
// is that answer (the console's code for a fixture that does not fit the
// schema). Internal, InvalidArgument (an IR the console could not decode is
// this client's fault, not the fixture's), a transport failure: an error the
// caller fails on, never written down as advice about the fixture.
func renderFinding(err error) (string, bool) {
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.FailedPrecondition {
		return "", false
	}
	return st.Message(), true
}
