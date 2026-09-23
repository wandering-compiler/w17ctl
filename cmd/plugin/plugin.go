package plugin

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/lockfile"
	"github.com/wandering-compiler/w17ctl/internal/pluginfetch"
	codegenpb "github.com/wandering-compiler/sdk/go/pb/w17compiler"
	"github.com/wandering-compiler/sdk/go/tooling/pathguard"
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
// plugin commands. The caller closes the returned conn.
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

// catalogue reports what the organisation's registry publishes, by reading its
// tags — one entry per plugin, at its highest release.
//
// It used to ask the console, which carried a catalogue compiled into it and
// answered from that. The console stopped serving plugin trees when a version
// became a git tag, so the question moved to where the answer now lives. The
// client does the listing for the same reason it does the fetching: the
// registry may be a repository only this machine's credentials can reach.
//
// Best effort by design. A registry that cannot be reached must not stop
// `plugin list` from reporting what the LOCK says is installed — that half is
// local, always available, and usually the half being asked about.
func catalogue(_ codegenpb.CodegenServiceClient) ([]*codegenpb.CataloguePlugin, error) {
	repo := pluginsRepo()
	names, err := pluginfetch.PublishedPlugins(context.Background(), repo)
	if err != nil {
		return nil, err
	}
	out := make([]*codegenpb.CataloguePlugin, 0, len(names))
	for _, n := range names {
		v, verr := pluginfetch.LatestVersion(context.Background(), repo, n)
		if verr != nil {
			continue
		}
		out = append(out, &codegenpb.CataloguePlugin{
			Name: n, Version: strings.TrimPrefix(v, "v"), Description: repo,
		})
	}
	return out, nil
}

// catalogueError turns the two refusals a catalogue RPC has that the operator
// can act on into CLI sentences.
//
// NotFound: the server's refusal already names what this console DOES serve;
// what it cannot know is which command lists it.
//
// Unimplemented: the console predates the catalogue RPCs. gRPC renders that as
// `unknown method FetchPlugin for service w17lock.console.rpc.Codegen`, which
// names a symbol and not a situation — and the situation is the whole point of
// this move, so it is the one failure that must not read as an internal error.
// The client no longer carries a catalogue, so an old console cannot serve
// `plugin install` at all; the fix is a console DEPLOY, and nothing about the
// raw status says so.
//
// Everything else passes through unchanged: a connect error, or an INTERNAL
// from a console that cannot read its own catalogue, is not "you asked for the
// wrong plugin", and dressing it as one sends the operator to the wrong place.
func catalogueError(op string, err error) error {
	st, ok := status.FromError(err)
	if !ok {
		return fmt.Errorf("%s: %w", op, err)
	}
	switch st.Code() {
	case codes.NotFound:
		return fmt.Errorf("%s: %s\n  fix: `w17ctl plugin list` names what this console serves", op, st.Message())
	case codes.Unimplemented:
		return fmt.Errorf(
			"%s: this console is older than this client.\n"+
				"  why: the client fetches a plugin from its registry and asks the console to\n"+
				"       validate the manifest before anything is written. That method is missing\n"+
				"       here, so the console cannot vouch for what was fetched — and installing\n"+
				"       without it would put an unchecked tree in the project.\n"+
				"  fix: deploy a console built from this version (or pin an older w17ctl).\n"+
				"  raw: %s", op, st.Message(),
		)
	}
	return fmt.Errorf("%s: %w", op, err)
}

// ListCmd implements `w17ctl plugin list`. Asks the console for its plugin
// catalogue and cross-references the lock's `plugins[]` to surface install
// state. One line per plugin; reports "no plugins available" when the console
// serves none and the lock records none.
type ListCmd struct {
	LockPath string `name:"lock" placeholder:"PATH" default:"w17/lock.yaml" help:"Path to the lock file."`
	Console  string `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console. Optional — falls back to the binary's compile-time default. Serves the plugin catalogue."`
}

