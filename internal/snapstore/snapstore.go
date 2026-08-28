// Package snapstore is the on-disk home for branch-scoped DB
// snapshots in the dev DB lifecycle
// (`docs/specs/storage/dev-db-lifecycle.md` S3). It owns the
// `w17/tmp/<branch>/db/<conn>.<ext>` layout, the atomic write/read of
// each store's dump, the `.gitignore` guarantee for the scratch tree,
// and a max-N LRU eviction policy.
//
// It is deliberately dialect-agnostic: it moves opaque byte streams to
// and from disk and drives the per-store migrate.Snapshotter (S1/S2) for
// the actual dump/restore. The caller pairs each connection with its
// Snapshotter + file extension via Conn; snapstore knows nothing about
// SQL vs gob.
//
// Snapshots are disposable dev scratch, never a backup/DR mechanism —
// the whole `w17/tmp/` tree is git-ignored and freely evictable.
package snapstore

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/wandering-compiler/w17ctl/internal/lockfile"
	"github.com/wandering-compiler/w17ctl/internal/scaffold"
	"github.com/wandering-compiler/sdk/go/tooling/migrate"
)

// Store manages branch-scoped snapshots under `<projectRoot>/w17/tmp`.
type Store struct {
	projectRoot string
	tmpDir      string
}

// New builds a Store rooted at a project directory. The scratch tree
// lives at `<projectRoot>/w17/tmp`, mirroring the other w17-owned
// project subtrees (`w17/lock.yaml`, `w17/languages`, `w17/ci`).
func New(projectRoot string) *Store {
	return &Store{
		projectRoot: projectRoot,
		tmpDir:      filepath.Join(projectRoot, "w17", "tmp"),
	}
}

// Conn pairs one connection's Snapshotter with the on-disk filename it
// dumps to. Name is the schema-declared connection name (the
// unambiguous key — two connections can share a dialect); Ext is the
// dump's file extension without the dot (e.g. "sql", "gob", "sqlite").
type Conn struct {
	Name        string
	Ext         string
	Snapshotter migrate.Snapshotter
}

func (c Conn) file() string { return c.Name + "." + c.Ext }

// branchKey encodes a git branch (or savepoint/initiative name) into a
// single safe path component. Branches legally contain '/' (`feature/x`);
// url.PathEscape turns that into `%2F`, keeping one directory level per
// branch and staying reversible for Branches().
//
// Names arrive as unsanitized user args (`initiative create <name>`,
// `db snapshot save <name>`), so a key that resolves to a traversal
// component (".", "..", or anything still bearing a path separator) is
// rejected — otherwise a name like ".." would make the path resolve
// ABOVE the scratch tree (RemoveNamed/dbDir/branchRoot escaping w17/tmp).
func branchKey(name string) (string, error) {
	key := url.PathEscape(name)
	if key == "" || key == "." || key == ".." ||
		strings.ContainsRune(key, '/') || strings.ContainsRune(key, os.PathSeparator) {
		return "", fmt.Errorf("snapstore: invalid name %q (unsafe path component)", name)
	}
	return key, nil
}

// dbDir is the directory holding a branch's per-store dumps.
func (s *Store) dbDir(branch string) (string, error) {
	key, err := branchKey(branch)
	if err != nil {
		return "", err
	}
	return filepath.Join(s.tmpDir, key, "db"), nil
}

// branchRoot is the branch's top dir (parent of db/) — the unit GC
// removes.
func (s *Store) branchRoot(branch string) (string, error) {
	key, err := branchKey(branch)
	if err != nil {
		return "", err
	}
	return filepath.Join(s.tmpDir, key), nil
}

// Has reports whether a snapshot directory exists for the branch. An
// invalid name "has" no snapshot (and is never used to touch disk).
func (s *Store) Has(branch string) bool {
	dir, err := s.dbDir(branch)
	if err != nil {
		return false
	}
	info, err := os.Stat(dir)
	return err == nil && info.IsDir()
}

