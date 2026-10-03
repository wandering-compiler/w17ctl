package plan

import (
	"context"
	"fmt"
	"os"
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
	p, err := planObserved(base, current, baselines, observed, nil)
	if err != nil {
		return nil, err
	}
	return p.plan, nil
}

// planned is one Plan RPC's answer, decoded.
type planned struct {
	plan  *applyplanpb.DevApplyPlan
	lossy []*codegenpb.LossyChange
	// checkpoint is the record the console composed for this run (opaque);
	// nil when the console predates the field.
	checkpoint []byte
}

// planObserved asks for the plan. `targets` nil sends none — the caller does
// not say which connections it applies to (the reconcile's fresh build, or
// `schema render`), and the console plans as it did before the field.
func planObserved(base, current []byte, baselines []*codegenpb.PlanBaseline, observed []*codegenpb.ObservedStore, targets []string) (*planned, error) {
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
	req := &codegenpb.PlanIRRequest{Base: base, Head: current, Baselines: baselines, Observed: observed}
	if targets != nil {
		req.Targets = &codegenpb.PlanTargets{Connections: targets}
	}
	resp, err := cl.Plan(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("migration plan: %w", err)
	}
	var plan applyplanpb.DevApplyPlan
	if err := proto.Unmarshal(resp.GetPlan(), &plan); err != nil {
		return nil, fmt.Errorf("migration plan: decode: %w", err)
	}
	out := &planned{plan: &plan, lossy: resp.GetLossy()}
	if ck := resp.GetCheckpoint(); ck != nil {
		out.checkpoint = ck.GetIr()
		if out.checkpoint == nil {
			out.checkpoint = []byte{} // present but empty IR — still the console's answer
		}
	}
	return out, nil
}

// SyncResult is what a dev sync did, and what the caller must record for it.
type SyncResult struct {
	// Plan is what was applied.
	Plan *applyplanpb.DevApplyPlan
	// Checkpoint is the record to store after this sync, exactly as the
	// console composed it — opaque bytes, never read here. It is the head for
	// what was applied and the previous record for every connection this run
	// left out, so a skipped change stays pending. nil: the console predates
	// the field (see Withheld).
	Checkpoint []byte
	// Withheld names the connections whose planned changes this run did NOT
	// apply while talking to such an older console. It cannot keep them
	// pending, so the caller must NOT advance the checkpoint: advancing would
	// record them as applied, and for a store this client cannot read the
	// record is the only base it is ever planned from.
	Withheld []string
}

// DevPlanAndApply is the dev DB lifecycle's diff-apply orchestration,
// thin-client edition: log destructive changes (compat over the API), plan
// base→current (planner over the API), apply to the LOCAL stores
// (migrate.DevApply). base/current are opaque compiled-IR bytes (nil base =
// initial state). Returns the plan so the caller advances the checkpoint only
// on a clean apply. logf receives one line per destructive finding; nil = a
// no-op.
func DevPlanAndApply(ctx context.Context, base, current []byte, applierFor migrate.ApplierFor, conns []string, logf func(string, ...any)) (*applyplanpb.DevApplyPlan, error) {
	return DevPlanAndApplyLossy(ctx, base, current, applierFor, conns, logf, LossyApply, nil)
}

// DevPlanAndApplyLossy is DevPlanAndApply plus the answer to "what if this
// destroys something".
//
// lossyMode is one of LossyRefuse / LossyApply / LossySnapshot. snapshot is
// called with the connections that would lose data, before anything is
// applied, and only in snapshot mode — supplied by the command layer because
// taking one is a command's job.
func DevPlanAndApplyLossy(ctx context.Context, base, current []byte, applierFor migrate.ApplierFor, conns []string, logf func(string, ...any), lossyMode string, snapshot func(conns []string) error) (*applyplanpb.DevApplyPlan, error) {
	var snap func([]string, []byte) error
	if snapshot != nil {
		snap = func(c []string, _ []byte) error { return snapshot(c) }
	}
	res, err := DevSync(ctx, base, current, applierFor, conns, logf, lossyMode, snap)
	if err != nil {
		return nil, err
	}
	return res.Plan, nil
}

