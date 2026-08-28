package project

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/devconfig"
	"github.com/wandering-compiler/w17ctl/internal/docker"
	"github.com/wandering-compiler/w17ctl/internal/lockfile"
	"github.com/wandering-compiler/w17ctl/internal/remote"
	"github.com/wandering-compiler/w17ctl/internal/remotecompose"
)

// projectServicesDir is the FIXED project-relative root of the generated
// service-bundle tree (`<dir>/<domain>-<role>/`) — w17-owned, never
// configurable (mirrors lock.ServicesDir; the lock never stores it).
const projectServicesDir = "w17/services"

// loadProjectName reads the project name from <root>/w17/lock.yaml via the
// offline lockfile reader (no console, no lockpb). Errors when the lock is
// missing/unparseable or carries no project name.
func loadProjectName(root string) (string, error) {
	lk, err := lockfile.Load(filepath.Join(root, lockfile.DefaultPath))
	if err != nil {
		return "", err
	}
	if lk.Project == "" {
		return "", fmt.Errorf("lock at %s has no project name", root)
	}
	return lk.Project, nil
}

// Cmd is the `w17ctl project` parent — the dev-machine-local
// registry of installed w17 projects. It owns the unique host-port
// assignments w17ctl hands each project (so two projects' exposed ports
// never collide on one machine) and the per-project run presets. All
// state lives in `~/.w17/config.yaml` (devconfig) — NEVER the signed
// lock, because port maps + "run only admin on my laptop" presets are
// per-machine concerns, not shared reproducible build inputs.
type Cmd struct {
	List         ProjectListCmd   `cmd:"" help:"List registered projects with their assigned host ports + active preset."`
	ImportFrom   ImportFromCmd    `cmd:"" name:"import-from" help:"Fold a whole SOURCE project into this one: its migration history comes across verbatim (re-signed for this project) and its tables join this project's schema. The source is not modified. Refuses if the source's history is not consistent or if any connection / table / message name is claimed by both. NB: distinct from 'project import', which only registers a project locally and allocates host ports."`
	Import       ProjectImportCmd `cmd:"" help:"Register an existing w17 project (default: current dir) and allocate it unique host ports."`
	Remove       ProjectRemoveCmd `cmd:"" help:"Unregister a project from the local registry (frees its assigned host ports for reuse)."`
	Ports        ProjectPortsCmd  `cmd:"" help:"Re-sync + show a project's assigned host ports (run after adding a connection / bundle)."`
	Ps           ProjectPsCmd     `cmd:"" help:"Show running containers across ALL registered projects (docker compose ps per project)."`
	ReleaseToOrg ReleaseToOrgCmd  `cmd:"" name:"release-to-org" help:"CONSOLE: offer this project to another organization (step 1 of 2). Run as the CURRENT organization. Nothing moves until that organization claims it; --to '' withdraws the offer."`
	Claim        ClaimCmd         `cmd:"" help:"CONSOLE: take a project another organization offered you (step 2 of 2). Run as the TARGET organization — its token is what proves you may receive it."`
	Preset       ProjectPresetCmd `cmd:"" help:"Manage a project's run presets — add / remove / activate / list. A preset selects which services to start + extra env to layer on. See 'project preset --help'."`
}

// --- shared helpers -------------------------------------------------

// findProjectRootFrom walks up from start looking for a `w17/`
// directory, returning the absolute project root. Mirrors
// realFindProjectRoot but from an explicit start (for `project import
// <path>`).
func findProjectRootFrom(start string) (string, error) {
	abs, err := filepath.Abs(start)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", start, err)
	}
	dir := abs
	for {
		marker := filepath.Join(dir, "w17")
		if info, err := os.Stat(marker); err == nil && info.IsDir() {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("not a w17 project (no w17/ directory at or above %s)", abs)
		}
		dir = parent
	}
}

// SyncPorts re-introspects a project's compose tree and (re)allocates
// its host ports in cfg. Returns the discovered slots so callers can
// print them. Pure side effect on cfg — the caller saves.
func SyncPorts(cfg *devconfig.Config, name, root string) ([]devconfig.Slot, error) {
	slots, _, err := devconfig.IntrospectSlots(root, projectServicesDir)
	if err != nil {
		return nil, err
	}
	cfg.AllocatePorts(name, slots)
	return slots, nil
}

