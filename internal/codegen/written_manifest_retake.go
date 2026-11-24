package codegen

import (
	"os"
	"path/filepath"
)

// Retake covers a rewrite of codegen's own outputs by another w17ctl command:
// the go.mod and go.sum of a generated module (w17/services/<bundle>/,
// w17/stubs/), which `sdk update` and `plugin update` edit and tidy.
//
// The record says which files codegen wrote and that nobody changed them since
// (wrote). A tidy changes them, so without this the next codegen read w17ctl's
// own tidy as a person's edit and refused every generated go.mod until
// --force — the flag the refusal tells you to diff first. The sequence the
// update itself prints (update, pin, codegen) ended in that refusal.
//
// Call Retake BEFORE the rewrite with the project-root-relative paths it may
// touch, and the returned commit AFTER it. Only a path the record names with
// the digest of what is on disk at the call is retaken: a file someone already
// edited stays theirs, and the next codegen still refuses it. commit records
// the new digest of each retaken path the rewrite changed; it does nothing
// when there is no record (the first-run case, where every existing output is
// codegen's anyway) or when nothing was retaken.
func Retake(root string, rels []string) (commit func()) {
	m, err := loadWrittenManifest(root)
	if err != nil || m == nil {
		return func() {}
	}
	var held []string
	for _, rel := range rels {
		rel = filepath.ToSlash(rel)
		target, err := containedJoin(root, rel)
		if err != nil {
			continue
		}
		if m.wrote(rel, target) {
			held = append(held, rel)
		}
	}
	if len(held) == 0 {
		return func() {}
	}
	return func() {
		now, err := loadWrittenManifest(root)
		if err != nil || now == nil {
			return
		}
		changed := false
		for _, rel := range held {
			target, err := containedJoin(root, rel)
			if err != nil {
				continue
			}
			d, err := fileDigest(target)
			if err != nil || now.Files[rel] == d {
				continue
			}
			now.Files[rel] = d
			changed = true
		}
		if !changed {
			return
		}
		p := filepath.Join(root, filepath.FromSlash(writtenManifestRel))
		if err := writeManifestFile(p, *now); err != nil {
			// A record left with the old digest only brings back the refusal
			// this exists to remove; it never lets an edit through.
			_ = os.Remove(p + ".tmp")
		}
	}
}