// Branches lists the branches that have a snapshot, decoded back from
// their on-disk keys, sorted. Entries that don't decode (foreign dirs)
// are skipped rather than erroring.
func (s *Store) Branches() ([]string, error) {
	entries, err := os.ReadDir(s.tmpDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("snapstore: read tmp dir: %w", err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		branch, err := url.PathUnescape(e.Name())
		if err != nil {
			continue // not one of ours
		}
		out = append(out, branch)
	}
	sort.Strings(out)
	return out, nil
}

// lockSet takes the cross-process exclusive lock guarding one snapshot
// SET (the per-connection dumps under dbDir) and returns its release
// closure. Every entry point that reads or writes a whole set holds it
// for the duration.
//
// Per-file atomicity is not enough: a snapshot is restored as a SET, so
// two savers interleaving at connection granularity can leave conn1 from
// one run beside conn2 from another — a cross-connection-inconsistent
// snapshot no serial order of the two saves could produce, applied
// silently by a later restore (T3-7 pass #9, C-F8).
//
// Serialising is the right shape here, not "one owner, second refused":
// a saver dumps LIVE store state from inside its critical section rather
// than rewriting a file from a stale in-memory copy, so the second run's
// set is a legal, fully-consistent successor to the first's. The lock is
// an flock(2) the kernel drops on process death, so a killed `w17ctl`
// never wedges the next one.
// The sentinel lives INSIDE the set's own directory (`<dbDir>/.set.lock`)
// rather than beside it: that keeps it in the directory the operation
// already has to create, and [Store.Files] skips dotfiles so it never
// shows up as a dump.
func (s *Store) lockSet(dbDir string) (func(), error) {
	release, err := lockfile.ForUpdate(filepath.Join(dbDir, setLockName))
	if err != nil {
		return nil, fmt.Errorf("snapstore: lock %s: %w", dbDir, err)
	}
	return release, nil
}

// setLockName is the base name of the per-set lock sentinel; ForUpdate
// appends ".lock".
const setLockName = ".set"

// Save dumps every connection's store into the branch's snapshot dir,
// each via its Snapshotter, then refreshes the branch dir's mtime so
// Evict's LRU sees this as the most-recent branch. The `.gitignore`
// entry for `w17/tmp/` is ensured on the way in. The whole set is
// written under the branch's set lock — see [Store.lockSet].
func (s *Store) Save(ctx context.Context, branch string, conns []Conn) error {
	if err := s.ensureGitignored(); err != nil {
		return err
	}
	dir, err := s.dbDir(branch)
	if err != nil {
		return err
	}
	release, err := s.lockSet(dir)
	if err != nil {
		return err
	}
	defer release()
	if err := s.saveConns(ctx, dir, conns); err != nil {
		return err
	}
	return s.touch(branch)
}

// saveConns dumps every connection into dbDir (created if absent), each
// to a temp file atomically renamed into place — a failed dump never
// leaves a half-written snapshot a later Load would trust. Shared by the
// branch-state Save and the named-savepoint SaveNamed.
func (s *Store) saveConns(ctx context.Context, dbDir string, conns []Conn) error {
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		return fmt.Errorf("snapstore: mkdir %s: %w", dbDir, err)
	}
	for _, c := range conns {
		if err := s.saveOneTo(ctx, dbDir, c); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) saveOneTo(ctx context.Context, dbDir string, c Conn) error {
	final := filepath.Join(dbDir, c.file())
	tmp, err := os.CreateTemp(dbDir, "."+c.file()+".tmp-*")
	if err != nil {
		return fmt.Errorf("snapstore Save %s: temp: %w", c.Name, err)
	}
	tmpPath := tmp.Name()
	if err := c.Snapshotter.Dump(ctx, tmp); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("snapstore Save %s: dump: %w", c.Name, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("snapstore Save %s: close: %w", c.Name, err)
	}
	if err := os.Rename(tmpPath, final); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("snapstore Save %s: rename: %w", c.Name, err)
	}
	return nil
}

// Load restores every connection's store from the branch's snapshot
// dir via its Snapshotter. A missing snapshot file for a listed
// connection errors (a partial restore is worse than a loud refusal —
// same posture as the apply factory). It holds the branch's set lock so
// a concurrent Save cannot slide a second run's files in mid-restore.
func (s *Store) Load(ctx context.Context, branch string, conns []Conn) error {
	if !s.Has(branch) {
		return fmt.Errorf("snapstore Load: no snapshot for branch %q", branch)
	}
	dir, err := s.dbDir(branch)
	if err != nil {
		return err
	}
	release, err := s.lockSet(dir)
	if err != nil {
		return err
	}
	defer release()
	return s.loadConns(ctx, dir, conns)
}

// loadConns restores every connection from dbDir. Shared by the
// branch-state Load and the named-savepoint LoadNamed.
func (s *Store) loadConns(ctx context.Context, dbDir string, conns []Conn) error {
	for _, c := range conns {
		path := filepath.Join(dbDir, c.file())
		f, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("snapstore Load %s: open: %w", c.Name, err)
		}
		err = c.Snapshotter.Restore(ctx, f)
		_ = f.Close()
		if err != nil {
			return fmt.Errorf("snapstore Load %s: restore: %w", c.Name, err)
		}
	}
	return nil
}

