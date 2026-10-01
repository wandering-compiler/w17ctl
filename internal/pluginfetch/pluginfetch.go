// Package pluginfetch materialises one plugin, at one published version, out
// of a git repository.
//
// It is TRANSPORT, deliberately and only. The client is allowed to fetch —
// that is the same class of work as dialling the console or writing files to
// disk — but every decision about what a plugin MEANS stays on the console:
// which version satisfies a constraint, whether the manifest is valid, how the
// tree is staged into a project. See the public-split architecture doc; the
// line this package must not cross is doing compiler work on the way past.
//
// The two checks it does make are not that line. Both are about whether the
// bytes on disk are the ones the caller asked for, which is the question a
// fetch is responsible for answering:
//
//   - the manifest must name the plugin the tag names, and the version the tag
//     names. `publish-plugins.sh` refuses to cut a disagreeing tag, but a repo
//     this client did not publish never went through that gate, and the
//     manifest is what travels on into the project once the tag is gone.
//   - the caller gets the resolved commit SHA and the content digest, so the
//     lock can pin something a moved tag cannot change.
package pluginfetch

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	"github.com/wandering-compiler/w17ctl/internal/pluginrender"
	"github.com/wandering-compiler/sdk/go/tooling/plugindigest"
)

// Source names one published plugin version.
type Source struct {
	// Repo is any git URL: the internal plugins repo, or a third party's.
	Repo string
	// Plugin is the catalogue name, which is also the top-level directory in
	// the repo and the tag prefix — one string, so the three cannot drift.
	Plugin string
	// Version is the tag's version part, `v`-prefixed (`v0.1.0-rc.1`).
	//
	// Empty when Commit is set. Exactly one of the two names what to fetch,
	// and validate() enforces that rather than picking a winner: a Source
	// carrying both would resolve to whichever the code happened to read
	// first, and the lock would then record a pin nobody asked for.
	Version string
	// Commit is a full 40-hex commit SHA, for fetching a plugin that has NOT
	// been released.
	//
	// The reason this exists: a plugin's author tree leads its published
	// releases by design, so the only way to try a fix before it is tagged was
	// to tag it. That turns every experiment into a release candidate in a
	// registry other people read. A commit is the honest way to say "this exact
	// tree, which nobody has published".
	//
	// FULL sha, never abbreviated: an abbreviation is not a stable name for a
	// tree (it can become ambiguous as the repository grows), and this value
	// goes into a LOCK, where it has to keep meaning the same bytes years
	// later. Abbreviation is a display concern — see shortSHA in cmd/plugin.
	Commit string
}

// Tag is the release tag a Source names, or "" when it names a commit.
func (s Source) Tag() string {
	if s.Version == "" {
		return ""
	}
	return s.Plugin + "/" + s.Version
}

// Ref is what git is asked for, and what the lock records: the release tag, or
// the commit SHA.
func (s Source) Ref() string {
	if s.Commit != "" {
		return s.Commit
	}
	return s.Tag()
}

// pinnedToCommit reports whether a recorded ref is a commit rather than a tag.
//
// Exported through [PinnedToCommit] because `plugin update` has to know: an
// update that resolved "the latest release" for a commit-pinned plugin would
// silently throw away the unreleased tree somebody installed on purpose.
func pinnedToCommit(ref string) bool { return isFullSHA(ref) }

// PinnedToCommit reports whether a lock's recorded git ref names a commit.
func PinnedToCommit(ref string) bool { return pinnedToCommit(ref) }

