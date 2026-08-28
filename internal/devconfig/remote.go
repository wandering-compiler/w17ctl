package devconfig

import (
	"fmt"
	"sort"
	"strings"
)

// Mode is a stack execution mode — where `w17ctl stack` builds and runs
// the docker stack.
type Mode string

const (
	// ModeLocal runs the docker stack on the local machine — the default,
	// historical behaviour.
	ModeLocal Mode = "local"

	// ModeRemote runs the docker stack on a registered remote docker host
	// over SSH, tunneling each published port back to the identical
	// localhost port so the dev experience is unchanged
	// (docs/experiments/remote-stack.md).
	ModeRemote Mode = "remote"
)

// Bind modes — the host interface docker publishes ports on.
const (
	// BindLoopback binds 127.0.0.1: ports reach only the local box (or, in
	// remote mode, only through the SSH tunnel). The secure default.
	BindLoopback = "loopback"
	// BindPublic binds 0.0.0.0: ports reach any interface — on a remote
	// host with a public IP that means the internet (subject to firewall).
	BindPublic = "public"
)

const (
	bindHostLoopback = "127.0.0.1"
	bindHostPublic   = "0.0.0.0"
)

// ParseBind validates a bind mode. "" is the loopback default.
func ParseBind(s string) (string, error) {
	switch s {
	case "", BindLoopback:
		return BindLoopback, nil
	case BindPublic:
		return BindPublic, nil
	default:
		return "", fmt.Errorf("invalid bind %q (want %q or %q)", s, BindLoopback, BindPublic)
	}
}

// BindHost maps an EXPLICIT bind mode to the docker host-publish
// interface — "public" → 0.0.0.0, "loopback" → 127.0.0.1. Used for the
// `stack bind` confirmation message; the unset ("") default is mode-aware,
// so use ResolveBindHost when a project has not pinned a bind.
func BindHost(bind string) string {
	if bind == BindPublic {
		return bindHostPublic
	}
	return bindHostLoopback
}

// ResolveBindHost is the mode-aware host-publish interface w17ctl injects
// as W17_BIND_HOST at `stack up`. An explicit bind wins; when unset the
// default depends on the mode:
//
//   - remote → 127.0.0.1 (secure: the stack runs on a shared server, and
//     the only consumer is the SSH tunnel, which dials loopback);
//   - local  → 0.0.0.0 (historical: local container consumers — e2e test
//     containers via host.docker.internal, LAN devices — need it, and a
//     laptop behind NAT is not internet-exposed).
//
// The generated compose defaults W17_BIND_HOST to 0.0.0.0, so a raw
// `docker compose up` (e.g. the e2e harness) keeps the historical bind;
// the loopback security on remote comes from w17ctl always injecting it.
func ResolveBindHost(mode Mode, bind string) string {
	switch bind {
	case BindPublic:
		return bindHostPublic
	case BindLoopback:
		return bindHostLoopback
	default: // unset — mode decides
		if mode == ModeRemote {
			return bindHostLoopback
		}
		return bindHostPublic
	}
}

// ParseMode validates a mode string as it appears in config or on a
// flag. The empty string is a valid "unset" (callers treat it as
// "fall through to the next resolution source"); any value other than
// "", "local", or "remote" is rejected.
func ParseMode(s string) (Mode, error) {
	switch s {
	case "":
		return "", nil
	case string(ModeLocal):
		return ModeLocal, nil
	case string(ModeRemote):
		return ModeRemote, nil
	default:
		return "", fmt.Errorf("invalid mode %q (want %q or %q)", s, ModeLocal, ModeRemote)
	}
}

// ResolveMode walks the resolution chain for the stack execution mode of
// the project at absPath, mirroring core.ResolveConsoleAddr. Precedence
// (first non-empty wins):
//
//  1. flag — kong's --mode value (the W17_STACK_MODE env var is folded in
//     by kong's `env` tag, so this single argument covers both flag+env).
//  2. the project's pinned Mode (`stack use-local` / `stack use-remote`).
//  3. the global DefaultMode.
//  4. local — the bottom default.
//
// Every source is validated; an invalid value anywhere is a hard error
// rather than a silent fall-through, so a typo in config surfaces.
func (c *Config) ResolveMode(absPath, flag string) (Mode, error) {
	if m, err := ParseMode(flag); err != nil {
		return "", fmt.Errorf("--mode: %w", err)
	} else if m != "" {
		return m, nil
	}
	if _, p := c.FindByPath(absPath); p != nil {
		if m, err := ParseMode(p.Mode); err != nil {
			return "", fmt.Errorf("project %s pinned mode: %w", absPath, err)
		} else if m != "" {
			return m, nil
		}
	}
	if m, err := ParseMode(c.DefaultMode); err != nil {
		return "", fmt.Errorf("default_mode: %w", err)
	} else if m != "" {
		return m, nil
	}
	return ModeLocal, nil
}

// ResolveRemote resolves which registered remote the project at absPath
// uses in remote mode. Precedence (first non-empty wins): flag → the
// project's pinned Remote → DefaultRemote. The resolved name must exist in
// Remotes; an empty resolution or an unknown name is an actionable error.
func (c *Config) ResolveRemote(absPath, flag string) (string, *Remote, error) {
	name := flag
	if name == "" {
		if _, p := c.FindByPath(absPath); p != nil {
			name = p.Remote
		}
	}
	if name == "" {
		name = c.DefaultRemote
	}
	if name == "" {
		return "", nil, fmt.Errorf("no remote configured — register one with `w17ctl stack remote add <name> --ssh user@host --path /srv/w17`, then select it with --remote, `stack use-remote <name>`, or set a default")
	}
	r := c.Remotes[name]
	if r == nil {
		return "", nil, fmt.Errorf("unknown remote %q — registered: %s", name, c.remoteNames())
	}
	return name, r, nil
}

// SetRemote stores (or replaces) a named remote, allocating the Remotes
// map on first use — mirrors Project.SetPreset.
func (c *Config) SetRemote(name string, r *Remote) {
	if c.Remotes == nil {
		c.Remotes = map[string]*Remote{}
	}
	c.Remotes[name] = r
}

// DeleteRemote removes the named remote and cascades: it clears
// DefaultRemote when it pointed at the removed one, and clears every
// project's Remote pin that referenced it (so nothing dangles at a
// now-unknown remote). Returns false — changing nothing — if no such
// remote exists.
func (c *Config) DeleteRemote(name string) bool {
	if _, ok := c.Remotes[name]; !ok {
		return false
	}
	delete(c.Remotes, name)
	if c.DefaultRemote == name {
		c.DefaultRemote = ""
	}
	for _, p := range c.Projects {
		if p.Remote == name {
			p.Remote = ""
		}
	}
	return true
}

// remoteNames returns the sorted registered remote names for error
// messages, or "(none)" when the registry is empty.
func (c *Config) remoteNames() string {
	if len(c.Remotes) == 0 {
		return "(none)"
	}
	names := make([]string, 0, len(c.Remotes))
	for n := range c.Remotes {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}