// Remove deletes a branch's entire snapshot tree (db/ + the branch
// dir). Idempotent — removing an absent branch is a no-op.
func (s *Store) Remove(branch string) error {
	root, err := s.branchRoot(branch)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(root); err != nil {
		return fmt.Errorf("snapstore Remove %s: %w", branch, err)
	}
	return nil
}

// Evict enforces a max-N branch budget: it keeps the `keep`
// most-recently-saved branches (by branch-dir mtime, refreshed on each
// Save) and removes the rest, returning the dropped branch names so the
// caller can log them. keep <= 0 removes nothing (disables eviction).
// The currently-active branch is the caller's responsibility to keep
// large enough room for; Evict has no notion of "current".
func (s *Store) Evict(keep int) ([]string, error) {
	if keep <= 0 {
		return nil, nil
	}
	branches, err := s.Branches()
	if err != nil {
		return nil, err
	}
	if len(branches) <= keep {
		return nil, nil
	}
	type aged struct {
		branch string
		mod    int64
	}
	withAge := make([]aged, 0, len(branches))
	for _, b := range branches {
		root, err := s.branchRoot(b)
		if err != nil {
			continue
		}
		info, err := os.Stat(root)
		if err != nil {
			continue
		}
		withAge = append(withAge, aged{branch: b, mod: info.ModTime().UnixNano()})
	}
	// Newest first; drop everything past the keep cutoff.
	sort.Slice(withAge, func(i, j int) bool { return withAge[i].mod > withAge[j].mod })
	var dropped []string
	for _, a := range withAge[min(keep, len(withAge)):] {
		if err := s.Remove(a.branch); err != nil {
			return dropped, err
		}
		dropped = append(dropped, a.branch)
	}
	sort.Strings(dropped)
	return dropped, nil
}

// touch refreshes the branch dir's mtime to now so Evict's LRU treats
// a just-saved branch as the most recent. Uses a sentinel write rather
// than os.Chtimes(now) since the runtime forbids wall-clock reads in
// some contexts; creating+removing a file bumps the dir mtime via the
// filesystem's own clock.
func (s *Store) touch(branch string) error {
	dir, err := s.branchRoot(branch)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".touch-*")
	if err != nil {
		return fmt.Errorf("snapstore touch %s: %w", branch, err)
	}
	name := f.Name()
	_ = f.Close()
	if err := os.Remove(name); err != nil {
		return fmt.Errorf("snapstore touch %s: cleanup: %w", branch, err)
	}
	return nil
}

// ensureGitignored makes sure the scratch tree (`w17/tmp/`, among the
// other w17-owned transient/secret paths) is ignored, so snapshots never
// show up in `git status`. It delegates to the single source of truth —
// scaffold.EnsureW17Gitignore, which owns `w17/.gitignore` — so a project
// whose snapshots run before (or without) `w17ctl init` still gets the
// full, self-describing ignore file rather than a lone `w17/tmp/` line in
// the project-root .gitignore. Idempotent.
func (s *Store) ensureGitignored() error {
	if _, err := scaffold.EnsureW17Gitignore(s.projectRoot); err != nil {
		return fmt.Errorf("snapstore gitignore: %w", err)
	}
	return nil
}

// --- Named savepoints (workflow-modes M3) -------------------------------
//
// A named savepoint is a manual point-in-time dump scoped to ONE
// initiative, living beside the initiative's branch state:
//
//	<tmp>/<initiative>/db/                    # branch/live state (Save/Load)
//	<tmp>/<initiative>/snapshots/<name>/db/   # named savepoints (SaveNamed/LoadNamed)
//	<tmp>/<initiative>/snapshots/<name>/schema  # pinned schema hash (consistency guard)
//
// Scoping to an initiative keeps a savepoint schema-consistent with that
// initiative's lineage — activating one is only sound back into its own
// initiative (the caller compares the pinned hash against the current
// checkpoint).

const namedSchemaFile = "schema"

func (s *Store) namedRoot(initiative, name string) (string, error) {
	ik, err := branchKey(initiative)
	if err != nil {
		return "", err
	}
	nk, err := branchKey(name)
	if err != nil {
		return "", err
	}
	return filepath.Join(s.tmpDir, ik, "snapshots", nk), nil
}

func (s *Store) namedDBDir(initiative, name string) (string, error) {
	root, err := s.namedRoot(initiative, name)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "db"), nil
}