func (c *ListCmd) Run() error {
	installed := installedFromLock(c.LockPath)

	cl, conn, err := dialCodegen(c.Console)
	if err != nil {
		return fmt.Errorf("plugin list: %w", err)
	}
	defer func() { _ = conn.Close() }()

	served, err := catalogue(cl)
	if err != nil {
		return fmt.Errorf("plugin list: %w", err)
	}
	if len(served) == 0 && len(installed) == 0 {
		fmt.Fprintln(core.Stdout, "no plugins available (console catalogue empty; no installs in lock)")
		return nil
	}

	// Everything the console serves — always listed, so the catalogue version
	// (which install would record) is visible next to what is installed.
	type row struct {
		name      string
		version   string
		source    string
		desc      string
		installed bool
	}
	rows := []row{}
	seen := map[string]bool{}
	for _, p := range served {
		r := row{name: p.GetName(), version: p.GetVersion(), source: "console", desc: p.GetDescription()}
		if inst, ok := installed[p.GetName()]; ok {
			r.installed = true
			r.version = inst.Version
			r.source = inst.Source
		}
		rows = append(rows, r)
		seen[p.GetName()] = true
	}
	// Lock-only entries (e.g. installed from a URL, or from a console whose
	// catalogue no longer carries the plugin) get a separate "external" row.
	// v1 has no URL plugins, but the listing has to surface them when they
	// appear.
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
		// The description answers "what is this for", which is otherwise only
		// discoverable by installing the plugin and reading its manifest.
		if d := firstSentence(r.desc); d != "" {
			fmt.Fprintf(core.Stdout, "  %s\n", d)
		}
	}
	return nil
}

// InstallCmd implements `w17ctl plugin install <name|url>`.
//
// v1 supports name-only — the named plugin must exist in the console's
// catalogue. URLs are accepted at the argument layer to reserve the surface
// for v2 but rejected at the handler with a steering message.
//
// Install:
//
//  1. Resolve the project root + lock.
//  2. Refuse if `<name>` is already in lock.plugins[], or if
//     `<root>/<proto_dir>/plugins/<name>/` already exists — v1 has no
//     `remove`, so the operator cleans up first. Both are local checks and
//     run BEFORE the console is dialled: a repeat install should not stream a
//     plugin tree only to refuse it.
//  3. Stream the plugin from the console into a sibling STAGING dir.
//  4. Validate the staged plugin.yaml server-side (InspectPluginManifest):
//     manifest errors, name/dir disagreement, requirement warnings.
//  5. Append `PluginInstall{name, version, source: "internal"}` to the lock
//     via EditLock (the server appends + re-signs).
//  6. Only then rename the staged tree into place.
type InstallCmd struct {
	Source  string `arg:"" name:"name|repo#tag" help:"Plugin name (the console's catalogue), or a release in a repository: <repo>#<plugin>/<version>."`
	Console string `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console. Optional — falls back to the binary's compile-time default. Serves the plugin catalogue + validates the manifest."`
}

