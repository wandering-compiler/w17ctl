// Package storageclient is the thin client's spine over the GENERATED
// console-v2 storage services (Initiative/Snapshot/Review/Approval/
// Environment/Checkpoint Query+Mutation). Every console-v2 service runs in
// the one storage bundle, so they share a single gRPC connection.
//
// w17ctl orchestrates these services DIRECTLY — there is no hand-written
// server facade (the facade pattern is not used here). The only logic is
// client-side glue: get-or-create, lazy materialize, head advance,
// "current branch → initiative" git-sync. "One trunk per project" is a DB
// partial-unique index, not app logic.
//
// This package is the foundation the console-state commands (compat,
// initiative, review, env, db) and `stack build` share; the commands hold
// only their flag parsing + presentation and call these methods.
package storageclient

import (
	"context"
	"fmt"
	"os/exec"
	"os/user"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wandering-compiler/w17ctl/internal/core"
	checkpointspb "github.com/wandering-compiler/sdk/go/pb/consoleapi/checkpoints"
	initiativespb "github.com/wandering-compiler/sdk/go/pb/consoleapi/initiatives"
	reviewpb "github.com/wandering-compiler/sdk/go/pb/consoleapi/review"
	consolerpcpb "github.com/wandering-compiler/sdk/go/pb/consoleapi/rpc"
	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
)

// DefaultStorageAddr is the compile-time default console storage endpoint
// (console-app's gRPC, reached via the dev TLS terminator). A var, not a const,
// so the build can bake the per-environment value via ldflags -X (like
// core.DefaultConsoleAddr); overridable at runtime by CONSOLE_STORAGE_ADDR / the
// --console flag.
var DefaultStorageAddr = "localhost:50051"

// storageMaxRecvMsgSize lifts gRPC's 4 MiB default receive cap for the
// storage tier, matching the recv cap core.Dial* uses on the other console
// dials — a snapshot's ir_schema blob (LoadIRBytes) can exceed 4 MiB on a
// large schema and would otherwise trip ResourceExhausted.
const storageMaxRecvMsgSize = 256 << 20

// bearerPerRPC attaches `authorization: Bearer <token>` to every storage-tier
// gRPC call, sourcing the token fresh per call from core.AuthTokenFn — the
// SAME seam core.Dial* uses for the codegen/registry/fetch dials, so the
// storage tier stays consistent for when these RPCs become auth-gated. With no
// token (not logged in) it adds nothing, identical to the pre-auth behavior, so
// it is safe to wire on unconditionally. RequireTransportSecurity is false
// because w17ctl dials the console over PLAINTEXT today (TLS is the deferred
// "Phase C"); the bearer rides the encrypted channel unchanged once TLS lands.
// addr is the console being dialed — the credential is resolved per
// address, not from the active instance (see core.AuthTokenFn).
type bearerPerRPC struct{ addr string }

func (b bearerPerRPC) GetRequestMetadata(_ context.Context, _ ...string) (map[string]string, error) {
	token := core.AuthTokenFn(b.addr)
	if token == "" {
		return nil, nil
	}
	return map[string]string{"authorization": "Bearer " + token}, nil
}

func (bearerPerRPC) RequireTransportSecurity() bool { return false }

// ResolveStorageAddr resolves the console endpoint the storage-tier clients
// dial. The storage tier (initiative / review / env / snapshot / checkpoint)
// is served by the SAME console gateway as codegen + migrate, so it must
// follow the SAME resolution chain — otherwise a project logged into a
// console on a non-default port (e.g. the dev console-app on :13444) would
// have `initiative`/`review`/`env` silently misroute to localhost:50051 and
// report stale "not yet materialized" while codegen reached the real console.
//
// Order (first non-empty wins): the explicit --console flag, then
// core.ResolveConsoleAddr — the logged-in console (authstore active instance)
// then the binary's compiled-in DefaultConsoleAddr. DefaultStorageAddr is the
// last-resort fallback (unconfigured + not logged in), preserved for
// back-compat and tests.
func ResolveStorageAddr(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if addr, err := core.ResolveConsoleAddr(""); err == nil && addr != "" {
		return addr
	}
	return DefaultStorageAddr
}

