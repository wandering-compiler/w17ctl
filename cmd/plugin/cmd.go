// Package plugin wires `w17ctl plugin <leaf>` — the plugin catalog
// commands (list / install / update / gen-pb). The internal catalog is
// embedded in the binary (embed/plugins, synced from plugins/* by
// `make sync-plugins-internal`); the command package owns both the
// embed and the install/update logic.
package plugin

// Cmd is the parent of `w17ctl plugin <leaf>` commands:
//
//   - `list`     — inventory of embedded + installed plugins
//   - `install`  — extract one plugin from the embedded catalog into the
//     project's `<proto_dir>/plugins/` tree + record it in the lock
//   - `update`   — re-extract one (or every) installed plugin from the
//     embedded catalog, refreshing the on-disk tree + the lock version
//
// `remove` is a v2 feature; until then operators manually
// `rm -rf <proto_dir>/plugins/<name>/` + drop the lock entry.
type Cmd struct {
	List    ListCmd    `cmd:"" help:"List embedded plugins + their install state in this project's lock."`
	Install InstallCmd `cmd:"" help:"Install one plugin from the embedded catalog into the project. Refuses URLs (v1 supports name-only)."`
	Update  UpdateCmd  `cmd:"" help:"Refresh one or every installed plugin's on-disk tree from the embedded catalog. Updates the recorded version in the lock."`
	GenPb   GenPbCmd   `cmd:"" name:"gen-pb" help:"Regenerate a plugin's standalone src/gen/pb from its proto/ (author-side dev/test loop; not used by project codegen)."`
}
