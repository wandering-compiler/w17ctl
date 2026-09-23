package pluginfetch

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ErrNoReleases is returned when a registry ANSWERED and publishes no release
// of the plugin asked for.
//
// A sentinel rather than a message a caller greps, because the two failures a
// resolve can have want different advice and must not be confused: a registry
// nobody can reach is fixed with access or a URL, while one that simply has no
// such plugin is fixed with another name. A CLI that appended "here is how to
// list what exists" to both would be telling someone with a broken remote to
// go and read a list they also cannot fetch.
var ErrNoReleases = errors.New("no releases published")

// LatestVersion returns the highest version published for one plugin in a
// repository, PRERELEASES INCLUDED.
//
// That last part is the whole reason this exists rather than a call into an
// off-the-shelf resolver. Every plugin release is an `-rc.N` today and will be
// until w17 ships as a service, and the standard rule — the one semver states
// and `go get @latest` implements — is that a prerelease is never selected
// while any release exists. Under that rule today's catalogue resolves
// correctly by accident, and the day someone cuts one clean `v0.1.0` every
// later candidate becomes invisible. The rule is written down here instead.
//
// # Why the client resolves this
//
// Deciding which version satisfies a constraint is the console's job, and this
// looks like that but is not: it is an ordering over OUR OWN tag format, not
// knowledge about schemas or compilation. It has to run here for a plainer
// reason too — the repository may be a third party's, reachable only with the
// developer's own credentials, so a rule that cannot run where the data is
// would not be a rule at all. What the console still decides is everything
// about the tree that comes back.
func LatestVersion(ctx context.Context, repo, plugin string) (string, error) {
	if repo == "" || plugin == "" {
		return "", fmt.Errorf("plugin versions: repository and plugin are both required")
	}
	out, err := git(ctx, "", "ls-remote", "--tags", repo, "refs/tags/"+plugin+"/*")
	if err != nil {
		return "", fmt.Errorf("plugin versions: listing %s releases in %s: %w\n%s", plugin, repo, err, indent(out))
	}
	versions := parseRefLines(out, plugin)
	if len(versions) == 0 {
		return "", fmt.Errorf(
			"plugin versions: %s publishes no releases of %s: %w\n\n"+
				"  why: a plugin version is a git tag named `<plugin>/v<version>`, so a repository\n"+
				"       with none has nothing this project could pin", repo, plugin, ErrNoReleases)
	}
	sort.Slice(versions, func(i, j int) bool { return CompareVersions(versions[i], versions[j]) < 0 })
	return versions[len(versions)-1], nil
}

// parseRefLines pulls the version halves out of `git ls-remote --tags` output.
// Peeled refs (`…^{}`) name the same release twice and are dropped.
func parseRefLines(out, plugin string) []string {
	seen := map[string]bool{}
	var versions []string
	for _, line := range strings.Split(out, "\n") {
		_, ref, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if !ok {
			continue
		}
		ref = strings.TrimSuffix(ref, "^{}")
		v := strings.TrimPrefix(ref, "refs/tags/"+plugin+"/")
		if v == ref || !strings.HasPrefix(v, "v") || seen[v] {
			continue
		}
		seen[v] = true
		versions = append(versions, v)
	}
	return versions
}

// CompareVersions orders two `vX.Y.Z[-prerelease]` strings: -1, 0 or +1.
//
// Semver's precedence rules, kept to the part this project uses:
//
//   - numeric comparison of major, minor and patch;
//   - a version WITH a prerelease is lower than the same version without one,
//     so promoting `v0.1.0-rc.2` to `v0.1.0` supersedes its own candidates;
//   - prerelease identifiers compare dot-separated part by part, numerically
//     when both sides are numeric. That is why the tag format is `rc.2` and
//     not `rc2`: glued, the comparison is lexical and `rc10` sorts BELOW
//     `rc2`, which nobody notices until the tenth candidate.
//
// An unparseable version sorts below a parseable one rather than erroring:
// this orders what a repository already contains, and one hand-made tag should
// not make every real release unreachable.
func CompareVersions(a, b string) int {
	na, pa := splitVersion(a)
	nb, pb := splitVersion(b)
	for i := 0; i < 3; i++ {
		if na[i] != nb[i] {
			if na[i] < nb[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case pa == "" && pb == "":
		return 0
	case pa == "": // a is the release, b a candidate for it
		return 1
	case pb == "":
		return -1
	}
	return comparePrerelease(pa, pb)
}

// splitVersion returns the numeric triple and the prerelease tail. Missing or
// unparseable components are -1, which sorts below any real version.
func splitVersion(v string) ([3]int, string) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	core, pre, _ := strings.Cut(v, "-")
	parts := strings.Split(core, ".")
	out := [3]int{-1, -1, -1}
	for i := 0; i < 3 && i < len(parts); i++ {
		n, err := strconv.Atoi(parts[i])
		if err != nil {
			return [3]int{-1, -1, -1}, pre
		}
		out[i] = n
	}
	return out, pre
}

func comparePrerelease(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		an, aerr := strconv.Atoi(as[i])
		bn, berr := strconv.Atoi(bs[i])
		switch {
		case aerr == nil && berr == nil:
			if an != bn {
				if an < bn {
					return -1
				}
				return 1
			}
		case aerr == nil: // numeric identifiers rank below alphanumeric ones
			return -1
		case berr == nil:
			return 1
		default:
			if as[i] != bs[i] {
				if as[i] < bs[i] {
					return -1
				}
				return 1
			}
		}
	}
	// A longer identifier list wins when every shared part is equal.
	switch {
	case len(as) < len(bs):
		return -1
	case len(as) > len(bs):
		return 1
	}
	return 0
}

// PublishedPlugins names every plugin a registry publishes, read from its tags.
//
// A registry's inventory IS its tag namespace: `<plugin>/v<version>`. Nothing
// else in the repository says which directories are plugins — a directory with
// no release is not something a project can pin, so it is not published.
func PublishedPlugins(ctx context.Context, repo string) ([]string, error) {
	if repo == "" {
		return nil, fmt.Errorf("plugin registry: no repository given")
	}
	out, err := git(ctx, "", "ls-remote", "--tags", repo)
	if err != nil {
		return nil, fmt.Errorf("plugin registry: listing %s: %w\n%s", repo, err, indent(out))
	}
	seen := map[string]bool{}
	var names []string
	for _, line := range strings.Split(out, "\n") {
		_, ref, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if !ok {
			continue
		}
		rest := strings.TrimPrefix(strings.TrimSuffix(ref, "^{}"), "refs/tags/")
		name, version, ok := strings.Cut(rest, "/")
		if !ok || name == "" || !strings.HasPrefix(version, "v") || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}
