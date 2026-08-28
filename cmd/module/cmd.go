package module

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/scaffold"
)

// Cmd is the parent of `w17ctl module <leaf>`. Today
// the only leaf is `add`; future: `module rename` /
// `module remove`.
type Cmd struct {
	Add AddCmd `cmd:"" help:"Scaffold a new module under an existing domain — creates proto/<protoDir>/domains/<DOMAIN>/<MODULE>/w17.proto. With --with-example seeds the four-layer (queries/mutations/business/types) shape with annotation samples."`
}

// AddCmd implements `w17ctl module add DOMAIN/MODULE`.
//
// The positional argument is path-shaped (`<domain>/<module>`)
// to mirror the directory structure the command creates and
// because operators tab-complete subdirs with `/`. The leading
// segment must be an existing domain — the command refuses
// when the domain directory is missing (steers to
// `w17ctl domain add`).
type AddCmd struct {
	Ident       string `arg:"" name:"ident" help:"Module identifier in <domain>/<module> shape (e.g. users/billing, app/cache). Both segments lowercase letters / digits / underscores."`
	WithExample bool   `name:"with-example" help:"Also lay down the four-layer (queries/mutations/business/types) example payload alongside the module sentinel."`
	Console     string `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console (owns the lock). Optional — falls back to the binary's compile-time default."`
}

// renderTemplateFn is a test seam for the skeleton render arm,
// which a fixed template + validated ctx cannot fail through real
// inputs. Overridden in _test.go to drive the error return.
var renderTemplateFn = scaffold.RenderTemplate

// Run implements the kong command interface.
func (c *AddCmd) Run() error {
	domain, module, err := parseModuleIdent(c.Ident)
	if err != nil {
		return fmt.Errorf("module add: %w", err)
	}

	root, err := core.FindProjectRoot()
	if err != nil {
		return fmt.Errorf("module add: %w", err)
	}
	view, err := core.DescribeLockFromRoot(c.Console, root)
	if err != nil {
		return fmt.Errorf("module add: %w", err)
	}

	protoDir := view.GetProtoDir()
	domainDir := filepath.Join(root, protoDir, "domains", domain)
	if info, err := os.Stat(domainDir); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("module add: domain %q does not exist at %s (run `w17ctl domain add %s` first)",
				domain, filepath.Join(protoDir, "domains", domain), domain)
		}
		return fmt.Errorf("module add: stat %s: %w", domainDir, err)
	} else if !info.IsDir() {
		return fmt.Errorf("module add: %s exists but is not a directory", domainDir)
	}

	moduleDir := filepath.Join(domainDir, module)
	if _, err := os.Stat(moduleDir); err == nil {
		return fmt.Errorf("module add: %s already exists (refusing to overwrite — delete the directory or pick a different name)",
			filepath.Join(protoDir, "domains", domain, module))
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("module add: stat %s: %w", moduleDir, err)
	}

	// F1 (plugin-system track) — a module name that collides with an
	// active plugin's `registered_as` is caught at codegen time by the IR
	// validator (D3). The early-warning pre-flight that recomputed it here
	// needed the proto loader (DiscoverActivations); a thin client doesn't
	// embed the loader, so the check moved fully to codegen-time (the real
	// gate) per the thin-client refactor (Step 3a).

	ctx := scaffold.Ctx{
		Project: scaffold.ProtoSafePackagePrefix(view.GetProject()),
		Domain:  domain,
		Module:  module,
	}

	var written []string
	if c.WithExample {
		w, err := scaffold.WriteExampleModule(moduleDir, ctx)
		if err != nil {
			return fmt.Errorf("module add: %w", err)
		}
		written = w
	} else {
		w, err := writeModuleSkeleton(moduleDir, ctx)
		if err != nil {
			return fmt.Errorf("module add: %w", err)
		}
		written = w
	}

	rel := filepath.Join(protoDir, "domains", domain, module)
	fmt.Fprintf(core.Stdout, "module add: scaffolded %s (%d file(s)):\n", rel, len(written))
	for _, p := range written {
		fmt.Fprintf(core.Stdout, "  %s\n", p)
	}
	return nil
}

