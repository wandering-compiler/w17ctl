package template

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/scaffold"
)

// templateSurface is one emittable example surface: the scaffold
// template body + a one-line blurb for `--list`.
type templateSurface struct {
	name  string // template name (diagnostics only)
	body  string // const template body (rendered with scaffold.Ctx)
	blurb string // one-line description for --list
}

// templateSurfaces maps a surface key to the scaffold template that
// teaches its proto shape. The four-layer + rest/admin/events bodies
// are the SAME consts `domain add --with-example` / `module add
// --with-example` lay down — one source of truth, no drift. The
// rpc/mcp/cli transports the example flow doesn't scaffold add their
// own teaching sentinels (see scaffold.go).
var templateSurfaces = map[string]templateSurface{
	"domain":      {"domain", scaffold.DomainWithExampleProto, "domain w17.proto sentinel (connection + eventbus channel cascade)"},
	"module":      {"module", scaffold.ModuleSkeletonProto, "module w17.proto sentinel"},
	"types":       {"types", scaffold.ExampleTypesProto, "DB-backed model with (w17.field)/(w17.db.column) annotations"},
	"queries":     {"queries", scaffold.ExampleQueryProto, "read-side service with (w17.db.method).query DQL"},
	"mutations":   {"mutations", scaffold.ExampleMutationProto, "write-side service with (w17.db.method).mutation DQL"},
	"business":    {"business", scaffold.ExampleBusinessProto, "hand-written facade service (no db annotations)"},
	"rest":        {"rest", scaffold.ExampleDomainRestProto, "(w17.rest_api) REST registry mapping RPCs to URLs"},
	"admin":       {"admin", scaffold.ExampleDomainAdminProto, "(w17.admin_api) Django-style admin bundle"},
	"events":      {"events", scaffold.ExampleEventsProto, "(w17.event) eventbus event registry"},
	"subscribers": {"subscribers", scaffold.ExampleSubscribersProto, "(w17.event_subscribers) subscription registry"},
	"rpc":         {"rpc", scaffold.ExampleRpcProto, "(w17.rpc_api) public gRPC gateway sentinel"},
	"mcp":         {"mcp", scaffold.ExampleMcpProto, "(w17.mcp_api) MCP tool surface sentinel"},
	"cli":         {"cli", scaffold.ExampleCliProto, "(w17.cli_api) CLI transport sentinel"},
}

// Cmd prints a commented example .proto for one w17 surface —
// the "show me how to annotate X" helper. It reuses the same scaffold
// bodies as `domain/module add --with-example` (single source of
// truth) and adds teaching sentinels for the rpc/mcp/cli transports
// the example flow doesn't scaffold. Output goes to core.Stdout (copy-paste)
// or, with --out, to a file.
type Cmd struct {
	Surface string `arg:"" optional:"" help:"Surface to emit (run --list for the catalogue): domain|module|types|queries|mutations|business|rest|admin|events|subscribers|rpc|mcp|cli."`
	Out     string `name:"out" short:"o" placeholder:"FILE" help:"Write to FILE instead of core.Stdout (refuses to overwrite an existing file)."`
	List    bool   `name:"list" help:"List the available surfaces and exit."`
	Project string `name:"project" default:"myapp" placeholder:"NAME" help:"Project package-prefix placeholder in the rendered proto."`
	Domain  string `name:"domain" default:"example" placeholder:"NAME" help:"Domain-name placeholder in the rendered proto."`
	Module  string `name:"module" default:"example" placeholder:"NAME" help:"Module-name placeholder in the rendered proto."`
}

// renderTemplateFn is a test seam for the render arm, which a fixed
// const template + validated ctx cannot fail through real inputs.
var renderTemplateFn = scaffold.RenderTemplate

func (c *Cmd) Run() error {
	if c.List {
		return c.runList()
	}
	if c.Surface == "" {
		return fmt.Errorf("template: a surface is required (or pass --list). One of: %s", strings.Join(sortedSurfaceKeys(), ", "))
	}
	s, ok := templateSurfaces[c.Surface]
	if !ok {
		return fmt.Errorf("template: unknown surface %q (one of: %s)", c.Surface, strings.Join(sortedSurfaceKeys(), ", "))
	}
	if err := scaffold.ValidateIdent("domain", c.Domain); err != nil {
		return fmt.Errorf("template: %w", err)
	}
	if err := scaffold.ValidateIdent("module", c.Module); err != nil {
		return fmt.Errorf("template: %w", err)
	}

	body, err := renderTemplateFn(s.name, s.body, scaffold.Ctx{
		Project: scaffold.ProtoSafePackagePrefix(c.Project),
		Domain:  c.Domain,
		Module:  c.Module,
	})
	if err != nil {
		return fmt.Errorf("template %s: %w", c.Surface, err)
	}

	if c.Out == "" {
		_, err := core.Stdout.Write(body)
		return err
	}
	if _, statErr := os.Stat(c.Out); statErr == nil {
		return fmt.Errorf("template: refusing to overwrite existing file %s", c.Out)
	} else if !os.IsNotExist(statErr) {
		return fmt.Errorf("template: stat %s: %w", c.Out, statErr)
	}
	if err := os.WriteFile(c.Out, body, 0o644); err != nil {
		return fmt.Errorf("template: write %s: %w", c.Out, err)
	}
	fmt.Fprintf(core.Stdout, "wrote %s (%s surface)\n", c.Out, c.Surface)
	return nil
}

func (c *Cmd) runList() error {
	fmt.Fprintln(core.Stdout, "Available surfaces — w17ctl template <surface>:")
	for _, k := range sortedSurfaceKeys() {
		fmt.Fprintf(core.Stdout, "  %-12s %s\n", k, templateSurfaces[k].blurb)
	}
	return nil
}

func sortedSurfaceKeys() []string {
	keys := make([]string, 0, len(templateSurfaces))
	for k := range templateSurfaces {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
