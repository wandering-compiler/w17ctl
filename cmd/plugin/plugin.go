package plugin

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"google.golang.org/grpc"

	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/lockfile"
	codegenpb "github.com/wandering-compiler/sdk/go/pb/w17compiler"
)

// inspectManifest ships a plugin's raw plugin.yaml + the installed-plugin set
// to the console's InspectPluginManifest RPC, which parses + validates it and
// runs the requirement check server-side (the plugin-system semantics are
// compiler-domain — the client holds no console/plugins). Returns the parsed
// identity (name/version) + any requirement warnings. installed may be nil for
// a pure parse (e.g. `plugin list`, which only needs the version).
func inspectManifest(cl codegenpb.CodegenServiceClient, manifestYAML []byte, source string, installed map[string]lockfile.Plugin) (*codegenpb.InspectPluginManifestResponse, error) {
	var inst []*codegenpb.InstalledPlugin
	for _, p := range installed {
		inst = append(inst, &codegenpb.InstalledPlugin{Name: p.Name, Version: p.Version})
	}
	ctx, cancel := core.ClientCtx()
	defer cancel()
	return cl.InspectPluginManifest(ctx, &codegenpb.InspectPluginManifestRequest{
		ManifestYaml: manifestYAML,
		Source:       source,
		Installed:    inst,
	})
}

// installedFromLock reads the installed-plugin set from the lock at lockPath
// via the offline lockfile reader (no lockpb). A missing/unreadable lock yields
// an empty map (a fresh project has no installs yet).
func installedFromLock(lockPath string) map[string]lockfile.Plugin {
	out := map[string]lockfile.Plugin{}
	lk, err := lockfile.Load(lockPath)
	if err != nil {
		return out
	}
	for _, p := range lk.Plugins {
		out[p.Name] = p
	}
	return out
}

// lockProtoDir resolves the project's proto dir from the console's lock
// projection (DescribeLock) — where plugins install under
// `<proto_dir>/plugins/<name>`. Best-effort: falls back to "proto".
func lockProtoDir(console, root string) string {
	if view, err := core.DescribeLockFromRoot(console, root); err == nil && view.GetProtoDir() != "" {
		return view.GetProtoDir()
	}
	return "proto"
}

// dialCodegen resolves the console address + dials CodegenService for the
// plugin commands (manifest inspection). The caller closes the returned conn.
func dialCodegen(console string) (codegenpb.CodegenServiceClient, *grpc.ClientConn, error) {
	addr, err := core.ResolveConsoleAddr(console)
	if err != nil {
		return nil, nil, err
	}
	cl, conn, err := core.DialCodegen(addr)
	if err != nil {
		return nil, nil, fmt.Errorf("connect %s: %w", addr, err)
	}
	return cl, conn, nil
}

// ListCmd implements `w17ctl plugin list`. Walks the
// embedded catalog (each subdirectory of `embed/plugins/`
// is one plugin), loads each plugin.yaml, cross-references
// the lock's `plugins[]` to surface install state. One line
// per plugin; reports "no plugins available" when the catalog
// is empty.
type ListCmd struct {
	LockPath string `name:"lock" placeholder:"PATH" default:"w17/lock.yaml" help:"Path to the lock file."`
	Console  string `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console. Optional — falls back to the binary's compile-time default. Used to parse each plugin's manifest for its version."`
}