// printPorts writes a project's assigned ports as a stable, aligned
// list (sorted by env var).
func printPorts(p *devconfig.Project, slots []devconfig.Slot) {
	if len(p.Ports) == 0 {
		fmt.Fprintln(core.Stdout, "  (no host-published ports — run codegen first, then `project ports`)")
		return
	}
	svcOf := map[string]string{}
	for _, s := range slots {
		svcOf[s.EnvVar] = s.Service
	}
	keys := make([]string, 0, len(p.Ports))
	for k := range p.Ports {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if svc := svcOf[k]; svc != "" {
			fmt.Fprintf(core.Stdout, "  %-5d  %s  (%s)\n", p.Ports[k], k, svc)
		} else {
			fmt.Fprintf(core.Stdout, "  %-5d  %s\n", p.Ports[k], k)
		}
	}
}

// resolveProject looks up a registered project by name, erroring with a
// helpful message when missing.
func resolveProject(cfg *devconfig.Config, name string) (*devconfig.Project, error) {
	p := cfg.Projects[name]
	if p == nil {
		return nil, fmt.Errorf("project %q is not registered — run `w17ctl project import` in its directory first", name)
	}
	return p, nil
}

// --- project list ---------------------------------------------------

type ProjectListCmd struct{}

func (c *ProjectListCmd) Run() error {
	cfg, err := core.LoadDevConfigFn()
	if err != nil {
		return err
	}
	if len(cfg.Projects) == 0 {
		fmt.Fprintln(core.Stdout, "no registered projects — `w17ctl project import` to add one")
		return nil
	}
	names := make([]string, 0, len(cfg.Projects))
	for n := range cfg.Projects {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		p := cfg.Projects[n]
		active := p.ActivePreset
		if active == "" {
			active = "(full stack)"
		}
		fmt.Fprintf(core.Stdout, "%s\n  path:   %s\n  preset: %s\n  ports:  %d assigned\n", n, p.Path, active, len(p.Ports))
	}
	return nil
}

// --- project import -------------------------------------------------

type ProjectImportCmd struct {
	Path string `arg:"" optional:"" help:"Path to (or inside) the project; default current dir."`
}

func (c *ProjectImportCmd) Run() error {
	start := c.Path
	if start == "" {
		start = "."
	}
	root, err := findProjectRootFrom(start)
	if err != nil {
		return err
	}
	name, err := loadProjectName(root)
	if err != nil {
		return err
	}

	cfg, err := core.LoadDevConfigFn()
	if err != nil {
		return err
	}
	if existing := cfg.Projects[name]; existing != nil && existing.Path != root {
		return fmt.Errorf("project name %q already registered at a different path (%s); remove it first or rename this project", name, existing.Path)
	}
	if cfg.Projects[name] == nil {
		cfg.Projects[name] = &devconfig.Project{Path: root, Ports: map[string]int{}}
		fmt.Fprintf(core.Stdout, "registered %q at %s\n", name, root)
	} else {
		cfg.Projects[name].Path = root
		fmt.Fprintf(core.Stdout, "re-synced %q at %s\n", name, root)
	}

	slots, err := SyncPorts(cfg, name, root)
	if err != nil {
		return err
	}
	if err := core.SaveDevConfigFn(cfg); err != nil {
		return err
	}
	fmt.Fprintln(core.Stdout, "assigned host ports:")
	printPorts(cfg.Projects[name], slots)
	return nil
}

// --- project remove -------------------------------------------------

type ProjectRemoveCmd struct {
	Name string `arg:"" help:"Project name to unregister."`
}

func (c *ProjectRemoveCmd) Run() error {
	cfg, err := core.LoadDevConfigFn()
	if err != nil {
		return err
	}
	if _, ok := cfg.Projects[c.Name]; !ok {
		return fmt.Errorf("project %q is not registered", c.Name)
	}
	delete(cfg.Projects, c.Name)
	if err := core.SaveDevConfigFn(cfg); err != nil {
		return err
	}
	fmt.Fprintf(core.Stdout, "unregistered %q (its host ports are free for reuse)\n", c.Name)
	return nil
}

// --- project ports --------------------------------------------------

type ProjectPortsCmd struct {
	Name   string `arg:"" optional:"" help:"Project name; default = the project in the current dir."`
	Rebase bool   `name:"rebase" help:"Drop the project's current port assignments and re-allocate them fresh from the port base (skipping ports in use right now). Use to move a project off a stale/colliding block."`
}

func (c *ProjectPortsCmd) Run() error {
	cfg, err := core.LoadDevConfigFn()
	if err != nil {
		return err
	}
	name, p, err := c.target(cfg)
	if err != nil {
		return err
	}
	if c.Rebase {
		// Clear the sticky assignments so SyncPorts re-allocates every
		// slot from the base upward — the only way to move a project off
		// a previously-assigned block (e.g. one that now collides).
		p.Ports = map[string]int{}
	}
	slots, err := SyncPorts(cfg, name, p.Path)
	if err != nil {
		return err
	}
	if err := core.SaveDevConfigFn(cfg); err != nil {
		return err
	}
	fmt.Fprintf(core.Stdout, "%s — host ports:\n", name)
	printPorts(p, slots)
	return nil
}