// StorageClients bundles the generated storage-tier clients w17ctl
// orchestrates over — sharing a single connection (Close tears it down).
// Client interfaces come from the console rpc-gateway lock pb
// (consolerpcpb, dialing w17lock.console.rpc.*) — the public surface the
// composed console-server actually serves. They reuse the backend message
// types (initiativespb / reviewpb / checkpointspb) verbatim, so the
// orchestration methods below construct the SAME request/response structs
// as before; only the service FQN they dial changed.
type StorageClients struct {
	IQ consolerpcpb.InitiativeQueryClient
	IM consolerpcpb.InitiativeMutationClient
	SQ consolerpcpb.SnapshotQueryClient
	SM consolerpcpb.SnapshotMutationClient

	RQ consolerpcpb.ReviewQueryClient
	RM consolerpcpb.ReviewMutationClient
	AQ consolerpcpb.ApprovalQueryClient
	AM consolerpcpb.ApprovalMutationClient
	EQ consolerpcpb.EnvironmentQueryClient
	EM consolerpcpb.EnvironmentMutationClient

	CQ consolerpcpb.CheckpointQueryClient
	CM consolerpcpb.CheckpointMutationClient

	Close func()
}

// DialStorageFn is the dial seam: a package var so tests can inject an
// in-process StorageClients (fake clients) without a live console, the
// same way core.DialClientFn / compat.DialCompilerFn are swapped.
// Production points it at DialStorage.
var DialStorageFn = DialStorage

// DialStorage opens the shared connection and constructs every client.
func DialStorage(addr string) (*StorageClients, error) {
	a := ResolveStorageAddr(addr)
	conn, err := grpc.NewClient(core.ConsoleTarget(a),
		// Always TLS — mirrors core.Dial*.
		core.ConsoleTransportCreds(a),
		// Attach the logged-in bearer (no-op when not logged in) + lift the
		// receive cap, mirroring core.Dial*. See bearerPerRPC above.
		grpc.WithPerRPCCredentials(bearerPerRPC{addr: a}),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(storageMaxRecvMsgSize)),
	)
	if err != nil {
		return nil, fmt.Errorf("dial console %s: %w", a, err)
	}
	return &StorageClients{
		IQ:    consolerpcpb.NewInitiativeQueryClient(conn),
		IM:    consolerpcpb.NewInitiativeMutationClient(conn),
		SQ:    consolerpcpb.NewSnapshotQueryClient(conn),
		SM:    consolerpcpb.NewSnapshotMutationClient(conn),
		RQ:    consolerpcpb.NewReviewQueryClient(conn),
		RM:    consolerpcpb.NewReviewMutationClient(conn),
		AQ:    consolerpcpb.NewApprovalQueryClient(conn),
		AM:    consolerpcpb.NewApprovalMutationClient(conn),
		EQ:    consolerpcpb.NewEnvironmentQueryClient(conn),
		EM:    consolerpcpb.NewEnvironmentMutationClient(conn),
		CQ:    consolerpcpb.NewCheckpointQueryClient(conn),
		CM:    consolerpcpb.NewCheckpointMutationClient(conn),
		Close: func() { _ = conn.Close() },
	}, nil
}

// ResolveProjectID returns the explicit --project value, falling back to
// the current project's lock project_id when run inside a w17 tree.
func ResolveProjectID(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if id := core.LockProjectIDBestEffort(); id != "" {
		return id, nil
	}
	return "", fmt.Errorf("no project — pass --project ID or run inside a w17 project tree (w17/lock.yaml)")
}

// userCurrentFn is indirected so tests can drive the OS-username fallback
// without depending on the test host actually failing user.Current.
var userCurrentFn = user.Current

// SelfActor is the local identity stamped into actor fields in v1
// (self-only multi-actor). The OS username stands in until the auth
// channel lands.
func SelfActor() string {
	if u, err := userCurrentFn(); err == nil && u.Username != "" {
		return u.Username
	}
	return "unknown"
}

// GitCurrentBranchFn is indirected so tests can drive git-sync resolution
// without a real repo. Production reads `git`.
var GitCurrentBranchFn = realGitCurrentBranch

func realGitCurrentBranch() string {
	out, err := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return ""
	}
	branch := strings.TrimSpace(string(out))
	if branch == "" || branch == "HEAD" {
		return ""
	}
	return branch
}

// IsTrunkBranch reports whether name is a reserved trunk branch.
func IsTrunkBranch(name string) bool {
	return name == "main" || name == "master" || name == "trunk"
}

