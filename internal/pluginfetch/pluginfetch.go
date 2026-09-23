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
	Version string
}

// Tag is the ref a release is published under.
func (s Source) Tag() string { return s.Plugin + "/" + s.Version }

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
	Digest  string
}

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

	// Blobless + sparse: one repo holds every plugin, and a consumer that
	// wants auth has no use for the rest of the tree or for any history.
	if out, err := git(ctx, "", "clone", "--quiet", "--depth", "1",
		"--branch", src.Tag(), "--filter=blob:none", "--sparse", src.Repo, work); err != nil {
		return Fetched{}, fmt.Errorf(
			"plugin fetch: %s %s is not published at %s (%w)\n\n"+
				"  why: a plugin version is a git TAG, so a pin that resolves to nothing is a\n"+
				"       version that was never released — or was released somewhere else.\n%s",
			src.Plugin, src.Version, src.Repo, err, indent(out))
	}
	if out, err := git(ctx, work, "sparse-checkout", "set", src.Plugin); err != nil {
		return Fetched{}, fmt.Errorf("plugin fetch: narrowing to %s: %w\n%s", src.Plugin, err, indent(out))
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

	if err := checkManifest(tree, src); err != nil {
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
	if err := stripSrcSuffix(dest); err != nil {
		return Fetched{}, err
	}

	digest, err := plugindigest.Of(dest)
	if err != nil {
		return Fetched{}, err
	}
	return Fetched{Dir: dest, Repo: src.Repo, Ref: src.Tag(), Version: src.Version, SHA: sha, Digest: digest}, nil
}

func (s Source) validate() error {
	switch {
	case s.Repo == "":
		return fmt.Errorf("plugin fetch: no repository given")
	case s.Plugin == "":
		return fmt.Errorf("plugin fetch: no plugin name given")
	case s.Version == "":
		return fmt.Errorf("plugin fetch: no version given for %s", s.Plugin)
	}
	// A name is a catalogue key and a directory, never a path: refusing
	// separators here is what stops a crafted pin from writing outside the
	// destination, and the refusal names the rule rather than the symptom.
	if path.Clean(s.Plugin) != s.Plugin || path.Base(s.Plugin) != s.Plugin {
		return fmt.Errorf("plugin fetch: %q is not a plugin name (names carry no path separators)", s.Plugin)
	}
	if !strings.HasPrefix(s.Version, "v") {
		return fmt.Errorf(
			"plugin fetch: version %q must start with 'v' (%s/v%s, not %s/%s)",
			s.Version, s.Plugin, s.Version, s.Plugin, s.Version)
	}
	return nil
}

// checkManifest is the consuming half of the guard publish-plugins.sh applies
// when cutting a tag. A third party's repo never met that gate.
func checkManifest(tree string, src Source) error {
	body, err := os.ReadFile(filepath.Join(tree, "plugin.yaml"))
	if err != nil {
		return fmt.Errorf("plugin fetch: %s/%s has no plugin.yaml — every plugin declares its own identity: %w",
			src.Plugin, src.Version, err)
	}
	name := manifestField(string(body), "name")
	version := manifestField(string(body), "version")

	if name != src.Plugin {
		return fmt.Errorf(
			"plugin fetch: %s declares name: %s — the tag and the plugin disagree about what this is",
			src.Tag(), name)
	}
	if "v"+version != src.Version {
		return fmt.Errorf(
			"plugin fetch: %s ships a manifest saying version: %s\n\n"+
				"  why: the manifest is what travels on into the project, where no tag is around to\n"+
				"       ask. A tree that disagrees with the tag it was published under is a version\n"+
				"       nobody downstream can trust.",
			src.Tag(), version)
	}
	return nil
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

// stripSrcSuffix renames `x.go.src` → `x.go` across the placed tree.
//
// A plugin repository publishes its Go files INERT: a live `.go` (let alone a
// `go.mod`) under `<proto_dir>/plugins/<name>/src` would make the plugin a
// module inside the consumer's own build, which it is not — its sources are
// staged into a service bundle with their import paths rewritten. The suffix
// is how the published form says "data, not code", and stripping it here is
// the same unpacking the catalogue path has always done on the client side.
func stripSrcSuffix(root string) error {
	var renames [][2]string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(p, ".src") {
			renames = append(renames, [2]string{p, strings.TrimSuffix(p, ".src")})
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("plugin fetch: %w", err)
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
