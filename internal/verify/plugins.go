package verify

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/wandering-compiler/w17ctl/internal/lockfile"
	"github.com/wandering-compiler/sdk/go/tooling/plugindigest"
)

// verifyPluginTrees checks each git-sourced plugin's committed tree against
// the digest its pin records.
//
// # Why this check exists at all
//
// A plugin tree stays COMMITTED in a consumer's repository (decided
// 2026-09-23: the per-activation copies are compiler output and already
// ignored, and what is tracked is one project-level tree that does not grow
// with domain count). Keeping it committed buys offline builds and costs the
// version guarantee — the pin becomes a label beside bytes nobody checks, and
// a tree edited in place is indistinguishable from the one the version
// promised.
//
// The digest is what closes that, and this is where it can actually run: at
// the files. The console never sees the plugin's whole tree — only the protos
// an upload carries — so a server-side comparison would cover a fraction and
// report confidence over the rest. Here the check is whole, and it sits in the
// release gate, which is the moment the question is asked: nothing broken or
// tampered reaches production.
//
// # What it does not claim
//
// A client can be modified to skip its own checks, so this is not a defence
// against a hostile operator; it is a defence against drift, a bad merge, and
// an edit someone forgot. The value it compares against is signed — the
// console signs the lock — so the pin itself cannot be quietly rewritten to
// match a doctored tree without breaking the signature this same gate checks.
func verifyPluginTrees(root, protoDir string, plugins []lockfile.Plugin) []error {
	var errs []error
	for _, p := range plugins {
		// Absent is not empty. An `internal` plugin has no git provenance, and
		// a lock written before the pin existed carries none either — neither
		// is a tree that failed a check, so neither is reported as one.
		if p.Source != "git" || p.Git == nil || p.Git.Digest == "" {
			continue
		}
		dir := filepath.Join(root, protoDir, "plugins", p.Name)
		if _, err := os.Stat(dir); err != nil {
			errs = append(errs, fmt.Errorf(
				"plugin %s: the lock records it installed at %s (%s) but the tree is not there\n"+
					"  fix: `w17ctl plugin install %s`, or remove the lock entry if it should not be installed",
				p.Name, p.Git.Ref, shortRef(p.Git.Commit), p.Name))
			continue
		}
		got, err := plugindigest.Of(dir)
		if err != nil {
			errs = append(errs, fmt.Errorf("plugin %s: %w", p.Name, err))
			continue
		}
		if got != p.Git.Digest {
			errs = append(errs, fmt.Errorf(
				"plugin %s: the committed tree is not the one %s publishes\n"+
					"  pinned:  %s\n"+
					"  on disk: %s\n"+
					"  why: the pin records the digest of the tree that was fetched, so a difference\n"+
					"       means the tree was edited in place, merged badly, or is a leftover of\n"+
					"       another version — a plugin is generated output and is not edited here\n"+
					"  fix: `w17ctl plugin update %s --to %s` to restore it, or publish the change\n"+
					"       as a release and pin that",
				p.Name, p.Git.Ref, p.Git.Digest, got, p.Name, versionOf(p.Git.Ref)))
		}
	}
	return errs
}

// shortRef abbreviates a commit for a message. Display only — the lock keeps
// the whole thing.
func shortRef(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// versionOf pulls the version half out of a `<plugin>/<version>` tag, for the
// `--to` the fix line suggests.
func versionOf(ref string) string {
	for i := len(ref) - 1; i >= 0; i-- {
		if ref[i] == '/' {
			return ref[i+1:]
		}
	}
	return ref
}

// hasPinnedPlugins reports whether anything here was checkable at all, so a
// project with no git-sourced plugins does not get a line claiming a check it
// never ran — the count this gate prints is how a reader knows what was
// covered.
func hasPinnedPlugins(plugins []lockfile.Plugin) bool {
	for _, p := range plugins {
		if p.Source == "git" && p.Git != nil && p.Git.Digest != "" {
			return true
		}
	}
	return false
}