// Record is what the caller stores after this sync applied: the console's
// checkpoint when it sent one; nothing (nil — do not advance) when an older
// console's plan held a change this run withheld; the head otherwise, as
// every sync recorded before the console could say.
//
// The snapshot a destructive sync takes is handed the same bytes, so the
// savepoint pins the hash the checkpoint will actually carry as "the way
// back from" — pinning the head's hash while the record differed would make
// `db snapshot activate` refuse the very return trip it is for.
func (r *SyncResult) Record(current []byte) []byte {
	switch {
	case r.Checkpoint != nil:
		return r.Checkpoint
	case len(r.Withheld) > 0:
		return nil
	}
	return current
}

// DevSync is DevPlanAndApplyLossy plus what the caller must RECORD for it —
// the one entry point a caller that advances the checkpoint uses.
//
// `conns` is the set of connections this run applies to. It is sent as the
// request's `targets`, and the console leaves every other connection out of
// the plan and returns the checkpoint to record (the previous state for what
// it left out). nil `conns` sends no targets: the console plans as it did
// before the field, and the caller records nothing from this run itself.
func DevSync(ctx context.Context, base, current []byte, applierFor migrate.ApplierFor, conns []string, logf func(string, ...any), lossyMode string, snapshot func(conns []string, record []byte) error) (*SyncResult, error) {
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

	// Read what the databases actually hold — that reading IS the base the
	// server plans against.
	observed, unreadable, err := observeStores(ctx, conns, applierFor)
	if err != nil {
		return nil, err
	}
	p, err := planObserved(base, current, nil, observed, conns)
	if err != nil {
		return nil, fmt.Errorf("devapply: plan: %w", err)
	}
	warnUnreadable(unreadable, p.checkpoint != nil)

	res := &SyncResult{Plan: p.plan, Checkpoint: p.checkpoint}
	lossy := p.lossy
	if p.checkpoint == nil {
		// An older console: it did not read `targets`, so its plan may hold a
		// bucket for a connection this run cannot apply.
		res.Plan, lossy, res.Withheld = untargetedLeftOut(p.plan, lossy, conns, logf)
	}

	// The destructive half of the plan, and the decision about it.
	//
	// Before this, a drop simply happened: the sync makes the database match
	// the protos, and a column the protos no longer describe is a column that
	// goes. That is right for the developer who just renamed a field and
	// wrong for the one with an hour of test data in that table, and nothing
	// asked which of the two was running the command.
	if len(lossy) > 0 {
		for _, l := range lossy {
			logf("⚠ %s", describeLoss(l))
		}
		switch lossyMode {
		case LossyRefuse:
			return nil, fmt.Errorf("%s", LossyRefusal(lossy))
		case LossySnapshot:
			if snapshot == nil {
				return nil, fmt.Errorf("devapply: --lossy=snapshot, but this caller cannot take one")
			}
			if err := snapshot(LossyConnections(lossy), res.Record(current)); err != nil {
				return nil, fmt.Errorf("devapply: snapshot before a destructive sync: %w", err)
			}
		}
	}

	if err := migrate.DevApply(ctx, res.Plan, applierFor); err != nil {
		return nil, wrapDesync(base, err)
	}
	return res, nil
}

// warnUnreadable says, per store this client could not read, what the
// console did with it — which depends on the console. A current one plans it
// from the checkpoint (it said so by returning one); an older one leaves it
// alone entirely, and saying "planned from the checkpoint" to that user would
// be the F13 lie again (a warning describing a plan that does not exist).
func warnUnreadable(conns []string, consoleKnowsTargets bool) {
	for _, conn := range conns {
		if consoleKnowsTargets {
			warnf("⚠ connection %q holds a schema this client cannot read (its dialect has no "+
				"live-schema observer), so it is planned from the console's checkpoint — the record "+
				"of what the last sync applied, none on a first sync — not from the database; if "+
				"the store was emptied or changed outside w17 since, that plan will not fit it and "+
				"its apply fails\n", conn)
			continue
		}
		warnf("⚠ connection %q holds a schema this client cannot read (its dialect has no "+
			"live-schema observer), and this console predates per-store planning: it is NOT "+
			"planned at all, so no schema change reaches it until the console is upgraded\n", conn)
	}
}

