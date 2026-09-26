// Package devconfig owns w17ctl's dev-machine-local state —
// `~/.w17/config.yaml`. This is the THIRD config home in the w17
// model, alongside:
//
//   - proto                 — code-aware (WHAT gets generated)
//   - w17/lock.yaml         — code-agnostic, SHARED + signed
//     (reproducible codegen across the team)
//   - ~/.w17/config.yaml    — dev-machine-LOCAL (this package)
//
// It holds the registry of installed projects, the unique host-port
// assignments w17ctl hands each project (so two projects' exposed
// ports never collide on one machine), and per-project run presets.
//
// None of this belongs in the signed lock: port maps must differ
// machine-to-machine (that is the whole point), and a "run only the
// admin binary on my weak laptop" preset is a personal-machine
// concern, not a shared, reproducible build input.
package devconfig

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// DefaultPortBase is where w17ctl starts allocating host ports when
// the config does not pin its own port_base. Chosen well above the
// conventional dialect/gateway defaults (5432 / 6379 / 3306 / 8080 /
// 16686 / 4222) so a freshly-allocated stack never lands on a port a
// hand-run `docker compose` would also pick. The 13xxx band is
// deliberately left free for hand-run localhost services (e.g. the
// dockerized console on :13000) — allocation only ever climbs from
// 14000 upward, so it never treads on that reserved band.
const DefaultPortBase = 14000

// Config is the whole `~/.w17/config.yaml` document.
type Config struct {
	// Version is the schema version (1 today). Forward-compat: an
	// unknown future version still loads — fields we don't know are
	// preserved on save only if round-tripped, which we do not do;
	// keep additions backward-compatible.
	Version int `yaml:"version"`

	// PortBase overrides DefaultPortBase for this machine. Zero =
	// DefaultPortBase.
	PortBase int `yaml:"port_base,omitempty"`

	// Projects is the registry, keyed by project name (lock.project).
	Projects map[string]*Project `yaml:"projects,omitempty"`

	// Remotes are named remote docker hosts, keyed by a user-chosen name.
	// Populated by `stack remote add`; consumed in remote mode to run the
	// docker stack on a beefy SSH-reachable box (see
	// docs/experiments/remote-stack.md). Empty = local-only, the default.
	Remotes map[string]*Remote `yaml:"remotes,omitempty"`

	// DefaultMode is the global default execution mode when a project does
	// not pin its own: "local" (default) or "remote". Empty == local.
	DefaultMode string `yaml:"default_mode,omitempty"`

	// DefaultRemote is the Remotes key used in remote mode when a project
	// does not pin one. Empty == no default (an explicit pin/flag is then
	// required to resolve a remote).
	DefaultRemote string `yaml:"default_remote,omitempty"`
}

// Remote is one registered remote docker host — the SSH-reachable box
// w17ctl offloads the docker stack to in remote mode. The server stays
// dumb (dockerd + sshd only); all orchestration runs from w17ctl.
type Remote struct {
	// SSH is the destination passed to ssh/rsync: user@host[:port]. An
	// ssh_config alias (a plain host name) is fine.
	SSH string `yaml:"ssh"`

	// Path is the base directory on the server; each project gets a
	// subdir <Path>/<project> that w17ctl rsyncs the tree into.
	Path string `yaml:"path"`
}

// Project is one registered project's local state.
type Project struct {
	// Path is the absolute filesystem path to the project root (the
	// directory holding w17/lock.yaml).
	Path string `yaml:"path"`

	// Ports maps a port-slot key (the override env-var name, e.g.
	// SHOP_GATEWAY_HOST_PORT) to the unique host port w17ctl assigned
	// it. Rewritten into the compose subprocess env on `stack up`.
	Ports map[string]int `yaml:"ports,omitempty"`

	// ActivePreset is the preset applied by a bare `stack up` (empty =
	// full stack, no env overlay, all services).
	ActivePreset string `yaml:"active_preset,omitempty"`

	// Presets are named run profiles for this project.
	Presets map[string]*Preset `yaml:"presets,omitempty"`

	// Autosync is this dev's local override of the project's
	// dev-DB workflow mode (docs/specs/storage/dev-db-lifecycle.md
	// §workflow modes). nil = no override (follow the lock's project
	// default); non-nil wins over the lock. true = Mode A (autosync —
	// the initiative follows the git branch, with auto snapshot/restore),
	// false = Mode B (manual — active initiative set via `initiative
	// activate`).
	Autosync *bool `yaml:"autosync,omitempty"`

	// ActiveInitiative is the dev's currently-active initiative in manual
	// (autosync-off) mode — the persistent pointer `initiative activate`
	// sets, which `stack build` + `db snapshot` then scope to. Empty =
	// the implicit "default" initiative. Ignored in autosync mode (the
	// git branch drives the initiative there).
	ActiveInitiative string `yaml:"active_initiative,omitempty"`

	// Mode pins this project's stack execution mode, winning over the
	// global DefaultMode. "" = follow DefaultMode; "local" | "remote".
	// Set by `stack local` / `stack remote use`.
	Mode string `yaml:"mode,omitempty"`

	// Remote pins which registered remote this project uses in remote
	// mode, winning over DefaultRemote. "" = follow DefaultRemote; else a
	// Remotes key.
	Remote string `yaml:"remote,omitempty"`

	// Bind is the host interface docker publishes this project's ports on,
	// injected as W17_BIND_HOST into the compose env (like the port map).
	// "" = loopback (127.0.0.1, secure default) | "loopback" | "public"
	// (0.0.0.0 — reachable off-box). Set by `stack bind`. It is a
	// dev-machine runtime concern (not a codegen input), so it lives here
	// next to the ports, NOT in the lock.
	Bind string `yaml:"bind,omitempty"`

	// PublicAck records that the developer has acknowledged (once, by
	// typing the confirmation phrase) that this project's containers are
	// exposed to the network — the guard for the dangerous public+remote
	// combination. Reset whenever Bind leaves "public".
	PublicAck bool `yaml:"public_ack,omitempty"`
}

