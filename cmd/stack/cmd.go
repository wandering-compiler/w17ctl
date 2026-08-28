package stack

import (
	"fmt"
	"os"
	"path/filepath"

	project "github.com/wandering-compiler/w17ctl/cmd/project"

	"github.com/wandering-compiler/w17ctl/internal/autosync"
	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/docker"
	"github.com/wandering-compiler/w17ctl/internal/remotecompose"

	"github.com/wandering-compiler/w17ctl/internal/devconfig"
)

// Project-management verbs folded in from the formerly-generated
// manage.sh. The compose verbs shell out to `docker compose` in the
// project root (where compose.yaml + compose.w17.yaml live); clean
// removes codegen output while preserving hand-written packages. `gen`
// is `w17ctl codegen`; the Go test suite is plain `go test ./...`.

// Cmd is the `w17ctl stack` parent — the local docker-compose
// lifecycle verbs (up / down / logs / ps / restart). `clean` stays
// top-level: it removes codegen output, not the compose stack.
type Cmd struct {
	Build   BuildCmd   `cmd:"" help:"Compile + build images, then dev diff-apply the current proto to the local stores (no run). See 'stack build --help'."`
	Up      UpCmd      `cmd:"" help:"Bring the local compose stack (or named services) up: docker compose up -d. Pass --build to rebuild images first; run 'stack build' for dev diff-apply."`
	Down    DownCmd    `cmd:"" help:"Tear the local compose stack down: docker compose down -v (drops volumes unless --keep-volumes)."`
	Reset   ResetCmd   `cmd:"" help:"Recover a tangled local dev DB: wipe the stores, re-apply db/init (current schema), and adopt it as the checkpoint baseline so the next 'stack build' is a no-op. The fix for the first-build db/init collision + any wiped-volume/checkpoint drift."`
	Logs    LogsCmd    `cmd:"" help:"Follow service logs: docker compose logs -f."`
	Ps      PsCmd      `cmd:"" help:"List the stack's containers: docker compose ps."`
	Restart RestartCmd `cmd:"" help:"Restart the stack (or named services): docker compose restart."`

	// Remote-mode registry + selection (docs/experiments/remote-stack.md):
	// offload the whole stack to an SSH docker host, apps still on localhost.
	Remote  RemoteCmd   `cmd:"" help:"Manage remote docker hosts (add / list / remove) + pin this project (remote use <name>). See 'stack remote --help'."`
	Local   UseLocalCmd `cmd:"" help:"Pin THIS project back to local mode (the counterpart to 'stack remote use')."`
	SetMode SetModeCmd  `cmd:"" name:"set-mode" help:"Set the GLOBAL default stack mode (local | remote) for projects that don't pin one."`
	Bind    BindCmd     `cmd:"" help:"Choose the host interface docker publishes ports on: loopback (127.0.0.1, secure default) | public (0.0.0.0). Public+remote is guarded by a typed confirmation."`
}

// UpCmd — bring the local stack (or named services) up.
//
// Unlike a raw `docker compose up`, this drives through the
// dev-machine-local project registry (devconfig): it auto-registers the
// project, allocates it unique host ports across ALL installed projects,
// and injects those ports (plus an optional preset's extra env) into the
// compose subprocess. So two projects never collide on an exposed port
// without any .env juggling. A preset (`--preset`, or the project's
// active preset) also selects which services to start — handy to run
// just the admin binary on a weak machine.
type UpCmd struct {
	Services []string `arg:"" optional:"" help:"Services to start; overrides the preset's selection. Empty = the preset's services, or the whole stack."`
	Preset   string   `name:"preset" short:"p" help:"Run preset to apply (services + extra env). Empty = the project's active preset, if any."`
	Build    bool     `name:"build" help:"Rebuild images before starting (docker compose up -d --build). For dev diff-apply of the proto to local stores, run 'stack build' (with --proto/--target) first."`
	modeFlags
}

