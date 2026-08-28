package connection

import (
	"fmt"
	"os"
	"strings"

	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/lockfile"
	"github.com/wandering-compiler/w17ctl/internal/prompter"
	codegenpb "github.com/wandering-compiler/sdk/go/pb/w17compiler"
)

// Cmd is the parent of `w17ctl connection <leaf>`
// commands: `add` (register a connection name + default mark),
// `set-default` (switch the lock's default-connection mark),
// `list` (read-only inventory).
//
// NOTE: a connection's dialect + version are NOT managed here — they
// are declared in proto (`(w17.module).connection` in the domain's
// w17.proto), which is the code-aware source of truth. The lock only
// tracks the connection NAME (for migration-target pinning + default
// resolution) and which connection is the project default. Codegen
// derives the dialect from the `<domain>-<dialect>` name suffix
// (dialectFromConnectionName).
//
// Public-split Block 2 (§8.2): the console owns the lock. These commands
// treat `w17/lock.yaml` as opaque bytes — reads go through the console's
// DescribeLock RPC, mutations through EditLock (which re-maps + re-signs
// server-side). The client constructs no lockpb and signs nothing.
type Cmd struct {
	Add        AddCmd        `cmd:"" help:"Register a connection name in the project's lock (+ optional default mark). Dialect/version live in proto (the domain w17.proto's (w17.module).connection), not here."`
	SetDefault SetDefaultCmd `cmd:"set-default" help:"Mark a connection as the project's default. Existing default is unset in the same write."`
	List       ListCmd       `cmd:"" help:"List the connections declared in the lock, with the default-connection marker."`
}

// AddCmd implements `w17ctl connection add`. It registers a
// connection NAME in the lock's connections[] (re-signed by the console on
// save) and optionally marks it the project default. The wizard prompts for:
//
//  1. Connection name — `<domain>-<role>` shape (validated non-empty +
//     contains a dash; the role suffix conventionally names the
//     dialect, e.g. app-postgres).
//  2. (Conditional) — when adding the 2nd+ connection to a lock that
//     has no `default: true` mark yet, asks whether to mark this one
//     the default. `--default` skips the prompt with an explicit answer.
//
// Dialect + version are deliberately NOT asked here: they belong to
// proto (`(w17.module).connection`), and codegen infers the dialect
// from the name suffix. The console authoritatively refuses a duplicate
// name or a conflicting default.
type AddCmd struct {
	LockPath string `name:"lock" placeholder:"PATH" default:"w17/lock.yaml" help:"Path to the lock file."`
	Console  string `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console (owns the lock — re-maps + re-signs). Optional — falls back to the binary's compile-time default."`
	Default  bool   `name:"default" help:"Mark the new connection as the project's default. Refused if another connection already holds the mark; use 'w17ctl connection set-default' to transfer."`
	Name     string `name:"name" placeholder:"NAME" help:"Connection name (<domain>-<role>, e.g. app-postgres). Empty = ask. Supplying it makes the add non-interactive."`
}

func (c *AddCmd) Run() error {
	// Q58-console-1: serialise the read → EditLock → write below against
	// concurrent lock-mutating runs so the second write can't clobber the
	// first's edit. Held across the wizard too, so the user's decision is
	// made against state no concurrent run can swap underneath it.
	release, lockErr := lockfile.ForUpdate(c.LockPath)
	if lockErr != nil {
		return fmt.Errorf("connection add: lock for update: %w", lockErr)
	}
	defer release()

	lockBytes, err := os.ReadFile(c.LockPath)
	if err != nil {
		return fmt.Errorf("connection add: read lock %s: %w", c.LockPath, err)
	}

	// Read the current connection set (for the wizard's dup/default
	// resolution) from the console — the client never parses the lock.
	view, err := core.DescribeLock(c.Console, lockBytes)
	if err != nil {
		return fmt.Errorf("connection add: %w", err)
	}

	prompter := prompter.NewStdinPrompter()
	name, markDefault, err := RunAddWizard(prompter, view.GetConnections(), c.Default, c.Name)
	if err != nil {
		return err
	}

	newBytes, err := core.EditLock(c.Console, lockBytes, &codegenpb.LockEditIntent{
		Intent: &codegenpb.LockEditIntent_AddConnection{
			AddConnection: &codegenpb.AddConnectionIntent{Name: name, MarkDefault: markDefault},
		},
	})
	if err != nil {
		return fmt.Errorf("connection add: %w", err)
	}
	if err := lockfile.WriteAtomic(c.LockPath, newBytes, 0o644); err != nil {
		return fmt.Errorf("connection add: write lock: %w", err)
	}

	defHint := ""
	if markDefault {
		defHint = " (default)"
	}
	fmt.Fprintf(core.Stdout, "connection add: %s%s → %s\n", name, defHint, c.LockPath)
	return nil
}

