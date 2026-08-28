// Package authstore owns w17ctl's machine-local credential store —
// `~/.w17/auth.yaml`. It is the home for w17ctl's AUTHENTICATION state,
// kept deliberately separate from devconfig's `~/.w17/config.yaml`:
//
//   - config.yaml  (devconfig)  — ports / presets / project registry
//   - auth.yaml    (this pkg)    — login tokens + identity + org cache
//
// Two reasons for the split: the file is sensitive (it holds bearer
// tokens, so it is written 0600), and its keying is its own concern —
// the store is keyed by INSTANCE (a console deployment, identified by
// its API URL), mirroring `gh`'s hosts.yml. A logged-in user has one
// token per instance: our hosted console by default, plus any
// enterprise self-hosted console they `login`ed against.
//
// The client treats the token as OPAQUE bytes — it is minted and
// validated by the console (the OAuth authorization server). w17ctl
// never inspects or re-signs it; this package only stores and retrieves
// it. See docs/specs/plugins/auth-cli-login-and-orgs.md.
package authstore

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/wandering-compiler/w17ctl/internal/devconfig"
)

// FileName is the credential file's basename under the w17ctl home.
const FileName = "auth.yaml"

// Store is the whole `~/.w17/auth.yaml` document.
type Store struct {
	// Version is the schema version (1 today). Keep additions
	// backward-compatible.
	Version int `yaml:"version"`

	// DefaultInstance is the URL key of the instance used when no
	// --console flag / W17_CONSOLE_ADDR is given. Empty = not logged in
	// anywhere (or no default chosen).
	DefaultInstance string `yaml:"default_instance,omitempty"`

	// Instances holds one entry per console the user has logged into,
	// keyed by the instance URL (== Instance.URL).
	Instances map[string]*Instance `yaml:"instances,omitempty"`
}

// Instance is one console deployment's credential + cached identity.
type Instance struct {
	// URL is the console API address (e.g. "console.w17.dev:443"). Also
	// the map key in Store.Instances.
	URL string `yaml:"url"`

	// Token is the opaque bearer the console minted for this user at
	// login. w17ctl attaches it as `authorization: Bearer <token>` on its
	// console gRPC calls and never inspects it.
	Token string `yaml:"token,omitempty"`

	// User is the cached identity behind the token (display only).
	User *User `yaml:"user,omitempty"`

	// Orgs is the cached set of organizations this identity is a member
	// of, refreshed at login (and on demand). Drives the init org picker
	// without a round-trip when fresh.
	Orgs []*Org `yaml:"orgs,omitempty"`

	// DefaultOrg is the org ID sent as the active org (`w17-org`) on every
	// console call — see core.ActiveOrgSlug, which resolves it to the slug
	// the wire carries. Set by `w17ctl org use <slug>`. Empty = send
	// nothing, which leaves the console's single-org inference to decide:
	// right for a one-org user, a deliberate refusal for a multi-org one.
	DefaultOrg string `yaml:"default_org,omitempty"`
}

// User is the cached principal identity (display only — authority is the
// token).
type User struct {
	ID    string `yaml:"id"`
	Email string `yaml:"email,omitempty"`
}

// Org is one cached organization membership (GitHub-style org within an
// instance).
type Org struct {
	ID   string `yaml:"id"`
	Slug string `yaml:"slug"`
	Name string `yaml:"name,omitempty"`
	// Kind is "private" | "company" (free-form; display + grouping).
	Kind string `yaml:"kind,omitempty"`
	// Role is the caller's role in the org: "owner" | "admin" | "member".
	Role string `yaml:"role,omitempty"`
}

// DefaultPath is the credential file path — `<W17 home>/auth.yaml`,
// sharing devconfig's W17_HOME-aware home resolution so the two config
// files always land in the same directory.
func DefaultPath() (string, error) {
	dir, err := devconfig.DefaultDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, FileName), nil
}

