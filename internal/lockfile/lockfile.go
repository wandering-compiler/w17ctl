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
	Project     string       `yaml:"project"`
	ProjectID   string       `yaml:"project_id"`
	Connections []Connection `yaml:"connections"`
	// Plugins is the installed-plugin set (the offline read the plugin
	// list/install/update commands use to cross-reference + dedup; the WRITE
	// rides the EditLock plugin intents).
	Plugins []Plugin `yaml:"plugins"`
}

// Plugin mirrors a lock plugins[] entry's identity fields.
type Plugin struct {
	Name    string `yaml:"name"`
	Version string `yaml:"version"`
	Source  string `yaml:"source"`
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
