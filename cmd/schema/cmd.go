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
	Render RenderCmd `cmd:"" help:"Render the project's DEV schema plan into the artefact a generated binary applies (<out>/dev-plan.json). Replaces the db/init bootstrap: that ran only on a FRESH postgres volume, so a schema change never reached a database that already existed."`
}
