package review

import (
	"fmt"
	"os"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	compat "github.com/wandering-compiler/w17ctl/internal/compat"
	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/storageclient"

	initiativespb "github.com/wandering-compiler/sdk/go/pb/consoleapi/initiatives"
	reviewpb "github.com/wandering-compiler/sdk/go/pb/consoleapi/review"
)

// displaySnapshot renders a snapshot id for an operator-facing message,
// naming the never-set case rather than printing an empty string.
func displaySnapshot(id string) string {
	if id == "" {
		return "(none)"
	}
	return id
}

// Cmd is `w17ctl review` — the PR-analog flow over compiled
// snapshots. Pure client orchestration over the generated storage
// services + the console's ClassifyIR RPC (semantic diff over the API,
// thin-client refactor Step 3d); no server facade. v1 ships
// self-approval. The prod-mode gate (merge into a prod environment) is
// the point where the compat engine becomes a hard ENFORCEMENT.
type Cmd struct {
	Console string `name:"console" placeholder:"HOST:PORT" env:"CONSOLE_STORAGE_ADDR" help:"Console storage endpoint. Defaults to the logged-in console (w17ctl login), else the compiled-in default."`

	Open    OpenCmd    `cmd:"" help:"Open a review on the current branch's initiative (diff vs trunk)."`
	Show    ShowCmd    `cmd:"" help:"Show a review: its semantic diff (compat) + approvals + status."`
	Approve ApproveCmd `cmd:"" help:"Approve a review (v1 self-approval)."`
	Merge   MergeCmd   `cmd:"" help:"Review-merge: advance trunk to the review head, gated by prod-mode compat; optionally deploy to an environment."`
}

// --- open ---------------------------------------------------------

type OpenCmd struct {
	Project string `name:"project" placeholder:"ID" help:"Project id. Empty = read from the current project's lock."`
	Name    string `name:"name" placeholder:"NAME" help:"Initiative name. Empty = current git branch."`
	By      string `name:"by" placeholder:"ACTOR" help:"Author actor. Empty = local OS user."`
}

func (c *OpenCmd) Run(parent *Cmd) error {
	project, err := storageclient.ResolveProjectID(c.Project)
	if err != nil {
		return err
	}
	name, _, err := storageclient.ResolveInitiativeTarget(c.Name)
	if err != nil {
		return err
	}
	author := c.By
	if author == "" {
		author = storageclient.SelfActor()
	}
	sc, err := storageclient.DialStorageFn(parent.Console)
	if err != nil {
		return err
	}
	defer sc.Close()

	initiative, err := sc.FindInitiative(project, name)
	if err != nil {
		return err
	}
	if initiative == nil || initiative.GetHeadSnapshotId() == "" {
		return fmt.Errorf("initiative %q has no snapshot yet — `w17ctl initiative push` first", name)
	}
	base, ok, err := sc.TrunkHead(project)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("trunk has no baseline snapshot yet — push to trunk (main/master) first")
	}

	ctx, cancel := core.ClientCtx()
	defer cancel()
	resp, err := sc.RM.Create(ctx, &reviewpb.CreateReviewReq{
		ProjectId: project, InitiativeId: initiative.GetId(),
		BaseSnapshotId: base, HeadSnapshotId: initiative.GetHeadSnapshotId(), Author: author,
	})
	if err != nil {
		return fmt.Errorf("open review: %w", err)
	}
	fmt.Fprintf(core.Stdout, "opened review %s on %q (by %s)\n", resp.GetId(), name, author)
	return nil
}

// --- show ---------------------------------------------------------

type ShowCmd struct {
	Project string `name:"project" placeholder:"ID" help:"Project id. Empty = read from the current project's lock."`
	ID      string `name:"id" placeholder:"REVIEW" help:"Review id. Empty = latest review on the current branch."`
	Mode    string `name:"mode" placeholder:"dev|prod" default:"dev" help:"Mode for the gate readout."`
}

