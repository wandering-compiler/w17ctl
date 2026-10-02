package schema

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

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
	// FileDecisions / FileCustomSQL are decisions read from
	// w17/migrate-decisions/, already in the console's form; sent with Decide.
	FileDecisions []string
	FileCustomSQL map[string]string
}

// DecidePayload merges --decide flags with file decisions.
func (a SchemaPushArgs) DecidePayload() ([]string, map[string]string, error) {
	flags, custom, err := BuildDecidePayload(a.Decide)
	if err != nil {
		return nil, nil, err
	}
	flags = append(append([]string(nil), flags...), a.FileDecisions...)
	for k, v := range a.FileCustomSQL {
		if custom == nil {
			custom = map[string]string{}
		}
		custom[k] = v
	}
	return flags, custom, nil
}

// PlanPush asks the console what a push of this schema WOULD do — a dry run:
// findings still open after the decisions sent, the connections a push would
// migrate, and the decisions that match no finding. The console stores
// nothing and the lock is not touched.
func PlanPush(args SchemaPushArgs) (*w17registrypb.PushSchemaResponse, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ir, err := LoadIRBytes(ctx, args.Protos, args.Imports, args.Console)
	if err != nil {
		return nil, fmt.Errorf("load schema: %w", err)
	}
	addr, err := core.ResolveConsoleAddr(args.Console)
	if err != nil {
		return nil, err
	}
	cl, conn, err := core.DialProjectRegistry(addr)
	if err != nil {
		return nil, fmt.Errorf("connect %s: %w", addr, err)
	}
	defer func() { _ = conn.Close() }()
	decideFlags, customSQL, err := args.DecidePayload()
	if err != nil {
		return nil, err
	}
	resp, err := cl.PushSchema(ctx, &w17registrypb.PushSchemaRequest{
		ProjectId:       args.ProjectID,
		Ir:              ir,
		Mode:            w17registrypb.PushMode_PUSH_MODE_DRY_RUN,
		ForceInitial:    args.ForceInitial,
		Decide:          decideFlags,
		DecideCustomSql: customSQL,
	})
	// A console that predates DRY_RUN refuses the unknown mode — which is
	// why it is a mode and not a flag an old server would ignore and then
	// run a real push for. Say what that refusal means.
	if status.Code(err) == codes.InvalidArgument && strings.Contains(status.Convert(err).Message(), "unknown mode") {
		return nil, fmt.Errorf("this console cannot plan without storing (it predates `migrate check`) — nothing was stored; upgrade the console: %w", err)
	}
	return resp, err
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

	decideFlags, customSQL, err := args.DecidePayload()
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
	// Can this run pin what it is about to store? Asked first: a mint that
	// stores and then cannot pin leaves history the lock does not point at.
	if !args.NoLock && args.LockPath != "" {
		if err := CheckCanPinLock(args.Console, args.LockPath, args.ProjectID); err != nil {
			return fmt.Errorf("migrate generate: %w", err)
		}
	}
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
	// Pin EVERY connection to the console's current head — not only the
	// connections this run stored for. A lock that missed an earlier pin (a
	// CI mint that stored its migrations and then failed at EditLock) would
	// otherwise catch up only for the connections the NEXT change happened to
	// touch, and print "lock: pinned" over the rest (a consumer, 2026-10-02;
	// review of #153). The heads include what this run just stored.
	listResp, listErr := cl.ListMigrations(ctx, &w17registrypb.ListMigrationsRequest{ProjectId: args.ProjectID})
	if listErr != nil {
		return fmt.Errorf("list migrations to pin lock: %w", listErr)
	}
	// …followed by what this run stored, so a console answering with an
	// empty or partial list still pins at least what was just minted.
	// PinLockTargets keeps the last entry per connection.
	pinFrom := append(listResp.GetMigrations(), migrations...)
	if len(pinFrom) == 0 {
		fmt.Fprintln(core.Stdout, "lock: nothing to pin (no migration stored for this project yet)")
		return nil
	}
	changed, err := PinLockTargets(args.Console, args.LockPath, args.ProjectID, pinFrom)
	if err != nil {
		return fmt.Errorf("pin lock %s: %w", args.LockPath, err)
	}
	if changed {
		fmt.Fprintf(core.Stdout, "lock: pinned %s\n", args.LockPath)
	} else {
		fmt.Fprintf(core.Stdout, "lock: unchanged — %s already pins the console's latest migrations\n", args.LockPath)
	}
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
func PinLockTargets(console, path, projectID string, migrations []*w17registrypb.Migration) (bool, error) {
	// Group migrations by connection; pick the last (latest)
	// per connection. Console returns oldest → newest; the last
	// occurrence is the head.
	latest := map[string]*w17registrypb.Migration{}
	for _, m := range migrations {
		latest[m.GetConnection()] = m
	}
	if len(latest) == 0 {
		return false, nil
	}

	// Q58-console-1: serialise the read → EditLock → write below across
	// concurrent `w17ctl push` runs so the second write can't clobber the
	// first's pins. Held until this function returns.
	release, lockErr := lockfile.ForUpdate(path)
	if lockErr != nil {
		return false, lockErr
	}
	defer release()

	// Read the current lock as opaque bytes (missing → empty, the server
	// creates a fresh signed lock stamped with project_id).
	lockBytes, readErr := os.ReadFile(path)
	if readErr != nil && !os.IsNotExist(readErr) {
		return false, readErr
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
		return false, err
	}
	// Same bytes back = the lock already pinned every head. Said so rather
	// than "pinned": a consumer's rerun printed "lock: pinned" over a lock
	// that had not moved, and read it as done (2026-10-02).
	if bytes.Equal(newBytes, lockBytes) {
		return false, nil
	}
	return true, lockfile.WriteAtomic(path, newBytes, 0o644)
}

// CheckCanPinLock asks the console to sign a no-op pin of the lock at path —
// the same RPC, credential and lock the pin after a mint will use — and
// discards the result. Run BEFORE a mint: a mint that stores its migrations
// and then cannot pin leaves history the lock does not point at (a CI role
// without EditLock did exactly that, 2026-10-02). Refusing here stores
// nothing. Writes nothing.
func CheckCanPinLock(console, path, projectID string) error {
	lockBytes, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	_, err = core.EditLock(console, lockBytes, &codegenpb.LockEditIntent{
		Intent: &codegenpb.LockEditIntent_PinTargets{
			PinTargets: &codegenpb.PinTargetsIntent{ProjectId: projectID},
		},
	})
	if err != nil {
		return fmt.Errorf("this run could store migrations but not pin %s to them, so nothing was stored: %w", path, err)
	}
	return nil
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
