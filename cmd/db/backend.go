package db

import (
	"context"
	"fmt"

	stack "github.com/wandering-compiler/w17ctl/cmd/stack"
	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/remotesnap"
	"github.com/wandering-compiler/w17ctl/internal/snapstore"
)

// snapBackend is the storage-mode-agnostic surface `db snapshot` drives.
// Local mode = snapstore (dumps under w17/tmp, via each store's
// Snapshotter). Remote mode = remotesnap (server-side pg_dump via
// docker-exec; dumps live on the server, never transit the laptop —
// docs/experiments/remote-stack.md §6, "remote-side").
type snapBackend interface {
	save(ctx context.Context, initiative, name, schemaHash string) error
	load(ctx context.Context, initiative, name string) error
	has(initiative, name string) (bool, error)
	list(initiative string) ([]string, error)
	schemaHash(initiative, name string) (string, error)
	remove(initiative, name string) error
}

// resolveBackend picks the local or remote snapshot backend for the
// project at root, per its resolved mode.
func resolveBackend(root string, targets []string) (snapBackend, error) {
	rs, _, remote, err := stack.ResolveRemoteSnap(root)
	if err != nil {
		return nil, err
	}
	if remote {
		return &remoteBackend{rs: rs, root: root, targets: targets}, nil
	}
	return &localBackend{st: snapstore.New(root), root: root, targets: targets}, nil
}

// --- local backend (snapstore) --------------------------------------

type localBackend struct {
	st      *snapstore.Store
	root    string
	targets []string
}

func (b *localBackend) save(ctx context.Context, initiative, name, schemaHash string) error {
	conns, err := resolveConns(b.root, b.targets)
	if err != nil {
		return err
	}
	return b.st.SaveNamed(ctx, initiative, name, schemaHash, conns)
}

func (b *localBackend) load(ctx context.Context, initiative, name string) error {
	conns, err := resolveConns(b.root, b.targets)
	if err != nil {
		return err
	}
	return b.st.LoadNamed(ctx, initiative, name, conns)
}

func (b *localBackend) has(initiative, name string) (bool, error) {
	return b.st.HasNamed(initiative, name), nil
}
func (b *localBackend) list(initiative string) ([]string, error) {
	return b.st.ListNamed(initiative)
}
func (b *localBackend) schemaHash(initiative, name string) (string, error) {
	return b.st.NamedSchemaHash(initiative, name)
}
func (b *localBackend) remove(initiative, name string) error {
	return b.st.RemoveNamed(initiative, name)
}

// --- remote backend (remotesnap) ------------------------------------

type remoteBackend struct {
	rs      remotesnap.Store
	root    string
	targets []string
}

// pgServices resolves the postgres services to dump. Non-pg stores are
// reported + skipped (remote-side snapshots are pg-only: the dump runs
// inside the postgres container via docker-exec).
func (b *remoteBackend) pgServices() ([]string, error) {
	specs, err := (&stack.BuildCmd{Targets: b.targets}).ResolveTargets(b.root)
	if err != nil {
		return nil, err
	}
	pg := stack.PostgresServices(specs)
	for _, s := range specs {
		if !contains(pg, s.Connection) {
			fmt.Fprintf(core.Stdout, "db snapshot: remote-side snapshots are postgres-only — skipping non-pg store %s\n", s.Connection)
		}
	}
	if len(pg) == 0 {
		return nil, fmt.Errorf("no postgres stores to snapshot remotely (remote-side snapshots are pg-only)")
	}
	return pg, nil
}

func (b *remoteBackend) save(_ context.Context, initiative, name, schemaHash string) error {
	pg, err := b.pgServices()
	if err != nil {
		return err
	}
	return b.rs.SaveNamed(initiative, name, schemaHash, pg)
}

func (b *remoteBackend) load(_ context.Context, initiative, name string) error {
	pg, err := b.pgServices()
	if err != nil {
		return err
	}
	return b.rs.LoadNamed(initiative, name, pg)
}

func (b *remoteBackend) has(initiative, name string) (bool, error) {
	return b.rs.HasNamed(initiative, name)
}
func (b *remoteBackend) list(initiative string) ([]string, error) {
	return b.rs.ListNamed(initiative)
}
func (b *remoteBackend) schemaHash(initiative, name string) (string, error) {
	return b.rs.NamedSchemaHash(initiative, name)
}
func (b *remoteBackend) remove(initiative, name string) error {
	return b.rs.RemoveNamed(initiative, name)
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
