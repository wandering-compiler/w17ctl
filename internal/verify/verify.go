// Package verify implements the `w17ctl verify` drift hook — the
// counterpart to codegen. Where codegen GENERATES every derived
// artifact, verify recomputes the committed generated locks from the
// current proto and reports drift, without running a full codegen.
//
// As a thin client, w17ctl recomputes nothing itself: it uploads the
// proto set (including the committed locks) to the console's VerifyAcl /
// VerifyEventbus RPCs, which recompute + compare server-side. The lock's
// SIGNATURE is likewise checked server-side via VerifyLock — the client
// holds no verifier key (public-split boundary §4).
package verify

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/wandering-compiler/w17ctl/internal/core"
	codegenpb "github.com/wandering-compiler/sdk/go/pb/w17compiler"
)

// Run recomputes the committed ACL + eventbus locks server-side and
// reports drift. `console` is the resolved --console flag value (empty
// → core resolves it from the lock / compile-time default). Progress
// lines are written to `out`. Returns a non-zero (error) result on
// drift so a CI step can fail the build.
func Run(out io.Writer, console string) error {
	root, err := core.FindProjectRoot()
	if err != nil {
		return err
	}
	// The committed lock bytes feed both the proto-dir projection (DescribeLock)
	// and the signature check (VerifyLock) — the client holds no lock types
	// (public-split §8.2), so it asks the console for the proto dir. Read
	// BEFORE dialing: a local problem should surface without a round trip.
	lockBytes, err := os.ReadFile(filepath.Join(root, "w17", "lock.yaml"))
	if err != nil {
		return fmt.Errorf("verify: read lock: %w", err)
	}
	// T2-5 pass #9 (B-F1). Zero bytes is a SUCCESSFUL read, and the server
	// answers its `len(lock) == 0` branch with ok=true plus an empty LockView.
	// The empty proto_dir then steered the surface detectors at the `proto`
	// convention, so a project on a non-default proto_dir had both drift checks
	// silently skipped while this printed ok — and `checked` counted the lock
	// arm regardless, so the count did not give it away. That branch is for a
	// caller that legitimately has no lock; a release gate is not one.
	if len(lockBytes) == 0 {
		return fmt.Errorf("verify: read lock: w17/lock.yaml is empty — a project being verified has a lock; regenerate it with `w17ctl codegen`")
	}
	addr, err := core.ResolveConsoleAddr(console)
	if err != nil {
		return err
	}
	cl, conn, err := core.DialCodegen(addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	dctx, dcancel := core.ClientCtx()
	view, err := cl.DescribeLock(dctx, &codegenpb.DescribeLockRequest{Lock: lockBytes})
	dcancel()
	if err != nil {
		return fmt.Errorf("verify: describe lock: %w", err)
	}
	protoDir := view.GetProtoDir()

	// T2-5 pass #9 (D-F1). A committed lock is verified because it EXISTS, not
	// because the project still declares the surface that produced it. The
	// detector greps for `w17.acl_`, and the generated lock carries that
	// literal in its own header comment — which neither `(w17.lock_checksum)`
	// (sorted NAME=id + reserved) nor the ed25519 signature covers. Editing a
	// comment therefore switched off the drift check AND the signature check
	// for that very file. Worse, the class it switched them off for — a lock
	// whose domain no longer declares a surface — is exactly the one this gate
	// was added to catch, and whose message the ACL verifier already writes.
	hasAcl, hasEventbus := surfacesToVerify(root, protoDir)

	// ACL / eventbus drift needs the proto set uploaded; gather it up front
	// (skipped entirely when the project declares neither surface).
	var files []*codegenpb.ProtoFile
	var goModule string
	if hasAcl || hasEventbus {
		files, err = core.ReadProtoTree(root, protoDir)
		if err != nil {
			return err
		}
		// Plugin-activated projects need the module path server-side so the
		// staged ACL/eventbus verification can expand plugin proto
		// placeholders (mirrors the codegen path).
		goModule = core.ReadGoModule(root, core.DefaultGenDir)
	}

	var checked int
	var errs []error

	// The lock signature is always checked server-side — the client holds
	// no verifier key (public-split boundary §4). Ship the raw committed
	// bytes (already read above); a hand-edited / tampered / unsigned lock
	// surfaces as drift.
	fmt.Fprintln(out, "verifying lock signature…")
	checked++
	{
		ctx, cancel := core.ClientCtx()
		res, verr := cl.VerifyLock(ctx, &codegenpb.VerifyLockRequest{Lock: lockBytes})
		cancel()
		if e := verifyErr(res, verr); e != nil {
			errs = append(errs, fmt.Errorf("lock: %w", e))
		}
	}

	if hasAcl {
		fmt.Fprintln(out, "verifying ACL lock…")
		checked++
		ctx, cancel := core.ClientCtx()
		res, verr := cl.VerifyAcl(ctx, &codegenpb.VerifyRequest{Files: files, GoModule: goModule})
		cancel()
		if e := verifyErr(res, verr); e != nil {
			errs = append(errs, fmt.Errorf("acl: %w", e))
		}
	}
	if hasEventbus {
		fmt.Fprintln(out, "verifying eventbus lock…")
		checked++
		ctx, cancel := core.ClientCtx()
		res, verr := cl.VerifyEventbus(ctx, &codegenpb.VerifyRequest{Files: files, GoModule: goModule})
		cancel()
		if e := verifyErr(res, verr); e != nil {
			errs = append(errs, fmt.Errorf("eventbus: %w", e))
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("verify: drift detected — re-run `w17ctl codegen` and commit the locks: %w", errors.Join(errs...))
	}
	fmt.Fprintf(out, "verify: ok (%d lock(s) in sync with proto)\n", checked)
	return nil
}

// verifyErr turns a Verify* RPC outcome into an error: a transport
// error surfaces as-is; an ok=false result becomes the drift message.
func verifyErr(res *codegenpb.VerifyResult, rpcErr error) error {
	if rpcErr != nil {
		return rpcErr
	}
	if !res.GetOk() {
		return errors.New(res.GetMessage())
	}
	return nil
}

// surfacesToVerify decides which drift checks run. Extracted so the decision
// is testable on its own: it lives inside a method that needs a console
// connection, and a rule this load-bearing should not be reachable only
// through a dialled RPC (T2-5 pass #13, D13-4).
//
// BOTH arms trigger on the committed LOCK as well as on the declared surface.
// The eventbus arm did not, so an orphaned eventbus lock was never verified —
// the same defect the ACL arm was hardened against in pass #9, surviving on
// the sibling that was not revisited at the time.
func surfacesToVerify(root, protoDir string) (hasAcl, hasEventbus bool) {
	hasAcl = core.HasAclLockFile(root, protoDir) || core.DetectAclSurface(root, protoDir)
	hasEventbus = core.HasEventbusLockFile(root, protoDir) || core.DetectEventbusSurface(root, protoDir)
	return hasAcl, hasEventbus
}