func (c *InstallCmd) Run() error {
	var git *gitSpec
	if looksLikeGitSpec(c.Source) {
		spec, err := parseGitSpec(c.Source)
		if err != nil {
			return fmt.Errorf("plugin install: %w", err)
		}
		git = &spec
	} else if name := strings.TrimSpace(c.Source); name != "" && !isPluginURL(name) {
		// A bare name is the organisation's own registry with the URL filled
		// in, resolved to its highest published release. Same mechanism, same
		// pin recorded — the only difference is who typed the repository.
		repo := pluginsRepo()
		version, verr := pluginfetch.LatestVersion(context.Background(), repo, name)
		if verr != nil {
			// The hint belongs ONLY to a registry that answered. Telling
			// someone whose remote is unreachable to go and read a list they
			// also cannot fetch sends them at the wrong problem.
			if errors.Is(verr, pluginfetch.ErrNoReleases) {
				return fmt.Errorf("plugin install: %w\n  fix: `w17ctl plugin list` names what this registry publishes", verr)
			}
			return fmt.Errorf("plugin install: %w", verr)
		}
		git = &gitSpec{Repo: repo, Plugin: name, Version: version}
	} else if isPluginURL(c.Source) {
		return fmt.Errorf(
			"plugin install: a repository install names the RELEASE too — `<repo>#<plugin>/<version>`.\n"+
				"  got:  %s\n"+
				"  want: %s#auth/v0.1.0-rc.1\n"+
				"  why:  a plugin version is a git tag, so the thing to install is a tag and not a repository",
			c.Source, strings.TrimSuffix(strings.TrimSpace(c.Source), "/"),
		)
	}

	name := strings.TrimSpace(c.Source)
	if git != nil {
		name = git.Plugin
	}
	if name == "" {
		return fmt.Errorf("plugin install: name argument is required")
	}

	root, err := core.FindProjectRoot()
	if err != nil {
		return fmt.Errorf("plugin install: %w", err)
	}
	lockPath := filepath.Join(root, "w17", "lock.yaml")
	protoDir := lockProtoDir(c.Console, root)

	installed := installedFromLock(lockPath)
	if _, dup := installed[name]; dup {
		return fmt.Errorf(
			"plugin install: %q already recorded in lock.plugins[]. v1 has no `plugin remove`;\n"+
				"  fix: manually `rm -rf %s` AND delete the lock entry (re-sign via `w17ctl <op>`) before reinstalling",
			name, filepath.Join(root, protoDir, "plugins", name),
		)
	}
	targetDir := filepath.Join(root, protoDir, "plugins", name)
	if _, statErr := os.Stat(targetDir); statErr == nil {
		return fmt.Errorf(
			"plugin install: %s already exists. v1 has no `plugin remove`;\n"+
				"  fix: manually `rm -rf %s` before reinstalling",
			targetDir, targetDir,
		)
	}

	cl, conn, err := dialCodegen(c.Console)
	if err != nil {
		return fmt.Errorf("plugin install: %w", err)
	}
	defer func() { _ = conn.Close() }()

	// Staged into a sibling temp dir rather than written straight to
	// targetDir: a failure anywhere below — stream, manifest refusal, or
	// EditLock — must leave the project exactly as it was, and a half-written
	// plugin tree that the "already exists" guard then blocks is the worst of
	// both outcomes.
	staging, err := newStagingDir(filepath.Join(root, protoDir, "plugins"), name)
	if err != nil {
		return err
	}
	var (
		manifestData []byte
		fetched      pluginfetch.Fetched
		manifestFrom = "console:" + name + "/plugin.yaml"
	)
	if git != nil {
		// The client fetches; the console still decides. Cloning is transport,
		// the same class of work as dialling or writing files — what stays
		// server-side is every judgement about the tree, starting with the
		// manifest check two statements below.
		fetched, err = pluginfetch.Fetch(context.Background(),
			pluginfetch.Source{Repo: git.Repo, Plugin: git.Plugin, Version: git.Version}, staging)
		if err != nil {
			_ = os.RemoveAll(staging)
			return fmt.Errorf("plugin install: %w", err)
		}
		manifestData, err = os.ReadFile(filepath.Join(staging, "plugin.yaml"))
		if err != nil {
			_ = os.RemoveAll(staging)
			return fmt.Errorf("plugin install: read fetched manifest: %w", err)
		}
		manifestFrom = git.Repo + "#" + git.Tag() + "/plugin.yaml"
	} else {
		manifestData, err = fetchPluginInto(cl, name, staging)
		if err != nil {
			_ = os.RemoveAll(staging)
			return catalogueError("plugin install", err)
		}
	}

	// Parse + validate the manifest + run the requirement check server-side.
	manifest, err := inspectManifest(cl, manifestData, manifestFrom, installed)
	if err != nil {
		_ = os.RemoveAll(staging)
		return catalogueError("plugin install", err)
	}
	if manifest.GetName() != name {
		_ = os.RemoveAll(staging)
		return fmt.Errorf(
			"plugin install: manifest name %q does not match catalog dir %q\n"+
				"  why: install path basename + manifest name must agree so `w17ctl plugin <op> %s` is unambiguous",
			manifest.GetName(), name, name,
		)
	}
	for _, w := range manifest.GetWarnings() {
		fmt.Fprintf(core.Stdout, "plugin install: warning: %s\n", w)
	}

	// Record the install in the lock via the InstallPlugin EditLock intent
	// (the server appends + re-signs; the client ships opaque lock bytes).
	lockBytes, readErr := os.ReadFile(lockPath)
	if readErr != nil && !os.IsNotExist(readErr) {
		_ = os.RemoveAll(staging)
		return fmt.Errorf("plugin install: read lock: %w", readErr)
	}
	newBytes, err := core.EditLock(c.Console, lockBytes, &codegenpb.LockEditIntent{
		Intent: &codegenpb.LockEditIntent_InstallPlugin{
			InstallPlugin: installIntent(manifest.GetName(), manifest.GetVersion(), git, fetched),
		},
	})
	if err != nil {
		_ = os.RemoveAll(staging)
		return fmt.Errorf("plugin install: record in lock: %w", err)
	}
	if err := os.WriteFile(lockPath, newBytes, 0o644); err != nil {
		_ = os.RemoveAll(staging)
		return fmt.Errorf("plugin install: write lock: %w", err)
	}
	// Lock persisted — same-filesystem sibling rename, so the tree appears
	// whole or not at all.
	if err := os.Rename(staging, targetDir); err != nil {
		_ = os.RemoveAll(staging)
		return fmt.Errorf("plugin install: install staged tree for %s: %w", name, err)
	}
	if git != nil {
		fmt.Fprintf(core.Stdout, "plugin install: %s@%s → %s (from %s at %s, lock re-signed)\n",
			manifest.GetName(), manifest.GetVersion(), targetDir, git.Tag(), shortSHA(fetched.SHA))
	} else {
		fmt.Fprintf(core.Stdout, "plugin install: %s@%s → %s (lock re-signed)\n", manifest.GetName(), manifest.GetVersion(), targetDir)
	}
	printActivationHint(manifest.GetName())
	return nil
}