// target resolves the project either from an explicit name or, when
// none is given, from the current working directory's registration.
func (c *ProjectPortsCmd) target(cfg *devconfig.Config) (string, *devconfig.Project, error) {
	if c.Name != "" {
		p, err := resolveProject(cfg, c.Name)
		return c.Name, p, err
	}
	root, err := core.FindProjectRoot()
	if err != nil {
		return "", nil, err
	}
	name, p := cfg.FindByPath(root)
	if p == nil {
		return "", nil, fmt.Errorf("the project at %s is not registered — run `w17ctl project import` first", root)
	}
	return name, p, nil
}

// --- project ps -----------------------------------------------------

type ProjectPsCmd struct{}

func (c *ProjectPsCmd) Run() error {
	cfg, err := core.LoadDevConfigFn()
	if err != nil {
		return err
	}
	if len(cfg.Projects) == 0 {
		fmt.Fprintln(core.Stdout, "no registered projects")
		return nil
	}
	names := make([]string, 0, len(cfg.Projects))
	for n := range cfg.Projects {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		p := cfg.Projects[n]
		fmt.Fprintf(core.Stdout, "=== %s (%s) ===\n", n, p.Path)
		if _, statErr := os.Stat(p.Path); statErr != nil {
			fmt.Fprintf(core.Stdout, "  (path missing — skipping)\n")
			continue
		}
		// Best-effort: a project with no stack up yet just prints an empty
		// table; a docker error shouldn't abort the whole sweep. A project
		// pinned to remote mode runs ps on its remote host over SSH so the
		// aggregate view isn't blank for off-machine stacks.
		if err := projectPs(cfg, n, p); err != nil {
			fmt.Fprintf(core.Stdout, "  (compose ps failed: %v)\n", err)
		}
	}
	return nil
}

// projectPs runs `docker compose ps` for one project in its resolved mode:
// locally, or on its remote host over SSH.
func projectPs(cfg *devconfig.Config, name string, p *devconfig.Project) error {
	mode, err := cfg.ResolveMode(p.Path, "")
	if err != nil {
		return err
	}
	if mode != devconfig.ModeRemote {
		return docker.RunComposeFn(p.Path, "ps")
	}
	_, r, err := cfg.ResolveRemote(p.Path, "")
	if err != nil {
		return err
	}
	dest, err := remote.ParseDest(r.SSH)
	if err != nil {
		return err
	}
	fmt.Fprintf(core.Stdout, "  (remote: %s)\n", r.SSH)
	out, err := remotecompose.Capture(remotecompose.Runner{Dest: dest, RemoteDir: path.Join(r.Path, name)}, "ps")
	if err != nil {
		return err
	}
	fmt.Fprint(core.Stdout, string(out))
	return nil
}

// --- project preset -------------------------------------------------

type ProjectPresetCmd struct {
	Add      PresetAddCmd      `cmd:"" help:"Create / overwrite a preset: which services to start (--service) + env to layer (--env KEY=VALUE)."`
	Remove   PresetRemoveCmd   `cmd:"" help:"Delete a preset."`
	Activate PresetActivateCmd `cmd:"" help:"Set a preset as the default applied by a bare 'stack up' (or --clear to reset to the full stack)."`
	List     PresetListCmd     `cmd:"" help:"List a project's presets."`
}

type PresetAddCmd struct {
	Project  string   `arg:"" help:"Project name."`
	Name     string   `arg:"" help:"Preset name."`
	Services []string `name:"service" help:"Compose service to start (repeatable); empty = whole stack."`
	Env      []string `name:"env" help:"Extra env as KEY=VALUE (repeatable), layered on top of the managed port vars."`
}

func (c *PresetAddCmd) Run() error {
	cfg, err := core.LoadDevConfigFn()
	if err != nil {
		return err
	}
	p, err := resolveProject(cfg, c.Project)
	if err != nil {
		return err
	}
	env, err := parseEnvKV(c.Env)
	if err != nil {
		return err
	}
	p.SetPreset(c.Name, &devconfig.Preset{Services: c.Services, Env: env})
	if err := core.SaveDevConfigFn(cfg); err != nil {
		return err
	}
	fmt.Fprintf(core.Stdout, "preset %q saved for %q (%d service(s), %d env override(s))\n", c.Name, c.Project, len(c.Services), len(env))
	return nil
}

