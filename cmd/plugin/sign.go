package plugin

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/pluginfetch"
	codegenpb "github.com/wandering-compiler/sdk/go/pb/w17compiler"
	"github.com/wandering-compiler/sdk/go/tooling/plugindigest"
)

// SignCmd signs a plugin tree for release: the publish-side half of the check
// `plugin install` runs.
//
// It writes `plugin.sig` beside the manifest and nothing else. Publishing the
// tree, tagging it and pushing it stay where they already are
// (scripts/publish-plugins.sh) — this command's whole job is to turn a
// directory into a signed directory, so it can be run on a mirror, on a
// checkout, or in CI without any of them needing to know how signing works.
//
// ⚠️ THE KEY IS NOT HERE. The digest is computed locally (hashing is not trust
// cryptography, and `plugindigest` is in the public SDK precisely so the
// publisher and every consumer get the same answer); the signature comes from
// the console. Public-split §4 — the same rule that put VerifyLock there, and
// the reason `w17ctl` can be handed to plugin authors at all.
type SignCmd struct {
	Dir     string `arg:"" name:"dir" help:"Plugin tree to sign — the directory holding plugin.yaml."`
	Console string `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console. Optional — falls back to the binary's compile-time default. Holds the signing key; it is never sent anywhere."`
	Print   bool   `name:"print" help:"Write the signature to stdout instead of to plugin.sig. For inspecting what a tree would be signed as, without changing it."`
}

func (c *SignCmd) Run() error {
	cl, conn, err := dialCodegen(c.Console)
	if err != nil {
		return fmt.Errorf("plugin sign: %w", err)
	}
	defer func() { _ = conn.Close() }()
	return runSign(cl, c.Dir, c.Print)
}

// runSign is Run with the console already dialled.
//
// Split out so the sequence can be tested against a recording client: the thing
// worth pinning here is the ORDER — sign, write, re-digest, confirm over the
// RELEASED tree — and dialling is the one part of it that has nothing to do
// with that.
func runSign(cl codegenpb.CodegenServiceClient, dir string, printOnly bool) error {
	manifestPath := filepath.Join(dir, "plugin.yaml")
	manifestData, err := os.ReadFile(manifestPath)
	if err != nil {
		return fmt.Errorf("plugin sign: read manifest: %w\n"+
			"  why: a plugin tree is identified by its plugin.yaml, and the console parses the "+
			"name and version to sign out of those bytes", err)
	}

	// Computed BEFORE anything is written, and over a tree plugindigest
	// already excludes plugin.sig from — the signature cannot be part of what
	// it signs, and re-signing an already-signed tree has to produce the same
	// digest as signing it the first time or a re-release would not verify.
	digest, err := plugindigest.Of(dir)
	if err != nil {
		return fmt.Errorf("plugin sign: %w", err)
	}

	resp, err := cl.SignPluginRelease(context.Background(), &codegenpb.SignPluginReleaseRequest{
		ManifestYaml: manifestData,
		Source:       manifestPath,
		Digest:       digest,
	})
	if err != nil {
		return catalogueError("plugin sign", err)
	}

	if printOnly {
		// Nothing is written, so there is no released tree to check against —
		// the signature is confirmed over the digest as taken, which is all
		// this mode can honestly claim.
		if err := confirmVerifiable(cl, manifestData, manifestPath, digest, resp.GetSignature()); err != nil {
			return fmt.Errorf("plugin sign: %w", err)
		}
		fmt.Fprintln(core.Stdout, resp.GetSignature())
		return nil
	}
	sigPath := filepath.Join(dir, pluginfetch.SignatureFile)
	// Verbatim: the console rendered this file and the console parses it. A
	// client that appended, trimmed or reformatted would be a third opinion
	// about a format only two ends need to agree on.
	if err := os.WriteFile(sigPath, []byte(resp.GetSignature()), 0o644); err != nil {
		return fmt.Errorf("plugin sign: write signature: %w", err)
	}

	// ORDER IS THE POINT OF THE NEXT TWO CALLS, and it was wrong first.
	//
	// The confirmation used to run BEFORE the signature was written, over the
	// digest taken before it — so it could never have caught a digest that
	// moved when the file landed, which is exactly what its comment claimed it
	// caught. It now runs on the released tree, over the digest RECOMPUTED
	// from it, which is the number a consumer arrives at.
	released, err := plugindigest.Of(dir)
	if err != nil {
		return fmt.Errorf("plugin sign: re-reading the signed tree: %w", err)
	}
	if err := digestSurvivedSigning(digest, released); err != nil {
		return fmt.Errorf("plugin sign: %w", err)
	}
	if err := confirmVerifiable(cl, manifestData, manifestPath, released, resp.GetSignature()); err != nil {
		return fmt.Errorf("plugin sign: %w", err)
	}

	// The console echoes what it PARSED, and it is printed rather than
	// assumed: a tree whose manifest says something other than what the
	// publisher meant to release is found here, not at somebody's install.
	fmt.Fprintf(core.Stdout, "plugin sign: %s %s signed under w17 %s\n",
		resp.GetPlugin(), resp.GetVersion(), resp.GetPlatform())
	fmt.Fprintf(core.Stdout, "  digest %s\n", digest)
	fmt.Fprintf(core.Stdout, "  wrote  %s\n", sigPath)
	return nil
}