// Preset is a named run profile: a subset of services to start plus
// extra env overrides layered on top of the w17ctl-managed port env.
type Preset struct {
	// Services is the list of compose service names to bring up. Empty
	// = the whole stack.
	Services []string `yaml:"services,omitempty"`

	// Env is extra environment layered onto the compose subprocess on
	// top of the managed port vars (e.g. W17_OTEL_ENDPOINT="" to skip
	// the tracing backend on a weak machine). A preset CAN override a
	// managed port var here, but that is rarely wanted.
	Env map[string]string `yaml:"env,omitempty"`
}

// DefaultDir is the w17ctl home — `$W17_HOME` if set, else
// `$HOME/.w17`. The w17ctl binary itself lives here (~/.w17/w17ctl).
func DefaultDir() (string, error) {
	if h := os.Getenv("W17_HOME"); h != "" {
		return h, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("devconfig: resolve home dir: %w", err)
	}
	return filepath.Join(home, ".w17"), nil
}

// DefaultPath is the config file path — `<DefaultDir>/config.yaml`.
func DefaultPath() (string, error) {
	dir, err := DefaultDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.yaml"), nil
}

// Load reads the config at path. A missing file is NOT an error — it
// returns a fresh, empty config (first run). The returned config has
// non-nil Projects and a resolved Version so callers can mutate it
// directly.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Config{Version: 1, Projects: map[string]*Project{}}, nil
		}
		return nil, fmt.Errorf("devconfig: read %s: %w", path, err)
	}
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("devconfig: parse %s: %w", path, err)
	}
	c.normalize()
	return &c, nil
}

// LoadDefault loads from DefaultPath.
func LoadDefault() (*Config, error) {
	path, err := DefaultPath()
	if err != nil {
		return nil, err
	}
	return Load(path)
}

// normalize fills zero values so callers never touch nil maps.
func (c *Config) normalize() {
	if c.Version == 0 {
		c.Version = 1
	}
	if c.Projects == nil {
		c.Projects = map[string]*Project{}
	}
	for _, p := range c.Projects {
		if p.Ports == nil {
			p.Ports = map[string]int{}
		}
	}
}

// EffectivePortBase returns PortBase or DefaultPortBase.
func (c *Config) EffectivePortBase() int {
	if c.PortBase > 0 {
		return c.PortBase
	}
	return DefaultPortBase
}