func (c *ShowCmd) Run(parent *Cmd) error {
	project, err := storageclient.ResolveProjectID(c.Project)
	if err != nil {
		return err
	}
	sc, err := storageclient.DialStorageFn(parent.Console)
	if err != nil {
		return err
	}
	defer sc.Close()

	review, err := sc.ResolveReview(project, c.ID)
	if err != nil {
		return fmt.Errorf("show: %w", err)
	}
	fmt.Fprintf(core.Stdout, "review %s\tstatus=%s\tauthor=%s\n", review.GetId(), review.GetStatus().String(), review.GetAuthor())

	// Approvals.
	actx, acancel := core.ClientCtx()
	apps, err := sc.AQ.ListByReview(actx, &reviewpb.ListApprovalsReq{ReviewId: review.GetId()})
	acancel()
	if err != nil {
		return err
	}
	for _, a := range apps.GetRows() {
		fmt.Fprintf(core.Stdout, "  approval: %s by %s\n", a.GetDecision().String(), a.GetActor())
	}

	// Semantic diff (compat), rendered client-side.
	base, err := sc.LoadIRBytes(review.GetBaseSnapshotId())
	if err != nil {
		return err
	}
	head, err := sc.LoadIRBytes(review.GetHeadSnapshotId())
	if err != nil {
		return err
	}
	fmt.Fprintln(core.Stdout, "  --- semantic diff (base → head) ---")
	resp, err := compat.ClassifyCompat(base, head, c.Mode)
	if err != nil {
		return err
	}
	compat.RenderReportPB(resp, c.Mode) // display only — show never errors on a block
	return nil
}

// --- approve ------------------------------------------------------

type ApproveCmd struct {
	Project string `name:"project" placeholder:"ID" help:"Project id. Empty = read from the current project's lock."`
	ID      string `name:"id" placeholder:"REVIEW" help:"Review id. Empty = latest review on the current branch."`
	By      string `name:"by" placeholder:"ACTOR" help:"Approver actor. Empty = local OS user (v1 self-approval)."`
}

func (c *ApproveCmd) Run(parent *Cmd) error {
	project, err := storageclient.ResolveProjectID(c.Project)
	if err != nil {
		return err
	}
	actor := c.By
	if actor == "" {
		actor = storageclient.SelfActor()
	}
	sc, err := storageclient.DialStorageFn(parent.Console)
	if err != nil {
		return err
	}
	defer sc.Close()

	review, err := sc.ResolveReview(project, c.ID)
	if err != nil {
		return fmt.Errorf("approve: %w", err)
	}
	actx, acancel := core.ClientCtx()
	_, err = sc.AM.Insert(actx, &reviewpb.InsertApprovalReq{
		ReviewId: review.GetId(), Actor: actor, Decision: reviewpb.Approval_APPROVE,
	})
	acancel()
	if err != nil {
		// `approvals` carries a (org_id, review_id, actor) unique index since
		// T3-7 D-F4 — one decision row per actor, so a future quorum count
		// cannot be satisfied by one actor firing N invocations. A retried CI
		// step is the ordinary way to hit it, and a retry was always a no-op
		// here, so it stays one: the database refuses the duplicate, we say so
		// and go on to (re-)stamp the decision.
		if !storageclient.IsUniqueViolation(err) {
			return fmt.Errorf("record approval: %w", err)
		}
		fmt.Fprintf(core.Stdout, "%s had already approved review %s (no second approval recorded)\n", actor, review.GetId())
	}
	dctx, dcancel := core.ClientCtx()
	_, err = sc.RM.SetDecision(dctx, &reviewpb.SetReviewDecisionReq{
		Id: review.GetId(), Status: reviewpb.Review_APPROVED, DecidedBy: actor,
		// The status this command READ. Without it an approve that lost a race
		// to a merge overwrote MERGED with APPROVED and told both callers they
		// had succeeded (D-F3).
		ExpectedStatus: review.GetStatus(),
	})
	dcancel()
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return fmt.Errorf("set approved: review %s changed under this approval (it was %s when read) — re-run `w17ctl review show` to see where it landed", review.GetId(), review.GetStatus())
		}
		return fmt.Errorf("set approved: %w", err)
	}
	fmt.Fprintf(core.Stdout, "approved review %s (by %s)\n", review.GetId(), actor)
	return nil
}

// --- merge --------------------------------------------------------

type MergeCmd struct {
	Project string `name:"project" placeholder:"ID" help:"Project id. Empty = read from the current project's lock."`
	ID      string `name:"id" placeholder:"REVIEW" help:"Review id. Empty = latest review on the current branch."`
	Env     string `name:"env" placeholder:"NAME" help:"Target environment. If it is in prod mode, BREAKING changes block the merge; the env's deployed snapshot advances to the merged head."`
	By      string `name:"by" placeholder:"ACTOR" help:"Actor. Empty = local OS user."`
}

