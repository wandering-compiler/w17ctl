package update

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"golang.org/x/mod/semver"
	"golang.org/x/term"

	plugincmd "github.com/wandering-compiler/w17ctl/cmd/plugin"
	sdkcmd "github.com/wandering-compiler/w17ctl/cmd/sdk"
	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/prompter"
	"github.com/wandering-compiler/w17ctl/internal/sdkupdate"
	"github.com/wandering-compiler/w17ctl/internal/updatecheck"
)

// reportFn is the check itself; a seam so tests drive it without a network.
var reportFn = report

// applyFn applies a report; a seam for the same reason.
var applyFn = apply

// report builds the full check for the project at root ("" = no project: the
// binary alone).
func report(console, root string, latest func() (string, error)) updatecheck.Report {
	src := updatecheck.Sources{}
	if root != "" {
		src = updatecheck.ProjectSources(root)
		src.SdkFloor, src.SdkFloorSource = sdkFloor(console, root)
	}
	src.ClientCurrent = core.Version
	src.ClientLatest = latest
	return updatecheck.Check(src)
}

// clientLatest asks the installer — the one resolver of "newest release" this
// binary uses (see [Cmd]) — on the same track `update` would install from.
func clientLatest() (string, error) { return (&Cmd{}).clientLatest() }

