// Package plugin wires `w17ctl plugin <leaf>` — the plugin catalogue
// commands (list / install / update / gen-pb). The catalogue is SERVED BY THE
// CONSOLE (ListPluginCatalog / FetchPlugin); the client carries no copy, so a
// plugin change reaches a consumer through a console deploy rather than
// through a client release. This package owns the install/update logic and
// nothing else.
package plugin

// Cmd is the parent of `w17ctl plugin <leaf>` commands:
//
//   - `list`     — inventory of the console's catalogue + installed plugins
//   - `install`  — fetch one plugin into the project's `<proto_dir>/plugins/`
//     tree + record it in the lock. Two sources: a NAME, served from the
//     console's catalogue, or a RELEASE in a repository, spelled
//     `<repo>#<plugin>/<version>`. The fragment is the git tag itself, so what
//     an operator types is what the repository carries. A repository install
//     also records the commit that tag resolved to and the digest of the tree
//     that landed, so the lock pins which BYTES were installed and not only
//     which version number
//   - `update`   — re-fetch one (or every) installed plugin from the
//     console's catalogue, refreshing the on-disk tree + the lock version
//
// `remove` is a v2 feature; until then operators manually
// `rm -rf <proto_dir>/plugins/<name>/` + drop the lock entry.
type Cmd struct {
	List    ListCmd    `cmd:"" help:"List the console's plugin catalogue + each plugin's install state in this project's lock."`
	Install InstallCmd `cmd:"" help:"Install one plugin: a catalogue name, or a release in a repository as <repo>#<plugin>/<version> (e.g. github.com/wandering-compiler/plugins#auth/v0.1.0-rc.1). The fragment is the git TAG, so what you type is what the repository carries."`
	Update  UpdateCmd  `cmd:"" help:"Refresh one or every installed plugin's on-disk tree from the CONSOLE's catalogue (FetchPlugin), not from this binary's embedded copy. Updates the recorded version in the lock."`
	GenPb   GenPbCmd   `cmd:"" name:"gen-pb" help:"Regenerate a plugin's standalone src/gen/pb from its proto/ (author-side dev/test loop; not used by project codegen)."`
}