func (c *ListCmd) Run() error {
	names, err := internalPluginNames()
	if err != nil {
		return fmt.Errorf("plugin list: %w", err)
	}
	installed := installedFromLock(c.LockPath)
	if len(names) == 0 && len(installed) == 0 {
		fmt.Fprintln(core.Stdout, "no plugins available (catalog empty; no installs in lock)")
		return nil
	}

	root, err := internalPluginsRoot()
	if err != nil {
		return fmt.Errorf("plugin list: %w", err)
	}

	cl, conn, err := dialCodegen(c.Console)
	if err != nil {
		return fmt.Errorf("plugin list: %w", err)
	}
	defer func() { _ = conn.Close() }()

	// Plugins in the embedded catalog — always loaded so we
	// surface the catalog version (which install would record).
	type row struct {
		name      string
		version   string
		source    string
		installed bool
	}
	rows := []row{}
	seen := map[string]bool{}
	for _, name := range names {
		manifestPath := name + "/plugin.yaml"
		data, rerr := fs.ReadFile(root, manifestPath)
		if rerr != nil {
			fmt.Fprintf(core.Stdout, "%-20s ERROR: %v\n", name, rerr)
			continue
		}
		m, perr := inspectManifest(cl, data, "embed:"+manifestPath, nil)
		if perr != nil {
			fmt.Fprintf(core.Stdout, "%-20s ERROR: %v\n", name, perr)
			continue
		}
		r := row{name: name, version: m.GetVersion(), source: "embedded"}
		if inst, ok := installed[name]; ok {
			r.installed = true
			r.version = inst.Version
			r.source = inst.Source
		}
		rows = append(rows, r)
		seen[name] = true
	}
	// Lock-only entries (e.g. installed from a URL the
	// current binary doesn't carry) get a separate "external"
	// row. v1 has no URL plugins, but the listing has to
	// surface them when they appear.
	for name, inst := range installed {
		if seen[name] {
			continue
		}
		rows = append(rows, row{
			name:      name,
			version:   inst.Version,
			source:    inst.Source,
			installed: true,
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].name < rows[j].name })

	for _, r := range rows {
		state := "available"
		if r.installed {
			state = "installed"
		}
		fmt.Fprintf(core.Stdout, "%-20s %-12s %-9s (%s)\n", r.name, r.version, state, r.source)
	}
	return nil
}

// InstallCmd implements `w17ctl plugin install <name|url>`.
//
// v1 supports name-only — the named plugin must exist in the
// embedded catalog. URLs are accepted at the argument layer to
// reserve the surface for v2 but rejected at the handler with
// a steering message.
//
// Install:
//
//  1. Resolve the project root + lock.
//  2. Resolve the named plugin from the embedded catalog.
//  3. Load + validate plugin.yaml; refuse the install on
//     manifest errors (missing fields, name mismatch, …).
//  4. Run [plugins.CheckRequires] against the running w17
//     version (currently a no-op — pre-launch — but the gate
//     is plumbed for when a version constant lands).
//  5. Target dir = `<root>/<proto_dir>/plugins/<name>/`.
//     Refuse if the dir already exists (v1 has no `remove`;
//     operator manually deletes per the implementation plan).
//     Refuse if `<name>` already in lock.plugins[] regardless
//     of disk state (operator must clean up first).
//  6. Walk the embedded tree + write each file to the target.
//  7. Append `PluginInstall{name, version, source: "internal"}`
//     to the lock + save (re-signs).
type InstallCmd struct {
	Source  string `arg:"" name:"name|url" help:"Plugin name (embedded catalog) or URL (v2 — not yet supported)."`
	Console string `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console. Optional — falls back to the binary's compile-time default. Used to validate the plugin manifest + check requirements."`
}