// isFullSHA reports whether s is a full 40-character hex commit id.
func isFullSHA(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// Fetched is what the lock records: where the tree landed, which release it
// was, and the two values that make the pin immutable.
//
// Ref and Version are carried back rather than left for the caller to rebuild
// from its own inputs — a caller that resolved "latest" does not otherwise
// know what it got, and one that rebuilt the string could rebuild it wrong.
type Fetched struct {
	Dir     string
	Repo    string
	Ref     string
	Version string
	SHA     string
	// Digest covers the tree AS LANDED — after stripSrcSuffix. It is the lock's
	// pin, and `w17ctl verify` recomputes it over the installed tree, so it has
	// to describe what is on disk in a consumer's project.
	Digest string
	// PublishedDigest covers the tree AS PUBLISHED — before stripSrcSuffix, so
	// `.go.src` still carries its suffix and the plugin's own tests are still
	// present.
	//
	// THE SIGNATURE IS ABOUT THIS ONE, and the two are never equal: the strip
	// renames every `.src` and deletes every test, so a tree that has been
	// through it hashes to something the publisher never signed. `plugin sign`
	// digests the registry's bytes (it runs on the rendered tree, before any
	// consumer has touched it), which makes this the only digest a claim can be
	// checked against.
	//
	// Keeping both is not redundancy — they answer two different questions.
	// "Are these the bytes that were released?" is a signature question and only
	// the published form can answer it. "Is this working tree still the one that
	// was installed?" is a pin question and only the landed form can.
	PublishedDigest string
	// Signature is the contents of SignatureFile, VERBATIM, when the published
	// tree carries one — and empty otherwise.
	//
	// Not trimmed, not parsed. The file is a structured body the console wrote
	// and the console reads; a client that tidied it would be a third opinion
	// about a format only two ends need to agree on. Verifying an artefact is a
	// console job (public-split §4).
	//
	// Empty is the ordinary case today — every plugin published before signing
	// existed is unsigned — and the console reports that differently from a
	// signature that fails to verify.
	Signature string
}

// SignatureFile is where a published plugin carries its signature, beside the
// manifest the signature is partly about.
//
// Excluded from the digest by construction: the digest covers the tree the
// signature is ABOUT, so a file whose content depends on the digest cannot be
// part of it.
const SignatureFile = "plugin.sig"

// Fetch materialises src into dest and returns what it resolved.
//
// dest is created; if it exists it is REPLACED, because a fetch that merged
// into a previous version's leftovers would produce a tree matching neither
// digest.
func Fetch(ctx context.Context, src Source, dest string) (Fetched, error) {
	if err := src.validate(); err != nil {
		return Fetched{}, err
	}

	work, err := os.MkdirTemp("", "w17-pluginfetch-*")
	if err != nil {
		return Fetched{}, fmt.Errorf("plugin fetch: %w", err)
	}
	defer func() { _ = os.RemoveAll(work) }()

	if err := materialise(ctx, src, work); err != nil {
		return Fetched{}, err
	}

	sha, err := git(ctx, work, "rev-parse", "HEAD")
	if err != nil {
		return Fetched{}, fmt.Errorf("plugin fetch: resolving the commit: %w", err)
	}

	tree := filepath.Join(work, src.Plugin)
	if fi, serr := os.Stat(tree); serr != nil || !fi.IsDir() {
		return Fetched{}, fmt.Errorf(
			"plugin fetch: %s carries no %s/ directory at %s — the tag exists but the plugin does not",
			src.Repo, src.Plugin, src.Tag())
	}

	version, err := checkManifest(tree, src)
	if err != nil {
		return Fetched{}, err
	}

	if err := os.RemoveAll(dest); err != nil {
		return Fetched{}, fmt.Errorf("plugin fetch: clearing %s: %w", dest, err)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return Fetched{}, fmt.Errorf("plugin fetch: %w", err)
	}
	// A rename keeps the tree out of a half-written state on the way in, and
	// leaves the clone's .git behind in the work dir where it belongs.
	if err := os.Rename(tree, dest); err != nil {
		if err := copyTree(tree, dest); err != nil {
			return Fetched{}, fmt.Errorf("plugin fetch: placing %s: %w", dest, err)
		}
	}
	published, digest, err := unpackDigests(dest)
	if err != nil {
		return Fetched{}, err
	}
	// Read before the digest is trusted for anything: a missing file is the
	// unsigned case and not an error, so ReadFile's error is deliberately
	// dropped rather than surfaced as a fetch failure.
	sig, _ := os.ReadFile(filepath.Join(dest, SignatureFile))

	return Fetched{
		Dir: dest, Repo: src.Repo, Ref: src.Ref(), Version: version,
		SHA: sha, Digest: digest, PublishedDigest: published, Signature: string(sig),
	}, nil
}

// materialise puts the repository's tree for src into work.
//
// Two shapes, because git offers no single one that takes either name:
//
//   - a TAG is `clone --branch`, which is one round trip and the path every
//     released install has always taken;
//   - a COMMIT cannot be cloned by name at all (`--branch` takes refs, and a
//     SHA is not a ref), so it is `init` + `fetch <sha>` + `checkout
//     FETCH_HEAD`. That needs the server to allow fetching an arbitrary
//     reachable object — GitHub does; a server that does not says so, and the
//     refusal below repeats it rather than guessing.
//
// Blobless + sparse in both: one repository holds every plugin, and a consumer
// that wants `auth` has no use for the rest of the tree or for any history.
func materialise(ctx context.Context, src Source, work string) error {
	if src.Commit == "" {
		if out, err := git(ctx, "", "clone", "--quiet", "--depth", "1",
			"--branch", src.Tag(), "--filter=blob:none", "--sparse", src.Repo, work); err != nil {
			return fmt.Errorf(
				"plugin fetch: %s %s is not published at %s (%w)\n\n"+
					"  why: a plugin version is a git TAG, so a pin that resolves to nothing is a\n"+
					"       version that was never released — or was released somewhere else.\n%s",
				src.Plugin, src.Version, src.Repo, err, indent(out))
		}
		if out, err := git(ctx, work, "sparse-checkout", "set", src.Plugin); err != nil {
			return fmt.Errorf("plugin fetch: narrowing to %s: %w\n%s", src.Plugin, err, indent(out))
		}
		return nil
	}

	// A blobless clone of the whole COMMIT GRAPH, then a checkout — not
	// `fetch <sha>`, which asks the server for one object by id.
	//
	// That shortcut is cheaper and conditional: it needs
	// `uploadpack.allowReachableSHA1InWant`, which is OFF in git by default.
	// GitHub happens to allow it, so a test over a local repository would pass
	// (local transport skips the check entirely) while some other host refused —
	// a guard proven on the wrong side of the wire.
	//
	// So: unconditional. `--filter=blob:none` keeps it cheap by leaving the file
	// contents on the server until the checkout asks for the ones it needs, and
	// dropping `--depth` is what makes an arbitrary commit reachable at all —
	// a depth-1 clone contains one commit, which is almost never the one asked
	// for.
	if out, err := git(ctx, "", "clone", "--quiet", "--filter=blob:none",
		"--sparse", "--no-checkout", src.Repo, work); err != nil {
		return fmt.Errorf(
			"plugin fetch: %s could not be read (%w)\n\n"+
				"  why: a commit install needs the repository's commit graph to find the sha.\n%s",
			src.Repo, err, indent(out))
	}
	// Sparse BEFORE the checkout, so the narrow set is what lands rather than
	// what gets pruned afterwards.
	if out, err := git(ctx, work, "sparse-checkout", "set", src.Plugin); err != nil {
		return fmt.Errorf("plugin fetch: narrowing to %s: %w\n%s", src.Plugin, err, indent(out))
	}
	if out, err := git(ctx, work, "checkout", "--quiet", src.Commit); err != nil {
		return fmt.Errorf(
			"plugin fetch: commit %s is not in %s (%w)\n\n"+
				"  why: the repository was read, so this is the sha and not the access — the commit\n"+
				"       does not exist there, or exists only in a fork or an unpushed branch.\n"+
				"  fix: `git rev-parse` it against the repository you are naming, or install a\n"+
				"       published tag instead.\n%s",
			shortID(src.Commit), src.Repo, err, indent(out))
	}
	return nil
}

func (s Source) validate() error {
	switch {
	case s.Repo == "":
		return fmt.Errorf("plugin fetch: no repository given")
	case s.Plugin == "":
		return fmt.Errorf("plugin fetch: no plugin name given")
	case s.Version == "" && s.Commit == "":
		return fmt.Errorf("plugin fetch: no version or commit given for %s", s.Plugin)
	case s.Version != "" && s.Commit != "":
		// Refused rather than resolved in favour of either. A Source carrying
		// both would fetch whichever the code read first and record the other
		// in the lock — a pin that names bytes nobody fetched.
		return fmt.Errorf(
			"plugin fetch: %s names both version %s and commit %s — one or the other",
			s.Plugin, s.Version, shortID(s.Commit))
	}
	// A name is a catalogue key and a directory, never a path: refusing
	// separators here is what stops a crafted pin from writing outside the
	// destination, and the refusal names the rule rather than the symptom.
	if path.Clean(s.Plugin) != s.Plugin || path.Base(s.Plugin) != s.Plugin {
		return fmt.Errorf("plugin fetch: %q is not a plugin name (names carry no path separators)", s.Plugin)
	}
	if s.Commit != "" {
		// A full sha, checked here rather than left to git. An abbreviation
		// resolves fine today and is not a stable name for a tree — this value
		// is going into a lock, where it must keep meaning the same bytes after
		// the repository has grown enough to make the prefix ambiguous.
		if !isFullSHA(s.Commit) {
			return fmt.Errorf(
				"plugin fetch: %q is not a full commit id — 40 lowercase hex characters\n\n"+
					"  why: the sha goes into w17/lock.yaml as the pin. An abbreviation can become\n"+
					"       ambiguous as the repository grows, so it is not a name a lock can keep.",
				s.Commit)
		}
		return nil
	}
	if !strings.HasPrefix(s.Version, "v") {
		return fmt.Errorf(
			"plugin fetch: version %q must start with 'v' (%s/v%s, not %s/%s)",
			s.Version, s.Plugin, s.Version, s.Plugin, s.Version)
	}
	return nil
}

// shortID abbreviates a sha for a MESSAGE. Never for anything stored.
func shortID(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// checkManifest is the consuming half of the guard publish-plugins.sh applies
// when cutting a tag. A third party's repo never met that gate.
// It also returns the version to record.
//
// For a TAG install that is src.Version and the manifest must agree with it.
// For a COMMIT install there is no tag to agree with, so the manifest is the
// only thing that knows, and its value is what travels into the lock — which is
// the same rule as before, just with the cross-check absent because there is
// nothing to cross-check against.
func checkManifest(tree string, src Source) (string, error) {
	body, err := os.ReadFile(filepath.Join(tree, "plugin.yaml"))
	if err != nil {
		return "", fmt.Errorf("plugin fetch: %s at %s has no plugin.yaml — every plugin declares its own identity: %w",
			src.Plugin, src.Ref(), err)
	}
	name := manifestField(string(body), "name")
	version := manifestField(string(body), "version")

	if name != src.Plugin {
		return "", fmt.Errorf(
			"plugin fetch: %s declares name: %s — the ref and the plugin disagree about what this is",
			src.Ref(), name)
	}
	if version == "" {
		return "", fmt.Errorf(
			"plugin fetch: %s ships a manifest with no version\n\n"+
				"  why: the manifest is what travels on into the project, where no ref is around to\n"+
				"       ask. A tree that cannot say which version it is cannot be pinned.",
			src.Ref())
	}
	if src.Commit != "" {
		// Deliberately NOT compared to anything. An unreleased tree usually
		// declares the version it is heading FOR, which is by definition not
		// published yet — refusing that would refuse every commit install,
		// which is the point of the feature.
		return "v" + version, nil
	}
	if "v"+version != src.Version {
		return "", fmt.Errorf(
			"plugin fetch: %s ships a manifest saying version: %s\n\n"+
				"  why: the manifest is what travels on into the project, where no tag is around to\n"+
				"       ask. A tree that disagrees with the tag it was published under is a version\n"+
				"       nobody downstream can trust.",
			src.Tag(), version)
	}
	return src.Version, nil
}

// manifestVersion is the version a LOCAL tree declares, with no tag to check it
// against.
//
// Deliberately not `checkManifest`: that function's whole job is to catch a tree
// disagreeing with the tag it was published under, and a local tree has no tag.
// Refusing a version here because it is unpublished would refuse every dev
// install, which is the case this exists for — an author works on the version
// they are heading FOR.
//
// A missing version is still a refusal: the manifest is what travels into the
// project, where nothing is around to ask.
func manifestVersion(tree string) (string, error) {
	body, err := os.ReadFile(filepath.Join(tree, "plugin.yaml"))
	if err != nil {
		return "", fmt.Errorf("plugin install: reading the manifest: %w", err)
	}
	version := manifestField(string(body), "version")
	if version == "" {
		return "", fmt.Errorf(
			"plugin install: this tree's plugin.yaml declares no version\n" +
				"  why: the manifest is what travels on into the project, where no tag is around " +
				"to ask. A tree that cannot say which version it is cannot be recorded.")
	}
	return "v" + version, nil
}

// manifestField reads one top-level scalar. Deliberately not a YAML parser:
// plugin.yaml's top level is flat, and the console is the side that validates
// the manifest properly — this is only the identity check a fetch owes its
// caller. Keeping it small is also what keeps this package transport.
func manifestField(body, key string) string {
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, key+":") {
			continue
		}
		v := strings.TrimSpace(strings.TrimPrefix(line, key+":"))
		return strings.Trim(v, `"'`)
	}
	return ""
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	// A fetch must never stop on a credential prompt: in CI there is nobody to
	// answer it, and the run would hang rather than fail.
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func indent(s string) string {
	if s == "" {
		return ""
	}
	return "  " + strings.ReplaceAll(s, "\n", "\n  ")
}

// unpackDigests takes BOTH digests of a placed tree and unpacks it, in the one
// order that is correct: published first, then strip, then landed.
//
// # Why this is a function and not four lines at each call site
//
// It was four lines at each call site, and the order was wrong in both. Signing
// digests the tree as the registry serves it; the client digested it after
// unpacking and sent THAT to be verified. Two honest computations over two
// different trees, so no signed release could ever verify — and nothing caught
// it, because the publish side and the install side each tested their own digest
// against itself. The ordering is the whole defect, so it gets one name, one
// place, and a test that reads it (TestUnpackDigestsOrder).
//
// Returns (published, landed): published covers the bytes a registry serves and
// is what a signature claims; landed covers what ends up in the project and is
// what the lock pins. They are never equal for a rendered tree.
func unpackDigests(dest string) (published, landed string, err error) {
	published, err = plugindigest.Of(dest)
	if err != nil {
		return "", "", err
	}
	if err = stripSrcSuffix(dest); err != nil {
		return "", "", err
	}
	landed, err = plugindigest.Of(dest)
	if err != nil {
		return "", "", err
	}
	return published, landed, nil
}

// stripSrcSuffix renames `x.go.src` → `x.go` across the placed tree, and DROPS
// the plugin's own tests.
//
// A plugin repository publishes its Go files INERT: a live `.go` (let alone a
// `go.mod`) under `<proto_dir>/plugins/<name>/src` would make the plugin a
// module inside the consumer's own build, which it is not — its sources are
// staged into a service bundle with their import paths rewritten. The suffix
// is how the published form says "data, not code", and stripping it here is
// the same unpacking the catalogue path has always done on the client side.
//
// # Why the tests are dropped here rather than never published
//
// They belong in the PUBLIC repository: a plugin is the one part of this system
// somebody outside the team reads, and a plugin whose tests are invisible asks
// to be trusted on its word. So the publish render carries them.
//
// They do not belong in a CONSUMER's tree. Nothing there runs them — the sources
// are staged into a bundle with rewritten import paths, and a test staged along
// with them would reference symbols the staging never placed. And the bulk is
// not marginal: measured on this repo, tests are +121% on auth's published Go
// (7,961 lines against 6,532), +89% on payment, +76% on agent. That is ten
// thousand lines committed into a consumer's repository for code they cannot
// run.
//
// The digest is computed AFTER this, over what actually landed. That keeps the
// property the pin exists for — two consumers on one release get one digest, and
// an edited tree still fails `verify` — while the value no longer equals the
// published tree's own digest. It never described the published bytes anyway: the
// `.src` suffixes are stripped before it is taken.
func stripSrcSuffix(root string) error {
	var (
		renames [][2]string
		drops   []string
	)
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		switch {
		case strings.HasSuffix(p, "_test.go.src"), strings.HasSuffix(p, "_test.go"):
			// Both spellings: `.src` is what the render publishes, and a bare
			// `_test.go` would arrive from a repository that publishes its author
			// tree directly. Neither has a job here.
			drops = append(drops, p)
		case strings.HasSuffix(p, ".src"):
			renames = append(renames, [2]string{p, strings.TrimSuffix(p, ".src")})
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("plugin fetch: %w", err)
	}
	for _, p := range drops {
		if err := os.Remove(p); err != nil {
			return fmt.Errorf("plugin fetch: dropping %s: %w", filepath.Base(p), err)
		}
	}
	for _, r := range renames {
		if err := os.Rename(r[0], r[1]); err != nil {
			return fmt.Errorf("plugin fetch: %w", err)
		}
	}
	return nil
}

// copyTree is the fallback for a rename across filesystems (a temp dir on
// another mount than the project).
func copyTree(src, dst string) error {
	return filepath.Walk(src, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(src, p)
		if rerr != nil {
			return rerr
		}
		target := filepath.Join(dst, rel)
		if fi.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		body, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		return os.WriteFile(target, body, fi.Mode().Perm())
	})
}

