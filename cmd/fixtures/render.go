package fixtures

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/protoscan"
	"github.com/wandering-compiler/w17ctl/internal/schema"
	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	"github.com/wandering-compiler/sdk/go/tooling/migrate"
)

// RenderCmd turns the project's fixture JSON into the artefact a generated
// binary applies — the authoring-time half of `<binary> fixtures apply`.
//
// The split it exists to serve: rendering a fixture is schema-aware, so the
// console does it; applying the result needs the database, so the binary that
// owns one does that. Between them sits every environment where neither holds
// at the same time — a compose stack whose Postgres is a container that will
// exist in ten seconds, a CI job, a deploy with no console reachable. There
// the console ran at BUILD time and is long gone when the database appears.
//
// So this writes the console's answer down, exactly as `migrate fetch` writes
// migration artefacts down, and the same directory is what the binary reads.
// Run it whenever a fixture changes; the output is generated and belongs in
// the diff, where a stale render shows up as one.
//
// It renders the fixtures IN THE WORKING TREE rather than the ones stored in
// the registry, and that is deliberate: a rendered artefact is read by a human
// alongside the fixture it came from, so the two must be the same file. The
// registry copy is what `fixtures apply` uses, and it is the copy `push` sends
// there.
type RenderCmd struct {
	Protos      []string `name:"proto" short:"p" placeholder:"PROTO" help:"Path to a .proto schema. Repeatable. Optional — omitted, the model protos are discovered under the lock's proto dir, the same way 'stack build' finds them, so a pipeline needs no per-project knowledge."`
	Imports     []string `name:"import" short:"I" placeholder:"DIR" help:"IGNORED — the console compiles the IR and resolves imports from the uploaded proto tree. Kept so existing scripts do not break; it warns."`
	Domain      string   `name:"domain" placeholder:"DOMAIN" help:"Only render this domain's fixtures; empty = every domain."`
	Group       string   `name:"group" placeholder:"GROUP" help:"Only render this group; empty = EVERY group. Unlike apply, rendering is not the moment to choose what a database gets — the binary picks a group at seed time, from what is here."`
	FixturesDir string   `name:"fixtures-dir" placeholder:"DIR" default:"fixtures" help:"Root of the authoring fixtures tree: <dir>/<domain>/<name>.json, or <dir>/<domain>/<group>/<name>.json."`
	Out         string   `name:"out" placeholder:"DIR" default:"w17/fixtures" help:"Where the rendered artefacts go. This is the directory '<binary> fixtures apply --fixtures' defaults to."`
	Console     string   `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console FixtureFetch service. Optional — falls back to the console you are logged into (w17ctl login), then the binary's compile-time default."`
	Prune       bool     `name:"prune" default:"true" negatable:"" help:"Delete rendered artefacts with no fixture behind them any more. On by default: a deleted fixture whose render survives is rows that come back on the next seed, with nothing in the tree explaining them."`
}

