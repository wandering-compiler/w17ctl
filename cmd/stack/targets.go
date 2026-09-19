package stack

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/wandering-compiler/w17ctl/internal/containerdump"

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
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, s := range specs {
		snap, serr := sf(s.Connection)
		if serr != nil {
			skipped = append(skipped, fmt.Sprintf("%s (%v)", s.Connection, serr))
			continue
		}
		// A snapshot is taken by the database's OWN client, and this machine
		// may not have one — deliberately, for a project that keeps every
		// tool in Docker. The store's container has one, so the dump runs
		// there instead.
		//
		// Only as a FALLBACK: where the host has the client, it is used, so a
		// snapshot does not silently depend on a container still running.
		if !hostHasClientFor(s.DSN) {
			if in := containerdump.For(ctx, s.DSN); in != nil {
				snap = in
			}
		}
		conns = append(conns, snapstore.Conn{
			Name:        s.Connection,
			Ext:         factory.SnapshotExt(s.DSN),
			Snapshotter: snap,
		})
	}
	return conns, skipped, nil
}

// hostHasClientFor reports whether THIS machine carries the dump client a
// store's dialect needs.
//
// Asked before the dump rather than after it fails: the failure is an exec
// error naming a binary, which reads like a bug in w17 rather than a missing
// package — and it arrives at the moment somebody chose the careful option.
func hostHasClientFor(dsn string) bool {
	var bin string
	switch {
	case strings.HasPrefix(dsn, "postgres://"), strings.HasPrefix(dsn, "postgresql://"):
		bin = "pg_dump"
	case strings.HasPrefix(dsn, "mysql://"):
		bin = "mysqldump"
	default:
		// Every other store dumps through its own protocol, with no external
		// client to miss.
		return true
	}
	_, err := exec.LookPath(bin)
	return err == nil
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