func (c *InstallCmd) Run() error {
	if isPluginURL(c.Source) {
		return fmt.Errorf(
			"plugin install: URL plugins are a v2 feature; install by name from the internal catalog (see `w17ctl plugin list`).\n"+
				"  got: %s", c.Source,
		)
	}
	name := strings.TrimSpace(c.Source)
	if name == "" {
		return fmt.Errorf("plugin install: name argument is required")
	}

	root, err := core.FindProjectRoot()
	if err != nil {
		return fmt.Errorf("plugin install: %w", err)
	}
	lockPath := filepath.Join(root, "w17", "lock.yaml")
	protoDir := lockProtoDir(c.Console, root)

	embedRoot, err := internalPluginsRoot()
	if err != nil {
		return fmt.Errorf("plugin install: %w", err)
	}
	if _, statErr := fs.Stat(embedRoot, name); statErr != nil {
		available := availableEmbedded()
		hint := strings.Join(available, ", ")
		if hint == "" {
			hint = "(catalog empty)"
		}
		return fmt.Errorf("plugin install: %q not in internal catalog. available: %s", name, hint)
	}

	manifestData, err := fs.ReadFile(embedRoot, name+"/plugin.yaml")
	if err != nil {
		return fmt.Errorf("plugin install: read embedded plugin.yaml: %w", err)
	}

	installed := installedFromLock(lockPath)
	if _, dup := installed[name]; dup {
		return fmt.Errorf(
			"plugin install: %q already recorded in lock.plugins[]. v1 has no `plugin remove`;\n"+
				"  fix: manually `rm -rf %s` AND delete the lock entry (re-sign via `w17ctl <op>`) before reinstalling",
			name, filepath.Join(root, protoDir, "plugins", name),
		)
	}

	cl, conn, err := dialCodegen(c.Console)
	if err != nil {
		return fmt.Errorf("plugin install: %w", err)
	}
	defer func() { _ = conn.Close() }()
	// Parse + validate the manifest + run the requirement check server-side.
	manifest, err := inspectManifest(cl, manifestData, "embed:"+name+"/plugin.yaml", installed)
	if err != nil {
		return err
	}
	if manifest.GetName() != name {
		return fmt.Errorf(
			"plugin install: manifest name %q does not match catalog dir %q\n"+
				"  why: install path basename + manifest name must agree so `w17ctl plugin <op> %s` is unambiguous",
			manifest.GetName(), name, name,
		)
	}
	for _, w := range manifest.GetWarnings() {
		fmt.Fprintf(core.Stdout, "plugin install: warning: %s\n", w)
	}

	targetDir := filepath.Join(root, protoDir, "plugins", name)
	if _, statErr := os.Stat(targetDir); statErr == nil {
		return fmt.Errorf(
			"plugin install: %s already exists. v1 has no `plugin remove`;\n"+
				"  fix: manually `rm -rf %s` before reinstalling",
			targetDir, targetDir,
		)
	}
	if err := extractEmbeddedPlugin(embedRoot, name, targetDir); err != nil {
		return fmt.Errorf("plugin install: %w", err)
	}

	// Record the install in the lock via the InstallPlugin EditLock intent
	// (the server appends + re-signs; the client ships opaque lock bytes).
	lockBytes, readErr := os.ReadFile(lockPath)
	if readErr != nil && !os.IsNotExist(readErr) {
		_ = os.RemoveAll(targetDir)
		return fmt.Errorf("plugin install: read lock: %w", readErr)
	}
	newBytes, err := core.EditLock(c.Console, lockBytes, &codegenpb.LockEditIntent{
		Intent: &codegenpb.LockEditIntent_InstallPlugin{
			InstallPlugin: &codegenpb.InstallPluginIntent{
				Name: manifest.GetName(), Version: manifest.GetVersion(), Source: "internal",
			},
		},
	})
	if err != nil {
		// Roll back the freshly-extracted tree so a retry isn't blocked by the
		// "already exists" guard above (install created targetDir from scratch).
		_ = os.RemoveAll(targetDir)
		return fmt.Errorf("plugin install: record in lock: %w", err)
	}
	if err := os.WriteFile(lockPath, newBytes, 0o644); err != nil {
		_ = os.RemoveAll(targetDir)
		return fmt.Errorf("plugin install: write lock: %w", err)
	}
	fmt.Fprintf(core.Stdout, "plugin install: %s@%s → %s (lock re-signed)\n", manifest.GetName(), manifest.GetVersion(), targetDir)
	return nil
}

// UpdateCmd implements `w17ctl plugin update <name>` and
// `w17ctl plugin update --all`. Both branches share the same
// per-plugin logic; --all just iterates over every installed
// plugin in the lock.
//
// Per-plugin update:
//
//  1. Lock entry must exist for `<name>` (otherwise the operator
//     wants `plugin install` instead).
//  2. Embedded catalog must carry `<name>` (URL-installed
//     plugins can't be updated through this binary).
//  3. Manifest loads + validates; CheckRequires runs against
//     the running w17 version.
//  4. Target dir is wiped + re-populated from the embedded
//     tree. Manifest's `version` becomes the new
//     `lock.plugins[].version`.
//  5. Lock is re-signed once at the end (one save per command
//     run regardless of how many plugins update).
type UpdateCmd struct {
	Name    string `arg:"" optional:"" name:"name" help:"Installed plugin to refresh. Mutually exclusive with --all."`
	All     bool   `name:"all" help:"Update every plugin recorded in lock.plugins[]."`
	Console string `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console. Optional — falls back to the binary's compile-time default. Used to validate the plugin manifest + check requirements."`
}