func (c *RenderCmd) Run() error {
	c.Group = normalizeGroup(c.Group)
	seeds, err := collectRenderInputs(c.FixturesDir, c.Domain, c.Group)
	if err != nil {
		return err
	}
	if len(seeds) == 0 {
		fmt.Fprintf(core.Stdout, "fixtures render: no fixtures under %s\n", c.FixturesDir)
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	protos, imports, cleanup, err := c.resolveProtos()
	if err != nil {
		return err
	}
	defer cleanup()
	if len(protos) == 0 {
		return fmt.Errorf(
			"fixtures render: no model protos found under %s\n"+
				"  why: rendering a fixture is schema-aware, and an empty file set renders every\n"+
				"       fixture to zero statements — a seed that silently does nothing\n"+
				"  fix: pass --proto explicitly, or run this from the project root",
			c.protoDir())
	}
	ir, err := schema.LoadIRBytes(ctx, protos, imports, c.Console)
	if err != nil {
		return fmt.Errorf("fixtures render: load schema: %w", err)
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

	written := map[string]bool{}
	for _, s := range seeds {
		body, rerr := os.ReadFile(s.path)
		if rerr != nil {
			return fmt.Errorf("fixtures render: read %s: %w", s.path, rerr)
		}
		resp, rerr := cl.RenderFixtureSeed(ctx, &applyfetchpb.RenderFixtureSeedRequest{
			Ir:     ir,
			Domain: s.domain,
			Json:   body,
		})
		if rerr != nil {
			return fmt.Errorf("fixtures render: %s/%s: %w", s.domain, s.name(), rerr)
		}
		out := migrate.FixtureSeed{Domain: s.domain, Name: s.name(), Statements: resp.GetStatements()}
		if err := migrate.WriteFixtureSeed(c.Out, out); err != nil {
			return fmt.Errorf("fixtures render: write %s/%s: %w", s.domain, s.name(), err)
		}
		written[s.domain+"/"+s.name()] = true
		// A fixture that HAS rows and renders NOTHING is a failure, not a
		// result. It means every model in it resolved to nothing under this
		// domain — almost always because the file is in the wrong directory:
		// the domain comes from the PATH, so `fixtures/<connection>/live.json`
		// renders zero while the identical file under `fixtures/<domain>/`
		// renders every row. `fixtures dump` defaults its directory to the
		// CONNECTION name, so it writes that wrong path itself.
		//
		// Reported with 642 rows in and 0 statements out, exit 0, no word
		// said (deinvo, 2026-09-21). Refusing costs nothing: a genuinely
		// empty fixture has no rows to lose.
		if len(resp.GetStatements()) == 0 && fixtureHasRows(body) {
			return fmt.Errorf(
				"fixtures render: %s/%s — the file has rows and rendered 0 statement(s)\n"+
					"  why: none of its models resolve under domain %q, which is taken from the\n"+
					"       DIRECTORY the file sits in, not from the file\n"+
					"  fix: move it under the domain that declares those models, or pass\n"+
					"       `fixtures dump --domain <domain>` so it is written there to begin with",
				s.domain, s.name(), s.domain)
		}
		fmt.Fprintf(core.Stdout, "fixtures render: %s/%s — %d statement(s)\n",
			s.domain, s.name(), len(resp.GetStatements()))
	}

	if c.Prune {
		pruned, perr := pruneRenderedSeeds(c.Out, c.Domain, c.Group, written)
		if perr != nil {
			return perr
		}
		for _, p := range pruned {
			fmt.Fprintf(core.Stdout, "fixtures render: removed %s (no fixture behind it)\n", p)
		}
	}
	fmt.Fprintf(core.Stdout, "fixtures render: %d fixture(s) → %s\n", len(seeds), c.Out)
	return nil
}

// resolveProtos returns the schema file set to render against: an explicit
// --proto when given, otherwise the model protos discovered under the lock's
// proto dir — the same discovery `stack build` uses, from the same package,
// so the two cannot come to disagree about what a model proto is.
func (c *RenderCmd) resolveProtos() (protos, imports []string, cleanup func(), err error) {
	cleanup = func() {}
	if len(c.Protos) > 0 {
		return c.Protos, c.Imports, cleanup, nil
	}
	root, err := core.FindProjectRoot()
	if err != nil {
		return nil, nil, cleanup, fmt.Errorf("fixtures render: %w", err)
	}
	base := filepath.Join(root, c.protoDir())
	models, modules, err := protoscan.DiscoverModelProtos(base)
	if err != nil {
		return nil, nil, cleanup, fmt.Errorf("fixtures render: discover model protos: %w", err)
	}
	if len(models) == 0 {
		return nil, nil, cleanup, nil
	}
	// Connection-declaring modules ride along: a KV or LOCAL_FS module
	// declares no table, so the model walk alone leaves its connection
	// looking undeclared to the IR build. Appended AFTER the emptiness check
	// so they can never make a table-less project look non-empty.
	models = append(models, modules...)
	// No vocabulary staging, no import roots — the console resolves both. See
	// the twin in `stack build` for what this used to do and why it stopped
	// meaning anything.
	return models, nil, cleanup, nil
}

// protoDir is the project's proto root, from the console's lock projection
// (best-effort: a console-down project falls back to the convention).
func (c *RenderCmd) protoDir() string {
	if view := core.DescribeLockBestEffort(c.Console); view != nil && view.GetProtoDir() != "" {
		return view.GetProtoDir()
	}
	return "proto"
}

// renderInput is one authoring fixture to render.
type renderInput struct {
	domain string
	group  string
	leaf   string
	path   string
}

// name is the FLAT registry key — the same encoding push, apply and the
// artefact layout use.
func (r renderInput) name() string { return registryFixtureName(r.group, r.leaf) }

// collectRenderInputs walks the authoring tree, optionally narrowed. A group
// filter of "" here means EVERY group, not the default one: rendering produces
// the whole artefact set and seeding chooses from it, so a render that
// silently dropped the named groups would leave the binary asking for a group
// that was never written.
func collectRenderInputs(dir, domain, group string) ([]renderInput, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("fixtures render: read %s: %w", dir, err)
	}
	var out []renderInput
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dom := e.Name()
		if domain != "" && dom != domain {
			continue
		}
		domainDir := filepath.Join(dir, dom)
		walkErr := filepath.WalkDir(domainDir, func(p string, d os.DirEntry, werr error) error {
			if werr != nil {
				return fmt.Errorf("read %s: %w", p, werr)
			}
			if d.IsDir() || !strings.HasSuffix(d.Name(), ".json") {
				return nil
			}
			rel, rerr := filepath.Rel(domainDir, p)
			if rerr != nil {
				return rerr
			}
			g := filepath.ToSlash(filepath.Dir(rel))
			if g == "." {
				g = ""
			}
			if group != "" && g != group {
				return nil
			}
			out = append(out, renderInput{
				domain: dom, group: g,
				leaf: strings.TrimSuffix(d.Name(), ".json"),
				path: p,
			})
			return nil
		})
		if walkErr != nil {
			return nil, walkErr
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].domain != out[j].domain {
			return out[i].domain < out[j].domain
		}
		return out[i].name() < out[j].name()
	})
	return out, nil
}

