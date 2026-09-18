// Package sdk wires the `w17ctl sdk` command. It is a thin kong adapter:
// it parses flags and delegates to internal/sdkupdate, which holds the
// implementation. (cmd/<command>/cmd.go is the conventional home of a
// command package's root command.)
package sdk

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/sdkupdate"
	codegenpb "github.com/wandering-compiler/sdk/go/pb/w17compiler"
)

// Cmd groups sdk subcommands.
//
// `update` and `pin` are two halves of one job and neither subsumes the
// other. `update` moves the FILES — go.mod + go.sum, in every module,
// including the hand-written one — over the module proxy alone, with no
// console and no Go toolchain, which is the whole reason it exists. `pin`
// records the version in the LOCK, which needs the console because the lock
// is signed.
//
// Without the pin, an update does not survive: codegen re-emits every
// GENERATED module's go.mod from the lock, so the next codegen run puts the
// old version back. `update` says so when it cannot reach the lock itself.
type Cmd struct {
	Update UpdateCmd `cmd:"" help:"Move this project onto a new public sdk/go release — rewrites the require + go.sum hashes in every module, including the hand-written one codegen won't touch."`
	Pin    PinCmd    `cmd:"" help:"Record the sdk/go version in the lock, so codegen emits it into every generated module and an update survives regeneration."`
	Unpin  UnpinCmd  `cmd:"" help:"Clear the recorded version — generated modules go back to resolving sdk/go independently."`
}

// UpdateCmd is `w17ctl sdk update`.
//
// Codegen pins the GENERATED modules, but it never rewrites the project's
// hand-written module (author-owned) and it resolves from the version the
// project already knows — so a project could not move onto a newer SDK at all.
// Doing it by hand needed go.sum hashes, i.e. a local Go toolchain, which a
// consumer is not required to have. This closes that loop over the module
// proxy alone.
type UpdateCmd struct {
	ProjectRoot string `arg:"" optional:"" default:"." help:"Project root. Defaults to cwd."`
	Version     string `name:"version" placeholder:"VERSION" help:"Pin this exact version (e.g. v0.0.0-20260716201145-36e33cc8168a). Default: the proxy's @latest."`
}

// Run delegates to the implementation, rendering progress to the shared
// output writer.
func (c *UpdateCmd) Run() error {
	// Whether anything MOVED decides whether the advice below is worth
	// printing. Read BEFORE the update rewrites it, and only for a version
	// the caller named — without one, "already up to date" means whatever
	// the proxy's @latest turns out to be, which is known only after the run.
	settled := c.Version != "" && sdkupdate.AlreadyAt(c.ProjectRoot, sdkupdate.SdkModule, c.Version)
	resolved, err := sdkupdate.Run(core.Stdout, c.ProjectRoot, c.Version)
	if err != nil {
		return err
	}
	// The bare form — the one a consumer actually types. It resolves @latest
	// itself, so the question "did anything move" can only be asked once that
	// is known: every module already required the version this run settled on.
	//
	// The first version of this suppression asked it of `c.Version` alone, so
	// it worked for `sdk update --version X` and never for `sdk update`. We
	// told a consumer it was fixed; they measured it again on a tree where
	// lock and go.mod agreed and the command changed not one byte, and the
	// advice was still there. Their word for why it matters is the right one:
	// it is advice people learn to skip, and they will skip it on the day it
	// is true.
	if !settled && resolved != "" {
		settled = sdkupdate.AlreadyAt(c.ProjectRoot, sdkupdate.SdkModule, resolved) &&
			lockAlreadyRecords(c.ProjectRoot, resolved)
	}
	if settled {
		// Nothing moved and the lock says the same thing, so there is
		// nothing to make stick.
		return nil
	}
	// The files are moved; the LOCK is not, and this command cannot move it
	// — the lock is signed by the console and this command deliberately
	// talks to nothing but the module proxy.
	//
	// Said out loud rather than left to be discovered, because the failure
	// is delayed and silent: everything builds now, and the next codegen
	// run rewrites every generated module's go.mod from the lock, putting
	// the old version back. Someone would then be looking at an update that
	// "did not take" with no reason visible anywhere.
	fmt.Fprintf(core.Stdout,
		"\nsdk update: the LOCK still records the old version — codegen re-emits generated go.mod files from it,\n"+
			"  so run `w17ctl sdk pin <version>` to make this stick past the next codegen.\n")
	return nil
}

