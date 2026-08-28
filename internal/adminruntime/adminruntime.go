// Package adminruntime vendors the public @w17/admin-runtime SPA
// framework into a consuming project. The runtime source lives at
// `sdk/ts/admin-runtime` (public FE SDK, mirrors sdk/go); `make
// sync-admin-runtime-w17ctl` copies it under `embed/admin-runtime/`
// (gitignored, node_modules/dist excluded, regenerated every build) so
// this PUBLIC client ships it embedded — no npm, no private srcgo, no
// round-trip through the console. When a codegen run emits an admin
// bundle, w17ctl materialises this tree into the project's
// `w17/admin-runtime/`, which the generated spa/package.json + Dockerfile
// reference with a local `file:` path. The consumer's Docker build then
// npm-installs + vite-builds the SPA and the admin Go binary embeds
// `spa/dist` (`//go:embed`) — the deployed artifact is a single
// standalone binary with zero JS/TS runtime dependency.
package adminruntime

import (
	"embed"
	"io/fs"
	"os"
	"path/filepath"
)

//go:embed all:embed/admin-runtime
var adminRuntimeFS embed.FS

// WriteTo materialises the embedded admin-runtime source tree into
// dstDir (e.g. `<project>/w17/admin-runtime`). Files are overwritten;
// the tree is small + stable so stale extras are not pruned. Only source
// is embedded — node_modules / dist are (re)produced by the consumer's
// build, not shipped.
func WriteTo(dstDir string) error {
	sub, err := fs.Sub(adminRuntimeFS, "embed/admin-runtime")
	if err != nil {
		return err
	}
	return fs.WalkDir(sub, ".", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		target := filepath.Join(dstDir, filepath.FromSlash(path))
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := fs.ReadFile(sub, path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
}
