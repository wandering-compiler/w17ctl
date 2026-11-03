// Package adminruntime vendors the public @w17/admin-runtime SPA
// framework into a consuming project. The runtime source lives at
// `sdk/ts/admin-runtime` (public FE SDK, mirrors sdk/go); `make
// sync-admin-runtime-w17ctl` copies it under `embed/admin-runtime/`
// (gitignored, node_modules/dist excluded, regenerated every build) so
// this PUBLIC client ships it embedded — no npm, no private srcgo, no
// round-trip through the console. When a project has an admin bundle,
// w17ctl materialises this tree into the project's `w17/admin-runtime/`,
// which the generated spa/package.json + Dockerfile reference with a
// local `file:` path. The consumer's Docker build then npm-installs +
// vite-builds the SPA and the admin Go binary embeds `spa/dist`
// (`//go:embed`) — the deployed artifact is a single standalone binary
// with zero JS/TS runtime dependency.
//
// # The stamp
//
// The vendored copy carries [StampName]: which w17ctl wrote it and the
// digest of the tree that client ships. Without it nothing could tell an
// old copy from a current one — the copy is gitignored output, so no diff
// shows it either — and a project kept building its SPA against whatever
// runtime an earlier client had left behind, with nothing saying so.
// [Inspect] compares the copy on disk against the tree THIS client ships;
// `w17ctl codegen` re-vendors on any difference and `w17ctl verify`
// refuses one.
package adminruntime

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/gofmtc"
)

//go:embed all:embed/admin-runtime
var adminRuntimeFS embed.FS

// StampName is the file inside the vendored tree that records who vendored
// it. A dotfile so it reads as metadata next to the package's own files; the
// SPA's build never looks at it.
const StampName = ".w17-vendor.json"

// Stamp is what [StampName] holds.
type Stamp struct {
	// W17ctl is the version of the client that vendored the tree ("dev" for
	// an unreleased build — honest, and deliberately not comparable).
	W17ctl string `json:"w17ctl"`
	// Digest identifies the tree that client ships: sha256 over every
	// embedded file's path and bytes. The digest, not the version, decides
	// freshness — two dev builds share a version and can ship different
	// runtimes.
	Digest string `json:"digest"`
}

// The embedded tree is fixed for the life of the process; read it once.
var (
	shipOnce  sync.Once
	shipFiles map[string][]byte
	shipPaths []string
	shipSum   string
	shipErr   error
)

func shipped() (map[string][]byte, []string, string, error) {
	shipOnce.Do(func() {
		sub, err := fs.Sub(adminRuntimeFS, "embed/admin-runtime")
		if err != nil {
			shipErr = err
			return
		}
		shipFiles = map[string][]byte{}
		shipErr = fs.WalkDir(sub, ".", func(p string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if d.IsDir() || p == StampName {
				return nil
			}
			data, err := fs.ReadFile(sub, p)
			if err != nil {
				return err
			}
			shipFiles[p] = data
			shipPaths = append(shipPaths, p)
			return nil
		})
		sort.Strings(shipPaths)
		shipSum = digestOf(shipPaths, shipFiles)
	})
	return shipFiles, shipPaths, shipSum, shipErr
}