// RunAddWizard prompts for the connection name (unless presetName is
// non-empty) + resolves the default mark, validating against the existing
// connection set (no duplicate names; no conflicting default marks).
// `defaultFlag` carries the `--default` CLI flag — when true, the wizard
// skips the interactive default-mark prompt with that answer. Pulled out of
// Run so tests drive it without disk I/O or a console.
//
// The console re-validates authoritatively on EditLock; the wizard's checks
// are just for a friendly local message + the interactive default prompt.
func RunAddWizard(p prompter.Prompter, existing []*codegenpb.LockConnection, defaultFlag bool, presetName string) (name string, markDefault bool, err error) {
	fmt.Fprintln(core.Stdout, "Adding a new connection to the project...")
	fmt.Fprintln(core.Stdout)

	name = presetName
	if name == "" {
		name, err = p.Text("Connection name (<domain>-<role>, e.g. app-postgres)", "")
		if err != nil {
			return "", false, err
		}
	}
	if name == "" {
		return "", false, fmt.Errorf("connection name is required")
	}
	if !strings.Contains(name, "-") {
		return "", false, fmt.Errorf("connection name %q does not follow the <domain>-<role> convention (e.g. \"app-postgres\")", name)
	}
	for _, c := range existing {
		if c.GetName() == name {
			return "", false, fmt.Errorf("connection %q already exists in the lock", name)
		}
	}

	markDefault, err = resolveDefaultMark(p, existing, name, defaultFlag)
	if err != nil {
		return "", false, err
	}
	return name, markDefault, nil
}

// resolveDefaultMark decides whether the new connection gets
// `Default: true`. Branches:
//
//   - --default flag set + no existing mark → mark.
//   - --default flag set + existing mark → refuse with a pointer at
//     `set-default`.
//   - flag unset + adding to ≥1-connection set + no existing mark →
//     interactive prompt (defaults to "no").
//   - otherwise → no mark.
func resolveDefaultMark(p prompter.Prompter, existing []*codegenpb.LockConnection, newName string, defaultFlag bool) (bool, error) {
	var markedName string
	for _, c := range existing {
		if c.GetDefault() {
			markedName = c.GetName()
			break
		}
	}
	switch {
	case defaultFlag:
		if markedName != "" && markedName != newName {
			return false, fmt.Errorf("connection add: --default refused — %q is already marked default; use `w17ctl connection set-default %s` after this add to transfer the mark", markedName, newName)
		}
		return true, nil
	case markedName != "":
		return false, nil
	case len(existing) == 0:
		return false, nil
	}
	choice, err := p.Select("Mark this connection as the project's default?", []string{"no", "yes"}, "no")
	if err != nil {
		return false, err
	}
	return choice == "yes", nil
}

// SetDefaultCmd implements `w17ctl connection set-default <name>`. Ships a
// SetDefaultConnection intent to the console, which validates `<name>`
// exists, transfers the mark, re-signs + returns the bytes the client writes.
type SetDefaultCmd struct {
	Name     string `arg:"" name:"name" help:"Connection name to mark as the default."`
	LockPath string `name:"lock" placeholder:"PATH" default:"w17/lock.yaml" help:"Path to the lock file."`
	Console  string `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console (owns the lock). Optional — falls back to the binary's compile-time default."`
}

func (c *SetDefaultCmd) Run() error {
	if err := core.EditLockOnDisk("connection set-default", c.Console, c.LockPath, &codegenpb.LockEditIntent{
		Intent: &codegenpb.LockEditIntent_SetDefaultConnection{
			SetDefaultConnection: &codegenpb.SetDefaultConnectionIntent{Name: c.Name},
		},
	}); err != nil {
		return err
	}
	fmt.Fprintf(core.Stdout, "connection set-default: %s (lock re-signed)\n", c.Name)
	return nil
}

// ListCmd implements `w17ctl connection list`. One connection per line; the
// resolved default carries a `(default)` marker. The connection set + the
// resolved default come from the console's DescribeLock (the client doesn't
// parse the lock).
type ListCmd struct {
	LockPath string `name:"lock" placeholder:"PATH" default:"w17/lock.yaml" help:"Path to the lock file."`
	Console  string `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console (owns the lock). Optional — falls back to the binary's compile-time default."`
}

func (c *ListCmd) Run() error {
	view, err := core.DescribeLockAt("connection list", c.Console, c.LockPath)
	if err != nil {
		return err
	}
	conns := view.GetConnections()
	if len(conns) == 0 {
		fmt.Fprintln(core.Stdout, "no connections declared in the lock")
		return nil
	}
	effective := view.GetDefaultConnection()
	for _, conn := range conns {
		marker := ""
		if conn.GetName() == effective {
			marker = " (default)"
		}
		fmt.Fprintf(core.Stdout, "%s%s\n", conn.GetName(), marker)
	}
	return nil
}