// Save writes the config to path atomically (temp file + rename),
// creating the parent directory if needed.
func Save(path string, c *Config) error {
	c.normalize()
	data, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("devconfig: marshal: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("devconfig: mkdir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".config-*.yaml")
	if err != nil {
		return fmt.Errorf("devconfig: temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op once the rename succeeds
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("devconfig: write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("devconfig: close temp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("devconfig: rename into place: %w", err)
	}
	return nil
}

// SaveDefault writes to DefaultPath.
func SaveDefault(c *Config) error {
	path, err := DefaultPath()
	if err != nil {
		return err
	}
	return Save(path, c)
}

// HasPreset reports whether the project has a run preset by that name.
func (p *Project) HasPreset(name string) bool {
	_, ok := p.Presets[name]
	return ok
}

// SetPreset stores (or replaces) the named run preset, allocating the
// Presets map on first use.
func (p *Project) SetPreset(name string, preset *Preset) {
	if p.Presets == nil {
		p.Presets = map[string]*Preset{}
	}
	p.Presets[name] = preset
}

// DeletePreset removes the named preset and clears ActivePreset when it
// pointed at the removed one (a removed active preset falls back to the
// full stack). Returns false — changing nothing — if no such preset
// exists.
func (p *Project) DeletePreset(name string) bool {
	if !p.HasPreset(name) {
		return false
	}
	delete(p.Presets, name)
	if p.ActivePreset == name {
		p.ActivePreset = ""
	}
	return true
}

// Activate marks the named preset as the one a bare `stack up` applies.
// Returns false — changing nothing — if no such preset exists. Pass ""
// via ClearActivePreset to reset to the full stack.
func (p *Project) Activate(name string) bool {
	if !p.HasPreset(name) {
		return false
	}
	p.ActivePreset = name
	return true
}

// ClearActivePreset resets the project to the full stack (no active
// preset) — what a bare `stack up` runs.
func (p *Project) ClearActivePreset() {
	p.ActivePreset = ""
}

// FindByPath returns the registered project (and its name) whose Path
// matches the given absolute path, or "", nil if none.
//
// ⚠️ DETERMINISTIC BY NAME, and that is a fix rather than a detail. This used to
// `range c.Projects` and return the first match — over a MAP, whose iteration
// order Go randomises per run. With two registry entries sharing a path, the
// answer to "which project am I in" therefore changed between invocations of
// the same command.
//
// A consumer had three entries on one path. `stack up` published the ports of
// one, `stack build` dialled another, and their lock named a third — so a dump
// reached a database in a DIFFERENT WORKSPACE on the same machine, and the
// empty snapshots that came back are what a branch switch then restored over
// their live stores (marb #75, and the mechanism behind #68).
//
// Sorting makes the wrong answer at least a STABLE wrong answer, which is what
// makes it findable. Duplicates themselves are refused by DuplicatePaths, and
// ResolveProject prefers the lock's own name over any of this.
func (c *Config) FindByPath(absPath string) (string, *Project) {
	names := make([]string, 0, len(c.Projects))
	for name := range c.Projects {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if p := c.Projects[name]; p.Path == absPath {
			return name, p
		}
	}
	return "", nil
}

// DuplicatePaths reports every path registered under more than one project
// name, with those names, sorted.
//
// Two entries on one path is not a state any command can resolve correctly: the
// registry is keyed by name and asked by path, so the question has more than one
// true answer and every caller picks one. Reporting it is the only honest
// handling — see marb #75, where the three answers differed by store PORT and
// the dump went to another workspace's database.
func (c *Config) DuplicatePaths() map[string][]string {
	byPath := map[string][]string{}
	for name, p := range c.Projects {
		if p != nil && p.Path != "" {
			byPath[p.Path] = append(byPath[p.Path], name)
		}
	}
	out := map[string][]string{}
	for path, names := range byPath {
		if len(names) > 1 {
			sort.Strings(names)
			out[path] = names
		}
	}
	return out
}

// ResolveProject answers "which registry entry is this checkout" the way the
// checkout itself answers it: by the `project:` its lock names.
//
// The lock carries that name and was right the whole time; nothing asked it.
// Falling back to the path is kept for a checkout with no lock yet, and then a
// duplicate path is REFUSED rather than silently picked — a wrong store is a
// dump of somebody else's database, or a wipe of it.
//
// Returns a nil project with no error when the checkout simply is not
// registered: that is the ordinary pre-`stack up` state, not a fault.
// ValidateProjectName refuses a registry key that cannot safely be ONE path
// component.
//
// The key is not only a label: `cmd/stack/mode.go` and `project ps` join it onto
// the remote base directory (`path.Join(r.Path, project)`), so a name like
// `../../other` puts `stack up` and `project ps` outside the base an operator
// configured. It reaches that join from three places — `project rename`, `project
// import` and EnsureRegistered — and two of those take the name from
// `w17/lock.yaml`, a file that travels with a repository. So the check belongs
// here, beside the registry, rather than at the one call site a reviewer happened
// to be reading.
func ValidateProjectName(name string) error {
	if name == "" {
		return fmt.Errorf("project name is empty")
	}
	if name == "." || name == ".." {
		return fmt.Errorf("project name %q is a path component with a meaning, not a name", name)
	}
	if strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("project name %q contains a path separator — it has to be a single directory component, "+
			"because remote mode joins it onto the base dir", name)
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("project name %q contains a control character", name)
		}
	}
	return nil
}

