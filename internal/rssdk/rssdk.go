// Package rssdk names where a Rust business tier's image takes the Rust
// business SDK from: the sdk repository's `rs/w17-business` directory (the
// runtime crate `w17-business` and the build helper `w17-business-build`),
// at the git ref the project's SDK version maps to. It is shared by the
// generator that renders the image (genmain.RenderBusinessRustDockerfile) and
// by the thin client's `codegen` (`--sdk-version` re-pins a rendered image;
// every run compares the author's Cargo.lock with the image's ref), so the
// two can never spell the requirement differently. Standard library and
// x/mod only.
//
// The client cannot import srcgo (its production closure is public sdk/go
// only), so it links a verbatim copy, w17ctl/internal/rssdk/rssdk.go: edit
// this file, run `make sync-sdk-pins-w17ctl`, and `make check-sdk-pins-w17ctl` (a
// prerequisite of publish-w17ctl-check) refuses the two drifting apart.
//
// There is no package registry: the author's Cargo.toml names the SDK as a
// git dependency at the tag `rs/<sdk_version>`, and Cargo.lock pins it. A
// release image builds exactly that lock (`--locked`) after checking it names
// the tag; an SDK that is not a release (a pseudo-version, co-development) is
// served to Cargo from a local repository instead, tagged with the ref the
// lock names, since a Cargo `[patch]` cannot stand in for a tag that does not
// exist yet.
package rssdk

import (
	"bufio"
	"bytes"
	"fmt"
	"regexp"
	"strings"

	"golang.org/x/mod/module"
)

// Repo is the repository the SDK's crates come from: the URL an author's
// Cargo.toml names (`git = "https://github.com/wandering-compiler/sdk"`). The
// Rust, TypeScript, Python and Go SDKs share one version line (lock
// sdk_version): every sdk release tags `rs/vX` beside `go/vX`, `py/vX` and
// `ts/vX`.
const Repo = "https://github.com/wandering-compiler/sdk"

// Dir is the SDK's directory inside Repo: the runtime crate, and the
// workspace whose member `build/` is the build helper.
const Dir = "rs/w17-business"

// Crate and BuildCrate are the package names of the runtime crate and of
// the build helper an author's build.rs calls. Both are git dependencies of
// the author's crate, both at the same tag.
const (
	Crate      = "w17-business"
	BuildCrate = "w17-business-build"
)

// Crates are the two packages whose source Cargo.lock must name.
var Crates = []string{Crate, BuildCrate}

// RefArg is the Dockerfile build argument the ref is written into. One line
// carries it, so Repin rewrites exactly that line, and a build by hand can
// override it (`--build-arg W17_SDK_RS_REF=<ref>`).
const RefArg = "W17_SDK_RS_REF"

// TagPrefix is the prefix of the sdk repository's Rust tags.
const TagPrefix = "rs/"

// Ref is the git ref the image takes the Rust business SDK at for an sdk/go
// version: a release (`v0.1.0-rc.69`) is the tag `rs/v0.1.0-rc.69` the sdk
// release cuts beside `go/v0.1.0-rc.69`; a pseudo-version
// (`v0.1.0-rc.69.0.20261110120000-0123456789ab`) names a commit no tag
// carries — an SDK under review — so the ref is that commit (abbreviated, as
// the pseudo-version holds it; the Dockerfile falls back to a full fetch for
// it, since a server answers a shallow fetch only for a full name).
func Ref(version string) string {
	if module.IsPseudoVersion(version) {
		if rev, err := module.PseudoVersionRev(version); err == nil && rev != "" {
			return rev
		}
	}
	return TagPrefix + version
}

// IsTag reports whether a ref is a release tag (`rs/<version>`), the one
// mode in which the image builds the author's Cargo.lock as it is.
func IsTag(ref string) bool { return strings.HasPrefix(ref, TagPrefix) }

// refArgRe matches the ARG line the Dockerfile carries the ref on, whatever
// the ref.
var refArgRe = regexp.MustCompile(`(?m)^ARG ` + RefArg + `=([^\s"'$\\]+)$`)

// ArgLine is the Dockerfile line that pins the ref for an sdk/go version.
func ArgLine(version string) string {
	return "ARG " + RefArg + "=" + Ref(version)
}