type PresetRemoveCmd struct {
	Project string `arg:"" help:"Project name."`
	Name    string `arg:"" help:"Preset name."`
}

func (c *PresetRemoveCmd) Run() error {
	cfg, err := core.LoadDevConfigFn()
	if err != nil {
		return err
	}
	p, err := resolveProject(cfg, c.Project)
	if err != nil {
		return err
	}
	if !p.DeletePreset(c.Name) {
		return fmt.Errorf("project %q has no preset %q", c.Project, c.Name)
	}
	if err := core.SaveDevConfigFn(cfg); err != nil {
		return err
	}
	fmt.Fprintf(core.Stdout, "removed preset %q from %q\n", c.Name, c.Project)
	return nil
}

type PresetActivateCmd struct {
	Project string `arg:"" help:"Project name."`
	Name    string `arg:"" optional:"" help:"Preset name to make default."`
	Clear   bool   `name:"clear" help:"Reset to the full stack (no active preset)."`
}

func (c *PresetActivateCmd) Run() error {
	cfg, err := core.LoadDevConfigFn()
	if err != nil {
		return err
	}
	p, err := resolveProject(cfg, c.Project)
	if err != nil {
		return err
	}
	if c.Clear {
		p.ClearActivePreset()
	} else {
		if c.Name == "" {
			return fmt.Errorf("give a preset name to activate, or pass --clear to reset to the full stack")
		}
		if !p.Activate(c.Name) {
			return fmt.Errorf("project %q has no preset %q", c.Project, c.Name)
		}
	}
	if err := core.SaveDevConfigFn(cfg); err != nil {
		return err
	}
	if p.ActivePreset == "" {
		fmt.Fprintf(core.Stdout, "%q now defaults to the full stack\n", c.Project)
	} else {
		fmt.Fprintf(core.Stdout, "%q now defaults to preset %q\n", c.Project, p.ActivePreset)
	}
	return nil
}

type PresetListCmd struct {
	Project string `arg:"" help:"Project name."`
}

func (c *PresetListCmd) Run() error {
	cfg, err := core.LoadDevConfigFn()
	if err != nil {
		return err
	}
	p, err := resolveProject(cfg, c.Project)
	if err != nil {
		return err
	}
	if len(p.Presets) == 0 {
		fmt.Fprintf(core.Stdout, "%q has no presets\n", c.Project)
		return nil
	}
	names := make([]string, 0, len(p.Presets))
	for n := range p.Presets {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		pr := p.Presets[n]
		marker := ""
		if n == p.ActivePreset {
			marker = " (active)"
		}
		svc := "all services"
		if len(pr.Services) > 0 {
			svc = strings.Join(pr.Services, ", ")
		}
		fmt.Fprintf(core.Stdout, "%s%s\n  services: %s\n  env:      %d override(s)\n", n, marker, svc, len(pr.Env))
	}
	return nil
}

// BuildUpEnv / PresetServices delegate to devconfig — the single source
// of truth shared with the local UI so CLI and UI build identical
// compose invocations.
func BuildUpEnv(p *devconfig.Project, preset *devconfig.Preset) []string {
	return devconfig.BuildUpEnv(p, preset)
}

func PresetServices(explicit []string, preset *devconfig.Preset) []string {
	return devconfig.PresetServices(explicit, preset)
}

// EnsureRegistered finds (or auto-registers) the project rooted at root
// in cfg, returning its registry name. Auto-registration mirrors
// `project import` so a bare `stack up` works before an explicit
// import. The caller saves cfg.
func EnsureRegistered(cfg *devconfig.Config, root string) (string, error) {
	if name, p := cfg.FindByPath(root); p != nil {
		return name, nil
	}
	name, err := loadProjectName(root)
	if err != nil {
		return "", err
	}
	if existing := cfg.Projects[name]; existing != nil && existing.Path != root {
		return "", fmt.Errorf("project name %q already registered at %s — resolve the name collision", name, existing.Path)
	}
	cfg.Projects[name] = &devconfig.Project{Path: root, Ports: map[string]int{}}
	fmt.Fprintf(core.Stdout, "registered %q at %s\n", name, root)
	return name, nil
}

// parseEnvKV turns ["K=V", ...] into a map, erroring on a missing `=`.
func parseEnvKV(pairs []string) (map[string]string, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(pairs))
	for _, kv := range pairs {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("invalid --env %q: expected KEY=VALUE", kv)
		}
		out[k] = v
	}
	return out, nil
}
