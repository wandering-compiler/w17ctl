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

	"regexp"

	"golang.org/x/mod/semver"

	"github.com/wandering-compiler/w17ctl/internal/core"
	codegenpb "github.com/wandering-compiler/sdk/go/pb/w17compiler"
)

// Run recomputes the committed ACL + eventbus locks server-side and
// reports drift. `console` is the resolved --console flag value (empty
// → core resolves it from the lock / compile-time default). Progress
// lines are written to `out`. Returns a non-zero (error) result on
// drift so a CI step can fail the build.
func Run(out io.Writer, console string, allowStalePins bool) error {
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
	// Ask for the compiler's pin floor along with the lock view — the
	// release gate below compares the generated bundles against it. Nothing
	// about the caller's own pins is needed: a floor is absolute, and a
	// project pinning ABOVE it is not stale.
	view, err := cl.DescribeLock(dctx, &codegenpb.DescribeLockRequest{
		Lock: lockBytes, WantCompilerPins: true,
	})
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

	// Ecosystem pins. A generated bundle's go.mod pins the libraries the
	// generated code links, and the console just said which versions this
	// compiler ships with. A bundle BELOW that floor was last regenerated by
	// an older compiler: internally consistent, it builds, and it may name a
	// version the compiler has since moved off for a CVE.
	//
	// This is the release gate, so it REFUSES. `w17ctl codegen` already
	// REPORTS a pin move to the developer who regenerates; what was missing
	// was anything between a stale bundle and production.
	//
	// Above the floor is fine, and deliberately: a project is free to pin
	// ahead, and the generator's own merge takes the higher of the two.
	if !allowStalePins {
		if cp := view.GetCompilerPins(); cp == nil {
			fmt.Fprintln(out, "ecosystem pins: skipped (the console returned no floor)")
		} else {
			stale, scanned := stalePinBundles(root, view.GetServicesDir(), cp)
			checked++
			fmt.Fprintf(out, "verifying ecosystem pins… (%d bundle(s))\n", scanned)
			for _, b := range stale {
				errs = append(errs, fmt.Errorf(
					"%s pins %s %s, below the %s this compiler ships — regenerate with `w17ctl codegen` "+
						"(or pass --allow-stale-pins to ship it anyway)",
					b.path, b.module, b.got, b.want))
			}
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

// stalePinBundle is one generated go.mod pinning a library below the floor
// the compiler ships with.
type stalePinBundle struct {
	path   string
	module string
	got    string
	want   string
}

// stalePinBundles walks the project's generated services and collects every
// bundle pinning one of the compiler's ecosystem libraries BELOW the version
// the compiler ships.
//
// The four libraries are the ones the in-repo tripwire
// (TestGeneratedBundles_DoNotPinOlderThanCompiler) checks, and this is
// deliberately the same comparison: that test is the oracle that caught the
// original finding, and it can only run in this repo because it reads the
// compiler's embedded manifest. Here the manifest travels instead.
//
// A missing require is not a finding. Bundle kinds import different libraries
// — only some touch nats or redis — so absence means "this bundle does not
// link it", never "this bundle is behind".
//
// The client classifies nothing and reproduces nothing: it reads versions out
// of go.mod and compares them with versions the console supplied.
func stalePinBundles(root, servicesDir string, floor *codegenpb.DepVersions) ([]stalePinBundle, int) {
	want := map[string]string{
		"google.golang.org/grpc":       floor.GetGrpc(),
		"google.golang.org/protobuf":   floor.GetProtobuf(),
		"github.com/nats-io/nats.go":   floor.GetNatsGo(),
		"github.com/redis/go-redis/v9": floor.GetGoRedis(),
	}
	dir := filepath.Join(root, filepath.FromSlash(servicesDir))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, 0
	}
	var out []stalePinBundle
	scanned := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		gomod := filepath.Join(dir, e.Name(), "go.mod")
		body, rerr := os.ReadFile(gomod)
		if rerr != nil {
			continue
		}
		scanned++
		rel, relErr := filepath.Rel(root, gomod)
		if relErr != nil {
			rel = gomod
		}
		for mod, floorVer := range want {
			if floorVer == "" {
				continue
			}
			got := requireVersionOf(body, mod)
			// A pseudo-version or a replace placeholder is not comparable
			// and not evidence; semver.Compare would order it arbitrarily.
			if got == "" || !semver.IsValid(got) || !semver.IsValid(floorVer) {
				continue
			}
			if semver.Compare(got, floorVer) < 0 {
				out = append(out, stalePinBundle{
					path: filepath.ToSlash(rel), module: mod, got: got, want: floorVer,
				})
			}
		}
	}
	return out, scanned
}

// requireVersionOf pulls one module's version out of go.mod bytes. Matches the
// require line whether it sits in a block or on its own.
func requireVersionOf(body []byte, module string) string {
	re := regexp.MustCompile(`(?m)^\s*(?:require\s+)?` + regexp.QuoteMeta(module) + `\s+(v\S+)`)
	m := re.FindSubmatch(body)
	if m == nil {
		return ""
	}
	return string(m[1])
}
