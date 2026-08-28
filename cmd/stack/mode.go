package stack

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/devconfig"
	"github.com/wandering-compiler/w17ctl/internal/lockfile"
	"github.com/wandering-compiler/w17ctl/internal/remote"
	"github.com/wandering-compiler/w17ctl/internal/remotecompose"
	"github.com/wandering-compiler/w17ctl/internal/remotesnap"
	"github.com/wandering-compiler/w17ctl/internal/remotesync"
	"github.com/wandering-compiler/w17ctl/internal/tunnel"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/factory"
)

// dbTunnelTimeout bounds how long we wait for the transient DB tunnel to
// carry before running the DB operation (best-effort — see WaitReady).
const dbTunnelTimeout = 10 * time.Second

// Mode wiring (docs/experiments/remote-stack.md, Slice 5). Each compose
// verb resolves its execution mode (flag → project pin → global default →
// local) and, in remote mode, runs against a remote docker host over SSH
// instead of the local daemon. The local path is byte-for-byte unchanged.

// modeFlags are the shared --mode/--remote overrides every compose verb
// carries. They default to the resolved mode, so users almost never pass
// them (mode is pinned per project via `stack use-remote` / global
// `set-mode`).
type modeFlags struct {
	Mode   string `name:"mode" enum:"local,remote," default:"" help:"Override the execution mode for this run: local | remote. Empty = the resolved mode (project pin → global default → local)."`
	Remote string `name:"remote" placeholder:"NAME" help:"Override which registered remote to use (remote mode only). Empty = the resolved remote."`
}

// resolveMode loads the dev config and resolves the effective mode for the
// project at root. Returned cfg is reused by the caller (avoids a second load).
func (m modeFlags) resolveMode(root string) (devconfig.Mode, *devconfig.Config, error) {
	cfg, err := core.LoadDevConfigFn()
	if err != nil {
		return "", nil, err
	}
	mode, err := cfg.ResolveMode(root, m.Mode)
	if err != nil {
		return "", nil, err
	}
	return mode, cfg, nil
}

// remoteTarget is the fully-resolved remote destination for a project.
type remoteTarget struct {
	Runner remotecompose.Runner // compose over SSH into RemoteDir
	Dest   remote.Dest          // parsed SSH destination
	Name   string               // registered remote name (diagnostics/state)
	Base   string               // the remote's base path (rsync target parent)
}

// resolveRemote resolves the remote docker host for a project and builds
// the compose runner + rsync/tunnel inputs. project is the registry name
// (the per-project subdir on the server).
func (m modeFlags) resolveRemote(cfg *devconfig.Config, root, project string) (remoteTarget, error) {
	name, r, err := cfg.ResolveRemote(root, m.Remote)
	if err != nil {
		return remoteTarget{}, err
	}
	dest, err := remote.ParseDest(r.SSH)
	if err != nil {
		return remoteTarget{}, fmt.Errorf("remote %q: %w", name, err)
	}
	return remoteTarget{
		Runner: remotecompose.Runner{Dest: dest, RemoteDir: path.Join(r.Path, project)},
		Dest:   dest,
		Name:   name,
		Base:   r.Path,
	}, nil
}

// projectNameFromLock reads the project name from <root>/w17/lock.yaml via
// the offline lock reader — no side effects (unlike EnsureRegistered),
// used by the read-only remote verbs (down/logs/ps/restart/build).
func projectNameFromLock(root string) (string, error) {
	lk, err := lockfile.Load(filepath.Join(root, lockfile.DefaultPath))
	if err != nil {
		return "", err
	}
	if lk.Project == "" {
		return "", fmt.Errorf("lock at %s has no project name", root)
	}
	return lk.Project, nil
}

// rsyncExcludeFiles discovers the layered rsync ignore files for a project:
// the console-generated base (w17/rsync.ignore, DO NOT EDIT) then the
// optional client override (.w17-rsyncignore at the root). baseFound
// reports whether the generated base exists — the guard warns when it does
// not (codegen has not produced it yet).
func rsyncExcludeFiles(root string) (files []string, baseFound bool) {
	base := filepath.Join(root, remotesync.BaseIgnoreFile)
	if fi, err := os.Stat(base); err == nil && !fi.IsDir() {
		files = append(files, base)
		baseFound = true
	}
	custom := filepath.Join(root, remotesync.CustomIgnoreFile)
	if fi, err := os.Stat(custom); err == nil && !fi.IsDir() {
		files = append(files, custom)
	}
	return files, baseFound
}

// syncTree pushes the project tree to the remote (one-way, --delete). Used
// by remote `build` (full) and `up` (cheap, idempotent — compose/env may
// have changed). Excludes are the layered ignore files when present, else
// the built-in default list (pre-Slice-7 fallback).
func syncTree(root string, tgt remoteTarget, project string) error {
	spec := remotesync.Spec{
		LocalRoot:  root,
		Dest:       tgt.Dest,
		RemotePath: tgt.Base,
		Project:    project,
	}
	if files, _ := rsyncExcludeFiles(root); len(files) > 0 {
		spec.ExcludeFroms = files
	} else {
		spec.Excludes = remotesync.DefaultExcludes()
	}
	fmt.Fprintf(core.Stdout, "syncing → %s\n", spec.RemoteTarget())
	return remotesync.Run(spec)
}

