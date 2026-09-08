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
//   - `install`  — fetch one plugin from the console's catalogue into the
//     project's `<proto_dir>/plugins/` tree + record it in the lock
//   - `update`   — re-fetch one (or every) installed plugin from the
//     console's catalogue, refreshing the on-disk tree + the lock version
//
// `remove` is a v2 feature; until then operators manually
// `rm -rf <proto_dir>/plugins/<name>/` + drop the lock entry.
type Cmd struct {
	List    ListCmd    `cmd:"" help:"List the console's plugin catalogue + each plugin's install state in this project's lock."`
	Install InstallCmd `cmd:"" help:"Install one plugin from the console's catalogue into the project. Refuses URLs (v1 supports name-only)."`
	Update  UpdateCmd  `cmd:"" help:"Refresh one or every installed plugin's on-disk tree from the embedded catalog. Updates the recorded version in the lock."`
	GenPb   GenPbCmd   `cmd:"" name:"gen-pb" help:"Regenerate a plugin's standalone src/gen/pb from its proto/ (author-side dev/test loop; not used by project codegen)."`
}