func (c *UpdateCmd) Run() error {
	if c.All && c.Name != "" {
		return fmt.Errorf("plugin update: pass --all OR a name, not both")
	}
	if !c.All && c.Name == "" {
		return fmt.Errorf("plugin update: pass a plugin name or --all")
	}

	root, err := core.FindProjectRoot()
	if err != nil {
		return fmt.Errorf("plugin update: %w", err)
	}
	lockPath := filepath.Join(root, "w17", "lock.yaml")

	embedRoot, err := internalPluginsRoot()
	if err != nil {
		return fmt.Errorf("plugin update: %w", err)
	}

	installed := installedFromLock(lockPath)
	var targets []string
	if c.All {
		for name := range installed {
			targets = append(targets, name)
		}
		sort.Strings(targets)
	} else {
		targets = []string{c.Name}
	}
	if len(targets) == 0 {
		fmt.Fprintln(core.Stdout, "plugin update: no installed plugins")
		return nil
	}

	cl, conn, err := dialCodegen(c.Console)
	if err != nil {
		return fmt.Errorf("plugin update: %w", err)
	}
	defer func() { _ = conn.Close() }()

	protoDir := lockProtoDir(c.Console, root)
	// Each refresh is staged into a sibling temp dir (never the live
	// tree) so a failure — extract, EditLock, or a sibling update in an
	// --all run — leaves every live plugin dir AND the lock untouched.
	// Only after the EditLock write succeeds are the staged trees swapped in.
	type stagedSwap struct{ target, staging string }
	var swaps []stagedSwap
	removeStaging := func() {
		for _, s := range swaps {
			_ = os.RemoveAll(s.staging)
		}
	}
	// pending collects the per-plugin version bumps to apply in one batched
	// SetPluginVersions EditLock at the end (so a --all run re-signs once).
	var pending []*codegenpb.PluginVersion
	for _, name := range targets {
		existing, ok := installed[name]
		if !ok {
			removeStaging()
			return fmt.Errorf("plugin update: %q is not installed (run `w17ctl plugin install %s` first)", name, name)
		}
		if existing.Source != "internal" && existing.Source != "" {
			// URL plugins land in v2; the embedded catalog can't
			// refresh them. Skip with a warning rather than abort
			// so --all still updates everything it can.
			fmt.Fprintf(core.Stdout, "plugin update: skipping %s (source=%s; only embedded plugins can be refreshed through this binary)\n", name, existing.Source)
			continue
		}
		if _, statErr := fs.Stat(embedRoot, name); statErr != nil {
			removeStaging()
			return fmt.Errorf("plugin update: %q not in internal catalog of this w17 binary; rebuild w17ctl against a catalog that ships it", name)
		}
		manifestData, err := fs.ReadFile(embedRoot, name+"/plugin.yaml")
		if err != nil {
			removeStaging()
			return fmt.Errorf("plugin update: read embedded plugin.yaml for %s: %w", name, err)
		}
		manifest, err := inspectManifest(cl, manifestData, "embed:"+name+"/plugin.yaml", installed)
		if err != nil {
			removeStaging()
			return err
		}
		if manifest.GetName() != name {
			removeStaging()
			return fmt.Errorf("plugin update: manifest name %q does not match catalog dir %q", manifest.GetName(), name)
		}
		for _, w := range manifest.GetWarnings() {
			fmt.Fprintf(core.Stdout, "plugin update: warning: %s\n", w)
		}

		target := filepath.Join(root, protoDir, "plugins", name)
		staging := filepath.Join(root, protoDir, "plugins", "."+name+".w17tmp")
		_ = os.RemoveAll(staging) // clear a stale staging dir from a prior aborted run
		if err := extractEmbeddedPlugin(embedRoot, name, staging); err != nil {
			_ = os.RemoveAll(staging)
			removeStaging()
			return fmt.Errorf("plugin update: %w", err)
		}
		// Queue the version bump for the batched EditLock.
		pending = append(pending, &codegenpb.PluginVersion{
			Name: name, Version: manifest.GetVersion(), Source: "internal",
		})
		swaps = append(swaps, stagedSwap{target: target, staging: staging})
		fmt.Fprintf(core.Stdout, "plugin update: %s → %s\n", name, manifest.GetVersion())
	}

	if len(pending) == 0 {
		return nil
	}
	// Persist all version bumps via one SetPluginVersions EditLock (server
	// mutates + re-signs; client ships opaque lock bytes).
	lockBytes, readErr := os.ReadFile(lockPath)
	if readErr != nil {
		removeStaging()
		return fmt.Errorf("plugin update: read lock: %w", readErr)
	}
	newBytes, err := core.EditLock(c.Console, lockBytes, &codegenpb.LockEditIntent{
		Intent: &codegenpb.LockEditIntent_SetPluginVersions{
			SetPluginVersions: &codegenpb.SetPluginVersionsIntent{Plugins: pending},
		},
	})
	if err != nil {
		// Lock not updated → leave every live dir untouched; drop the
		// staged trees so a retry starts clean.
		removeStaging()
		return fmt.Errorf("plugin update: record in lock: %w", err)
	}
	if err := os.WriteFile(lockPath, newBytes, 0o644); err != nil {
		removeStaging()
		return fmt.Errorf("plugin update: write lock: %w", err)
	}
	// Lock persisted. Swap each staged tree into place — same-filesystem
	// sibling rename, so the replace is atomic per plugin.
	for _, s := range swaps {
		if err := os.RemoveAll(s.target); err != nil {
			return fmt.Errorf("plugin update: replace %s: %w", s.target, err)
		}
		if err := os.Rename(s.staging, s.target); err != nil {
			return fmt.Errorf("plugin update: install staged tree for %s: %w", s.target, err)
		}
	}
	fmt.Fprintln(core.Stdout, "plugin update: lock re-signed")
	return nil
}

