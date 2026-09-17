// Package schema is `w17ctl schema` — the authoring-time half of the DEV
// schema path.
//
// The split it serves: planning a schema change is a compiler concern and
// happens on the console; applying it needs the database, so the binary that
// owns one does that. Between them sits every environment where neither holds
// at once — a compose stack whose Postgres appears seconds before the services
// do, a CI job, a fresh checkout. There the console ran at BUILD time and is
// gone by the time the database exists.
//
// So the plan is written down, exactly as a rendered fixture is, and the
// generated binary executes it with `<binary> schema apply`.
package schema

// Cmd is the `w17ctl schema` parent.
type Cmd struct {
	Render RenderCmd `cmd:"" help:"Render the project's DEV schema SNAPSHOT — one create-from-empty per connection — into the artefact a generated binary applies at start-up (schema-snapshot.json + a .ddl beside it). It BOOTSTRAPS an empty database; it cannot reconcile one that already has a schema. For that use 'w17ctl stack build'."`
}
