// Package lockfile is the thin client's OFFLINE lock reader: a minimal,
// w17ctl-owned view of the routing fields the deploy path needs to read from
// w17/lock.yaml WITHOUT a console round-trip. Apply and rollback are
// deliberately offline — they never contact the console; FETCH is the single
// ONLINE step, and it is the caller that opens this file first. (The header
// listed fetch among the offline commands until T2-5 pass #14, D14-13.) It reads only the
// raw stored fields (connection names + their pinned migration targets +
// project_id); it does NOT resolve Effective* defaults (those are a console
// concern via DescribeLock) and does NOT verify the signature (verification is
// a console concern per the public-split boundary — the client does zero
// crypto). The lock is treated as local config data the client may read, never
// as the private srcgo lockpb/console-lock types.
package lockfile

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// DefaultPath is the project-root-relative location of the lock file.
const DefaultPath = "w17/lock.yaml"

// WriteAtomic writes data to path crash-safely: it writes to a temp file in
// the SAME directory (so the final os.Rename is a same-filesystem atomic
// swap, never a cross-device copy) and renames it into place. A crash
// mid-write leaves the old lock intact rather than a truncated signed lock —
// the truncate-in-place os.WriteFile alternative can corrupt the lock on a
// partial write. The lock is not secret, so perm is typically 0o644.
func WriteAtomic(path string, data []byte, perm fs.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("lockfile: temp for %s: %w", path, err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("lockfile: write %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("lockfile: close %s: %w", path, err)
	}
	if err := os.Chmod(tmpPath, perm); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("lockfile: chmod %s: %w", path, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("lockfile: rename %s: %w", path, err)
	}
	return nil
}

// Lock is the subset of w17/lock.yaml the offline deploy path reads. Unknown
// fields (generated_code, secrets, signature, …) are ignored by the YAML
// decoder — this struct is intentionally partial.
type Lock struct {
	// Project is the human project name (the proto package prefix source);
	// ProjectID is the registry key. Both are raw stored strings.
	Project   string `yaml:"project"`
	ProjectID string `yaml:"project_id"`
	// OrgID is the organization this project belongs to, stamped by the
	// console when it signs the lock. Empty on a lock written before the
	// field existed, or one the console has not re-signed since.
	OrgID       string       `yaml:"org_id"`
	Connections []Connection `yaml:"connections"`
	// Plugins is the installed-plugin set (the offline read the plugin
	// list/install/update commands use to cross-reference + dedup; the WRITE
	// rides the EditLock plugin intents).
	Plugins []Plugin `yaml:"plugins"`
	// GeneratedCode carries the paths the project chose at init. Read
	// offline so a command can derive what the operator would otherwise
	// have to retype — `migrate generate` used to demand every --proto by
	// hand while the answer sat here.
	GeneratedCode GeneratedCode `yaml:"generated_code"`
}

// GeneratedCode mirrors the lock's generated_code block (the fields read
// offline; the console owns the rest).
type GeneratedCode struct {
	ProtoDir string `yaml:"proto_dir"`
	// Stubs is the Go stub tree root (`srcgo/gen` conventionally). Its FIRST
	// SEGMENT is the directory holding the project's hand-written Go module,
	// which is where `init` scaffolds the go.mod — so it is how a client finds
	// that module without assuming the convention.
	//
	// Read from disk rather than from DescribeLock on purpose: the projection
	// deliberately does NOT carry a gen dir (field 18 of LockView is reserved
	// for a version of exactly that, added and withdrawn), because the console
	// applies the project's real value server-side. That is right for what the
	// SERVER composes; it leaves the client needing a local answer for a local
	// question — which file on this disk holds the module — and the lock is
	// already here.
	Stubs string `yaml:"stubs"`

	// The other roots codegen writes into. Read for ONE question the client
	// alone can answer: will this project's git commit a generated tree?
	//
	// A `.gitignore` reaches only its own directory and below, so the one
	// codegen writes into `w17/` cannot cover a root outside it — there is no
	// file for the console to fix, and the console never sees the consumer's
	// repo anyway. A consumer put a React client at
	// `frontend/apps/rehab/api` and committed 91 generated files without being
	// told; the warning that now says so needs these roots.
	//
	// ⚠️ An EMPTY `output_root` is not a root outside `w17/` — it means the
	// default, which codegen derives under the stubs root. Treat it as absent
	// rather than as the project directory.
	PbStubs     []GeneratedRoot `yaml:"pb_stubs"`
	Clients     []GeneratedRoot `yaml:"clients"`
	GrpcClients []GeneratedRoot `yaml:"grpc_clients"`
}

// GeneratedRoot is one `generated_code` entry's output location. Only the root
// is read here — the language and wire format are the console's business.
type GeneratedRoot struct {
	OutputRoot string `yaml:"output_root"`
}

// OutputRoots returns every declared, non-default generated root beside the
// stubs root: the set a project's own `.gitignore` has to cover.
func (g GeneratedCode) OutputRoots() []string {
	var out []string
	add := func(r string) {
		if r = strings.Trim(strings.TrimSpace(r), "/"); r != "" {
			out = append(out, r)
		}
	}
	add(g.Stubs)
	for _, e := range g.PbStubs {
		add(e.OutputRoot)
	}
	for _, e := range g.Clients {
		add(e.OutputRoot)
	}
	for _, e := range g.GrpcClients {
		add(e.OutputRoot)
	}
	return out
}

// GenDir is the directory holding the project's hand-written Go module: the
// first segment of the stubs root. Empty when the lock does not say, and the
// caller then keeps the convention.
func (g GeneratedCode) GenDir() string {
	s := strings.Trim(g.Stubs, "/")
	if s == "" {
		return ""
	}
	if i := strings.Index(s, "/"); i > 0 {
		return s[:i]
	}
	return s
}

// Plugin mirrors a lock plugins[] entry's identity fields.
type Plugin struct {
	Name    string `yaml:"name"`
	Version string `yaml:"version"`
	Source  string `yaml:"source"`

	// Git carries a git-sourced plugin's provenance. Nil for `internal`
	// plugins and for locks written before the field existed — absent means
	// "this lock predates the question", not "no provenance required", so a
	// reader skips the check rather than failing it.
	Git *GitPin `yaml:"git"`
}

// GitPin mirrors the lock's git coordinates for one plugin. The client reads
// them to know what to re-fetch and what the tree on disk is supposed to be;
// it does not decide anything with them — the console owns that.
type GitPin struct {
	Repo   string `yaml:"repo"`
	Ref    string `yaml:"ref"`
	Commit string `yaml:"commit"`
	Digest string `yaml:"digest"`
}

// Connection mirrors the lock's connection entry's routing fields. Dialect /
// version live in proto, not the lock; the lock tracks the pinned deploy target.
type Connection struct {
	Name                string `yaml:"name"`
	TargetMigrationID   string `yaml:"target_migration_id"`
	TargetContentSha256 string `yaml:"target_content_sha256"`
}

// Load reads + YAML-decodes the lock at path into the partial Lock view. It
// does not verify the signature (the client does no crypto) — it only reads the
// routing fields. A missing / unreadable / malformed file is an error the
// caller surfaces.
func Load(path string) (*Lock, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("lockfile: read %s: %w", path, err)
	}
	var lk Lock
	if err := yaml.Unmarshal(data, &lk); err != nil {
		return nil, fmt.Errorf("lockfile: parse %s: %w", path, err)
	}
	return &lk, nil
}
