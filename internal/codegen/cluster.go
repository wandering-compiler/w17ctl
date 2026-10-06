package codegen

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/rand/v2"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/wandering-compiler/w17ctl/internal/core"
	codegenpb "github.com/wandering-compiler/sdk/go/pb/w17compiler"
)

// A codegen run on the cluster (docs/specs/console/codegen-on-the-cluster.md
// §6): the console PLACES the run and signs the lock, a codegen worker behind a
// relay RUNS it. Two connections, two phases:
//
//  1. PlaceGenerate{start} on the console → while `queued`, PlaceGenerate{poll}
//     every `retry_after_ms` → `granted{address, cert_fingerprint, ticket}`.
//  2. DialWorker(address, fingerprint, ticket) → the GenerateProject stream,
//     unchanged from when it ran on the console, carrying the console-signed
//     lock as `lock_yaml`.
//
// A failure is classified, never blindly retried: an answer about the INPUT or
// the CALLER is final; a refusal from the cluster (no worker, a ticket that
// cannot be redeemed, a draining relay, a full worker) is answered with a new
// placement; a connection lost mid-run is answered with ONE new placement by
// default (--retries). Nothing is retried after a clean end of the stream.

// ClusterErrorDomain is the google.rpc.ErrorInfo domain the relay and the
// worker put on a refusal that a new placement can cure.
const ClusterErrorDomain = "cluster.w17"

// placeAgainReasons are the ErrorInfo reasons (domain ClusterErrorDomain) that
// mean "this placement cannot run; another one may". Any other reason in the
// domain is final — an unknown reason is not evidence that retrying helps.
var placeAgainReasons = map[string]bool{
	"NO_WORKER":             true,
	"TICKET_NOT_REDEEMABLE": true,
	"RELAY_DRAINING":        true,
	"WORKER_LOST":           true,
	"ADMISSION_FULL":        true,
	"ADMISSION_TIMEOUT":     true,
	// The console's answer to a poll on a placement that lapsed (abandoned,
	// cancelled, the relay restarted) or that it holds for nobody else.
	"RESERVATION_UNKNOWN": true,
	// The pool had no relay that could take it — a relay restarting comes
	// back; one that does not is an operator's problem, reported after the
	// last attempt.
	"NO_RELAY": true,
}

// DefaultRetries is how many times a run that lost its connection mid-stream
// is placed and run again. One: a lost relay or worker is usually a deploy or
// a crash, and one fresh placement lands on a healthy one; a second loss in a
// row is a pattern worth reporting rather than hiding.
const DefaultRetries = 1

// The tunables of the cluster flow, package vars so tests can shrink the waits
// and observe them.
var (
	// placeAttempts bounds the placements a run may fail before it gives up
	// (refusals from the cluster, an unreachable console).
	placeAttempts = 5
	// placeBackoff is the wait before the 2nd, 3rd, ... placement; the last
	// entry repeats. Jittered by jitterFn.
	placeBackoff = []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second}
	// placeCallTimeout is the deadline of ONE PlaceGenerate call (start or
	// poll). The queue wait itself is not bounded here: the relay abandons a
	// reservation nobody polls, and polling is what keeps it alive.
	placeCallTimeout = 30 * time.Second
	// streamAttemptTimeout is the deadline of ONE GenerateProject stream on a
	// worker — header, upload, the whole server-side pipeline, the download.
	// Per attempt: a re-run gets a fresh window rather than the remainder of
	// one the lost run spent.
	streamAttemptTimeout = 600 * time.Second
	// streamPerLine is added to streamAttemptTimeout per uploaded proto line:
	// a worker runs on ONE core, and a large project takes minutes there —
	// 744 s measured at 206 662 lines (docs/specs/console/codegen-on-the-cluster.md
	// §2). 7 ms a line gives that run ~2.7x headroom; see attemptDeadline.
	streamPerLine = 7 * time.Millisecond
	// queueTimeout bounds how long a run waits in the queue for a worker.
	// Long enough for a deploy (a worker drains its run, up to 15 min, before
	// the new one starts) and a busy pool; short enough that a pool with no
	// worker at all fails a CI job instead of hanging it.
	queueTimeout = 30 * time.Minute
	// maxPollBlips is how many bare Unavailable polls in a row keep the place.
	maxPollBlips = 3
	// defaultPollAfter is used when a `queued` answer names no retry_after_ms.
	defaultPollAfter = 1 * time.Second

	sleepFn  = time.Sleep
	jitterFn = func(d time.Duration) time.Duration {
		// ±25 %, so a fleet of pipelines refused together does not come back
		// together.
		if d <= 0 {
			return d
		}
		return d*3/4 + time.Duration(rand.Int64N(int64(d)/2+1)) //nolint:gosec // G404: backoff jitter, not a secret.
	}
)