// untargetedLeftOut is the fallback for an OLDER console only — one that did
// not read `targets` and returned no checkpoint. It removes from the plan,
// and from what it would destroy, every connection this run has no target
// for, and returns those it removed a non-empty bucket for.
//
// Applying such a bucket would fail the whole sync on "no --target
// configured"; counting its losses would refuse a sync over a store it does
// not touch. And because that console cannot keep the change pending, the
// caller must not record it as applied (SyncResult.Withheld) — said here,
// loudly, per connection.
//
// nil `conns` is the caller that resolves nothing (the reconcile's fresh
// build) and keeps every bucket; the unnamed default bucket is never left
// out, because no target list can name it.
func untargetedLeftOut(plan *applyplanpb.DevApplyPlan, lossy []*codegenpb.LossyChange, conns []string, logf func(string, ...any)) (*applyplanpb.DevApplyPlan, []*codegenpb.LossyChange, []string) {
	if conns == nil {
		return plan, lossy, nil
	}
	targeted := make(map[string]bool, len(conns))
	for _, c := range conns {
		targeted[c] = true
	}
	out := &applyplanpb.DevApplyPlan{}
	proto.Merge(out, plan)
	out.Migrations = nil
	var withheld []string
	for _, m := range plan.GetMigrations() {
		if c := m.GetConnection(); c != "" && !targeted[c] {
			logf("⚠ connection %q has schema changes planned but NOT applied — this run has no target "+
				"for it, and this console cannot keep them pending, so the checkpoint is NOT advanced: "+
				"the next sync plans them again", c)
			withheld = append(withheld, c)
			continue
		}
		out.Migrations = append(out.Migrations, m)
	}
	var kept []*codegenpb.LossyChange
	for _, l := range lossy {
		if c := l.GetConnection(); c != "" && !targeted[c] {
			continue
		}
		kept = append(kept, l)
	}
	return out, kept, withheld
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

// observeStores reads each connection's live schema — the BASE the server
// plans from.
//
// Only a SCHEMALESS store (one whose applier says so — KV / queue / object)
// is skipped in silence: it has nothing to report, and the server leaves a
// connection it heard nothing about alone.
//
// Everything else that goes unreported is an ERROR, and the difference
// matters now in a way it did not before. This reading used to be evidence
// for a drift check, where absent evidence refused nothing. It is now the
// BASE: a store that goes unreported is planned as "already at the desired
// schema", so it would be quietly skipped and the build would report a
// converged store it never touched. Three ways that used to happen, none of
// them silent now — two refuse, one warns:
//
//   - the applier cannot be CONSTRUCTED (pgx.Connect is eager, so an
//     unreachable database errors right there — the `continue` this
//     replaced reported an unreachable store as converged);
//   - the dialect is schema-ful but has no observer (only Postgres
//     implements Observe) — a WARNING rather than a refusal: the console
//     plans a store it heard nothing about from the checkpoint this request
//     carries (the record of the last sync; none → a full CREATE), and
//     `examples/pg-native` ships exactly that pairing. Refusing it made every
//     mixed-dialect project unbuildable (pass #48 F13); planning it as
//     "already converged", which is what the console did until pass #49 M1,
//     froze it while this warning described a plan that did not exist;
//   - the observation itself fails.
//
// warnf writes an operator-facing warning. A variable so a test can prove the
// message is actually EMITTED: the property F13 cares about is loudness, and a
// warning nobody can observe in a test is the same silence under a new name.
var warnf = func(format string, a ...any) { fmt.Fprintf(os.Stderr, format, a...) }

func observeStores(ctx context.Context, conns []string, applierFor migrate.ApplierFor) (out []*codegenpb.ObservedStore, unreadable []string, err error) {
	for _, conn := range conns {
		ap, err := applierFor(conn)
		if err != nil {
			return nil, nil, fmt.Errorf("devapply: connecting to connection %q: %w\n\n"+
				"  why: the sync is planned against what each database HOLDS, so an unreachable\n"+
				"       one cannot be planned for at all — skipped, it would be reported as\n"+
				"       already converged without ever being touched", conn, err)
		}
		obs, ok := ap.(migrate.ObserveCapable)
		if !ok {
			_, schemaless := ap.(migrate.Schemaless)
			_ = ap.Close()
			if schemaless {
				continue
			}
			// F13, corrected after `make ci`: this was a hard refusal, and it
			// made every mixed-dialect project unbuildable — `examples/pg-native`
			// ships a MySQL store beside its Postgres one and stopped dead.
			//
			// The finding's complaint is the SILENCE, not the skip: a store
			// nobody can read is planned against no observation at all, so the
			// plan for it is made blind. Saying so leaves the behaviour that
			// works and removes the part that was a defect. Refusing instead
			// is the "right about the defect, wrong about the remedy" shape
			// this dimension recorded in round 1 (pass #40 XF4, where the
			// proposed reject broke a shipped feature).
			//
			// What the console does with it depends on the console (pass #49
			// M1), so the warning is printed once the plan says which one this
			// is — see warnUnreadable.
			unreadable = append(unreadable, conn)
			continue
		}
		live, oerr := obs.Observe(ctx)
		_ = ap.Close()
		if oerr != nil {
			return nil, nil, fmt.Errorf("devapply: reading the schema of connection %q: %w\n\n"+
				"  why: the sync is planned against what this database HOLDS, so an unreadable\n"+
				"       one cannot be planned for at all — skipped, it would be reported as\n"+
				"       already converged", conn, oerr)
		}
		store := &codegenpb.ObservedStore{Connection: conn}
		for _, t := range live.Tables {
			ot := &codegenpb.ObservedTable{
				Schema:     t.Schema,
				Name:       t.Name,
				PrimaryKey: t.PrimaryKey,
				Checks:     t.Checks,
				CheckDefs:  t.CheckDefs,
			}
			// The live MEMBER SETS ride the wire next to the check names.
			// Without them the server-side member comparison is inert for
			// every RPC consumer — the client read the sets and threw them
			// away, which is how the 2026-09-20 choices-drift fix stayed
			// live only for the console's own in-process reader.
			if len(t.CheckMembers) > 0 {
				ot.CheckMembers = map[string]*codegenpb.CheckMemberSet{}
				for name, members := range t.CheckMembers {
					ot.CheckMembers[name] = &codegenpb.CheckMemberSet{Members: members}
				}
			}
			for _, c := range t.Columns {
				ot.Columns = append(ot.Columns, &codegenpb.ObservedColumn{
					Name: c.Name, Type: c.DataType, Nullable: c.Nullable, DefaultExpr: c.Default,
					// The observation already reads attgenerated/attidentity;
					// dropping them here would leave the server unable to tell
					// a generation expression from a default, because both
					// arrive through the same slot (F35).
					Generated: c.Generated, Identity: c.Identity,
				})
			}
			for _, idx := range t.Indexes {
				ot.Indexes = append(ot.Indexes, &codegenpb.ObservedIndex{
					Name: idx.Name, Unique: idx.Unique, Definition: idx.Definition,
				})
			}
			for _, fk := range t.ForeignKeys {
				ot.ForeignKeys = append(ot.ForeignKeys, &codegenpb.ObservedForeignKey{
					Name: fk.Name, Columns: fk.Columns,
					TargetTable: fk.TargetTable, TargetColumn: fk.TargetColumn,
					OnDelete: fk.OnDelete,
				})
			}
			store.Tables = append(store.Tables, ot)
		}
		out = append(out, store)
	}
	return out, unreadable, nil
}
