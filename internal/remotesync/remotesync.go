// Package remotesync builds and runs the one-shot rsync that pushes a
// project's tree to a remote docker host in remote-stack mode
// (docs/experiments/remote-stack.md §5). Local is the sole authority —
// the transfer is strictly one-way local→remote, with `--delete` so a
// file codegen removed locally is removed remote too (else it lingers and
// gets baked into the image).
//
// There is no sync daemon: Go always compiles and builds are on-demand
// (`stack build`), so a single `rsync -az --delete` at build time is all
// that is needed. The argv is a pure function of the Spec (unit-tested);
// the exec is behind the RunSyncFn seam (stubbed in tests).
package remotesync

import (
	"os"
	"os/exec"
	"path"
	"strings"

	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/remote"
)

const (
	// BaseIgnoreFile is the console-GENERATED rsync ignore list, relative
	// to the project root. It lives under w17/ (DO NOT EDIT — regenerated
	// by codegen, Slice 7) even though its patterns exclude paths in the
	// parent tree: rsync resolves --exclude-from patterns against the
	// transfer root (the project root), independent of where the file sits.
	BaseIgnoreFile = "w17/rsync.ignore"

	// CustomIgnoreFile is the OPTIONAL client-owned override, at the
	// project root so codegen never touches it. Layered on top of the base
	// list. Absent by default.
	CustomIgnoreFile = ".w17-rsyncignore"
)

// Spec fully describes one push. Everything the argv needs is here, so
// Args() is a pure function tests can assert without a live transfer.
type Spec struct {
	// LocalRoot is the absolute source project root. Its CONTENTS (note
	// the trailing slash Args adds) are mirrored into the remote subdir.
	LocalRoot string

	// Dest is the parsed SSH destination of the target remote.
	Dest remote.Dest

	// RemotePath is the base dir on the server; the project lands in the
	// <RemotePath>/<Project>/ subdir.
	RemotePath string

	// Project is the project name — the per-project subdir on the server.
	Project string

	// ExcludeFroms are --exclude-from file paths, applied in order. They
	// layer: the console-generated base (BaseIgnoreFile) first, then the
	// optional client override (CustomIgnoreFile). Non-empty ExcludeFroms
	// WINS over the inline Excludes.
	ExcludeFroms []string

	// Excludes are inline --exclude patterns, used only when ExcludeFroms
	// is empty (the built-in fallback before codegen has produced the base
	// ignore file). DefaultExcludes() is the built-in MVP list.
	Excludes []string

	// DryRun adds --rsync --dry-run (preview what would transfer/delete).
	DryRun bool
}

// DefaultExcludes is the built-in ignore list for the MVP (before the
// console generates a project-specific one). It keeps VCS metadata, JS
// deps, snapshots, and local build caches out of the image context. DB
// data lives in docker-managed volumes outside the tree, so it is never
// in scope for --delete regardless; the *.snapshot / .snapshots entries
// are belt-and-braces.
func DefaultExcludes() []string {
	return []string{
		".git/",
		"node_modules/",
		"w17/tmp/", // dev DB snapshot scratch (git-ignored, staged locally)
		"*.snapshot",
		".snapshots/",
		".DS_Store",
	}
}

// RemoteTarget is the rsync destination: "<sshTarget>:<RemotePath>/<Project>/".
// The trailing slash pairs with the trailing slash Args puts on the
// source so rsync mirrors directory CONTENTS, not a nested dir.
func (s Spec) RemoteTarget() string {
	dir := path.Join(s.RemotePath, s.Project)
	return s.Dest.Target + ":" + dir + "/"
}

// Args builds the rsync argv (everything after the program name). Pure —
// no filesystem or network access — so it is fully unit-testable.
func (s Spec) Args() []string {
	args := []string{"-az", "--delete"}
	if s.DryRun {
		args = append(args, "--dry-run")
	}
	if shell := s.Dest.RsyncShell(); shell != "" {
		args = append(args, "-e", shell)
	}
	if len(s.ExcludeFroms) > 0 {
		for _, f := range s.ExcludeFroms {
			args = append(args, "--exclude-from", f)
		}
	} else {
		for _, ex := range s.Excludes {
			args = append(args, "--exclude", ex)
		}
	}
	// Trailing slash on the source → mirror contents into the target dir.
	args = append(args, strings.TrimRight(s.LocalRoot, "/")+"/", s.RemoteTarget())
	return args
}

// RunSyncFn is the seam tests stub; production runs realRunSync.
var RunSyncFn = realRunSync

func realRunSync(s Spec) error {
	cmd := exec.Command("rsync", s.Args()...)
	cmd.Stdout = core.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// Run pushes the spec's tree to the remote via the RunSyncFn seam.
func Run(s Spec) error { return RunSyncFn(s) }