// PinCmd implements `w17ctl sdk pin <version>` — the half `update` cannot do.
//
// It ships a LockEditIntent and writes back what the console returns: the
// lock is signed, so the client never edits it, and the version's SHAPE is
// validated server-side. That rule is about the Go module ecosystem and
// belongs to the compiler, not to a client meant to hold none.
type PinCmd struct {
	Version  string `arg:"" name:"version" help:"Go module version — v1.4.0, or a pseudo-version like v0.0.0-20260906130301-7b3d9c9300c9."`
	LockPath string `name:"lock" placeholder:"PATH" default:"w17/lock.yaml" help:"Path to the lock file."`
	Console  string `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console (owns the lock). Optional — falls back to the binary's compile-time default."`
}

func (c *PinCmd) Run() error {
	if err := core.EditLockOnDisk("sdk pin", c.Console, c.LockPath, &codegenpb.LockEditIntent{
		Intent: &codegenpb.LockEditIntent_SetSdkVersion{
			SetSdkVersion: &codegenpb.SetSdkVersionIntent{Version: c.Version},
		},
	}); err != nil {
		return err
	}
	// Name the next step: the pin reaches a go.mod only through codegen, so
	// a bare "ok" leaves someone looking at unchanged files.
	fmt.Fprintf(core.Stdout, "sdk pin: %s (lock re-signed — run codegen to write it into every generated module's go.mod)\n", c.Version)
	return nil
}

// UnpinCmd implements `w17ctl sdk unpin`.
//
// Its own leaf rather than `pin ""`: an empty positional argument is a typo
// far more often than an intention, and this one un-pins every module of the
// project.
type UnpinCmd struct {
	LockPath string `name:"lock" placeholder:"PATH" default:"w17/lock.yaml" help:"Path to the lock file."`
	Console  string `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console (owns the lock). Optional — falls back to the binary's compile-time default."`
}

func (c *UnpinCmd) Run() error {
	if err := core.EditLockOnDisk("sdk unpin", c.Console, c.LockPath, &codegenpb.LockEditIntent{
		Intent: &codegenpb.LockEditIntent_SetSdkVersion{
			SetSdkVersion: &codegenpb.SetSdkVersionIntent{Version: ""},
		},
	}); err != nil {
		return err
	}
	fmt.Fprintf(core.Stdout, "sdk unpin: cleared (lock re-signed — run codegen; generated modules go back to resolving sdk/go on their own)\n")
	return nil
}

// lockAlreadyRecords reports whether the project's lock already names this SDK
// version — the second half of "nothing to make stick".
//
// The advice below is about a DISAGREEMENT between the modules on disk and the
// lock codegen re-emits them from. Modules being up to date is not enough on
// its own: a tree can have both at the new version (nothing to say) or the
// modules moved with the lock left behind (the whole point of the advice).
//
// Read with a regexp rather than the lock decoder on purpose: this command
// talks to nothing but the module proxy — no console — and the lock is signed,
// so the one field it reads here is read, never written. A lock it cannot find
// or parse answers false, which keeps the advice.
func lockAlreadyRecords(root, version string) bool {
	b, err := os.ReadFile(filepath.Join(root, "w17", "lock.yaml"))
	if err != nil {
		return false
	}
	m := sdkVersionInLock.FindSubmatch(b)
	if m == nil {
		return false
	}
	return string(m[1]) == version
}

// sdkVersionInLock reads the one lock field this command needs.
var sdkVersionInLock = regexp.MustCompile(`(?m)^sdk_version:\s*"?([^"\s]+)"?\s*$`)
