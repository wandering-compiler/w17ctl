package fixtures

// Cmd is the single `w17ctl fixtures` parent — fixture-seeding verbs
// live here. Today there is one:
//
//	fixtures apply — fetch a console-rendered fixture seed (a set of
//	                 parameterized upserts) and execute it against a
//	                 local target store in one transaction.
//
// This absorbs the former w17migrate `apply-fixtures` step into the
// thin client: the render is schema-aware and done server-side (the
// console's FixtureFetch service); w17ctl only executes the returned,
// pre-bound statements. The client carries zero codegen / compiler
// logic — it dials the public FixtureFetch service (NOT the private
// registry client), so there is no console/client (registrypb)
// dependency on this path.
type Cmd struct {
	Apply ApplyCmd `cmd:"" help:"Apply a console-rendered fixture seed (parameterized upserts) to a connection's target store (DSN via W17_TARGET_<CONN> env). Fetches the seed from the console, executes all statements in one transaction."`
	Fetch FetchCmd `cmd:"" help:"Download stored fixtures as editable JSON (one <out>/<domain>/<name>.json per fixture). Optional --domain scope. The authoring-side sync."`
}
