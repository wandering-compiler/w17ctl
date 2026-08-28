package fixtures

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/wandering-compiler/w17ctl/internal/core"
	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
)

// FetchCmd downloads stored fixtures as their editable canonical JSON — the
// authoring-side sync absorbed from w17migrate's `fetch fixtures`. The console
// encodes each stored Fixture; the client just writes one
// `<out>/<domain>/<name>.json` per file (no schema, no fixtures package, no
// registrypb client-side).
type FetchCmd struct {
	Domain      string `name:"domain" placeholder:"DOMAIN" help:"Only fetch this domain's fixtures; empty = every domain."`
	FixturesDir string `name:"out" placeholder:"DIR" default:"fixtures" help:"Output root; each fixture writes to <out>/<domain>/<name>.json (a grouped fixture round-trips to <out>/<domain>/<group>/<name>.json)."`
	LockPath    string `name:"lock" placeholder:"PATH" default:"w17/lock.yaml" help:"Path to the lock file."`
	Console     string `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console FixtureFetch service. Optional — falls back to the console you are logged into (w17ctl login), then the binary's compile-time default."`
}

func (c *FetchCmd) Run() error {
	projectID := core.LockProjectIDBestEffort()
	if projectID == "" {
		return fmt.Errorf("fixtures fetch: could not resolve project_id from the lock (run inside a w17 project with %s)", c.LockPath)
	}
	addr, err := core.ResolveConsoleAddr(c.Console)
	if err != nil {
		return err
	}
	cl, conn, err := core.DialFixtureFetch(addr)
	if err != nil {
		return fmt.Errorf("connect %s: %w", addr, err)
	}
	defer func() { _ = conn.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	resp, err := cl.FetchFixtureFiles(ctx, &applyfetchpb.FetchFixtureFilesRequest{
		ProjectId: projectID,
		Domain:    c.Domain,
	})
	if err != nil {
		return fmt.Errorf("fetch fixtures: %w", err)
	}
	files := resp.GetFiles()
	if len(files) == 0 {
		fmt.Fprintln(core.Stdout, "fixtures fetch: nothing stored")
		return nil
	}
	for _, f := range files {
		out := filepath.Join(c.FixturesDir, f.GetDomain(), f.GetName()+".json")
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", filepath.Dir(out), err)
		}
		if err := os.WriteFile(out, f.GetJson(), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", out, err)
		}
		fmt.Fprintf(core.Stdout, "fixtures fetch: %s/%s (%d row(s)) → %s\n",
			f.GetDomain(), f.GetName(), f.GetRowCount(), out)
	}
	return nil
}