// digestSurvivedSigning requires the digest taken before the signature was
// written to equal the one the released tree now yields.
//
// The one place the CIRCULARITY is visible. `plugin.sig` did not used to be
// excluded from the digest, and with that exclusion missing every release would
// have been born unverifiable — the digest signed and the digest a consumer
// computes would differ by exactly the file the publisher had just written.
// Always, not occasionally.
//
// It fails at publish rather than at somebody else's install, which is the only
// difference that matters here.
func digestSurvivedSigning(signed, released string) error {
	if signed == released {
		return nil
	}
	return fmt.Errorf(
		"writing the signature changed the tree's digest (%s → %s)\n"+
			"  why: the signature is over the digest, so a digest that moves when the "+
			"signature lands means no consumer can ever recompute what was signed\n"+
			"  fix: %s must be excluded from plugindigest — it is, so if you are reading "+
			"this the exclusion has been undone",
		shortDigest(signed), shortDigest(released), pluginfetch.SignatureFile)
}

func shortDigest(d string) string {
	if len(d) <= 12 {
		return d
	}
	return d[:12]
}

// confirmVerifiable asks the console to verify the signature it has just
// produced, exactly as an install will.
//
// The signature and the released tree are both in hand here and nowhere else,
// so this is the only moment the two halves can be checked against each other.
// A release that fails this is one nobody could have installed, and the
// publisher finds out instead of the consumer.
//
// It goes through `inspectManifest` rather than dialling the RPC itself, and
// the package's one-caller guard is what says so: a second call site is a
// second place that could forget to act on the verdict. This one wants MORE
// than that path enforces — it requires VERIFIED where an install tolerates
// UNSIGNED — so it adds a condition on top rather than opening its own door.
func confirmVerifiable(cl codegenpb.CodegenServiceClient, manifest []byte, source, digest, signature string) error {
	// PublishedDigest and NOT Digest, which stays empty on purpose.
	//
	// The tree in hand at signing time IS the published form — `plugin sign`
	// runs on the rendered tree, before any consumer has unpacked it — so this
	// is the digest a signature is checked against. There is no LANDED tree
	// here at all, and filling `Digest` with the same number would assert one
	// hashes to this, which no one has computed. It would also hide the next
	// mistake of this kind: if something ever reads the landed digest on this
	// path, it should fail loudly rather than agree by coincidence.
	resp, err := inspectManifest(cl, manifest, source, nil, pluginfetch.Fetched{
		PublishedDigest: digest, Signature: signature,
	})
	if err != nil {
		return fmt.Errorf("confirming the signature verifies: %w", err)
	}
	if state := resp.GetSignature().GetState(); state != codegenpb.SignatureVerdict_VERIFIED {
		return fmt.Errorf(
			"the console signed this tree and then would not verify it (%s: %s)\n"+
				"  why: a release in this state installs nowhere, and every consumer sees "+
				"INVALID — which reads as a tree that has been altered\n"+
				"  fix: this is a console problem, not a tree problem — its signing key and "+
				"its trust set disagree",
			state, resp.GetSignature().GetDetail())
	}
	return nil
}