// clientLatest with this command's own --version / --stable applied, so the
// version the report announces is the one `--all` then installs.
func (c *Cmd) clientLatest() (string, error) {
	if c.Version != "" {
		return c.Version, nil
	}
	args := []string{"--print-version"}
	if c.allowPrerelease() {
		args = append(args, "--pre")
	}
	out, err := RunInstallerFn(args)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// sdkFloor is the higher of this client's floor and the console's — what the
// code about to be generated needs. A console that cannot be asked leaves the
// client's own.
func sdkFloor(console, root string) (floor, source string) {
	floor, source = core.SdkFloor, "this w17ctl"
	view, err := core.DescribeLockFromRoot(console, root)
	if err != nil {
		return floor, source
	}
	if f := view.GetSdkFloor(); semver.IsValid(f) && (!semver.IsValid(floor) || semver.Compare(floor, f) < 0) {
		floor, source = f, "the console"
	}
	return floor, source
}

// report is the command's own check: its flags decide the client track.
func (c *Cmd) report(root string) updatecheck.Report {
	return reportFn(c.Console, root, c.clientLatest)
}

func (c *Cmd) runCheck() error {
	root, _ := core.FindProjectRoot()
	updatecheck.Render(core.Stdout, c.report(root))
	return nil
}

// runAll applies every item of the report, each through the command that owns
// it, client last — this process is the OLD binary, and everything before it
// does not depend on which one runs it.
func (c *Cmd) runAll() error {
	root, err := core.FindProjectRoot()
	if err != nil {
		return fmt.Errorf("update --all: run it from a project (%w); `w17ctl update` alone updates the binary", err)
	}
	r := c.report(root)
	updatecheck.Render(core.Stdout, r)
	if c.DryRun {
		// A preview, and only that: every step after this rewrites go.mod
		// files, re-signs the lock or moves plugins.
		fmt.Fprintln(core.Stdout, "update --all --dry-run: nothing changed")
		return nil
	}
	if _, err := applyFn(c.Console, root, r, c); err != nil {
		return err
	}
	return nil
}

// apply moves what the report found. It returns true when it replaced this
// binary, so a caller about to keep working knows it is now the old one.
func apply(console, root string, r updatecheck.Report, self *Cmd) (bool, error) {
	clientMoved := false
	for _, it := range r.Items {
		switch it.Kind {
		case updatecheck.KindSDK:
			if _, err := sdkupdate.Run(core.Stdout, root, it.Latest); err != nil {
				return false, fmt.Errorf("update: sdk/go: %w", err)
			}
			pin := &sdkcmd.PinCmd{Version: it.Latest, LockPath: "w17/lock.yaml", Console: console}
			if err := inProject(root, pin.Run); err != nil {
				return false, fmt.Errorf("update: sdk pin: %w", err)
			}
		}
	}
	for _, it := range r.Items {
		if it.Kind == updatecheck.KindPlugin {
			up := &plugincmd.UpdateCmd{All: true, Console: console}
			if err := inProject(root, up.Run); err != nil {
				return false, fmt.Errorf("update: plugins: %w", err)
			}
			break
		}
	}
	for _, it := range r.Items {
		if it.Kind == updatecheck.KindClient {
			if self == nil {
				self = &Cmd{}
			}
			if err := self.runSelf(); err != nil {
				return false, err
			}
			clientMoved = true
		}
	}
	return clientMoved, nil
}

// inProject runs fn with the working directory at root — the lock paths the
// owning commands default to are project-relative.
func inProject(root string, fn func() error) error {
	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	if err := os.Chdir(root); err != nil {
		return err
	}
	defer func() { _ = os.Chdir(wd) }()
	return fn()
}

// ErrUpdateDeclined is codegen stopping because the operator chose to.
var ErrUpdateDeclined = errors.New("codegen: stopped before generating — nothing was written")

// Interactive reports whether codegen may ask: a person at a terminal, not a
// pipeline. CI is never asked — there the floor refusal codegen already has is
// the answer, and a prompt would hang the job.
var Interactive = func() bool {
	if os.Getenv("CI") != "" {
		return false
	}
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
}

// NewPrompter is the input source for the codegen prompt; a seam for tests.
var NewPrompter = prompter.NewStdinPrompter

// PreGenerate is the check `w17ctl codegen` runs before generating.
//
// At a terminal only. A REQUIRED update (the project pins an sdk/go older than
// the floor the code is generated against) is offered, and declining stops
// codegen: the code it would write cannot build, and codegen would refuse it
// afterwards anyway. An AVAILABLE update is offered with "continue anyway" in
// the alpha phase, where any release may carry a breaking fix and the advice to
// adopters is to stay current; from v1 it is one line and codegen goes on.
//
// If the binary itself was replaced, codegen stops so it is re-run by the new
// one — this process is the old client.
// PreGenerateTimeout bounds the check codegen runs first. It asks the network
// (the installer, the module proxy, every plugin's registry) and codegen did
// not use to; a captive portal or a stalled proxy must cost codegen this much
// at most, never a hang.
var PreGenerateTimeout = 20 * time.Second

// reportWithin runs the check with a deadline. On expiry the check is
// abandoned (its subprocesses end with this process) and ok is false.
func reportWithin(d time.Duration, console, root string) (updatecheck.Report, bool) {
	done := make(chan updatecheck.Report, 1)
	go func() { done <- reportFn(console, root, clientLatest) }()
	select {
	case r := <-done:
		return r, true
	case <-time.After(d):
		return updatecheck.Report{}, false
	}
}

func PreGenerate(console, root string) error {
	if !Interactive() {
		return nil
	}
	r, ok := reportWithin(PreGenerateTimeout, console, root)
	if !ok {
		fmt.Fprintf(core.Stdout, "codegen: update check had no answer in %s — skipped (`w17ctl update --check` to look)\n", PreGenerateTimeout)
		return nil
	}
	if len(r.Items) == 0 {
		return nil
	}
	required, available := r.Required(), r.Available()
	alpha := updatecheck.Alpha(core.Version)
	if len(required) == 0 && !alpha {
		fmt.Fprintf(core.Stdout, "codegen: %d update(s) available — `w17ctl update --check`\n", len(available))
		return nil
	}
	fmt.Fprintln(core.Stdout, "codegen: before generating —")
	updatecheck.Render(core.Stdout, r)

	const (
		optUpdate   = "update now"
		optContinue = "continue without updating"
		optAbort    = "stop"
	)
	options := []string{optUpdate, optAbort}
	question := "a REQUIRED update is missing — the code would not build without it:"
	if len(required) == 0 {
		options = []string{optUpdate, optContinue, optAbort}
		question = "w17 is in alpha: releases can carry breaking fixes, and staying current is the advice:"
	}
	choice, err := NewPrompter().Select(question, options, optUpdate)
	if err != nil {
		return err
	}
	switch choice {
	case optContinue:
		return nil
	case optAbort:
		return ErrUpdateDeclined
	}
	moved, err := applyFn(console, root, r, nil)
	if err != nil {
		return err
	}
	if moved {
		return fmt.Errorf("codegen: w17ctl was updated — run `w17ctl codegen` again so the NEW binary generates")
	}
	return nil
}