// pruneRenderedSeeds deletes artefacts in the rendered tree that this run did
// not produce, within the scope this run covered.
//
// Scoped to what was rendered, deliberately: a `--domain app` render must not
// delete the billing domain's artefacts, which it had no input for and no
// opinion about. Absence of input is not evidence of deletion when the input
// was never looked at.
func pruneRenderedSeeds(root, domain, group string, keep map[string]bool) ([]string, error) {
	existing, err := migrate.LoadFixtureSeedNames(os.DirFS(root))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("fixtures render: scan %s: %w", root, err)
	}
	var removed []string
	for _, s := range existing {
		if domain != "" && s.Domain != domain {
			continue
		}
		if group != "" && s.Group() != group {
			continue
		}
		if keep[s.Domain+"/"+s.Name] {
			continue
		}
		p := filepath.Join(root, filepath.FromSlash(s.Domain+"/"+s.Name)+".seed.json")
		if err := os.Remove(p); err != nil {
			return nil, fmt.Errorf("fixtures render: remove %s: %w", p, err)
		}
		removed = append(removed, s.Domain+"/"+s.Name)
	}
	return removed, nil
}

// fixtureHasRows reports whether a fixture document carries any row at all.
//
// The distinction the refusal above rests on: zero statements from zero rows
// is an empty file, which is fine; zero statements from rows is a seed that
// silently does nothing.
func fixtureHasRows(body []byte) bool {
	var doc struct {
		Rows []json.RawMessage `json:"rows"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		// Unreadable is not this check's to report — the render call above
		// already failed on it, or the server will.
		return false
	}
	return len(doc.Rows) > 0
}