// Load reads the store at path. A missing file is NOT an error — it
// returns a fresh, empty store (not logged in yet). The returned store
// has a non-nil Instances map and a resolved Version so callers can
// mutate it directly.
func Load(path string) (*Store, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Store{Version: 1, Instances: map[string]*Instance{}}, nil
		}
		return nil, fmt.Errorf("authstore: read %s: %w", path, err)
	}
	var s Store
	if err := yaml.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("authstore: parse %s: %w", path, err)
	}
	s.normalize()
	return &s, nil
}

// LoadDefault loads from DefaultPath.
func LoadDefault() (*Store, error) {
	path, err := DefaultPath()
	if err != nil {
		return nil, err
	}
	return Load(path)
}

// normalize fills zero values so callers never touch nil maps and each
// instance's URL matches its map key (the key is authoritative).
func (s *Store) normalize() {
	if s.Version == 0 {
		s.Version = 1
	}
	if s.Instances == nil {
		s.Instances = map[string]*Instance{}
	}
	for key, inst := range s.Instances {
		if inst == nil {
			delete(s.Instances, key)
			continue
		}
		if inst.URL == "" {
			inst.URL = key
		}
	}
}

// Save writes the store to path atomically (temp file + rename) with
// 0600 permissions (it holds bearer tokens), creating the parent
// directory if needed.
func Save(path string, s *Store) error {
	s.normalize()
	data, err := yaml.Marshal(s)
	if err != nil {
		return fmt.Errorf("authstore: marshal: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("authstore: mkdir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".auth-*.yaml")
	if err != nil {
		return fmt.Errorf("authstore: temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op once the rename succeeds
	// CreateTemp already makes the file 0600, but be explicit — a leaked
	// token is the worst-case failure here.
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("authstore: chmod temp: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("authstore: write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("authstore: close temp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("authstore: rename into place: %w", err)
	}
	return nil
}

// SaveDefault writes to DefaultPath.
func SaveDefault(s *Store) error {
	path, err := DefaultPath()
	if err != nil {
		return err
	}
	return Save(path, s)
}

// Instance returns the stored instance for url, or nil if none.
func (s *Store) Instance(url string) *Instance {
	if s.Instances == nil {
		return nil
	}
	return s.Instances[url]
}

// SetInstance upserts an instance, keyed by its URL. It is the first
// login bootstrap: if no default instance is set yet, this one becomes
// the default. Panics-free on a nil receiver map (normalizes first).
func (s *Store) SetInstance(inst *Instance) {
	if inst == nil || inst.URL == "" {
		return
	}
	s.normalize()
	s.Instances[inst.URL] = inst
	if s.DefaultInstance == "" {
		s.DefaultInstance = inst.URL
	}
}

// RemoveInstance drops an instance (logout). If it was the default, the
// default is cleared (a remaining instance is NOT auto-promoted — the
// user chooses explicitly).
func (s *Store) RemoveInstance(url string) {
	if s.Instances != nil {
		delete(s.Instances, url)
	}
	if s.DefaultInstance == url {
		s.DefaultInstance = ""
	}
}

// ActiveInstance returns the default instance, or nil if no default is
// set (or the default points at a missing entry).
func (s *Store) ActiveInstance() *Instance {
	if s.DefaultInstance == "" {
		return nil
	}
	return s.Instance(s.DefaultInstance)
}

// SetDefaultInstance points the default at url. It is a no-op (returns
// false) if no such instance is stored — the caller should not be able
// to default to an instance they never logged into.
func (s *Store) SetDefaultInstance(url string) bool {
	if s.Instance(url) == nil {
		return false
	}
	s.DefaultInstance = url
	return true
}

// TokenFor returns the bearer token stored for url, or "" if the
// instance is unknown or tokenless.
func (s *Store) TokenFor(url string) string {
	inst := s.Instance(url)
	if inst == nil {
		return ""
	}
	return inst.Token
}

// Org returns the cached org with the given slug on the instance, or
// nil. Convenience for the init org picker / `org use`.
func (inst *Instance) Org(slug string) *Org {
	if inst == nil {
		return nil
	}
	for _, o := range inst.Orgs {
		if o != nil && o.Slug == slug {
			return o
		}
	}
	return nil
}