// FromDir materialises a plugin from a LOCAL author tree into dest.
//
// The dev-loop counterpart of Fetch, and it goes the long way round on purpose:
// it RENDERS the author tree into its published form first, then unpacks that,
// so what lands in dest is the tree a consumer receives rather than the tree
// the author happens to have on disk.
//
// ⚠️ THOSE DIFFER, and installing the author tree directly would test bytes
// nobody is ever served. The Go goes inert (a live `.go` under a consumer's
// proto dir makes the plugin a module in their build), `src/gen/pb` is left out
// because it is regenerated, and the tests travel into the published form and
// are dropped again on the way here — so an author tree copied straight in
// would carry tests that staging cannot place and pb that the next codegen
// replaces.
//
// No commit and no repository: there is nothing to pin. What comes back has a
// Version from the manifest and a Digest over what landed, which is what the
// install records, and `source: local` is how the lock says a pin was not
// possible rather than pretending to one.
func FromDir(src, dest string) (Fetched, error) {
	abs, err := filepath.Abs(src)
	if err != nil {
		return Fetched{}, fmt.Errorf("plugin install: %w", err)
	}
	if _, serr := os.Stat(filepath.Join(abs, "plugin.yaml")); serr != nil {
		return Fetched{}, fmt.Errorf(
			"plugin install: %s carries no plugin.yaml — that file is what makes a "+
				"directory a plugin: %w", abs, serr)
	}

	work, err := os.MkdirTemp("", "w17-pluginlocal-*")
	if err != nil {
		return Fetched{}, fmt.Errorf("plugin install: %w", err)
	}
	defer func() { _ = os.RemoveAll(work) }()

	published := filepath.Join(work, "published")
	if _, rerr := pluginrender.Plugin(abs, published); rerr != nil {
		return Fetched{}, rerr
	}

	if err := os.RemoveAll(dest); err != nil {
		return Fetched{}, fmt.Errorf("plugin install: clearing %s: %w", dest, err)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return Fetched{}, fmt.Errorf("plugin install: %w", err)
	}
	if err := os.Rename(published, dest); err != nil {
		if cerr := copyTree(published, dest); cerr != nil {
			return Fetched{}, fmt.Errorf("plugin install: placing %s: %w", dest, cerr)
		}
	}
	// The same unpack a repository install runs, so the two paths cannot
	// disagree about what a consumer's tree looks like — or about which digest
	// the signature is checked against.
	pubDigest, digest, err := unpackDigests(dest)
	if err != nil {
		return Fetched{}, err
	}

	version, err := manifestVersion(dest)
	if err != nil {
		return Fetched{}, err
	}
	sig, _ := os.ReadFile(filepath.Join(dest, SignatureFile))

	return Fetched{
		Dir: dest, Version: version, Digest: digest, PublishedDigest: pubDigest, Signature: string(sig),
	}, nil
}

// NameInDir is the plugin name a local tree declares.
//
// The manifest names it, never the path. A directory can be called anything —
// `./plugins/payment`, `./p`, `.` — and a tree's identity is the one thing it is
// required to state, so taking the basename would let a rename on disk change
// what gets installed and what the lock records.
func NameInDir(dir string) (string, error) {
	body, err := os.ReadFile(filepath.Join(dir, "plugin.yaml"))
	if err != nil {
		return "", fmt.Errorf("%s carries no plugin.yaml — that file is what makes a "+
			"directory a plugin: %w", dir, err)
	}
	name := manifestField(string(body), "name")
	if name == "" {
		return "", fmt.Errorf("%s/plugin.yaml declares no name", dir)
	}
	return name, nil
}
