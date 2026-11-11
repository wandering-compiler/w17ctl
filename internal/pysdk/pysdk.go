// Package pysdk names where a Python business tier's image installs the
// Python business SDK from: the sdk repository's `py/` directory, at the git
// ref the project's SDK version maps to. It is shared by the generator that
// renders the image (genmain.RenderBusinessPythonDockerfile) and by the thin
// client's `codegen --sdk-version` (W17_SDK_VERSION), which re-pins a
// rendered image the way it re-pins the generated go.mods — so the two can
// never spell the requirement differently. Standard library and x/mod only.
//
// The client cannot import srcgo (its production closure is public sdk/go
// only), so it links a verbatim copy, w17ctl/internal/pysdk/pysdk.go: edit
// this file, run `make sync-sdk-pins-w17ctl`, and `make check-sdk-pins-w17ctl` (a
// prerequisite of publish-w17ctl-check) refuses the two drifting apart.
package pysdk

import (
	"regexp"

	"golang.org/x/mod/module"
)

// Repo is the repository the image installs the Python
// business SDK from: its `py/` directory, at the ref Ref
// derives from the project's SDK version. The Python and Go SDKs share one
// version line (lock sdk_version): every sdk release tags `py/vX` beside
// `go/vX`.
const Repo = "https://github.com/wandering-compiler/sdk"

// Ref is the git ref pip installs the Python business SDK
// at for an sdk/go version: a release (`v0.1.0-rc.53`) is the tag
// `py/v0.1.0-rc.53` the sdk release cuts beside `go/v0.1.0-rc.53`; a
// pseudo-version (`v0.1.0-rc.53.0.20261108120000-0123456789ab`) names a commit
// no tag carries — an SDK under review — so the ref is that commit. The same
// derivation the server renders the Dockerfile with and `w17ctl codegen
// --sdk-version` re-pins it with (Repin).
func Ref(version string) string {
	if module.IsPseudoVersion(version) {
		if rev, err := module.PseudoVersionRev(version); err == nil && rev != "" {
			return rev
		}
	}
	return "py/" + version
}

// PipSpec is the pip requirement the image installs the
// Python business SDK from, for an sdk/go version.
func PipSpec(version string) string {
	return "git+" + Repo + "@" + Ref(version) + "#subdirectory=py"
}

// pipSpecRe matches the requirement PipSpec
// writes, whatever the ref.
var pipSpecRe = regexp.MustCompile(`git\+` + regexp.QuoteMeta(Repo) + `@[^"#\s]+#subdirectory=py`)

// Repin rewrites the SDK requirement of a rendered Python
// business Dockerfile to another sdk/go version — the Python half of
// `w17ctl codegen --sdk-version` (W17_SDK_VERSION), which re-pins the
// generated go.mods the same way after the run. It reports whether the file
// carried the requirement: a co-dev image installs the checkout's sdk/py and
// has none.
func Repin(dockerfile []byte, version string) ([]byte, bool) {
	if !pipSpecRe.Match(dockerfile) {
		return dockerfile, false
	}
	spec := PipSpec(version)
	return pipSpecRe.ReplaceAllLiteral(dockerfile, []byte(spec)), true
}
