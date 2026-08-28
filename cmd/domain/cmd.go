package domain

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/scaffold"
)

// Cmd is the parent of `w17ctl domain <leaf>` commands.
// Today the only leaf is `add`; future: `domain rename` /
// `domain remove`.
type Cmd struct {
	Add AddCmd `cmd:"" help:"Scaffold a new domain — creates proto/<protoDir>/domains/<NAME>/w17.proto. With --with-example seeds a four-layer example module so operators can see the full shape on disk."`
}

// AddCmd implements `w17ctl domain add NAME`.
//
// Outputs (relative to the project root):
//
//	<protoDir>/domains/<NAME>/w17.proto            (cascade sentinel)
//
// With --with-example:
//
//	<protoDir>/domains/<NAME>/example/w17.proto
//	<protoDir>/domains/<NAME>/example/types/models.proto
//	<protoDir>/domains/<NAME>/example/queries/note_query.proto
//	<protoDir>/domains/<NAME>/example/mutations/note_mutation.proto
//	<protoDir>/domains/<NAME>/example/business/notes_business.proto
//
// Refuses when the project lock is missing (steers to
// `w17ctl init`) or the target domain directory already
// exists (steers to `--with-example` if the existing dir is
// empty / lacks the sentinel — but this iter just refuses).
type AddCmd struct {
	Name        string `arg:"" name:"name" help:"Domain identifier (lowercase letters, digits, underscores; e.g. users, billing, analytics)."`
	WithExample bool   `name:"with-example" help:"Also lay down an example/ module showing the four-layer (queries/mutations/business/types) shape with annotation samples."`
	Console     string `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console (owns the lock). Optional — falls back to the binary's compile-time default."`
}

// Test seams (overridden in _test.go) for the --with-example
// companion-file failure arms. Once the freshly-created domain
// dir is writable they cannot fail through real filesystem state,
// so these vars let the tests drive the error returns.
var (
	writeExampleModuleFn = scaffold.WriteExampleModule
	renderTemplateFn     = scaffold.RenderTemplate
	writeFileFn          = os.WriteFile
)

// Run implements the kong command interface.
func (c *AddCmd) Run() error {
	if err := scaffold.ValidateIdent("domain", c.Name); err != nil {
		return fmt.Errorf("domain add: %w", err)
	}

	root, err := core.FindProjectRoot()
	if err != nil {
		return fmt.Errorf("domain add: %w", err)
	}
	view, err := core.DescribeLockFromRoot(c.Console, root)
	if err != nil {
		return fmt.Errorf("domain add: %w", err)
	}

	protoDir := view.GetProtoDir()
	domainDir := filepath.Join(root, protoDir, "domains", c.Name)
	if _, err := os.Stat(domainDir); err == nil {
		return fmt.Errorf("domain add: %s already exists (refusing to overwrite — delete the directory or pick a different name)", filepath.Join(protoDir, "domains", c.Name))
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("domain add: stat %s: %w", domainDir, err)
	}

	ctx := scaffold.Ctx{
		Project: scaffold.ProtoSafePackagePrefix(view.GetProject()),
		Domain:  c.Name,
	}

	domainTemplate := scaffold.DomainSkeletonProto
	if c.WithExample {
		// --with-example pulls in events too; the domain
		// w17.proto needs the channels[] cascade so those
		// events compile cleanly.
		domainTemplate = scaffold.DomainWithExampleProto
	}
	written, err := writeDomainSentinel(domainDir, ctx, domainTemplate)
	if err != nil {
		return fmt.Errorf("domain add: %w", err)
	}

	if c.WithExample {
		exDir := filepath.Join(domainDir, "example")
		exCtx := ctx
		exCtx.Module = "example"
		ex, err := writeExampleModuleFn(exDir, exCtx)
		if err != nil {
			return fmt.Errorf("domain add: example: %w", err)
		}
		written = append(written, ex...)

		// Domain-level companion files. Each lives at the
		// domain root because its surface aggregates across
		// modules:
		//   - rest.proto   (REV-017 (w17.rest_api) registry)
		//
		// admin.proto (REV-150 (w17.admin_api)) is deliberately NOT
		// scaffolded: the admin surface requires an auth block wired to a
		// real login flow (login_method/user_lookup → the auth plugin's
		// admin_auth.AuthService.*), which a self-contained example can't
		// lay down — the admin generator rejects an auth-less surface, so
		// including it would make --with-example fail codegen out of the
		// box. Operators reach the admin shape via `w17ctl template admin`.
		companions := []struct {
			rel, name, body string
		}{
			{"rest.proto", "example_domain_rest", scaffold.ExampleDomainRestProto},
		}
		for _, comp := range companions {
			body, err := renderTemplateFn(comp.name, comp.body, exCtx)
			if err != nil {
				return fmt.Errorf("domain add: example %s: %w", comp.rel, err)
			}
			dst := filepath.Join(domainDir, comp.rel)
			if err := writeFileFn(dst, body, 0o644); err != nil {
				return fmt.Errorf("domain add: write %s: %w", dst, err)
			}
			written = append(written, scaffold.RelFromProject(dst))
		}
	}

	rel := filepath.Join(protoDir, "domains", c.Name)
	fmt.Fprintf(core.Stdout, "domain add: scaffolded %s (%d file(s)):\n", rel, len(written))
	for _, p := range written {
		fmt.Fprintf(core.Stdout, "  %s\n", p)
	}
	// Hand-written Service/Facade bodies live OUTSIDE the generated
	// w17/ tree — in the project's own top-level srcgo module — and the
	// generated <domain>-business bundle imports them via RegisterBusiness.
	// Never hand-edit anything under w17/services/ (DO NOT EDIT + fully
	// regenerated). See w17/specs/architecture.md (zero-code isolation).
	fmt.Fprintf(core.Stdout,
		"\nhand-written facade → srcgo/domains/%s/grpcapi/business/ "+
			"(outside w17/; wired into the generated bundle via RegisterBusiness)\n",
		c.Name)
	return nil
}

// writeDomainSentinel lays down the domain-level w17.proto
// sentinel from one of the domain templates (bare skeleton
// vs. with-example). Returns the project-relative paths of
// every file it wrote (callers fold them into the final log
// line).
func writeDomainSentinel(domainDir string, ctx scaffold.Ctx, tmpl string) ([]string, error) {
	if err := os.MkdirAll(domainDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", domainDir, err)
	}
	body, err := scaffold.RenderTemplate("domain_sentinel", tmpl, ctx)
	if err != nil {
		return nil, err
	}
	w17Path := filepath.Join(domainDir, "w17.proto")
	if err := os.WriteFile(w17Path, body, 0o644); err != nil {
		return nil, fmt.Errorf("write %s: %w", w17Path, err)
	}
	return []string{scaffold.RelFromProject(w17Path)}, nil
}