func (c *MergeCmd) Run(parent *Cmd) error {
	project, err := storageclient.ResolveProjectID(c.Project)
	if err != nil {
		return err
	}
	actor := c.By
	if actor == "" {
		actor = storageclient.SelfActor()
	}
	sc, err := storageclient.DialStorageFn(parent.Console)
	if err != nil {
		return err
	}
	defer sc.Close()

	review, err := sc.ResolveReview(project, c.ID)
	if err != nil {
		return fmt.Errorf("merge: %w", err)
	}

	// Idempotency: an already-merged review is a no-op success. This is what
	// makes `review merge` the CI two-merge bridge — CI calls it after the git
	// PR merges, and a retry on an already-merged review is harmless.
	if review.GetStatus() == reviewpb.Review_MERGED {
		fmt.Fprintf(core.Stdout, "review %s already merged (no-op)\n", review.GetId())
		return nil
	}

	// Drift detection (v1: detect, don't resolve). The review's base was
	// the trunk tip at open; if trunk has advanced since, re-validate the
	// semantic diff against the CURRENT trunk tip and surface it.
	//
	// SCOPE NOTE (T3-7 D-F1). This block still only RENDERS — it re-classifies
	// in "dev" mode and never blocks, so a merge whose base is stale proceeds.
	// That is deliberately left alone here and tracked as its own finding: it
	// is a POLICY question (should a stale base block, and in which mode?)
	// that is equally true of a purely sequential run, whereas D-F1 was a
	// concurrency defect — two merges producing an outcome no serial order
	// can. The compare-and-set below closes the concurrency half exactly:
	// it refuses a write whose premise expired DURING this merge, and says
	// nothing about a premise that was already stale when the merge started.
	trunk, err := sc.TrunkInitiative(project)
	if err != nil {
		return err
	}
	if trunk == nil {
		return fmt.Errorf("project has no trunk initiative")
	}
	// The premise of everything below: the trunk head as it was when this
	// merge read it. SetHead re-asserts it, so a merge that raced this one
	// makes the advance fail loudly instead of overwriting it (D-F1).
	trunkHeadAtRead := trunk.GetHeadSnapshotId()
	if trunk.GetHeadSnapshotId() != "" && trunk.GetHeadSnapshotId() != review.GetBaseSnapshotId() {
		fmt.Fprintf(core.Stdout, "⚠ trunk advanced since the review opened — re-validating against current trunk tip:\n")
		tbase, err := sc.LoadIRBytes(trunk.GetHeadSnapshotId())
		if err != nil {
			return err
		}
		thead, err := sc.LoadIRBytes(review.GetHeadSnapshotId())
		if err != nil {
			return err
		}
		dresp, err := compat.ClassifyCompat(tbase, thead, "dev")
		if err != nil {
			return err
		}
		compat.RenderReportPB(dresp, "dev")
	}

	// Prod-mode gate: re-run compat against the target env's deployed
	// snapshot; a prod env BLOCKS a BREAKING merge (the only new
	// enforcement — same compat report the author already saw).
	var env *reviewpb.Environment
	if c.Env != "" {
		env, err = sc.FindEnv(project, c.Env)
		if err != nil {
			return err
		}
		if env == nil {
			return fmt.Errorf("no environment %q (create it with `w17ctl env create`)", c.Env)
		}
		mode := "dev"
		if env.GetMode() == reviewpb.Environment_PROD {
			mode = "prod"
		}
		base, err := sc.LoadIRBytes(env.GetDeployedSnapshotId())
		if err != nil {
			return err
		}
		head, err := sc.LoadIRBytes(review.GetHeadSnapshotId())
		if err != nil {
			return err
		}
		resp, err := compat.ClassifyCompat(base, head, mode)
		if err != nil {
			return err
		}
		if resp.GetBlocked() {
			fmt.Fprintf(os.Stderr, "compat gate (%s, env %q):\n", mode, c.Env)
			_ = compat.RenderReportPB(resp, mode)
			return fmt.Errorf("merge BLOCKED: BREAKING change against prod env %q — make it backward-compatible first", c.Env)
		}
	}

	// KNOWN LIMITATION (non-atomic merge): the four mutations below
	// (SetHead → SetDecision → SetStatus → SetDeployed) are independent RPCs
	// with no surrounding transaction, so a failure partway leaves trunk
	// advanced while the review/initiative/env lag. This is bounded — not
	// silently corrupting — because the sequence is replay-safe: the
	// Review_MERGED short-circuit above makes a re-run a no-op once the
	// review is closed, and SetHead to the same head is idempotent, so the
	// operator's recovery is simply to re-run `review merge`. Making this
	// truly atomic requires a server-side multi-op RPC (as `initiative push`
	// already has, G7); tracked for when one lands.
	//
	// That reasoning is about a partial FAILURE, and T3-7 D-F1 found what it
	// does not cover: under two CONCURRENT merges nothing fails and nothing
	// replays. Both read this trunk head, both wrote it, last writer won, and
	// the loser's review stayed recorded MERGED while trunk pointed at the
	// winner's snapshot — unfindable afterwards, since snapshots carry no
	// parent pointer. Every write below therefore carries the value it READ
	// (Expected*), so the storage tier refuses a write whose premise expired.
	// That is deliberately NOT a transaction: a transaction would make the
	// four writes atomic without making any of them conditional, so two
	// merges would still serialize into the same lost update.
	//
	// Advance trunk head to the merged snapshot (trunk resolved above).
	thctx, thcancel := core.ClientCtx()
	_, err = sc.IM.SetHead(thctx, &initiativespb.SetHeadReq{
		Id:                     trunk.GetId(),
		HeadSnapshotId:         review.GetHeadSnapshotId(),
		ExpectedHeadSnapshotId: trunkHeadAtRead,
	})
	thcancel()
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return fmt.Errorf("advance trunk: trunk moved from %s under this merge (another merge landed first) — re-run `w17ctl review merge` to re-validate against the new trunk tip", displaySnapshot(trunkHeadAtRead))
		}
		return fmt.Errorf("advance trunk: %w", err)
	}

	// Close the review.
	dctx, dcancel := core.ClientCtx()
	_, err = sc.RM.SetDecision(dctx, &reviewpb.SetReviewDecisionReq{
		Id: review.GetId(), Status: reviewpb.Review_MERGED, DecidedBy: actor,
		ExpectedStatus: review.GetStatus(),
	})
	dcancel()
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return fmt.Errorf("close review: review %s was decided by someone else while this merge ran (it was %s when read); trunk is already advanced — re-run `w17ctl review merge` to finish", review.GetId(), review.GetStatus())
		}
		return fmt.Errorf("close review: %w", err)
	}

	// Close the initiative (lifecycle: merge closes review + initiative).
	if review.GetInitiativeId() != "" {
		ictx, icancel := core.ClientCtx()
		_, err = sc.IM.SetStatus(ictx, &initiativespb.SetStatusReq{Id: review.GetInitiativeId(), Status: initiativespb.Initiative_MERGED})
		icancel()
		if err != nil {
			return fmt.Errorf("close initiative: %w", err)
		}
	}

	// Deploy to the target environment (advance its baseline). Conditional on
	// the baseline the gate above actually diffed against: if it moved, this
	// merge was classified against a snapshot that is no longer deployed, and
	// letting the write land is what turns D-F1 into "the prod gate passes a
	// breaking change".
	if env != nil {
		edctx, edcancel := core.ClientCtx()
		_, err = sc.EM.SetDeployed(edctx, &reviewpb.SetEnvDeployedReq{
			Id:                         env.GetId(),
			DeployedSnapshotId:         review.GetHeadSnapshotId(),
			ExpectedDeployedSnapshotId: env.GetDeployedSnapshotId(),
		})
		edcancel()
		if err != nil {
			if status.Code(err) == codes.NotFound {
				return fmt.Errorf("deploy to env %q: its deployed snapshot moved from %s under this merge, so the compat gate ran against a baseline that is no longer live; trunk is already advanced — re-run `w17ctl review merge --env %s`", c.Env, displaySnapshot(env.GetDeployedSnapshotId()), c.Env)
			}
			return fmt.Errorf("deploy to env %q: %w", c.Env, err)
		}
	}

	fmt.Fprintf(core.Stdout, "merged review %s → trunk advanced to %s", review.GetId(), review.GetHeadSnapshotId())
	if env != nil {
		fmt.Fprintf(core.Stdout, " + deployed to %q", c.Env)
	}
	fmt.Fprintln(core.Stdout)
	return nil
}