func (c *UpCmd) Run() error {
	root, err := core.FindProjectRoot()
	if err != nil {
		return err
	}
	cfg, err := core.LoadDevConfigFn()
	if err != nil {
		return err
	}
	name, err := project.EnsureRegistered(cfg, root)
	if err != nil {
		return err
	}
	p := cfg.Projects[name]

	// Re-sync host-port assignments so a connection / bundle added since
	// the last up is allocated a (still-unique) port.
	slots, err := project.SyncPorts(cfg, name, root)
	if err != nil {
		return err
	}
	if err := core.SaveDevConfigFn(cfg); err != nil {
		return err
	}

	// Resolve the preset: explicit flag, else the project's active one.
	presetName := c.Preset
	if presetName == "" {
		presetName = p.ActivePreset
	}
	var preset *devconfig.Preset
	if presetName != "" {
		preset = p.Presets[presetName]
		if preset == nil {
			return fmt.Errorf("project %q has no preset %q", name, presetName)
		}
		fmt.Fprintf(core.Stdout, "applying preset %q\n", presetName)
	}

	services := project.PresetServices(c.Services, preset)
	env := project.BuildUpEnv(p, preset)

	mode, err := cfg.ResolveMode(root, c.Mode)
	if err != nil {
		return err
	}
	// Inject the mode-aware host bind: remote defaults to loopback (secure —
	// only the SSH tunnel reaches it), local to 0.0.0.0 (container consumers
	// like e2e tests need it). An explicit `stack bind` wins.
	env = append(env, "W17_BIND_HOST="+devconfig.ResolveBindHost(mode, p.Bind))

	if mode == devconfig.ModeRemote {
		return c.runRemote(cfg, root, name, p, env, services)
	}

	// Preflight: catch a foreign process holding one of the host ports we
	// are about to publish, and fail with a readable error before docker
	// even tries to bind (which would otherwise crash mid-bring-up).
	if err := preflightPorts(root, p, slots, services); err != nil {
		return err
	}
	args := []string{"up", "-d"}
	if c.Build {
		args = append(args, "--build")
	}
	args = append(args, services...)
	if err := docker.RunComposeEnvFn(root, env, args...); err != nil {
		return err
	}
	// `stack up` doesn't sync the DB to the current branch (that's
	// `stack build`'s job) — nudge if it looks stale after a switch.
	if hint := autosync.StaleDBHint(root); hint != "" {
		fmt.Fprintln(core.Stdout, hint)
	}
	return nil
}

// runRemote brings the stack up on the resolved remote docker host: push
// the tree (rsync), `docker compose up -d` over SSH (ports publish on the
// server), then open the localhost tunnels so the dev UX is unchanged.
// Port preflight is skipped — the ports publish on the REMOTE host, and
// the local tunnel bind is checked when the forwards open.
func (c *UpCmd) runRemote(cfg *devconfig.Config, root, name string, p *devconfig.Project, env, services []string) error {
	tgt, err := c.resolveRemote(cfg, root, name)
	if err != nil {
		return err
	}
	if err := syncTree(root, tgt, name); err != nil {
		return fmt.Errorf("stack up: rsync: %w", err)
	}
	args := []string{"up", "-d"}
	if c.Build {
		args = append(args, "--build")
	}
	args = append(args, services...)
	if err := remotecompose.Run(tgt.Runner, env, args...); err != nil {
		return fmt.Errorf("stack up: remote compose: %w", err)
	}
	if err := openTunnels(name, tgt, p.Ports); err != nil {
		return fmt.Errorf("stack up: tunnels: %w", err)
	}
	if hint := autosync.StaleDBHint(root); hint != "" {
		fmt.Fprintln(core.Stdout, hint)
	}
	return nil
}

// DownCmd — tear the local stack down. Drops volumes by default
// (matches the former manage.sh `down -v`); --keep-volumes preserves data.
type DownCmd struct {
	KeepVolumes bool `name:"keep-volumes" help:"Keep volume data (omit -v). Default drops volumes — ALL local DB data is lost."`
	modeFlags
}

func (c *DownCmd) Run() error {
	root, err := core.FindProjectRoot()
	if err != nil {
		return err
	}
	args := []string{"down"}
	if !c.KeepVolumes {
		args = append(args, "-v")
	}
	mode, cfg, err := c.resolveMode(root)
	if err != nil {
		return err
	}
	if mode == devconfig.ModeRemote {
		name, err := projectNameFromLock(root)
		if err != nil {
			return err
		}
		tgt, err := c.resolveRemote(cfg, root, name)
		if err != nil {
			return err
		}
		// Tear the remote stack down first, then always drop the tunnels
		// (even if compose errored — a half-open tunnel is worse).
		cerr := remotecompose.Run(tgt.Runner, nil, args...)
		if terr := closeTunnels(name); terr != nil && cerr == nil {
			cerr = terr
		}
		return cerr
	}
	return docker.RunComposeFn(root, args...)
}

// LogsCmd — follow service logs.
type LogsCmd struct {
	Services []string `arg:"" optional:"" help:"Services to tail; empty = all."`
	NoFollow bool     `name:"no-follow" help:"Print current logs and exit instead of following."`
	modeFlags
}

