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
	// Set by `stack use-local` / `stack use-remote`.
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
func (c *Config) FindByPath(absPath string) (string, *Project) {
	for name, p := range c.Projects {
		if p.Path == absPath {
			return name, p
		}
	}
	return "", nil
}