// digestOf is length-prefixed so no two different trees can concatenate to
// the same input (a path ending where content begins).
func digestOf(paths []string, files map[string][]byte) string {
	h := sha256.New()
	for _, p := range paths {
		b := files[p]
		h.Write([]byte(strconv.Itoa(len(p)) + ":" + p + strconv.Itoa(len(b)) + ":"))
		h.Write(b)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// Digest is the digest of the runtime this client ships.
func Digest() (string, error) {
	_, _, d, err := shipped()
	return d, err
}

// Current is the stamp this client writes.
func Current() (Stamp, error) {
	d, err := Digest()
	if err != nil {
		return Stamp{}, err
	}
	return Stamp{W17ctl: version(), Digest: d}, nil
}

func version() string {
	if core.Version == "" {
		return "dev"
	}
	return core.Version
}

// State is how a vendored tree relates to the one this client ships.
type State int

const (
	// Fresh: stamped with this client's digest, every shipped file on disk
	// as shipped.
	Fresh State = iota
	// Missing: no vendored tree at all.
	Missing
	// Unstamped: a tree with no stamp — vendored by a client from before the
	// stamp existed, so of unknown age. Treated as stale.
	Unstamped
	// Stale: stamped by a client that ships a different runtime.
	Stale
	// Modified: the stamp matches, but files on disk differ from it — edited
	// in place, or partly deleted.
	Modified
)

// Status is [Inspect]'s answer.
type Status struct {
	State State
	// On is the stamp found on disk (zero when none, or unreadable).
	On Stamp
	// Want is the stamp this client would write.
	Want Stamp
	// Differs names up to a few shipped files that do not match on disk
	// (Modified only).
	Differs []string
}

// Describe renders the status for a person: which client wrote the copy
// and which one is asking.
func (s Status) Describe() string {
	switch s.State {
	case Fresh:
		return fmt.Sprintf("up to date (w17ctl %s, %s)", s.On.W17ctl, short(s.On.Digest))
	case Missing:
		return "not vendored"
	case Unstamped:
		return fmt.Sprintf("carries no vendor stamp — written by a w17ctl older than the stamp, so of unknown age; this w17ctl (%s) ships %s",
			s.Want.W17ctl, short(s.Want.Digest))
	case Stale:
		return fmt.Sprintf("vendored by w17ctl %s (%s); this w17ctl (%s) ships %s",
			s.On.W17ctl, short(s.On.Digest), s.Want.W17ctl, short(s.Want.Digest))
	case Modified:
		return fmt.Sprintf("stamped as w17ctl %s's runtime but edited on disk (%v) — the vendored runtime is a verbatim copy, never edited here",
			s.On.W17ctl, s.Differs)
	}
	return "unknown"
}

func short(d string) string {
	if len(d) > len("sha256:")+12 {
		return d[:len("sha256:")+12]
	}
	return d
}

// Inspect compares the tree at dir against the one this client ships,
// without writing anything.
func Inspect(dir string) (Status, error) {
	files, paths, _, err := shipped()
	if err != nil {
		return Status{}, err
	}
	want, err := Current()
	if err != nil {
		return Status{}, err
	}
	st := Status{Want: want}
	if fi, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		st.State = Missing
		return st, nil
	} else if err != nil {
		return Status{}, err
	} else if !fi.IsDir() {
		return Status{}, fmt.Errorf("%s is not a directory", dir)
	}
	raw, err := os.ReadFile(filepath.Join(dir, StampName))
	if errors.Is(err, fs.ErrNotExist) {
		st.State = Unstamped
		return st, nil
	} else if err != nil {
		return Status{}, err
	}
	if json.Unmarshal(raw, &st.On) != nil || st.On.Digest == "" {
		// An unreadable stamp says no more than an absent one.
		st.On = Stamp{}
		st.State = Unstamped
		return st, nil
	}
	if st.On.Digest != want.Digest {
		st.State = Stale
		return st, nil
	}
	for _, p := range paths {
		if !gofmtc.SameOnDisk(filepath.Join(dir, filepath.FromSlash(p)), files[p]) {
			st.Differs = append(st.Differs, p)
			if len(st.Differs) == 3 {
				break
			}
		}
	}
	if len(st.Differs) > 0 {
		st.State = Modified
	}
	return st, nil
}

// Sync makes dir hold exactly the runtime this client ships, then stamps
// it. It writes only the files that differ (the runtime changes only when
// w17ctl does, and rewriting ~75 identical files on every regen woke file
// watchers and re-indexed the SPA for nothing), and removes the files a
// previous runtime shipped that this one does not — an old module left
// beside the new ones can still be imported. `node_modules` and `dist` are
// never touched: they are the consumer's build products, not shipped.
//
// It returns how many files it wrote or removed, the stamp excluded.
func Sync(dir string) (int, error) {
	files, paths, _, err := shipped()
	if err != nil {
		return 0, err
	}
	want, err := Current()
	if err != nil {
		return 0, err
	}
	changed := 0
	for _, p := range paths {
		target := filepath.Join(dir, filepath.FromSlash(p))
		if gofmtc.SameOnDisk(target, files[p]) {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return changed, err
		}
		if err := os.WriteFile(target, files[p], 0o644); err != nil {
			return changed, err
		}
		changed++
	}
	n, err := pruneExtras(dir, files)
	changed += n
	if err != nil {
		return changed, err
	}
	body, err := json.MarshalIndent(want, "", "  ")
	if err != nil {
		return changed, err
	}
	body = append(body, '\n')
	stamp := filepath.Join(dir, StampName)
	if !gofmtc.SameOnDisk(stamp, body) {
		if err := os.WriteFile(stamp, body, 0o644); err != nil {
			return changed, err
		}
	}
	return changed, nil
}

func pruneExtras(dir string, files map[string][]byte) (int, error) {
	removed := 0
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel != "." && (d.Name() == "node_modules" || d.Name() == "dist") {
				return filepath.SkipDir
			}
			return nil
		}
		if rel == StampName {
			return nil
		}
		if _, ok := files[rel]; ok {
			return nil
		}
		if err := os.Remove(p); err != nil {
			return err
		}
		removed++
		return nil
	})
	return removed, err
}

// ProjectDir is where a project's vendored runtime lives, relative to the
// project root: beside the services tree, under the same w17/ root (the
// generated spa/package.json reaches it as ../../../).
func ProjectDir(servicesDir string) string {
	w17Root := "w17"
	if s := filepath.ToSlash(servicesDir); s != "" {
		w17Root = strings.SplitN(s, "/", 2)[0]
	}
	return path.Join(w17Root, "admin-runtime")
}

// ProjectHasAdmin reports whether the project on disk holds an admin bundle
// whose SPA depends on the vendored runtime — a generated
// `<services>/<domain>-admin/spa/package.json` naming @w17/admin-runtime.
func ProjectHasAdmin(root, servicesDir string) bool {
	if servicesDir == "" {
		servicesDir = path.Join("w17", "services")
	}
	matches, _ := filepath.Glob(filepath.Join(root, filepath.FromSlash(servicesDir), "*-admin", "spa", "package.json"))
	for _, m := range matches {
		if b, err := os.ReadFile(m); err == nil && strings.Contains(string(b), `"@w17/admin-runtime"`) {
			return true
		}
	}
	return false
}