// failClass is what a failure calls for.
type failClass int

const (
	// failFatal — report and stop: the input, the caller or the protocol.
	failFatal failClass = iota
	// failPlaceAgain — the cluster refused this placement; place again.
	failPlaceAgain
	// failLost — the connection was lost; run again on a new placement.
	failLost
)

// classifyFailure names what an error calls for and a cause the messages use.
func classifyFailure(err error) (failClass, string) {
	if errors.Is(err, context.DeadlineExceeded) {
		return failFatal, "deadline exceeded"
	}
	st, ok := status.FromError(err)
	if !ok {
		// Not a gRPC status: produced by this client (a placement missing its
		// address or fingerprint), so nothing a retry would change.
		return failFatal, "refused by the client"
	}
	if core.IsWorkerPinRefusal(err) {
		// A certificate other than the pinned one is an identity failure, not
		// a network one, however gRPC codes it. Never retried: the next
		// placement would name the same relay.
		return failFatal, "relay certificate does not match the console's pin"
	}
	for _, d := range st.Details() {
		switch dd := d.(type) {
		case *codegenpb.CodegenError:
			return failFatal, "rejected by the compiler"
		case *errdetails.ErrorInfo:
			if dd.GetDomain() == ClusterErrorDomain {
				if placeAgainReasons[dd.GetReason()] {
					return failPlaceAgain, "the cluster refused the placement (" + dd.GetReason() + ")"
				}
				return failFatal, "the cluster refused the run (" + dd.GetReason() + ")"
			}
		}
	}
	switch st.Code() {
	case codes.Unavailable:
		return failLost, "connection lost"
	case codes.DeadlineExceeded:
		return failFatal, "deadline exceeded"
	case codes.Unauthenticated, codes.PermissionDenied:
		return failFatal, "not authorised"
	case codes.InvalidArgument, codes.FailedPrecondition, codes.OutOfRange:
		return failFatal, "the input was refused"
	case codes.ResourceExhausted:
		// A bare ResourceExhausted is the size cap (max_lines): the same input
		// is refused by every worker. A full worker says ADMISSION_FULL above.
		return failFatal, "the input is larger than the cluster admits"
	}
	return failFatal, strings.ToLower(st.Code().String())
}

// clusterRun is one `w17ctl codegen` run's cluster half.
type clusterRun struct {
	placer core.Placer
	// lock is the lock as read from disk: PlaceGenerateStart.lock and
	// GenerateProjectRequest.lock.
	lock []byte
	// protoLines is the uploaded proto size the console admits against.
	protoLines int64
	// header is the stream's first message, minus the two lock fields each
	// attempt fills in.
	header          *codegenpb.GenerateProjectRequest
	files, genFiles []*codegenpb.ProtoFile
	// retries is how many times a run that lost its connection is run again.
	retries int
	out     io.Writer
	// root is the project root: a SignRequest's prior lock is read from it.
	root string
}

// clusterResult is a run's buffered output plus what the placement handed back.
type clusterResult struct {
	writes   []*codegenpb.GeneratedFile
	deletes  []string
	warnings []string
	sdkReqs  []*codegenpb.SdkRequirement
	// signs are files the worker could not sign; signAll turns each into a
	// write the console signed, or fails the run.
	signs []*codegenpb.SignRequest
	// signedLock is the console-signed lock of the placement that ran — the
	// bytes `w17/lock.yaml` gets.
	signedLock []byte
	// placementWarnings came back with the placement (an org stamp refreshed).
	placementWarnings []string
}

// lockPath is where the console-signed lock lands.
const lockPath = "w17/lock.yaml"