func isPluginURL(s string) bool {
	s = strings.TrimSpace(s)
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "git+") || strings.HasPrefix(s, "ssh://")
}

func availableEmbedded() []string {
	names, err := internalPluginNames()
	if err != nil {
		return nil
	}
	sort.Strings(names)
	return names
}

// extractEmbeddedPlugin walks `embedRoot` under `<name>/` and
// writes every file to `target/` preserving the relative
// layout. plugin.yaml lands at `target/plugin.yaml`; the proto/
// subtree and the optional src/ subtree (authored Go: handlers/,
// lib/, gen/, plugin.go + go.mod + go.sum) mirror 1:1.
//
// .src → strip rename: the plugin tree on disk has real `.go`
// files (src/handlers/, src/lib/, src/gen/, src/plugin.go) + a
// real `src/go.mod` + `src/go.sum`. Mirroring those verbatim
// under srcgo/ would either (a) make .go files part of the
// srcgo Go module's `./...` walk + break the host build, or
// (b) cause `go:embed all:embed/plugins` to refuse the embed
// because Go treats a directory containing `go.mod` as a
// nested module and skips it. `make sync-plugins-internal`
// copies all four with a `.src` suffix (`.go.src`, `go.mod.src`,
// `go.sum.src`) so Go tooling treats them as inert data while
// embedded; we strip the suffix here so the extracted project
// dir has real `.go` + `go.mod` + `go.sum` ready for the
// consuming project's codegen.
func extractEmbeddedPlugin(embedRoot fs.FS, name, target string) error {
	src := name
	if err := os.MkdirAll(target, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", target, err)
	}
	return fs.WalkDir(embedRoot, src, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		dst := filepath.Join(target, rel)
		// Strip the `.src` suffix from any file that carries it
		// (.go.src, go.mod.src, go.sum.src). `make sync-plugins-internal`
		// adds the suffix at mirror time to keep these files inert
		// during the host build / go:embed pass; extract restores
		// the real names for the consuming project's tooling.
		dst = strings.TrimSuffix(dst, ".src")
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		body, err := fs.ReadFile(embedRoot, path)
		if err != nil {
			return fmt.Errorf("read embed %s: %w", path, err)
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", filepath.Dir(dst), err)
		}
		if err := os.WriteFile(dst, body, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", dst, err)
		}
		return nil
	})
}
