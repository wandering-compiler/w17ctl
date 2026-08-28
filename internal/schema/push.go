package schema

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/lockfile"
	"github.com/wandering-compiler/w17ctl/internal/storageclient"
	codegenpb "github.com/wandering-compiler/sdk/go/pb/w17compiler"
	w17registrypb "github.com/wandering-compiler/sdk/go/pb/w17registry"
)

// The compiled-IR push to the console (formerly `w17ctl schema
// create/update`) now lives under `w17ctl migrate generate` — see
// GenerateCmd in cmd/migrate. The shared push machinery
// (RunSchemaPush + PinLockTargets + the loader/findings helpers) stays
// here.

// SchemaPushMode discriminates create vs update at the shared driver
// (RunSchemaPush): both modes thread the same loader + dial path, only
// the RPC method differs. SchemaPushAuto resolves to create/update at
// runtime by probing whether the project already has a schema — the
// shape `migrate generate` uses so a caller never picks create vs
// update by hand.
type SchemaPushMode int

const (
	SchemaPushAuto SchemaPushMode = iota
	SchemaPushCreate
	SchemaPushUpdate
)

// pushModeToProto maps the local SchemaPushMode onto the public contract enum.
func pushModeToProto(m SchemaPushMode) w17registrypb.PushMode {
	switch m {
	case SchemaPushCreate:
		return w17registrypb.PushMode_PUSH_MODE_CREATE
	case SchemaPushUpdate:
		return w17registrypb.PushMode_PUSH_MODE_UPDATE
	default:
		return w17registrypb.PushMode_PUSH_MODE_AUTO
	}
}

type SchemaPushArgs struct {
	Protos       []string
	Imports      []string
	ProjectID    string
	Console      string
	LockPath     string
	NoLock       bool
	Mode         SchemaPushMode
	ForceInitial bool     // auto mode only: force create (refuse if a schema exists)
	Decide       []string // raw `--decide` flag strings; forwarded to the console to resolve NEEDS_CONFIRM findings
	Initiative   string   // explicit change-request id; empty = derive from the current git branch
}

// core.Stdout is the writer RunSchemaPush prints to. Production main() uses
// os.Stdout; tests capture into a buffer to assert content.

func RunSchemaPush(args SchemaPushArgs) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// The IR build resolves the proto dir + default connection server-side
	// (DescribeLock) — no client-side lock read here.
	ir, err := LoadIRBytes(ctx, args.Protos, args.Imports, args.Console)
	if err != nil {
		return fmt.Errorf("load schema: %w", err)
	}

	addr, err := core.ResolveConsoleAddr(args.Console)
	if err != nil {
		return err
	}
	cl, conn, err := core.DialProjectRegistry(addr)
	if err != nil {
		return fmt.Errorf("connect %s: %w", addr, err)
	}
	defer func() { _ = conn.Close() }()

	decideFlags, customSQL, err := BuildDecidePayload(args.Decide)
	if err != nil {
		return err
	}

	// Which change request these migrations belong to. Reported either way
	// (see CurrentInitiative.Why) — an unstamped push is one no freeze can
	// ever collapse, so it must not pass unnoticed.
	initiative := storageclient.ResolveCurrentInitiative(args.Console, args.ProjectID, args.Initiative)
	fmt.Fprintf(core.Stdout, "migrate generate: %s\n", initiative.Describe())

	// The create-vs-update decision (auto probe of whether a schema is already
	// stored, --initial force) is resolved SERVER-side now — the client just
	// ships the mode + the IR bytes.
	resp, err := cl.PushSchema(ctx, &w17registrypb.PushSchemaRequest{
		ProjectId:       args.ProjectID,
		Ir:              ir,
		Mode:            pushModeToProto(args.Mode),
		ForceInitial:    args.ForceInitial,
		Decide:          decideFlags,
		DecideCustomSql: customSQL,
		InitiativeId:    initiative.ID,
	})
	if err != nil {
		return err
	}
	migrations := resp.GetMigrations()

	if findings := resp.GetFindings(); len(findings) > 0 {
		return PrintFindingsErr(findings)
	}

	label := "initial schema"
	if resp.GetResolvedMode() == w17registrypb.PushMode_PUSH_MODE_UPDATE {
		label = "schema revision"
	}
	fmt.Fprintf(core.Stdout, "migrate generate: %d migration(s) stored for project %s [%s]\n",
		len(migrations), args.ProjectID, label)
	for _, m := range migrations {
		fmt.Fprintf(core.Stdout, "  - %s [%s]  sha256=%s\n",
			m.GetId(), m.GetConnection(), ShortHash(m.GetContentSha256()))
	}

	if args.NoLock {
		fmt.Fprintln(core.Stdout, "lock: skipped (--no-lock)")
		return nil
	}
	if args.LockPath == "" {
		// kong default fills this; defensive guard.
		fmt.Fprintln(core.Stdout, "lock: skipped (no path)")
		return nil
	}
	if err := PinLockTargets(args.Console, args.LockPath, args.ProjectID, migrations); err != nil {
		return fmt.Errorf("pin lock %s: %w", args.LockPath, err)
	}
	fmt.Fprintf(core.Stdout, "lock: pinned %s\n", args.LockPath)
	return nil
}