// run places and runs until a clean stream end or a final failure. On failure
// the returned result (possibly nil) carries the warnings that arrived before
// it, so the caller can print them ahead of the error.
func (r *clusterRun) run() (*clusterResult, error) {
	placeFailures, reruns := 0, 0
	for {
		granted, signed, placeWarnings, err := r.place()
		if err != nil {
			class, cause := classifyFailure(err)
			if class == failLost {
				// Losing the CONSOLE during placement runs nothing; it is a
				// placement that did not happen.
				class, cause = failPlaceAgain, "the console is unreachable"
			}
			if class == failFatal {
				return nil, fatalPlacementError(cause, err)
			}
			placeFailures++
			if placeFailures >= placeAttempts {
				return nil, fmt.Errorf("codegen: %s; gave up after %d placement attempt(s): %w", cause, placeFailures, err)
			}
			wait := jitterFn(backoffFor(placeFailures))
			fmt.Fprintf(r.out, "codegen: %s — placing the run again in %s (attempt %d of %d)\n",
				cause, wait.Round(100*time.Millisecond), placeFailures+1, placeAttempts)
			sleepFn(wait)
			continue
		}

		res, err := r.stream(granted, signed)
		if err == nil {
			if err := r.signAll(res); err != nil {
				return res, err
			}
			res.signedLock = signed
			res.placementWarnings = placeWarnings
			return res, nil
		}
		class, cause := classifyFailure(err)
		switch class {
		case failPlaceAgain:
			placeFailures++
			if placeFailures >= placeAttempts {
				return res, fmt.Errorf("codegen: %s; gave up after %d placement attempt(s): %w", cause, placeFailures, err)
			}
			wait := jitterFn(backoffFor(placeFailures))
			fmt.Fprintf(r.out, "codegen: %s — placing the run again in %s (attempt %d of %d)\n",
				cause, wait.Round(100*time.Millisecond), placeFailures+1, placeAttempts)
			sleepFn(wait)
		case failLost:
			if reruns >= r.retries {
				return res, fmt.Errorf("codegen: %s to the codegen worker mid-run; gave up after %d re-run(s) (--retries %d): %w",
					cause, reruns, r.retries, err)
			}
			reruns++
			fmt.Fprintf(r.out, "codegen: %s to the codegen worker mid-run — running again on a new placement (re-run %d of %d)\n",
				cause, reruns, r.retries)
		default:
			return res, fatalStreamError(cause, err, attemptDeadline(r.protoLines))
		}
	}
}

// backoffFor is the un-jittered wait after the n-th failed placement (n ≥ 1).
func backoffFor(n int) time.Duration {
	if n-1 < len(placeBackoff) {
		return placeBackoff[n-1]
	}
	return placeBackoff[len(placeBackoff)-1]
}

// fatalPlacementError names the cause of a final placement failure.
func fatalPlacementError(cause string, err error) error {
	if hasCodegenError(err) {
		return formatCodegenError(err)
	}
	return fmt.Errorf("codegen: the console did not place the run (%s): %w", cause, err)
}

// fatalStreamError names the cause of a final failure of the run itself.
func fatalStreamError(cause string, err error, deadline time.Duration) error {
	if hasCodegenError(err) {
		// The compiler's own diagnostics, exactly as when it ran on the console.
		return formatCodegenError(err)
	}
	if errors.Is(err, context.DeadlineExceeded) || status.Code(err) == codes.DeadlineExceeded {
		return fmt.Errorf("codegen: the run exceeded its %s deadline on the codegen worker: %w", deadline, err)
	}
	return fmt.Errorf("codegen: the run failed on the codegen worker (%s): %w", cause, err)
}

func hasCodegenError(err error) bool {
	st, ok := status.FromError(err)
	if !ok {
		return false
	}
	for _, d := range st.Details() {
		if _, ok := d.(*codegenpb.CodegenError); ok {
			return true
		}
	}
	return false
}

