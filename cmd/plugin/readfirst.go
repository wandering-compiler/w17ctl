package plugin

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/lockfile"
	"github.com/wandering-compiler/w17ctl/internal/pluginfetch"
)

// readFirst is one CHANGELOG subsection a plugin marks "(read first)": the
// steps an upgrade across that release needs before anything else, such as a
// breaking change to how every client authenticates.
type readFirst struct {
	version string // the release it belongs to, as the CHANGELOG writes it
	heading string // the `### ` line, without the hashes
	body    []string
}

// printReadFirst prints every "read first" subsection of the fetched tree's
// CHANGELOG that an update from `existing` to `to` crosses.
//
// A release that needs reading first used to be noticed only by someone who
// opened the CHANGELOG: `plugin update` printed the new version and nothing
// else, and a consumer who moved across a breaking release learned it from
// compile errors. The fetched tree carries its CHANGELOG, so the sections are
// in hand at the moment they matter.
//
// The release the lock records is excluded, since the project is already on
// it — unless the lock pins a commit: an unreleased tree declares the version
// it was heading for, so a pin taken before that release was tagged is NOT on
// it, and its "read first" applies.
func printReadFirst(name string, existing lockfile.Plugin, to, tree string) {
	data, err := os.ReadFile(filepath.Join(tree, "CHANGELOG.md"))
	if err != nil {
		return // a plugin without a CHANGELOG has nothing to say here
	}
	fromInclusive := existing.Git != nil && pluginfetch.PinnedToCommit(existing.Git.Ref)
	sections := readFirstBetween(string(data), existing.Version, fromInclusive, to)
	for _, s := range sections {
		fmt.Fprintf(core.Stdout, "\nplugin update: %s %s — %s\n", name, s.version, s.heading)
		for _, l := range s.body {
			fmt.Fprintf(core.Stdout, "  %s\n", l)
		}
	}
	if len(sections) > 0 {
		fmt.Fprintf(core.Stdout, "\nplugin update: the above is from %s/CHANGELOG.md; do it before building.\n\n", name)
	}
}

// readFirstBetween returns the "read first" subsections of the releases after
// `from` (or from `from` itself when fromInclusive) up to and including `to`,
// oldest first. Versions are compared as releases (`0.1.0-rc.25`).
func readFirstBetween(changelog, from string, fromInclusive bool, to string) []readFirst {
	inRange := func(v string) bool {
		if to != "" && pluginfetch.CompareVersions("v"+v, "v"+to) > 0 {
			return false
		}
		if from == "" {
			return true
		}
		c := pluginfetch.CompareVersions("v"+v, "v"+from)
		return c > 0 || (fromInclusive && c == 0)
	}
	var out []readFirst
	var version string
	var cur *readFirst
	flush := func() {
		if cur != nil {
			for len(cur.body) > 0 && strings.TrimSpace(cur.body[len(cur.body)-1]) == "" {
				cur.body = cur.body[:len(cur.body)-1]
			}
			out = append(out, *cur)
			cur = nil
		}
	}
	for _, line := range strings.Split(changelog, "\n") {
		switch {
		case strings.HasPrefix(line, "## "):
			flush()
			version = strings.TrimSpace(strings.TrimPrefix(line, "## "))
		case strings.HasPrefix(line, "### "):
			flush()
			heading := strings.TrimSpace(strings.TrimPrefix(line, "### "))
			if strings.Contains(strings.ToLower(heading), "read first") && inRange(version) {
				cur = &readFirst{version: version, heading: heading}
			}
		case cur != nil:
			if len(cur.body) == 0 && strings.TrimSpace(line) == "" {
				continue
			}
			cur.body = append(cur.body, line)
		}
	}
	flush()
	// The CHANGELOG lists the newest release first; an upgrade reads them in
	// the order they happened.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}