// HasNamed reports whether a named savepoint exists for the initiative.
// An invalid initiative/savepoint name "has" no savepoint.
func (s *Store) HasNamed(initiative, name string) bool {
	dir, err := s.namedDBDir(initiative, name)
	if err != nil {
		return false
	}
	info, err := os.Stat(dir)
	return err == nil && info.IsDir()
}

// SaveNamed dumps conns to a named savepoint under the initiative,
// pinning schemaHash (the initiative's current checkpoint/schema hash)
// so a later activate can verify the savepoint belongs to the lineage it
// is restored into. Overwrites an existing savepoint of the same name.
func (s *Store) SaveNamed(ctx context.Context, initiative, name, schemaHash string, conns []Conn) error {
	if err := s.ensureGitignored(); err != nil {
		return err
	}
	dbDir, err := s.namedDBDir(initiative, name)
	if err != nil {
		return err
	}
	release, err := s.lockSet(dbDir)
	if err != nil {
		return err
	}
	defer release()
	if err := s.saveConns(ctx, dbDir, conns); err != nil {
		return err
	}
	root, err := s.namedRoot(initiative, name)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(root, namedSchemaFile), []byte(schemaHash+"\n"), 0o644); err != nil {
		return fmt.Errorf("snapstore SaveNamed %s/%s: pin schema: %w", initiative, name, err)
	}
	return nil
}

// LoadNamed restores a named savepoint into the live stores.
func (s *Store) LoadNamed(ctx context.Context, initiative, name string, conns []Conn) error {
	if !s.HasNamed(initiative, name) {
		return fmt.Errorf("snapstore LoadNamed: no savepoint %q for initiative %q", name, initiative)
	}
	dbDir, err := s.namedDBDir(initiative, name)
	if err != nil {
		return err
	}
	release, err := s.lockSet(dbDir)
	if err != nil {
		return err
	}
	defer release()
	return s.loadConns(ctx, dbDir, conns)
}

// NamedSchemaHash returns the schema hash pinned at SaveNamed time (the
// consistency-guard input). Empty when none was pinned.
func (s *Store) NamedSchemaHash(initiative, name string) (string, error) {
	root, err := s.namedRoot(initiative, name)
	if err != nil {
		return "", err
	}
	body, err := os.ReadFile(filepath.Join(root, namedSchemaFile))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("snapstore NamedSchemaHash %s/%s: %w", initiative, name, err)
	}
	return strings.TrimSpace(string(body)), nil
}

// ListNamed lists the initiative's savepoint names, decoded + sorted.
func (s *Store) ListNamed(initiative string) ([]string, error) {
	ik, err := branchKey(initiative)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(s.tmpDir, ik, "snapshots"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("snapstore ListNamed %s: %w", initiative, err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name, derr := url.PathUnescape(e.Name())
		if derr != nil {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// RemoveNamed deletes a named savepoint. Idempotent.
func (s *Store) RemoveNamed(initiative, name string) error {
	root, err := s.namedRoot(initiative, name)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(root); err != nil {
		return fmt.Errorf("snapstore RemoveNamed %s/%s: %w", initiative, name, err)
	}
	return nil
}

// lastLiveFile records which branch w17 last built into the live local
// stores — the branch-switch detector compares the current git branch
// against it. Lives directly under w17/tmp (not per-branch).
const lastLiveFile = ".last-live-branch"

// LastLive returns the branch w17 last built into the live stores, or
// "" when none recorded yet (fresh project / first build).
func (s *Store) LastLive() (string, error) {
	body, err := os.ReadFile(filepath.Join(s.tmpDir, lastLiveFile))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("snapstore LastLive: %w", err)
	}
	return strings.TrimSpace(string(body)), nil
}

// SetLastLive records `branch` as the live branch (called after a build
// / reconcile leaves the stores matching it). Creates w17/tmp if absent.
func (s *Store) SetLastLive(branch string) error {
	if err := os.MkdirAll(s.tmpDir, 0o755); err != nil {
		return fmt.Errorf("snapstore SetLastLive: mkdir: %w", err)
	}
	if err := os.WriteFile(filepath.Join(s.tmpDir, lastLiveFile), []byte(branch+"\n"), 0o644); err != nil {
		return fmt.Errorf("snapstore SetLastLive: %w", err)
	}
	return nil
}

// Files lists the dump filenames stored for a branch (for diagnostics
// / `w17 stack` status output). Empty when the branch has no snapshot.
func (s *Store) Files(branch string) ([]string, error) {
	dir, err := s.dbDir(branch)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("snapstore Files %s: %w", branch, err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out, nil
}