// place opens a placement and polls it until a worker is granted. The signed
// lock and its warnings come with the answer to `start` and are kept across
// polls.
func (r *clusterRun) place() (*codegenpb.PlaceGenerateGranted, []byte, []string, error) {
	resp, err := r.placeCall(&codegenpb.PlaceGenerateRequest{Step: &codegenpb.PlaceGenerateRequest_Start{
		Start: &codegenpb.PlaceGenerateStart{Lock: r.lock, ProtoLines: r.protoLines},
	}})
	if err != nil {
		return nil, nil, nil, err
	}
	signed, warnings := resp.GetSignedLockYaml(), resp.GetWarnings()
	lastPosition := int32(-1)
	var waited time.Duration
	blips := 0
	for {
		if g := resp.GetGranted(); g != nil {
			if len(signed) == 0 {
				return nil, nil, nil, errors.New("the console granted a placement without the signed lock — refusing to run on it")
			}
			return g, signed, warnings, nil
		}
		q := resp.GetQueued()
		if q == nil {
			return nil, nil, nil, errors.New("the console answered the placement with neither queued nor granted")
		}
		if q.GetPosition() != lastPosition {
			fmt.Fprintf(r.out, "codegen: waiting for a codegen worker — position %d in the queue\n", q.GetPosition())
			lastPosition = q.GetPosition()
		}
		wait := time.Duration(q.GetRetryAfterMs()) * time.Millisecond
		if wait <= 0 {
			wait = defaultPollAfter
		}
		// A queue that never moves — a pool with no worker attached — must
		// not hold a run (or a CI job) forever.
		if waited >= queueTimeout {
			return nil, nil, nil, fmt.Errorf("no codegen worker became free within %s (still position %d in the queue) — "+
				"the pool may have no worker attached; an operator can check the backoffice admin", queueTimeout, q.GetPosition())
		}
		sleepFn(wait)
		waited += wait
		resp, err = r.placeCall(&codegenpb.PlaceGenerateRequest{Step: &codegenpb.PlaceGenerateRequest_Poll{
			Poll: &codegenpb.PlaceGeneratePoll{Reservation: q.GetReservation()},
		}})
		// A blip between the console and the relay keeps the place: the same
		// handle is polled again (the console keeps it on Unavailable), a few
		// times, before the run gives the place up and places afresh.
		for err != nil && blips < maxPollBlips && isBareUnavailable(err) {
			blips++
			sleepFn(wait)
			waited += wait
			resp, err = r.placeCall(&codegenpb.PlaceGenerateRequest{Step: &codegenpb.PlaceGenerateRequest_Poll{
				Poll: &codegenpb.PlaceGeneratePoll{Reservation: q.GetReservation()},
			}})
		}
		if err != nil {
			return nil, nil, nil, err
		}
		blips = 0
	}
}

// isBareUnavailable is a connection-level Unavailable with no cluster reason:
// a blip, as opposed to a refusal that names its cause.
func isBareUnavailable(err error) bool {
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.Unavailable {
		return false
	}
	for _, d := range st.Details() {
		if _, ok := d.(*errdetails.ErrorInfo); ok {
			return false
		}
	}
	return true
}

// attemptDeadline is the window of ONE GenerateProject attempt: the base, plus
// time per proto line, because the worker generates on one core and a large
// project takes minutes there.
func attemptDeadline(protoLines int64) time.Duration {
	if protoLines < 0 {
		protoLines = 0
	}
	return streamAttemptTimeout + time.Duration(protoLines)*streamPerLine
}

func (r *clusterRun) placeCall(req *codegenpb.PlaceGenerateRequest) (*codegenpb.PlaceGenerateResponse, error) {
	ctx, cancel := context.WithTimeout(context.Background(), placeCallTimeout)
	defer cancel()
	return r.placer.PlaceGenerate(ctx, req)
}