// parseModuleIdent splits `<domain>/<module>` into validated
// halves. Refuses anything other than exactly one slash —
// callers misspelling the form get an explicit steering
// message rather than a path-not-found later.
func parseModuleIdent(ident string) (domain, module string, err error) {
	parts := strings.Split(ident, "/")
	if len(parts) != 2 {
		return "", "", fmt.Errorf("identifier %q must be in <domain>/<module> shape (e.g. users/billing)", ident)
	}
	domain, module = parts[0], parts[1]
	if err := scaffold.ValidateIdent("domain", domain); err != nil {
		return "", "", err
	}
	if err := scaffold.ValidateIdent("module", module); err != nil {
		return "", "", err
	}
	return domain, module, nil
}

// emptyModuleLayerDirs are the per-layer directories every
// new module gets even when bare. Operators see the
// expected shape immediately and can drop their first
// .proto file in the right place without having to consult
// docs.
var emptyModuleLayerDirs = []string{"types", "queries", "mutations", "business"}

// writeModuleSkeleton lays down the bare module shape:
//
//   - w17.proto                 (cascade sentinel)
//   - events/events.proto       (empty registry — package + comment)
//   - types/.gitkeep            (empty dir)
//   - queries/.gitkeep          (empty dir)
//   - mutations/.gitkeep        (empty dir)
//   - business/.gitkeep         (empty dir)
//
// The .gitkeep placeholders are the standard idiom for
// "tracked empty directory" — git ignores empty dirs but
// will commit zero-byte files, so the structure survives
// `git clone`.
//
// Deliberately NO subscribers.proto (same call as
// WriteExampleModule). `subscribers*.proto` is a reserved
// sentinel filename, and the eventbus parser hard-fails on
// one that carries no `(w17.event_subscribers)` option or
// sits outside the domain root — a `<module>/subscribers/`
// skeleton is both, so emitting it shipped a project that
// couldn't codegen. A subscriber surface also needs
// hand-written handlers to be meaningful. Consumers reach
// for `w17ctl template subscribers`, which says where the
// file goes. Pinned by
// TestWriteModuleSkeleton_NoSubscriberSentinel.
func writeModuleSkeleton(moduleDir string, ctx scaffold.Ctx) ([]string, error) {
	if err := os.MkdirAll(moduleDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", moduleDir, err)
	}

	files := []struct {
		rel, name, body string
	}{
		{"w17.proto", "module_skeleton", scaffold.ModuleSkeletonProto},
		{"events/events.proto", "empty_events", scaffold.EmptyEventsProto},
	}
	var written []string
	for _, f := range files {
		body, err := renderTemplateFn(f.name, f.body, ctx)
		if err != nil {
			return written, err
		}
		dst := filepath.Join(moduleDir, f.rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return written, fmt.Errorf("mkdir %s: %w", filepath.Dir(dst), err)
		}
		if err := os.WriteFile(dst, body, 0o644); err != nil {
			return written, fmt.Errorf("write %s: %w", dst, err)
		}
		written = append(written, scaffold.RelFromProject(dst))
	}

	// Empty layer dirs with .gitkeep placeholders so the
	// structure survives a git clone.
	for _, sub := range emptyModuleLayerDirs {
		dir := filepath.Join(moduleDir, sub)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return written, fmt.Errorf("mkdir %s: %w", dir, err)
		}
		gitkeep := filepath.Join(dir, ".gitkeep")
		if err := os.WriteFile(gitkeep, nil, 0o644); err != nil {
			return written, fmt.Errorf("write %s: %w", gitkeep, err)
		}
		written = append(written, scaffold.RelFromProject(gitkeep))
	}

	return written, nil
}