func (c *Config) ResolveProject(lockProject, absPath string) (string, *Project, string, error) {
	if lockProject != "" {
		p, ok := c.Projects[lockProject]
		if !ok {
			// ⚠️ This used to REFUSE, and refusing was wrong in the ordinary
			// case. Renaming a project in the console is a legitimate act an
			// owner is allowed to perform — deinvo renamed theirs to match a
			// GitHub branch — while the registry key was written at `init` from
			// the org slug and nothing ever paired the two. Eight client
			// releases did not care; rc.52 blocked `stack build` outright and
			// took their dev stack down, so a rename was punished and the
			// advice ("run stack up") repaired nothing: `stack up` resolves by
			// PATH, so it kept working, and the two commands disagreed about
			// one tree.
			//
			// What the name is FOR is disambiguating several entries on one
			// path (marb #75). With exactly one entry here there is nothing to
			// disambiguate, so the single entry is the answer — said out loud,
			// with the command that aligns the two names and keeps the ports.
			// DuplicatePaths only reports paths carrying MORE than one entry, so
			// the single-entry case is FindByPath's to answer — asking
			// DuplicatePaths for a count of one gets zero, which is how the
			// first version of this fix refused the very case it was written for.
			byPath := c.DuplicatePaths()[absPath]
			name, only := c.FindByPath(absPath)
			switch {
			case len(byPath) <= 1 && only != nil:
				return name, only, fmt.Sprintf(
					"w17/lock.yaml names project %q; this machine's registry calls this directory %q — using %q.\n"+
						"  Both names describe the same checkout, and renaming a project is allowed, so this is not an error.\n"+
						"  To stop the mismatch: `w17ctl project rename %s` (keeps the ports this machine already assigned).",
					lockProject, name, name, lockProject), nil
			case len(byPath) > 1:
				return "", nil, "", fmt.Errorf(
					"w17/lock.yaml names project %q, which is not in the project registry (~/.w17/config.yaml), "+
						"and this directory has %d entries under other names:\n"+
						"  %s\n"+
						"each carries its own published store ports, so there is no single honest answer here\n"+
						"fix: rename one of them to %q (`w17ctl project rename %s`) and remove the rest",
					lockProject, len(byPath), strings.Join(byPath, ", "), lockProject, lockProject)
			}
			// Nothing registered for this directory at all — and that is a FRESH
			// CHECKOUT, not a mistake. marb's CI runner has no `~/.w17` by
			// construction: their `w17-codegen` job passes `--no-build` exactly
			// so nothing starts, and the refusal's advice (`stack up`) says to
			// bring up a compose stack to fix a missing local file. rc.52 stopped
			// their codegen on it.
			//
			// The registry is a CACHE of this machine's port allocations, not a
			// statement about whether the project exists. With no entry there are
			// no ports to get WRONG — the honest outcome is "no local stores
			// resolve here", which is what a runner with no databases wants and
			// what `codegen` already does. Every caller handles a nil project:
			// each store is then skipped by name with the reason.
			//
			// The two refusals that remain are the ones with a genuine ambiguity
			// or a genuine hazard: several entries under other names (below), and
			// an entry whose PATH is another checkout (further down) — that one
			// would hand back another tree's ports, which is #75's whole point,
			// and a shared runner is exactly where it could happen.
			return lockProject, nil, fmt.Sprintf(
				"this machine's registry (~/.w17/config.yaml) has no entry for this checkout, so no local " +
					"store resolves from it — fine on a fresh checkout or CI runner, where there are none.\n" +
					"  `w17ctl stack up` registers it and assigns host ports when you do want them here.",
			), nil
		}
		// ⚠️ The registry entry must be THIS checkout. A lock copied from another
		// tree — or left behind by one — names a project registered against a
		// DIFFERENT directory, and trusting the name alone would hand back that
		// checkout's store ports: the same wrong-database dump this function
		// exists to prevent, arriving by the door built to stop it.
		if p.Path != "" && absPath != "" && p.Path != absPath {
			return "", nil, "", fmt.Errorf(
				"w17/lock.yaml names project %q, but the registry has that project at %s — this checkout is %s\n"+
					"its store ports belong to that other directory, so building here would reach its databases\n"+
					"fix: correct `project:` in this lock, or re-register this checkout with `w17ctl stack up`",
				lockProject, p.Path, absPath)
		}
		return lockProject, p, "", nil
	}
	if dup := c.DuplicatePaths()[absPath]; len(dup) > 1 {
		return "", nil, "", fmt.Errorf(
			"the project registry has %d entries for this directory (%s) and the lock does not name which:\n"+
				"  %s\n"+
				"every one of them carries its own published store ports, so commands disagree about where this "+
				"project's databases are — and a dump or a wipe aimed at the wrong port reaches another project's "+
				"database on this machine\n"+
				"fix: set `project:` in w17/lock.yaml to the entry you mean, or remove the others from "+
				"~/.w17/config.yaml",
			len(dup), absPath, strings.Join(dup, ", "))
	}
	name, p := c.FindByPath(absPath)
	return name, p, "", nil
}
