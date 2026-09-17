package plan

import (
	"context"
	"fmt"
	"strings"

	"google.golang.org/protobuf/proto"

	compat "github.com/wandering-compiler/w17ctl/internal/compat"
	"github.com/wandering-compiler/w17ctl/internal/core"
	applyplanpb "github.com/wandering-compiler/sdk/go/pb/applyplan"
	codegenpb "github.com/wandering-compiler/sdk/go/pb/w17compiler"

	"github.com/wandering-compiler/sdk/go/tooling/migrate"
)

// Migration planning is a COMPILER concern (the engine.Plan diff engine), so
// w17ctl (the thin client) gets the plan from the console's
// CodegenService.Plan RPC and only APPLIES it to the local stores
// (migrate.DevApply) — the dev DB lifecycle's "plan on the server, apply
// locally" split. base/current cross the wire as OPAQUE compiled-IR bytes (a
// checkpoint's ir_schema or a fresh CompileIR output); the returned plan is
// opaque bytes too, decoded only to feed the local apply driver.

// PlanMigration asks the console's CodegenService.Plan for the dev diff
// base→current (prev/curr IR, as opaque bytes). base may be nil (brand-new
// initiative → full create). Returns the rendered per-connection migration
// plan (empty-diff buckets omitted by the engine).
// baselines, when non-empty, pin the applied-ledger row each connection's
// freshly built database must record. Only `schema render` passes them: it
// produces the artefact a DEPLOYED database is built from, and that database
// is the one a deploy gate later inspects. The dev diff-apply below passes
// none — it reconciles a developer's local store, which nothing gates.
func PlanMigration(base, current []byte, baselines []*codegenpb.PlanBaseline) (*applyplanpb.DevApplyPlan, error) {
	return PlanMigrationObserved(base, current, baselines, nil)
}

// PlanMigrationObserved is PlanMigration plus what the caller's databases
// actually hold right now.
//
// `base` is what the console BELIEVES they hold — the checkpoint it recorded
// after the last apply — and until something compares the two, a diff can be
// planned against a state that does not exist and applied to one that does.
// The comparison happens server-side, because the client carries IR as opaque
// bytes by design and cannot read what `base` says.
//
// `observed` nil or empty means "I could not look", which is not "nothing is
// there" and refuses nothing.
func PlanMigrationObserved(base, current []byte, baselines []*codegenpb.PlanBaseline, observed []*codegenpb.ObservedStore) (*applyplanpb.DevApplyPlan, error) {
	addr, err := core.ResolveConsoleAddr("")
	if err != nil {
		return nil, err
	}
	cl, conn, err := core.DialCodegen(addr)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()

	ctx, cancel := core.ClientCtx()
	defer cancel()
	resp, err := cl.Plan(ctx, &codegenpb.PlanIRRequest{Base: base, Head: current, Baselines: baselines, Observed: observed})
	if err != nil {
		return nil, fmt.Errorf("migration plan: %w", err)
	}
	var plan applyplanpb.DevApplyPlan
	if err := proto.Unmarshal(resp.GetPlan(), &plan); err != nil {
		return nil, fmt.Errorf("migration plan: decode: %w", err)
	}
	return &plan, nil
}

// DevPlanAndApply is the dev DB lifecycle's diff-apply orchestration,
// thin-client edition: log destructive changes (compat over the API), plan
// base→current (planner over the API), apply to the LOCAL stores
// (migrate.DevApply). base/current are opaque compiled-IR bytes (nil base =
// initial state). Returns the plan so the caller advances the checkpoint only
// on a clean apply. logf receives one line per destructive finding; nil = a
// no-op.
func DevPlanAndApply(ctx context.Context, base, current []byte, applierFor migrate.ApplierFor, conns []string, logf func(string, ...any)) (*applyplanpb.DevApplyPlan, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}

	// Loudly log destructive DB changes (WARN + BREAKING) — same set the
	// review prod-mode gate classifies, fetched from Classify. Mode is
	// irrelevant to the findings (it only drives the gate), so "dev".
	resp, err := compat.ClassifyCompat(base, current, "dev")
	if err != nil {
		return nil, err
	}
	for _, f := range resp.GetFindings() {
		if f.GetDomain() == "DB" && (f.GetSeverity() == "WARN" || f.GetSeverity() == "BREAKING") {
			logf("⚠ destructive dev change [%s] %s: %s", f.GetSeverity(), f.GetSymbol(), f.GetMessage())
		}
	}

	// Read what the databases actually hold before asking for a plan against
	// what the console thinks they hold. Best-effort by design: a store with
	// no introspection reports nothing, and nothing refuses nothing.
	plan, err := PlanMigrationObserved(base, current, nil, observeStores(ctx, conns, applierFor))
	if err != nil {
		return nil, fmt.Errorf("devapply: plan: %w", err)
	}
	if err := migrate.DevApply(ctx, plan, applierFor); err != nil {
		return nil, wrapDesync(base, err)
	}
	return plan, nil
}

// wrapDesync turns the checkpoint-desync trap into an actionable hint. When
// base is empty the planner produced a full CREATE (brand-new initiative), so
// an "already exists" apply failure means the store already has the objects —
// the console's checkpoint and the live DB have drifted out of sync (a
// wiped/reset console, a store built out-of-band, or an interrupted earlier
// build that applied but never advanced the checkpoint). A bare "relation
// already exists" is inscrutable in that case; every other error passes
// through unchanged.
func wrapDesync(base []byte, err error) error {
	if len(base) != 0 || !isAlreadyExists(err) {
		return err
	}
	return fmt.Errorf("%w\n\n"+
		"hint: the console has no checkpoint for this initiative, so a full schema CREATE was planned — "+
		"but the store already contains these objects. The console and the live DB are out of sync. Either:\n"+
		"  • wipe + rebuild the store:  w17 stack build --reconcile   (or: docker compose down -v && w17 stack up --build)\n"+
		"  • or, if the live schema already matches the current proto, reset just this store so the full CREATE lands cleanly.", err)
}

// isAlreadyExists reports whether a dev-apply error is a relation/table
// "already exists" collision (postgres 42P07 / mysql / sqlite phrasings all
// contain the substring), the signature of the checkpoint-desync trap.
func isAlreadyExists(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "already exists")
}

// observeStores reads each connection's live schema, for the server to compare
// against the checkpoint it is about to plan from.
//
// Every failure here is SILENT and yields nothing for that store, which is the
// only safe direction: an unreadable database must not become "an empty
// database", because the server would then refuse a caller whose only sin was
// a store it cannot introspect. Absent observation is absent evidence.
func observeStores(ctx context.Context, conns []string, applierFor migrate.ApplierFor) []*codegenpb.ObservedStore {
	var out []*codegenpb.ObservedStore
	for _, conn := range conns {
		ap, err := applierFor(conn)
		if err != nil {
			continue
		}
		obs, ok := ap.(migrate.ObserveCapable)
		if !ok {
			_ = ap.Close()
			continue
		}
		live, oerr := obs.Observe(ctx)
		_ = ap.Close()
		if oerr != nil {
			continue
		}
		store := &codegenpb.ObservedStore{Connection: conn}
		for _, t := range live.Tables {
			ot := &codegenpb.ObservedTable{Schema: t.Schema, Name: t.Name}
			for _, c := range t.Columns {
				ot.Columns = append(ot.Columns, &codegenpb.ObservedColumn{
					Name: c.Name, Type: c.DataType, Nullable: c.Nullable,
				})
			}
			store.Tables = append(store.Tables, ot)
		}
		out = append(out, store)
	}
	return out
}