// PinLockTargets writes/updates the lock file at path so each
// connection that received migrations in the push gets
// target_migration_id pinned to the LATEST migration for that
// connection (the natural "deploy to here" target). Connections
// not present in the response are left untouched (multi-domain
// services may push subsets).
//
// On a fresh lock (file doesn't exist) the console stamps project_id; on an
// existing lock it rejects a project_id mismatch and upserts each affected
// connection in place — all server-side via the PinTargets EditLock intent
// (the client holds no lock types). The client reads/writes the lock file as
// OPAQUE signed bytes; the flock serialises concurrent pushes' read-edit-write.
func PinLockTargets(console, path, projectID string, migrations []*w17registrypb.Migration) error {
	// Group migrations by connection; pick the last (latest)
	// per connection. Console returns oldest → newest; the last
	// occurrence is the head.
	latest := map[string]*w17registrypb.Migration{}
	for _, m := range migrations {
		latest[m.GetConnection()] = m
	}
	if len(latest) == 0 {
		return nil
	}

	// Q58-console-1: serialise the read → EditLock → write below across
	// concurrent `w17ctl push` runs so the second write can't clobber the
	// first's pins. Held until this function returns.
	release, lockErr := lockfile.ForUpdate(path)
	if lockErr != nil {
		return lockErr
	}
	defer release()

	// Read the current lock as opaque bytes (missing → empty, the server
	// creates a fresh signed lock stamped with project_id).
	lockBytes, readErr := os.ReadFile(path)
	if readErr != nil && !os.IsNotExist(readErr) {
		return readErr
	}

	targets := make([]*codegenpb.PinTarget, 0, len(latest))
	for connName, mig := range latest {
		targets = append(targets, &codegenpb.PinTarget{
			Connection:          connName,
			TargetMigrationId:   mig.GetId(),
			TargetContentSha256: mig.GetContentSha256(),
		})
	}

	newBytes, err := core.EditLock(console, lockBytes, &codegenpb.LockEditIntent{
		Intent: &codegenpb.LockEditIntent_PinTargets{
			PinTargets: &codegenpb.PinTargetsIntent{ProjectId: projectID, Targets: targets},
		},
	})
	if err != nil {
		return err
	}
	return lockfile.WriteAtomic(path, newBytes, 0o644)
}

// PrintFindingsErr prints decision-needed findings + returns a
// non-nil error so kong's exit hook fires. Naive MVP behavior:
// console didn't store anything; user refines + retries.
func PrintFindingsErr(findings []*w17registrypb.Finding) error {
	fmt.Fprintln(core.Stdout, "schema push refused — decisions required:")
	for _, f := range findings {
		fmt.Fprintf(core.Stdout, "  %s.%s (#%d) — %s\n",
			f.GetTableName(), f.GetColumnName(), f.GetFieldNumber(), f.GetAxis())
		if f.GetRationale() != "" {
			fmt.Fprintf(core.Stdout, "    why: %s\n", f.GetRationale())
		}
	}
	return fmt.Errorf("%d unresolved finding(s); console did not store the push", len(findings))
}

// ShortHash returns the first 12 chars of a 64-char hex hash for
// log brevity. Empty input returns "(none)".
func ShortHash(h string) string {
	if h == "" {
		return "(none)"
	}
	if len(h) >= 12 {
		return h[:12]
	}
	return h
}
