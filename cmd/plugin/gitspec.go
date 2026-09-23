package plugin

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/wandering-compiler/w17ctl/internal/lockfile"

	"github.com/wandering-compiler/w17ctl/internal/pluginfetch"
	codegenpb "github.com/wandering-compiler/sdk/go/pb/w17compiler"
)

// gitSpec is one plugin release named by where it lives: `<repo>#<tag>`, where
// the tag is exactly the ref the repository carries.
//
//	https://github.com/wandering-compiler/plugins#auth/v0.1.0-rc.1
//	git@github.com:wandering-compiler/plugins.git#auth/v0.1.0-rc.1
//	github.com/wandering-compiler/plugins#auth/v0.1.0-rc.1
//
// The fragment is the TAG rather than a plugin-and-version pair spelled some
// other way, and that is the point: what a person types is what
// `git ls-remote --tags` prints, so there is no translation step to get wrong.
// The plugin name is the tag's prefix, so the name and the ref cannot drift
// from each other either.
type gitSpec struct {
	Repo    string
	Plugin  string
	Version string
}

// Tag is the ref this spec names.
func (g gitSpec) Tag() string { return g.Plugin + "/" + g.Version }

// DefaultPluginsRepo is where a bare plugin NAME resolves to.
//
// `w17ctl plugin install auth` is sugar for the organisation's own registry —
// the same mechanism as any other repository, with the URL filled in. It is a
// client-side default, like the compile-time console address, and not a rule
// about plugins: pointing it elsewhere with W17_PLUGINS_REPO changes which
// registry "by name" means, and spelling the repository out always wins.
//
// The console used to answer this question by SERVING the plugin's bytes from
// a catalogue compiled into it. That made the console a file server for
// plugins and a second source of truth beside the registry, which is what this
// replaced.
const DefaultPluginsRepo = "https://github.com/wandering-compiler/plugins"

// pluginsRepo resolves the registry a bare name refers to.
func pluginsRepo() string {
	if v := strings.TrimSpace(os.Getenv("W17_PLUGINS_REPO")); v != "" {
		return v
	}
	return DefaultPluginsRepo
}

// looksLikeGitSpec reports whether the argument is meant as a repository
// rather than a catalogue name. A plugin name is a single path segment, so a
// `#` cannot appear in one — which makes this unambiguous rather than a guess,
// and lets a malformed spec produce a real error instead of "no such plugin".
func looksLikeGitSpec(s string) bool { return strings.Contains(s, "#") }

// parseGitSpec splits `<repo>#<plugin>/<version>`.
func parseGitSpec(s string) (gitSpec, error) {
	s = strings.TrimSpace(s)
	repo, frag, ok := strings.Cut(s, "#")
	if !ok {
		return gitSpec{}, fmt.Errorf("%q names no release — expected <repo>#<plugin>/<version>", s)
	}
	repo, frag = strings.TrimSpace(repo), strings.TrimSpace(frag)
	if repo == "" {
		return gitSpec{}, fmt.Errorf("%q names no repository before the `#`", s)
	}

	plugin, version, ok := strings.Cut(frag, "/")
	if !ok || plugin == "" || version == "" {
		return gitSpec{}, fmt.Errorf(
			"%q: the part after `#` is the release TAG, `<plugin>/<version>` — for example "+
				"`%s#auth/v0.1.0-rc.1`", s, repo)
	}
	// The tag's version half may itself contain slashes only by accident; a
	// plugin name is one segment, so anything further is not a version.
	if strings.Contains(version, "/") {
		return gitSpec{}, fmt.Errorf(
			"%q: %q is not a version — a release tag is `<plugin>/<version>` and the plugin name "+
				"is a single segment", s, version)
	}
	if !strings.HasPrefix(version, "v") {
		return gitSpec{}, fmt.Errorf(
			"%q: version %q must start with `v` (tags are `%s/v%s`)", s, version, plugin, version)
	}

	return gitSpec{Repo: normaliseRepo(repo), Plugin: plugin, Version: version}, nil
}

