package codegen

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/wandering-compiler/w17ctl/internal/core"
	codegenpb "github.com/wandering-compiler/sdk/go/pb/w17compiler"
)

// writtenManifestRel is codegen's record of what it wrote: every output's
// path and digest. It answers two questions a header cannot. (1) Who wrote a
// header-less file, below. (2) Which files the orphan prune may delete — a
// banner is copied along with a file, so only the record tells codegen's file
// from a person's copy of it (pruneStale).
//
// A stamped output proves its own provenance: a copy on disk without the
// banner was written by someone else. A format the generator cannot stamp
// (JSON, a .env.defaults, a package.json) has no such proof, so a hand-written
// file at one of those paths was overwritten by `--force` like any other
// differing file. This file is the proof those formats lack: for every
// output, the digest of the bytes codegen left on disk. A header-less target
// codegen may overwrite is one this record names WITH that digest; anything
// else at the path — never recorded, or edited since — is refused in both
// modes, as an unstamped copy of a stamped output is.
//
// GITIGNORED (w17/.gitignore), not committed. What it records is this
// checkout's disk: most header-less outputs live in the ignored compiler
// output (w17/services/, w17/stubs/, the web clients), whose bytes differ
// between machines, so a committed copy would churn and conflict on facts
// that are not the project's. The cost is a fresh clone, which starts without
// it — see loadWrittenManifest.
const writtenManifestRel = "w17/.codegen-manifest.json"

const writtenManifestVersion = 1

type writtenManifest struct {
	Version int `json:"version"`
	// Files maps a project-root-relative slash path to the sha256 (hex) of
	// the bytes codegen left there.
	Files map[string]string `json:"files"`
}

// loadWrittenManifest reads the record, or returns nil when there is none.
//
// nil is the FIRST RUN of a project that predates the record (or a fresh
// clone): nothing on disk says who wrote a header-less target, so every one
// that exists is treated as codegen's for this one run — the behaviour before
// the record existed — and the run then writes the record, after which the
// rule applies. Refusing instead would stop every existing project on its
// first codegen with a list of files it has always regenerated.
//
// An unreadable or malformed record is an error, not a first run: falling
// back to "everything is codegen's" on a damaged file is exactly the
// overwrite this record exists to prevent. Deleting it is the way back.
// writtenManifestExists reports whether a record is on disk, readable or not.
func writtenManifestExists(root string) bool {
	_, err := os.Lstat(filepath.Join(root, filepath.FromSlash(writtenManifestRel)))
	return err == nil
}

func loadWrittenManifest(root string) (*writtenManifest, error) {
	p := filepath.Join(root, filepath.FromSlash(writtenManifestRel))
	b, err := os.ReadFile(p) // #nosec G304 -- fixed path under the project root
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", writtenManifestRel, err)
	}
	var m writtenManifest
	if err := json.Unmarshal(b, &m); err != nil || m.Version != writtenManifestVersion || m.Files == nil {
		return nil, fmt.Errorf("%s is not a codegen record this client reads (delete it: the next codegen "+
			"treats the existing header-less outputs as its own once and writes a new one)", writtenManifestRel)
	}
	return &m, nil
}

// wrote reports whether the record names rel with the digest of what is at
// target now: codegen wrote it, and nobody changed it since.
func (m *writtenManifest) wrote(rel, target string) bool {
	want, ok := m.Files[rel]
	if !ok {
		return false
	}
	got, err := fileDigest(target)
	return err == nil && got == want
}

// fileDigest is the sha256 of the file with CRLF line endings read as LF.
// The same bytes checked out by git with core.autocrlf (or through a Windows
// editor that kept the content) are the same file; a digest that told them
// apart would call codegen's own output "changed since codegen wrote it".
func fileDigest(p string) (string, error) {
	b, err := os.ReadFile(p) // #nosec G304 -- a containedJoin'd target
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n")))
	return hex.EncodeToString(sum[:]), nil
}

// headerlessOutput reports whether a write op's body is one the header
// cannot speak for — the outputs the record covers.
func headerlessOutput(contents []byte) bool {
	head := contents
	if len(head) > 4096 {
		head = head[:4096]
	}
	return !generatedMarkerRe.Match(head) && !infraMarkerRe.Match(head)
}

// recordWrittenManifest writes the record for this run's outputs: each one's
// digest as it is on disk NOW. Every output, banner or not — the banner
// decides OWNERSHIP of a stamped file (notOwnedByCodegen), but only the record
// says which banner-carrying files codegen wrote, and that is what the orphan
// prune deletes by (pruneStale). Called when the write pass ends
// (however it ends) and again at the end of the run, so a later step that
// rewrites an output it owns is recorded as codegen's too. A generate-if-
// missing scaffold is not recorded: once written it is the author's.
//
// An entry for a path this run did not write is KEPT while the file is still
// on disk. The orphan prune removes only files that carry a banner, so a
// header-less output of a surface switched off (a package.json, a
// .env.example) stays where it was — and when the surface comes back, the
// record must still say whose it is, or codegen refuses its own file. The
// entry keeps its old digest, so an edit made meanwhile is still caught. It
// drops out once the file is gone.
//
// A record that cannot be written is removed rather than left stale — a stale
// one would refuse codegen's own output next run, while none at all is the
// first-run case — and the run says so.
func recordWrittenManifest(root string, writes []*codegenpb.GeneratedFile) {
	m := writtenManifest{Version: writtenManifestVersion, Files: map[string]string{}}
	inRun := make(map[string]bool, len(writes))
	for _, f := range writes {
		rel := filepath.ToSlash(f.GetRelativePath())
		inRun[rel] = true
		if f.GetIfMissing() {
			continue
		}
		target, err := containedJoin(root, rel)
		if err != nil {
			continue
		}
		if d, err := fileDigest(target); err == nil {
			m.Files[rel] = d
		}
	}
	if prior, err := loadWrittenManifest(root); err == nil && prior != nil {
		for rel, d := range prior.Files {
			if inRun[rel] {
				continue
			}
			if target, err := containedJoin(root, rel); err == nil {
				if _, err := os.Stat(target); err == nil {
					m.Files[rel] = d
				}
			}
		}
	}
	p := filepath.Join(root, filepath.FromSlash(writtenManifestRel))
	err := writeManifestFile(p, m)
	if err != nil {
		_ = os.Remove(p)
		fmt.Fprintf(core.Stdout, "warning: could not record what codegen wrote (%v); the next run treats the "+
			"existing header-less outputs as its own once\n", err)
	}
}

func writeManifestFile(p string, m writtenManifest) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}
