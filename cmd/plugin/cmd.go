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
	Install InstallCmd `cmd:"" help:"Install one plugin: a catalogue name, a release as <repo>#<plugin>/<version> (github.com/wandering-compiler/plugins#auth/v0.1.0-rc.1 — the fragment is the git TAG, so what you type is what the repository carries), or an UNRELEASED tree as <repo>#<plugin>@<40-hex-sha> for trying a fix before it is published."`
	Update  UpdateCmd  `cmd:"" help:"Refresh one or every installed plugin's on-disk tree to a published release. Updates the recorded version in the lock. REFUSES a commit-pinned plugin unless --to names a version: an update with no target means \"the newest release\", which is the tree a commit pin chose not to be on."`
	GenPb   GenPbCmd   `cmd:"" name:"gen-pb" help:"Regenerate a plugin's standalone src/gen/pb from its proto/ (author-side dev/test loop; not used by project codegen)."`
	Dev     DevCmd     `cmd:"" help:"Run a plugin AS ITSELF: stand up a throwaway w17 project, activate this plugin in it, install the tree the way a consumer receives it, and generate. The step a plugin's own unit tests cannot reach — the storage tier from its proto, the handlers staged with rewritten imports, the bundle compiling — because none of that exists until a project activates it."`
	Render  RenderCmd  `cmd:"" help:"Render an author tree into the PUBLISHED form a registry serves: the Go goes inert (.src) so the plugin is not a module in a consumer's build, src/gen/pb is left out because it is regenerated, and the tests travel so a reader outside the team can check the plugin. What plugin dev installs, and what a publish uploads."`
	Sign    SignCmd    `cmd:"" help:"Sign a plugin tree for release: digest it locally, ask the console to sign that digest under the plugin's own name and version, and write plugin.sig beside the manifest. The signing key never leaves the console. Publishing and tagging stay in scripts/publish-plugins.sh."`
}