// printActivationHint tells the operator the step that installing does NOT do.
//
// Installing writes the plugin's tree and records it in the lock. It does not
// switch anything on: a plugin is activated by naming it in a domain's
// `(w17.domain).plugins`, and until that happens `codegen` emits nothing for
// it — no file, no mention, no warning. A consumer reported that as a broken
// plugin, which is the right reading of the evidence they had.
//
// Printed here rather than documented somewhere because this is the moment the
// question is asked, and everything needed to answer it is in hand.
func printActivationHint(name string) {
	fmt.Fprintf(core.Stdout, `
Installing does not enable it. Add the plugin to a domain's sentinel
(<proto-dir>/domains/<domain>/w17.proto):

    import "w17/domain.proto";

    option (w17.domain) = {
      plugins: [
        { source_name: %q }
      ]
    };

then run `+"`w17ctl codegen --force`"+`. Until a domain names it, codegen emits
nothing for this plugin.
`, name)
}

// firstSentence trims a manifest description to its first sentence, so a list
// of plugins stays a list rather than becoming a page.
func firstSentence(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if i := strings.Index(s, ". "); i > 0 {
		return s[:i+1]
	}
	const max = 96
	if len(s) > max {
		return strings.TrimSpace(s[:max]) + "…"
	}
	return s
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
//  2. The console's catalogue must carry `<name>` (URL-installed
//     plugins can't be updated through this path).
//  3. The fetched manifest is validated server-side; CheckRequires runs
//     against the running w17 version.
//  4. The refreshed tree is staged, then swapped in. The manifest's
//     `version` becomes the new `lock.plugins[].version`.
//  5. Lock is re-signed once at the end (one save per command
//     run regardless of how many plugins update).
type UpdateCmd struct {
	Name    string `arg:"" optional:"" name:"name" help:"Installed plugin to refresh. Mutually exclusive with --all."`
	All     bool   `name:"all" help:"Update every plugin recorded in lock.plugins[]."`
	To      string `name:"to" placeholder:"vX.Y.Z" help:"For a git-sourced plugin: the release to move to. Omitted, the highest published one is used (release candidates included). Refused with --all, which spans plugins whose version lines are independent."`
	Console string `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console. Optional — falls back to the binary's compile-time default. Serves the plugin catalogue + validates the manifest."`
}

func (c *UpdateCmd) Run() error {
	if c.All && c.Name != "" {
		return fmt.Errorf("plugin update: pass --all OR a name, not both")
	}
	if !c.All && c.Name == "" {
		return fmt.Errorf("plugin update: pass a plugin name or --all")
	}
	if c.To != "" && c.All {
		return fmt.Errorf(
			"plugin update: --to names ONE release, and --all spans plugins whose version lines are independent\n"+
				"  fix: `w17ctl plugin update <name> --to %s`", c.To)
	}

	root, err := core.FindProjectRoot()
	if err != nil {
		return fmt.Errorf("plugin update: %w", err)
	}
	lockPath := filepath.Join(root, "w17", "lock.yaml")

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
	// tree) so a failure — fetch, EditLock, or a sibling update in an
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
		// `internal` and `git` are now the same road. `internal` always meant
		// "the organisation's own catalogue", and that catalogue became the
		// organisation's own REGISTRY — so an update refreshes it by name from
		// there and records the pin it did not have before. Every plugin
		// installed before the registry existed migrates on its first update,
		// which is the only moment the information needed to pin it is in hand.
		isGit := existing.Source == "git" || existing.Source == "internal" || existing.Source == ""
		if !isGit {
			// A source nothing here can refresh (a `url:` entry). Skip with a
			// warning rather than abort, so --all still updates what it can.
			fmt.Fprintf(core.Stdout, "plugin update: skipping %s (source=%s; only catalogue and git plugins can be refreshed)\n", name, existing.Source)
			continue
		}

		target := filepath.Join(root, protoDir, "plugins", name)
		staging, err := newStagingDir(filepath.Join(root, protoDir, "plugins"), name)
		if err != nil {
			return err
		}
		var (
			manifestData []byte
			fetched      pluginfetch.Fetched
			manifestFrom = "console:" + name + "/plugin.yaml"
		)
		if isGit {
			manifestData, fetched, err = updateFromGit(c.To, name, existing, staging)
			if err != nil {
				_ = os.RemoveAll(staging)
				removeStaging()
				return err
			}
			// fetched.Repo, not existing.Git.Repo: a plugin migrating from
			// `internal` has no pin yet, so the lock's is nil and reaching
			// through it panics. What was actually fetched always knows.
			manifestFrom = fetched.Repo + "#" + fetched.Ref + "/plugin.yaml"
		} else {
			manifestData, err = fetchPluginInto(cl, name, staging)
			if err != nil {
				_ = os.RemoveAll(staging)
				removeStaging()
				return catalogueError("plugin update", err)
			}
		}
		// Registered for cleanup only once the fetch has produced a tree, so
		// the failure arms above don't have to distinguish "staged" from
		// "about to be staged".
		swaps = append(swaps, stagedSwap{target: target, staging: staging})

		manifest, err := inspectManifest(cl, manifestData, manifestFrom, installed)
		if err != nil {
			removeStaging()
			return catalogueError("plugin update", err)
		}
		if manifest.GetName() != name {
			removeStaging()
			return fmt.Errorf("plugin update: manifest name %q does not match catalog dir %q", manifest.GetName(), name)
		}
		for _, w := range manifest.GetWarnings() {
			fmt.Fprintf(core.Stdout, "plugin update: warning: %s\n", w)
		}

		// Queue the version bump for the batched EditLock.
		pending = append(pending, updateIntent(name, manifest.GetVersion(), isGit, existing, fetched))
		if isGit {
			fmt.Fprintf(core.Stdout, "plugin update: %s → %s (%s at %s)\n",
				name, manifest.GetVersion(), fetched.Ref, shortSHA(fetched.SHA))
		} else {
			fmt.Fprintf(core.Stdout, "plugin update: %s → %s\n", name, manifest.GetVersion())
		}
	}

	if len(pending) == 0 {
		removeStaging()
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

// fetchPluginInto streams one plugin's tree from the console's catalogue into
// `target`, and returns the raw plugin.yaml it carried.
//
// The manifest comes back from HERE rather than from a second RPC because it
// arrives in the stream anyway, and the caller has to hold it: the tree is
// staged before the manifest is validated, so the bytes that get validated
// must be the bytes that were written — reading the manifest separately would
// leave a window where the console serves one plugin and the project gets
// another.
//
// Files are written AS THEY ARRIVE rather than accumulated: the codegen RPCs
// stream one file per message precisely so a project's size is not capped by a
// single gRPC message, and re-accumulating the stream client-side would put
// that cap straight back (see core.RecvGeneratedFiles).
//
// .src → strip rename: the plugin tree on disk has real `.go` files
// (src/handlers/, src/lib/, src/gen/, src/plugin.go) + a real `src/go.mod` +
// `src/go.sum`. Mirroring those verbatim into the console's own source tree
// would either (a) make .go files part of that module's `./...` walk + break
// the build, or (b) cause `go:embed` to refuse the tree because Go treats a
// directory containing `go.mod` as a nested module and skips it. `make
// sync-plugins-internal` copies all four with a `.src` suffix (`.go.src`,
// `go.mod.src`, `go.sum.src`) so Go tooling treats them as inert data on the
// server; the suffix is stripped here so the installed project dir has real
// `.go` + `go.mod` + `go.sum` ready for the consuming project's codegen.
// newStagingDir makes a staging directory nothing else will touch.
//
// ⚠️ Both call sites used a FIXED name, `.<plugin>.w17tmp`, and opened with
// `os.RemoveAll` "to clear a stale staging dir from a prior aborted run". Two
// runs staging the same plugin in one project therefore shared a directory and
// each one's opening move DELETED the other's half-fetched tree — and `w17ctl
// plugin update --all` in one terminal against an install in another is not an
// exotic pairing. No advisory lock is taken anywhere in these commands, so
// nothing upstream serialises them (T3-7 pass #15, D15-4).
//
// A unique name also removes the reason the RemoveAll was there: a dir this
// process just created cannot hold another run's leftovers. An aborted run
// leaves its own directory behind, which is litter rather than damage, and the
// `.`-prefix keeps it out of the way.
func newStagingDir(pluginsDir, name string) (string, error) {
	if err := os.MkdirAll(pluginsDir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", pluginsDir, err)
	}
	dir, err := os.MkdirTemp(pluginsDir, "."+name+".w17tmp-")
	if err != nil {
		return "", fmt.Errorf("staging dir for %s: %w", name, err)
	}
	return dir, nil
}

func fetchPluginInto(cl codegenpb.CodegenServiceClient, name, target string) ([]byte, error) {
	ctx, cancel := core.ClientCtx()
	defer cancel()
	stream, err := cl.FetchPlugin(ctx, &codegenpb.FetchPluginRequest{Name: name})
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", target, err)
	}

	var manifest []byte
	write := func(f *codegenpb.GeneratedFile) error {
		rel := strings.TrimSuffix(f.GetRelativePath(), ".src")
		if rel == "plugin.yaml" {
			manifest = f.GetContents()
		}
		// SERVER-SUPPLIED path: contain it under target so a buggy or
		// compromised console cannot escape the plugin dir via `..`/absolute.
		dst, err := pathguard.Join(target, rel)
		if err != nil {
			return fmt.Errorf("server file path %q escapes the plugin dir: %w", f.GetRelativePath(), err)
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", filepath.Dir(dst), err)
		}
		if err := os.WriteFile(dst, f.GetContents(), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", dst, err)
		}
		return nil
	}
	if _, err := core.RecvGeneratedFiles(stream, write); err != nil {
		return nil, err
	}
	if manifest == nil {
		// The console guarantees a manifest for anything it lists, so this is
		// a broken catalogue rather than a bad request — say which, because
		// the fix is a console deploy and not a different command.
		return nil, fmt.Errorf("plugin %q arrived from the console without a plugin.yaml (its catalogue is broken — redeploy the console)", name)
	}
	return manifest, nil
}