// RefOf returns the ref a rendered Rust business Dockerfile pins, and whether
// it carries one: a co-dev image builds the checkout's rs/w17-business and has
// none, and neither has a Go, Python or TypeScript image.
func RefOf(dockerfile []byte) (string, bool) {
	m := refArgRe.FindSubmatch(dockerfile)
	if m == nil {
		return "", false
	}
	return string(m[1]), true
}

// Repin rewrites the SDK ref of a rendered Rust business Dockerfile to
// another sdk/go version — the Rust half of `w17ctl codegen --sdk-version`
// (W17_SDK_VERSION). The image decides its mode from the ref at build time
// (a tag: the author's Cargo.lock, checked and --locked; a commit: the SDK
// served from a local repository), so the one line is all a re-pin changes.
// It reports whether the file carried the ref.
func Repin(dockerfile []byte, version string) ([]byte, bool) {
	if !refArgRe.Match(dockerfile) {
		return dockerfile, false
	}
	return refArgRe.ReplaceAllLiteral(dockerfile, []byte(ArgLine(version))), true
}

// Source is the `source` Cargo.lock records for a crate of the sdk repository
// at a tag: `git+<Repo>?tag=<tag>`, the commit after `#` aside. Cargo writes
// the tag percent-encoded (`rs%2Fv0.1.0-rc.69`); NormalizeSource undoes that.
func Source(tag string) string { return "git+" + Repo + "?tag=" + tag }

// NormalizeSource is a Cargo.lock `source` value reduced to what CheckCargoLock
// compares: the pinned commit (`#<sha>`) dropped, a `.git` suffix on the
// repository dropped (Cargo treats both spellings as one repository), and the
// percent-encoded `/` of the tag decoded.
func NormalizeSource(src string) string {
	if i := strings.IndexByte(src, '#'); i >= 0 {
		src = src[:i]
	}
	src = strings.Replace(src, Repo+".git?", Repo+"?", 1)
	return strings.ReplaceAll(src, "%2F", "/")
}

// LockedSources reads a Cargo.lock and returns, for each package name, the
// `source` of every entry with that name ("" for an entry without one: a
// path dependency or the workspace's own crate).
func LockedSources(lock []byte) map[string][]string {
	out := map[string][]string{}
	name, inPackage := "", false
	flush := func(src string) {
		if inPackage && name != "" {
			out[name] = append(out[name], src)
		}
	}
	var src string
	sc := bufio.NewScanner(bytes.NewReader(lock))
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "[[package]]":
			flush(src)
			name, src, inPackage = "", "", true
		case strings.HasPrefix(line, "[") && line != "[[package]]":
			flush(src)
			name, src, inPackage = "", "", false
		case inPackage && strings.HasPrefix(line, "name = "):
			name = strings.Trim(strings.TrimPrefix(line, "name = "), `"`)
		case inPackage && strings.HasPrefix(line, "source = "):
			src = strings.Trim(strings.TrimPrefix(line, "source = "), `"`)
		}
	}
	flush(src)
	return out
}

// CheckCargoLock holds an author's Cargo.lock to the tag the image builds:
// both crates of the SDK (Crates) are locked, every entry of each from the
// sdk repository at `tag`. A release image runs the same check before
// `cargo build --locked` (it refuses), and `w17ctl codegen` on the
// developer's disk (it warns); the error names what the lock says, what it
// should say and the fix.
func CheckCargoLock(lock []byte, tag string) error {
	want := Source(tag)
	sources := LockedSources(lock)
	for _, crate := range Crates {
		got := sources[crate]
		if len(got) == 0 {
			return fmt.Errorf("Cargo.lock has no %s: the crate depends on %s and %s, both at the tag %s of %s\n"+
				"  fix: add both to Cargo.toml (`%s = { git = %q, tag = %q }`, the second under [build-dependencies]), run `cargo update -p %s -p %s`, commit Cargo.lock",
				crate, Crate, BuildCrate, tag, Repo, crate, Repo, tag, Crate, BuildCrate)
		}
		for _, src := range got {
			if NormalizeSource(src) == want {
				continue
			}
			shown := src
			if shown == "" {
				shown = "a path (no source)"
			}
			return fmt.Errorf("Cargo.lock takes %s from %s, but the project's sdk_version is %s (the image builds the SDK at %s)\n"+
				"  fix: set `tag = %q` for %s and %s in Cargo.toml, run `cargo update -p %s -p %s`, commit Cargo.lock",
				crate, shown, strings.TrimPrefix(tag, TagPrefix), want, tag, Crate, BuildCrate, Crate, BuildCrate)
		}
	}
	return nil
}