// ResolveInitiativeTarget applies git-sync: explicit --name wins; otherwise
// derive from the current branch. Any reserved name (main/master/trunk, per
// IsTrunkBranch) resolves to the canonical ("trunk", true) on BOTH paths — so
// `--name main` and a literal `trunk` branch both map to the one canonical
// trunk, never a divergent initiative named "main" or a non-trunk named "trunk".
func ResolveInitiativeTarget(explicit string) (name string, isTrunk bool, err error) {
	if explicit != "" {
		if IsTrunkBranch(explicit) {
			return "trunk", true, nil
		}
		return explicit, false, nil
	}
	branch := GitCurrentBranchFn()
	if branch == "" {
		return "", false, fmt.Errorf("not on a git branch (detached HEAD or no repo) — pass --name")
	}
	if IsTrunkBranch(branch) {
		return "trunk", true, nil
	}
	return branch, false, nil
}

// LoadIRBytes fetches a snapshot and returns its ir_schema as OPAQUE bytes
// (the marshalled IR) — the client passes these straight to the compat /
// plan RPCs without ever decoding the compiler's IR type. Empty id → nil
// (the "initial state" base).
func (s *StorageClients) LoadIRBytes(id string) ([]byte, error) {
	if id == "" {
		return nil, nil
	}
	ctx, cancel := core.ClientCtx()
	defer cancel()
	snap, err := s.SQ.Get(ctx, &initiativespb.GetSnapshotReq{Id: id})
	if err != nil {
		return nil, fmt.Errorf("load snapshot %s: %w", id, err)
	}
	return snap.GetIrSchema(), nil
}

// GetCheckpoint returns the checkpoint for (project, user, initiative), or
// nil when none exists yet (a brand-new branch — diff base is empty IR).
func (s *StorageClients) GetCheckpoint(project, user, initiative string) (*checkpointspb.Checkpoint, error) {
	ctx, cancel := core.ClientCtx()
	defer cancel()
	resp, err := s.CQ.Get(ctx, &checkpointspb.GetCheckpointReq{
		ProjectId: project, UserId: user, Initiative: initiative,
	})
	if err != nil {
		return nil, err
	}
	if len(resp.GetRows()) == 0 {
		return nil, nil
	}
	return resp.GetRows()[0], nil
}

// AdvanceCheckpoint upserts the checkpoint for (project, user, initiative)
// to a new IR, returning the (stable across advances) row id.
func (s *StorageClients) AdvanceCheckpoint(project, user, initiative string, ir []byte, lockHash, compilerVersion string) (string, error) {
	ctx, cancel := core.ClientCtx()
	defer cancel()
	resp, err := s.CM.Advance(ctx, &checkpointspb.AdvanceCheckpointReq{
		ProjectId: project, UserId: user, Initiative: initiative,
		IrSchema: ir, LockHash: lockHash, CompilerVersion: compilerVersion,
	})
	if err != nil {
		return "", fmt.Errorf("advance checkpoint: %w", err)
	}
	return resp.GetId(), nil
}

// TrunkHead returns the trunk initiative's head snapshot id (+ whether a
// trunk with a snapshot exists). Used as the compat/review base.
func (s *StorageClients) TrunkHead(project string) (head string, ok bool, err error) {
	ctx, cancel := core.ClientCtx()
	defer cancel()
	list, err := s.IQ.List(ctx, &initiativespb.ListInitiativesReq{ProjectId: project})
	if err != nil {
		return "", false, err
	}
	for _, i := range list.GetRows() {
		if i.GetIsTrunk() {
			return i.GetHeadSnapshotId(), i.GetHeadSnapshotId() != "", nil
		}
	}
	return "", false, nil
}

// TrunkInitiative returns the trunk initiative row (or nil).
func (s *StorageClients) TrunkInitiative(project string) (*initiativespb.Initiative, error) {
	ctx, cancel := core.ClientCtx()
	defer cancel()
	list, err := s.IQ.List(ctx, &initiativespb.ListInitiativesReq{ProjectId: project})
	if err != nil {
		return nil, err
	}
	for _, i := range list.GetRows() {
		if i.GetIsTrunk() {
			return i, nil
		}
	}
	return nil, nil
}

// FindInitiative returns the initiative for (project,name) or nil when
// none exists (the unique index guarantees ≤1 row).
func (s *StorageClients) FindInitiative(project, name string) (*initiativespb.Initiative, error) {
	ctx, cancel := core.ClientCtx()
	defer cancel()
	resp, err := s.IQ.Find(ctx, &initiativespb.FindInitiativeReq{ProjectId: project, Name: name})
	if err != nil {
		return nil, err
	}
	if len(resp.GetRows()) == 0 {
		return nil, nil
	}
	return resp.GetRows()[0], nil
}