// openTunnels (re)opens the SSH forwards for a project's published ports
// plus any extra control forwards, recording the detached child's pid so
// a later `stack down` can kill it. Idempotent: it first tears down any
// tunnel already recorded for this project (a previous `up`).
//
// The teardown + spawn + record runs inside tunnel.Replace's state lock,
// so two concurrent ups can't each spawn an ssh child and have the second
// record hide the first — that child would outlive every `stack down`.
func openTunnels(project string, tgt remoteTarget, ports map[string]int, extra ...tunnel.Forward) error {
	fwds := tunnel.ForwardsFromPorts(ports, extra...)
	if len(fwds) == 0 {
		return closeTunnels(project)
	}
	if err := tunnel.Replace(project, func() (tunnel.State, error) {
		sup := &tunnel.Supervisor{Dest: tgt.Dest, Forwards: fwds}
		if err := sup.Open(); err != nil {
			return tunnel.State{}, err
		}
		return tunnel.State{PID: sup.PID(), Remote: tgt.Name, Forwards: fwds}, nil
	}); err != nil {
		return err
	}
	fmt.Fprintf(core.Stdout, "opened %d tunnel(s) to %q — apps on localhost as usual\n", len(fwds), tgt.Name)
	return nil
}

// ResolveRemoteSnap builds the remote-side snapshot store for the project
// at root when it resolves to remote mode. ok=false ⇒ local mode (the
// caller uses the local snapstore). project is the registry name. Snapshots
// live under `<remote_path>/.snapshots/<project>` — a sibling of the rsync
// target, so `--delete` never touches them.
func ResolveRemoteSnap(root string) (store remotesnap.Store, project string, ok bool, err error) {
	cfg, err := core.LoadDevConfigFn()
	if err != nil {
		return remotesnap.Store{}, "", false, err
	}
	mode, err := cfg.ResolveMode(root, "")
	if err != nil {
		return remotesnap.Store{}, "", false, err
	}
	if mode != devconfig.ModeRemote {
		return remotesnap.Store{}, "", false, nil
	}
	name, err := projectNameFromLock(root)
	if err != nil {
		return remotesnap.Store{}, "", false, err
	}
	var mf modeFlags
	tgt, err := mf.resolveRemote(cfg, root, name)
	if err != nil {
		return remotesnap.Store{}, "", false, err
	}
	return remotesnap.Store{
		Dest:      tgt.Dest,
		RemoteDir: tgt.Runner.RemoteDir,
		Root:      path.Join(tgt.Base, ".snapshots", name),
	}, name, true, nil
}

// PostgresServices returns the postgres connection (= compose service)
// names among specs — the stores remotesnap can dump via docker-exec.
func PostgresServices(specs []factory.TargetSpec) []string {
	var out []string
	for _, s := range specs {
		if strings.HasPrefix(strings.ToLower(s.DSN), "postgres") {
			out = append(out, s.Connection)
		}
	}
	return out
}

// closeTunnels tears down a project's recorded tunnel (kills the detached
// ssh child + drops the state file). A no-op when none is open. The
// read-kill-remove sequence runs under the project's state lock.
func closeTunnels(project string) error {
	return tunnel.Close(project)
}

// withRemoteDB runs fn with the project's remote stores reachable at their
// localhost:<hostPort> DSNs (Slice 6, option A). If a persistent `stack
// up` tunnel is already open, the DB is reachable — fn runs directly.
// Otherwise it opens a TRANSIENT SSH tunnel over the published ports,
// waits (best-effort) for it to carry, runs fn, and tears the transient
// tunnel down — never disturbing a persistent one (it writes no state).
func withRemoteDB(project string, dest remote.Dest, ports map[string]int, fn func() error) error {
	if _, ok, err := tunnel.LoadState(project); err != nil {
		return err
	} else if ok {
		return fn() // persistent `up` tunnel carries the DB port already
	}
	fwds := tunnel.ForwardsFromPorts(ports)
	if len(fwds) == 0 {
		return fn() // nothing published/allocated → nothing to tunnel
	}
	sup := &tunnel.Supervisor{Dest: dest, Forwards: fwds}
	if err := sup.Open(); err != nil {
		return fmt.Errorf("open DB tunnel: %w", err)
	}
	defer func() { _ = sup.Close() }()
	if !tunnel.WaitReady(fwds, dbTunnelTimeout) {
		fmt.Fprintln(core.Stdout, "warning: DB tunnel not confirmed ready — proceeding (a connection error means it did not come up)")
	}
	return fn()
}
