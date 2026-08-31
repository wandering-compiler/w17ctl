package stack

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/wandering-compiler/w17ctl/internal/devconfig"
	"github.com/wandering-compiler/w17ctl/internal/localtarget"
	"github.com/wandering-compiler/w17ctl/internal/snapstore"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/factory"
)

// SnapshotConns turns resolved dev-diff-apply targets into the
// per-store snapstore.Conn list (a Snapshotter + on-disk file extension
// per connection) that snapstore.Save/Load drive. It is the bridge
// between the target DSNs and the branch-snapshot store — used by the
// branch-switch reconcile and (later) the manual `db snapshot` command.
// A connection whose dialect has no snapshot adapter is skipped (its
// name returned in `skipped`) rather than failing the whole snapshot.
func SnapshotConns(specs []factory.TargetSpec) (conns []snapstore.Conn, skipped []string, err error) {
	sf := factory.SnapshotterFromTargets(specs)
	for _, s := range specs {
		snap, serr := sf(s.Connection)
		if serr != nil {
			skipped = append(skipped, fmt.Sprintf("%s (%v)", s.Connection, serr))
			continue
		}
		conns = append(conns, snapstore.Conn{
			Name:        s.Connection,
			Ext:         factory.SnapshotExt(s.DSN),
			Snapshotter: snap,
		})
	}
	return conns, skipped, nil
}

// modelTableMarker is the option a model proto carries on its
// table-backed messages. Its presence distinguishes model protos
// (which ir.BuildMany turns into tables) from query/mutation/service
// protos (which carry `(w17.db.method)` + `service` blocks instead), so
// `stack build` can auto-discover exactly the schema files.
var modelTableMarker = []byte("(w17.db.table)")

// moduleMarker is the FILE-level option that declares a module's
// connection. It has to join the discovery set even though such a file
// often declares no table at all, because ir.BuildManyWithOptions builds
// its domain-wide connection registry from the files it is HANDED: a
// connection declared in a module that this walk skipped is, to that
// build, undeclared. A KV/LOCAL_FS module is the shape that makes the
// gap reachable — it holds uploads, so it never declares a table, so the
// table marker alone can never pick it up, while a sibling module's
// `(w17.field).upload.connection` points straight at it.
//
// deinvo hit this on 2026-08-30: `codegen`, `verify` and `test` compile
// the whole tree and accepted their project; `stack build` compiled the
// table-declaring subset and refused it with `upload.connection
// "core-uploads" is not declared in this domain (declared:
// core-postgres)`. The refusal was right about what it saw — the file
// set was wrong. Note the registry's permissive gate does NOT cover
// this: it opens only when NO connection is declared at all, and here a
// sibling module declared one, so the check ran against a half-built
// registry.
var moduleMarker = []byte("(w17.module)")

// discoverModelProtos walks a proto root and returns the `.proto` files
// the IR build needs, split by what they contribute:
//
//   - models — those declaring a `(w17.db.table)`, which the build turns
//     into tables. Emptiness of THIS list is what makes a diff-apply a
//     no-op, and it has to stay that way: a build handed zero tables
//     would diff an empty schema against the checkpoint and propose
//     dropping every table in it.
//   - modules — those declaring a `(w17.module)`, which contribute only
//     the connection registry. They ride ALONG with models; they never
//     make an empty project look non-empty.
//
// Returns sorted paths for determinism; an empty result (no models /
// missing dir) is not an error.
func discoverModelProtos(protoRoot string) (models, modules []string, err error) {
	walkErrOut := filepath.WalkDir(protoRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if os.IsNotExist(walkErr) {
				return nil
			}
			return walkErr
		}
		if d.IsDir() || !strings.HasSuffix(path, ".proto") {
			return nil
		}
		body, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		switch {
		case bytes.Contains(body, modelTableMarker):
			models = append(models, path)
		case bytes.Contains(body, moduleMarker):
			// Only when it declares no table of its own — a module file
			// that also carries tables is already a model and must not be
			// listed twice.
			modules = append(modules, path)
		}
		return nil
	})
	if walkErrOut != nil {
		return nil, nil, walkErrOut
	}
	sort.Strings(models)
	sort.Strings(modules)
	return models, modules, nil
}

// resolveLocalTargets derives the local-store dev-diff-apply targets for
// the project's connections from the lock + the dev-machine port
// allocation (devconfig) — the SAME source `stack up` publishes its
// stores on. This is what lets `w17ctl stack build` dev-diff-apply with no
// `--target` flags: the published host port + the dev credentials
// codegen bakes into the compose are fully derivable.
//
// Resolution per connection `<domain>-<dialect>`:
//   - dialect inferred from the name suffix (postgres/mysql/redis);
//   - host port read from devconfig `Ports[W17_<CONN>_HOST_PORT]` (the
//     allocation `stack up` injects) — absent ⇒ the stack isn't up, so
//     there's nothing to apply to (skipped);
//   - credentials = the connection domain (codegen's dev default:
//     user=pass=db=<domain> for SQL stores).
//
// SQLite/NATS/S3 and any connection without an allocated port are
// skipped and reported in `skipped` so the caller can tell the dev why a
// store wasn't touched.
func resolveLocalTargets(connNames []string, p *devconfig.Project) (specs []factory.TargetSpec, skipped []string) {
	for _, name := range connNames {
		dsn, skip := localtarget.ResolveDSN(name, p)
		if dsn == "" {
			skipped = append(skipped, skip)
			continue
		}
		specs = append(specs, factory.TargetSpec{Connection: name, DSN: dsn})
	}
	return specs, skipped
}