// IsUniqueViolation reports whether err is the storage tier's duplicate-row
// refusal.
//
// The discrimination lives in the `w17.ErrorDetail` grpcerr.Wrap attaches, NOT
// in the status code: codeForKind collapses UNIQUE to InvalidArgument (the C-3
// collapse), which is why the `codes.AlreadyExists` arm this function replaces
// was dead code. What still fired was a `strings.Contains(err.Error(), "unique
// violation")` — true only because Wrap happens to build the message as
// `<method>: <kind> violation`. That is a coincidence of wording, not a
// contract; the detail code is the contract, so read it. The substring is
// kept as a fallback for a server older than the detail.
func IsUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	st, ok := status.FromError(err)
	if ok {
		for _, d := range st.Details() {
			if det, ok := d.(*w17pb.ErrorDetail); ok && det.GetCode() == "UNIQUE_VIOLATION" {
				return true
			}
		}
	}
	return status.Code(err) == codes.AlreadyExists ||
		strings.Contains(err.Error(), "unique violation")
}

// GetOrCreate is the client-side get-or-create, and it is one on BOTH halves
// of its own create race (T3-7 D-F2).
//
// Two `initiative push` on one branch — a dev and a CI job, or two CI shards —
// both Find nothing and both Create; the initiatives(org_id, project_id, name)
// unique index refuses the second. That loser used to die with a raw "unique
// violation" although the row it wanted now existed: only the isTrunk half of
// the error arm was handled. Losing the race is not an error, it is the other
// legal serial order, so the loser re-reads and returns the winner's row.
//
// The "one trunk per project" partial-unique index is the case where the
// re-read finds nothing (the winner has a different name) — that one really is
// a refusal, and keeps its explanation.
func (s *StorageClients) GetOrCreate(project, name string, isTrunk bool, actor string) (init *initiativespb.Initiative, created bool, err error) {
	existing, err := s.FindInitiative(project, name)
	if err != nil {
		return nil, false, err
	}
	if existing != nil {
		return existing, false, nil
	}
	ctx, cancel := core.ClientCtx()
	defer cancel()
	resp, err := s.IM.Create(ctx, &initiativespb.CreateInitiativeReq{
		ProjectId: project, Name: name,
		Status: initiativespb.Initiative_OPEN, IsTrunk: isTrunk, CreatedBy: actor,
	})
	if err != nil {
		if IsUniqueViolation(err) {
			// Someone created it between our Find and our Create. Re-read: if
			// the row we wanted is there, this call did its job.
			if winner, ferr := s.FindInitiative(project, name); ferr == nil && winner != nil {
				return winner, false, nil
			}
			if isTrunk {
				return nil, false, fmt.Errorf("a trunk already exists for this project (only one is allowed); %q cannot be a second trunk", name)
			}
		}
		return nil, false, fmt.Errorf("create initiative: %w", err)
	}
	gctx, gcancel := core.ClientCtx()
	defer gcancel()
	got, err := s.IQ.Get(gctx, &initiativespb.GetInitiativeReq{Id: resp.GetId()})
	if err != nil {
		return nil, false, err
	}
	return got, true, nil
}

// ResolveReview returns the review by --id, or the latest review on the
// current branch's initiative when id is empty.
func (s *StorageClients) ResolveReview(project, explicitID string) (*reviewpb.Review, error) {
	if explicitID != "" {
		ctx, cancel := core.ClientCtx()
		defer cancel()
		return s.RQ.Get(ctx, &reviewpb.GetReviewReq{Id: explicitID})
	}
	name, _, err := ResolveInitiativeTarget("")
	if err != nil {
		return nil, err
	}
	initiative, err := s.FindInitiative(project, name)
	if err != nil {
		return nil, err
	}
	if initiative == nil {
		return nil, fmt.Errorf("no initiative %q (nothing to review)", name)
	}
	ctx, cancel := core.ClientCtx()
	defer cancel()
	list, err := s.RQ.ListByInitiative(ctx, &reviewpb.ListReviewsReq{InitiativeId: initiative.GetId()})
	if err != nil {
		return nil, err
	}
	if len(list.GetRows()) == 0 {
		return nil, fmt.Errorf("no review on initiative %q yet (open one first)", name)
	}
	return list.GetRows()[0], nil
}

// FindEnv returns the environment for (project,name) or nil.
func (s *StorageClients) FindEnv(project, name string) (*reviewpb.Environment, error) {
	ctx, cancel := core.ClientCtx()
	defer cancel()
	resp, err := s.EQ.Find(ctx, &reviewpb.FindEnvReq{ProjectId: project, Name: name})
	if err != nil {
		return nil, err
	}
	if len(resp.GetRows()) == 0 {
		return nil, nil
	}
	return resp.GetRows()[0], nil
}