// normaliseRepo turns a bare host/path into something git can clone.
//
// `github.com/org/repo` is how people write and read a repository, and it is
// what the design conversation called "a github path" — but git needs a
// transport. Anything already carrying a scheme or an scp-style `user@host:`
// is passed through untouched, so this only ever ADDS the obvious default and
// never rewrites an explicit choice.
func normaliseRepo(repo string) string {
	switch {
	case strings.Contains(repo, "://"), strings.Contains(repo, "@"):
		return repo
	case strings.HasPrefix(repo, "/"), strings.HasPrefix(repo, "."):
		// A local path, which the tests and an air-gapped mirror both use.
		return repo
	default:
		return "https://" + repo
	}
}

// installIntent builds the lock edit for an install, carrying provenance only
// when there is provenance to carry.
//
// One function rather than an inline branch because the pairing it encodes is
// a rule the server enforces and the lock validates: `git` and the coordinates
// travel together, and `internal` travels with none. Written twice, the two
// would eventually disagree.
func installIntent(name, version string, git *gitSpec, fetched pluginfetch.Fetched) *codegenpb.InstallPluginIntent {
	if git == nil {
		return &codegenpb.InstallPluginIntent{Name: name, Version: version, Source: "internal"}
	}
	return &codegenpb.InstallPluginIntent{
		Name: name, Version: version, Source: "git",
		Git: &codegenpb.PluginGitPin{
			Repo:   git.Repo,
			Ref:    git.Tag(),
			Commit: fetched.SHA,
			Digest: fetched.Digest,
		},
	}
}

// shortSHA abbreviates for human output only. The lock records the whole
// thing — an abbreviation is a display choice, never a stored one.
func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// updateFromGit refreshes one git-sourced plugin into `staging`, resolving
// which release to move to.
//
// `to` empty means the highest published release, candidates included — see
// pluginfetch.LatestVersion for why that rule is written down rather than
// inherited. The repository comes from the lock, never from a flag: an update
// moves a plugin along ITS OWN release line, and letting a flag change where
// that line lives would make "update" a different operation wearing the same
// name.
func updateFromGit(to, name string, existing lockfile.Plugin, staging string) ([]byte, pluginfetch.Fetched, error) {
	// A plugin installed before the registry existed carries no repository, so
	// the organisation's own is where it came from by definition — `internal`
	// named exactly that. This is what migrates those entries.
	repo := pluginsRepo()
	if existing.Git != nil && existing.Git.Repo != "" {
		repo = existing.Git.Repo
	}
	ctx := context.Background()
	version := to
	if version == "" {
		latest, err := pluginfetch.LatestVersion(ctx, repo, name)
		if err != nil {
			return nil, pluginfetch.Fetched{}, fmt.Errorf("plugin update: %w", err)
		}
		version = latest
	}

	fetched, err := pluginfetch.Fetch(ctx,
		pluginfetch.Source{Repo: repo, Plugin: name, Version: version}, staging)
	if err != nil {
		return nil, pluginfetch.Fetched{}, fmt.Errorf("plugin update: %w", err)
	}
	manifestData, err := os.ReadFile(filepath.Join(staging, "plugin.yaml"))
	if err != nil {
		return nil, pluginfetch.Fetched{}, fmt.Errorf("plugin update: read fetched manifest: %w", err)
	}
	return manifestData, fetched, nil
}

// updateIntent builds one plugin's version bump, carrying the NEW provenance
// for a git plugin.
//
// The pairing is the same rule installIntent encodes, and it matters more
// here: a bump that moved the version and kept the old coordinates would leave
// the lock naming bytes nobody fetched — a pin that still validates and no
// longer describes anything.
func updateIntent(name, version string, isGit bool, existing lockfile.Plugin, fetched pluginfetch.Fetched) *codegenpb.PluginVersion {
	if !isGit {
		return &codegenpb.PluginVersion{Name: name, Version: version, Source: "internal"}
	}
	return &codegenpb.PluginVersion{
		Name: name, Version: version, Source: "git",
		Git: &codegenpb.PluginGitPin{
			Repo:   fetched.Repo,
			Ref:    fetched.Ref,
			Commit: fetched.SHA,
			Digest: fetched.Digest,
		},
	}
}
