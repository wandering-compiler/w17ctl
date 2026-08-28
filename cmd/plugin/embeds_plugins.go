package plugin

import (
	"embed"
	"io/fs"
)

// internalPluginsFS embeds the catalog of plugins shipped inside
// the w17ctl binary. The source of truth is the repo-root
// `plugins/<name>/` trees; `make sync-plugins-internal` mirrors
// them under `embed/plugins/` so this `//go:embed` walks a
// stable tree on every build. The mirror is `.gitignore`d —
// always go through `make build-w17ctl`, which chains the sync
// step before `go build`.
//
// The catalog ships the `auth` and `payment` plugins today;
// future internal plugins land alongside them (one `plugins/<name>/`
// dir each). Per the plugin-system implementation plan
// (`docs/specs/plugins/implementation-plan.md`).
//
//go:embed all:embed/plugins
var internalPluginsFS embed.FS

// internalPluginsRootFn defaults to the real go:embed-backed
// lookup; tests override it to inject a synthetic catalog via
// `fstest.MapFS` so install / list paths can be exercised
// without modifying the on-disk source tree.
var internalPluginsRootFn = realInternalPluginsRoot

// internalPluginsRoot returns the embedded fs rooted at the
// plugin catalog (strips the `embed/plugins/` prefix). `list`,
// `install`, `update` all walk relative paths under this root —
// `<plugin-name>/plugin.yaml`, `<plugin-name>/types/...`.
func internalPluginsRoot() (fs.FS, error) {
	return internalPluginsRootFn()
}

func realInternalPluginsRoot() (fs.FS, error) {
	return fs.Sub(internalPluginsFS, "embed/plugins")
}

// internalPluginNames returns the set of plugin directories
// present in the embedded catalog. README.md (and any other
// top-level file) is filtered out — only directories qualify.
// Empty slice when no concrete plugins ship yet.
func internalPluginNames() ([]string, error) {
	root, err := internalPluginsRoot()
	if err != nil {
		return nil, err
	}
	entries, err := fs.ReadDir(root, ".")
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		out = append(out, e.Name())
	}
	return out, nil
}
