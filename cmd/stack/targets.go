package stack

import (
	"fmt"

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
