package plugin

import (
	"context"
	"errors"
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
//
// # The commit form
//
//	github.com/wandering-compiler/plugins#auth@3f2a1c…   (40 hex)
//
// `@` rather than another `/`, so the two forms are told apart by the
// SEPARATOR and never by guessing at the shape of what follows. A plugin name
// is one path segment and a tag version starts with `v`, so neither form can be
// read as the other.
//
// It exists because a plugin's author tree leads its published releases by
// design, and the only way to try an unreleased fix was to tag it — turning
// every experiment into a release candidate in a registry other people read.
type gitSpec struct {
	Repo    string
	Plugin  string
	Version string
	// Commit names an UNRELEASED tree: `<repo>#<plugin>@<sha>`.
	//
	// Empty for the ordinary tag form. Exactly one of the two is set.
	Commit string
}

// Tag is the release tag this spec names, or "" when it names a commit.
func (g gitSpec) Tag() string {
	if g.Commit != "" {
		return ""
	}
	return g.Plugin + "/" + g.Version
}

// Ref is what git is asked for and what the lock records.
func (g gitSpec) Ref() string {
	if g.Commit != "" {
		return g.Commit
	}
	return g.Tag()
}

// source is the fetch coordinates this spec resolves to.
func (g gitSpec) source() pluginfetch.Source {
	return pluginfetch.Source{
		Repo: g.Repo, Plugin: g.Plugin, Version: g.Version, Commit: g.Commit,
	}
}

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

	// The commit form first: `@` is not part of any tag we publish, so its
	// presence decides which form this is before anything else is parsed.
	if name, commit, isCommit := strings.Cut(frag, "@"); isCommit {
		switch {
		case name == "" || commit == "":
			return gitSpec{}, fmt.Errorf(
				"%q: the commit form is `<plugin>@<sha>` — for example "+
					"`%s#auth@%s`", s, repo, strings.Repeat("0", 40))
		case strings.Contains(name, "/"):
			return gitSpec{}, fmt.Errorf(
				"%q: %q is not a plugin name — a name is a single segment, so the commit form is "+
					"`<plugin>@<sha>` and carries no slash", s, name)
		case !pluginfetch.PinnedToCommit(commit):
			return gitSpec{}, fmt.Errorf(
				"%q: %q is not a full commit id — 40 lowercase hex characters\n\n"+
					"  why: the sha is what lands in w17/lock.yaml as the pin, and an abbreviation\n"+
					"       stops naming one tree as the repository grows.\n"+
					"  fix: `git rev-parse <ref>` in the plugins repository prints the full id.",
				s, commit)
		}
		return gitSpec{Repo: normaliseRepo(repo), Plugin: name, Commit: commit}, nil
	}

	plugin, version, ok := strings.Cut(frag, "/")
	if !ok || plugin == "" || version == "" {
		return gitSpec{}, fmt.Errorf(
			"%q: the part after `#` is the release TAG, `<plugin>/<version>` — for example "+
				"`%s#auth/v0.1.0-rc.1`; for an UNRELEASED tree it is `<plugin>@<sha>`", s, repo)
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
func installIntent(name, version string, git *gitSpec, local bool, fetched pluginfetch.Fetched) *codegenpb.InstallPluginIntent {
	if local {
		// ⚠️ NO PIN, and saying so is the point. A local tree has no commit and
		// no repository; recording `internal` would claim it came from the
		// registry, and recording `git` with empty coordinates would be a pin
		// that resolves to nothing. `local` is the lock admitting that
		// provenance was not available — which is true, and which is what lets
		// `plugin update` and `verify` treat it as the dev tree it is instead
		// of silently reaching for a registry that never served it.
		return &codegenpb.InstallPluginIntent{Name: name, Version: version, Source: "local"}
	}
	if git == nil {
		return &codegenpb.InstallPluginIntent{Name: name, Version: version, Source: "internal"}
	}
	return &codegenpb.InstallPluginIntent{
		Name: name, Version: version, Source: "git",
		Git: &codegenpb.PluginGitPin{
			Repo: git.Repo,
			// The tag, or the sha for an unreleased tree. `Commit` carries the
			// resolved sha either way, so a commit pin records the same value
			// twice — deliberately: `Ref` is WHAT WAS ASKED FOR and `Commit` is
			// what it resolved to, and for a commit those are one thing. That is
			// also how `update` recognises a commit pin later.
			Ref:    git.Ref(),
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

	// A COMMIT pin is never moved implicitly.
	//
	// `update` with no target means "the highest published release", and for a
	// commit-pinned plugin that is precisely the tree somebody chose NOT to be
	// on. Worse, it usually reads as an upgrade: an unreleased tree declares the
	// version it is heading for, so `rc.8` on disk against `rc.7` published
	// resolves "latest" to rc.7 and the fetch silently REPLACES the tree under
	// test with an older released one, leaving a lock that says rc.7 and a
	// developer wondering where their fix went.
	//
	// So it refuses and names both ways out. `--all` reaches here too, which is
	// the case that matters most: it is the sweep nobody is watching.
	if existing.Git != nil && pluginfetch.PinnedToCommit(existing.Git.Ref) && to == "" {
		return nil, pluginfetch.Fetched{}, &commitPinnedError{
			Name: name, Repo: repo, Commit: existing.Git.Ref,
		}
	}

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

// commitPinnedError says an implicit update cannot move a commit-pinned plugin.
//
// A TYPE rather than a message, because the two callers need different things
// from the same fact and the difference is the whole point:
//
//   - `update <name>` names one plugin, so this is a REFUSAL. Skipping what
//     somebody asked for by name and reporting success is the shape this repo
//     keeps finding — a step that does nothing and says it worked.
//   - `update --all` is a sweep, so this is a SKIP. The first cut returned a
//     plain error here and the loop returned it, which aborted the whole run:
//     one commit-pinned plugin and nothing else got updated either. And `--to`
//     is refused WITH `--all`, so every sweep hit it. The plugin is what cannot
//     be upgraded, not the command.
//
// The `url:` source in the same loop already had exactly this shape, which is
// where the wording below comes from.
type commitPinnedError struct {
	Name   string
	Repo   string
	Commit string
}

func (e *commitPinnedError) Error() string {
	return fmt.Sprintf(
		"plugin update: %s is pinned to commit %s, not to a release\n\n"+
			"  why: an update with no target moves a plugin to the highest PUBLISHED release.\n"+
			"       A commit has no place in that ordering — nothing can say which releases are\n"+
			"       above or below it — and an unreleased tree usually declares the version it is\n"+
			"       heading for, so this would read as an upgrade while silently replacing the\n"+
			"       code under test with an older released one.\n"+
			"  fix: `--to <version>` to join the release line, or `plugin install\n"+
			"       %s#%s@<sha>` to move to another commit.",
		e.Name, shortSHA(e.Commit), e.Repo, e.Name)
}

// skipLine is what a sweep prints instead of stopping.
func (e *commitPinnedError) skipLine() string {
	return fmt.Sprintf(
		"plugin update: skipping %s (pinned to commit %s; a commit has no place in the release "+
			"ordering, so there is no \"newest\" to move to)",
		e.Name, shortSHA(e.Commit))
}

// asCommitPinned reports whether err is the commit-pin refusal.
func asCommitPinned(err error) (*commitPinnedError, bool) {
	var e *commitPinnedError
	ok := errors.As(err, &e)
	return e, ok
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