// stream runs GenerateProject once on the granted worker and buffers the op
// stream. On failure the result carries the warnings received so far.
func (r *clusterRun) stream(g *codegenpb.PlaceGenerateGranted, signed []byte) (*clusterResult, error) {
	cl, conn, err := core.DialWorker(g.GetAddress(), g.GetCertFingerprint(), g.GetTicket())
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), attemptDeadline(r.protoLines))
	defer cancel()
	stream, err := cl.GenerateProject(ctx)
	if err != nil {
		return nil, err
	}
	// The header goes first and ALONE carries the non-file fields; the tree
	// follows in chunks. One message cannot hold it: a project's protos reach
	// gRPC's default 4 MiB around 95k lines.
	header, _ := proto.Clone(r.header).(*codegenpb.GenerateProjectRequest)
	header.Lock = r.lock
	header.LockYaml = signed
	if err := stream.Send(header); err != nil {
		return nil, recvStatus(stream, err)
	}
	if err := sendProtoChunks(stream, r.files, r.genFiles); err != nil {
		return nil, recvStatus(stream, err)
	}
	if err := stream.CloseSend(); err != nil {
		return nil, recvStatus(stream, err)
	}

	// Buffer the whole op stream — the collision pre-scan (R-console-4) needs
	// the full write set before touching disk.
	res := &clusterResult{}
	for {
		op, recvErr := stream.Recv()
		if errors.Is(recvErr, io.EOF) {
			return res, nil
		}
		if recvErr != nil {
			return res, recvErr
		}
		switch o := op.GetOp().(type) {
		case *codegenpb.GeneratedOp_Write:
			// The lock is the CONSOLE's: it signed it at placement, and the
			// client writes those bytes. A worker holds no key and never
			// authors the lock; whatever one sends for it is dropped.
			if path.Clean(strings.TrimPrefix(o.Write.GetRelativePath(), "./")) == lockPath {
				continue
			}
			res.writes = append(res.writes, o.Write)
		case *codegenpb.GeneratedOp_Delete:
			res.deletes = append(res.deletes, o.Delete)
		case *codegenpb.GeneratedOp_Warning:
			res.warnings = append(res.warnings, o.Warning)
		case *codegenpb.GeneratedOp_SdkRequirement:
			res.sdkReqs = append(res.sdkReqs, o.SdkRequirement)
		case *codegenpb.GeneratedOp_Sign:
			res.signs = append(res.signs, o.Sign)
		}
	}
}

// recvStatus turns a Send failure into the stream's real status. A Send that
// fails returns io.EOF when the SERVER ended the stream; the reason (a refusal
// from the relay, the worker's own error) is only on Recv.
func recvStatus(stream interface {
	Recv() (*codegenpb.GeneratedOp, error)
}, sendErr error) error {
	if !errors.Is(sendErr, io.EOF) {
		return sendErr
	}
	for {
		if _, err := stream.Recv(); err != nil {
			if errors.Is(err, io.EOF) {
				return sendErr
			}
			return err
		}
	}
}

// countProtoLines is the upload size the console admits against: newline
// count, plus one for a file not ending in a newline.
func countProtoLines(files []*codegenpb.ProtoFile) int64 {
	var total int64
	for _, f := range files {
		c := f.GetContents()
		if len(c) == 0 {
			continue
		}
		total += int64(strings.Count(string(c), "\n"))
		if c[len(c)-1] != '\n' {
			total++
		}
	}
	return total
}

// signAll has the console sign every file the worker could not, and turns each
// into an ordinary write. A file that cannot be signed fails the run: it is
// never written unsigned, and nothing else of the run is written either.
func (r *clusterRun) signAll(res *clusterResult) error {
	for _, req := range res.signs {
		if req.GetKind() != codegenpb.SignRequest_ACL_LOCK {
			return fmt.Errorf("codegen: the worker asked to sign %s as %s, which this w17ctl cannot — update it", req.GetPath(), req.GetKind())
		}
		rel := path.Clean(strings.TrimPrefix(req.GetPath(), "./"))
		if rel == ".." || strings.HasPrefix(rel, "../") || path.IsAbs(rel) {
			return fmt.Errorf("codegen: the worker asked to sign %q, outside the project", req.GetPath())
		}
		prior, err := os.ReadFile(filepath.Join(r.root, filepath.FromSlash(rel)))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("codegen: read %s to have it extended: %w", rel, err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), placeCallTimeout)
		resp, err := r.placer.SignAclLock(ctx, &codegenpb.SignAclLockRequest{
			Lock: r.lock, Domain: req.GetDomain(), Prior: prior, Unsigned: req.GetUnsigned(),
		})
		cancel()
		if err != nil {
			return fmt.Errorf("codegen: the console did not sign %s: %w", rel, err)
		}
		res.writes = append(res.writes, &codegenpb.GeneratedFile{RelativePath: rel, Contents: resp.GetSigned()})
	}
	res.signs = nil
	return nil
}
