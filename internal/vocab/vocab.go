package vocab

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// w17VocabFS embeds the canonical w17 proto vocabulary
// (`proto/{w17,w17compiler,common}/`) so client-side proto parsing
// (`module add` activation collision check; future
// `push`-side plugin staging) can resolve
// `import "w17/<file>.proto"` without operator-supplied
// --import paths.
//
// Source of truth lives at `proto/w17/`; `make
// sync-w17-vocab-w17ctl` mirrors it under `embed/w17/`
// (gitignored, regenerated on every build).
//
//go:embed all:embed/w17 all:embed/w17compiler all:embed/common
var w17VocabFS embed.FS

// mkdirTempFn is indirected so tests can drive the tempdir-creation
// failure path (and hand back a read-only dir to exercise the
// mid-walk write-failure + cleanup path) without depending on a real
// os.MkdirTemp error. Production uses os.MkdirTemp.
var mkdirTempFn = os.MkdirTemp

// ExtractW17Vocab materialises the embedded vocabulary into
// a tempdir + returns the tempdir path + a cleanup func.
// Caller's deferred cleanup() removes the tempdir.
//
// Mirrors the jsclient pattern (`srcgo/domains/client/
// jsclient/main.go::ExtractW17Vocab`). `fs.Sub` strips the
// `embed/` prefix so paths land as `w17/<file>.proto` under
// the tempdir — exactly the shape `import "w17/db.proto"`
// expects to resolve against.
func ExtractW17Vocab() (string, func(), error) {
	tmpdir, err := mkdirTempFn("", "w17ctl-w17-vocab-*")
	if err != nil {
		return "", func() {}, err
	}
	cleanup := func() { _ = os.RemoveAll(tmpdir) }
	sub, err := fs.Sub(w17VocabFS, "embed")
	if err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("embed.Sub: %w", err)
	}
	err = fs.WalkDir(sub, ".", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == "." {
			return nil
		}
		target := filepath.Join(tmpdir, path)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		body, err := fs.ReadFile(sub, path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, body, 0o644)
	})
	if err != nil {
		cleanup()
		return "", func() {}, err
	}
	return tmpdir, cleanup, nil
}