func (c *LogsCmd) Run() error {
	root, err := core.FindProjectRoot()
	if err != nil {
		return err
	}
	args := []string{"logs"}
	if !c.NoFollow {
		args = append(args, "-f")
	}
	args = append(args, c.Services...)
	return c.composeVerb(root, args...)
}

// composeVerb runs a context-free compose verb (logs/ps/restart) in the
// resolved mode — locally in the project root, or remote-exec over SSH.
// No rsync / tunnel changes: these verbs act on an already-running stack.
func (m modeFlags) composeVerb(root string, args ...string) error {
	mode, cfg, err := m.resolveMode(root)
	if err != nil {
		return err
	}
	if mode == devconfig.ModeRemote {
		name, err := projectNameFromLock(root)
		if err != nil {
			return err
		}
		tgt, err := m.resolveRemote(cfg, root, name)
		if err != nil {
			return err
		}
		return remotecompose.Run(tgt.Runner, nil, args...)
	}
	return docker.RunComposeFn(root, args...)
}

// PsCmd — list the stack's containers.
type PsCmd struct {
	modeFlags
}

func (c *PsCmd) Run() error {
	root, err := core.FindProjectRoot()
	if err != nil {
		return err
	}
	return c.composeVerb(root, "ps")
}

// RestartCmd — restart the stack (or named services).
type RestartCmd struct {
	Services []string `arg:"" optional:"" help:"Services to restart; empty = all."`
	modeFlags
}

func (c *RestartCmd) Run() error {
	root, err := core.FindProjectRoot()
	if err != nil {
		return err
	}
	return c.composeVerb(root, append([]string{"restart"}, c.Services...)...)
}

// CleanCmd — remove codegen output (pb stubs, FE clients, languages, the
// generated bits of each bundle) while preserving hand-written packages.
type CleanCmd struct {
	Console string `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console (owns the lock — read for the codegen-owned paths). Optional — falls back to the binary's compile-time default."`
}

func (c *CleanCmd) Run() error {
	root, err := core.FindProjectRoot()
	if err != nil {
		return err
	}
	// The codegen-owned paths (+ services dir) come from the console's lock
	// projection (§8.2 — the client holds no lock types).
	view, err := core.DescribeLockFromRoot(c.Console, root)
	if err != nil {
		return fmt.Errorf("clean: %w", err)
	}
	servicesDir := view.GetServicesDir()
	for _, rel := range view.GetCleanPaths() {
		abs := filepath.Join(root, rel)
		if _, statErr := os.Stat(abs); statErr != nil {
			continue
		}
		if rel == servicesDir {
			cleanServicesDir(abs)
			continue
		}
		if err := os.RemoveAll(abs); err != nil {
			return fmt.Errorf("clean %s: %w", rel, err)
		}
		fmt.Fprintf(core.Stdout, "removed %s\n", rel)
	}
	return nil
}

// cleanServicesDir removes the codegen-owned children of every bundle
// under servicesDir, leaving developer-authored sub-packages standing.
// Mirrors the former manage.sh `_w17_clean_services_dir`. The codegen-
// reserved children are deleted; a bundle's src/ + the bundle dir + the
// services dir are dropped only when nothing hand-written survives
// underneath (os.Remove on a non-empty dir fails silently, like rmdir).
func cleanServicesDir(servicesDir string) {
	entries, err := os.ReadDir(servicesDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		bundle := filepath.Join(servicesDir, e.Name())
		// Generated subtrees: storage (src/mutation, src/query,
		// src/eventbus) + gateway (src/rest, src/mcp transports +
		// schema/ docs). downloads.go / w17events.go live under
		// src/rest now, covered by the src/rest RemoveAll.
		for _, sub := range []string{"deploy", "src/mutation", "src/query", "src/eventbus", "src/rest", "src/mcp", "schema"} {
			_ = os.RemoveAll(filepath.Join(bundle, sub))
		}
		for _, f := range []string{
			"go.mod", "go.sum", "Dockerfile", "compose.yaml",
			".env", ".env.defaults", ".env.example",
			"src/main.go",
		} {
			_ = os.Remove(filepath.Join(bundle, f))
		}
		_ = os.Remove(filepath.Join(bundle, "src")) // drops src/ only if empty
		_ = os.Remove(bundle)                       // drops bundle only if empty
	}
	_ = os.Remove(servicesDir) // drops services dir only if empty
	fmt.Fprintf(core.Stdout, "cleaned %s (preserved hand-written packages)\n", servicesDir)
}
