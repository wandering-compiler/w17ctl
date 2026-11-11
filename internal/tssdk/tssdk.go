// Package tssdk names where a TypeScript business tier's image builds the
// TypeScript business SDK from: the sdk repository's `ts/business` directory,
// at the git ref the project's SDK version maps to. It is shared by the
// generator that renders the image (genmain.RenderBusinessTypeScriptDockerfile)
// and by the thin client's `codegen --sdk-version` (W17_SDK_VERSION), which
// re-pins a rendered image the way it re-pins the generated go.mods — so the
// two can never spell the requirement differently. Standard library and x/mod
// only.
//
// The client cannot import srcgo (its production closure is public sdk/go
// only), so it links a verbatim copy, w17ctl/internal/tssdk/tssdk.go: edit
// this file, run `make sync-sdk-pins-w17ctl`, and `make check-sdk-pins-w17ctl`
// (a prerequisite of publish-w17ctl-check) refuses the two drifting apart.
//
// There is no package registry and no release asset: npm cannot install a
// subdirectory of a git repository, so the image fetches the ref, builds the
// package (`npm ci && npm run build && npm pack`) and installs the packed
// tarball.
package tssdk

import (
	"regexp"

	"golang.org/x/mod/module"
)

// Repo is the repository the image fetches the TypeScript business SDK from.
// The TypeScript, Python and Go SDKs share one version line (lock
// sdk_version): every sdk release tags `ts/vX` beside `go/vX` and `py/vX`.
const Repo = "https://github.com/wandering-compiler/sdk"

// Dir is the SDK's directory inside Repo.
const Dir = "ts/business"

// PackageName is the npm name of the TypeScript business SDK. The image
// installs it into /app/node_modules, where both the runner and an author's
// `import … from "@w17/business"` resolve it — one instance.
const PackageName = "@w17/business"

// Main is the runner's entry point inside the package: `serve` and `health`.
const Main = "dist/main.js"

// RefArg is the Dockerfile build argument the ref is written into. One line
// carries it, so Repin rewrites exactly that line, and a build by hand can
// override it (`--build-arg W17_SDK_TS_REF=<ref>`).
const RefArg = "W17_SDK_TS_REF"

// Ref is the git ref the image builds the TypeScript business SDK at for an
// sdk/go version: a release (`v0.1.0-rc.53`) is the tag `ts/v0.1.0-rc.53`
// the sdk release cuts beside `go/v0.1.0-rc.53`; a pseudo-version
// (`v0.1.0-rc.53.0.20261108120000-0123456789ab`) names a commit no tag
// carries — an SDK under review — so the ref is that commit (abbreviated, as
// the pseudo-version holds it; the Dockerfile falls back to a full fetch for
// it, since a server answers a shallow fetch only for a full name).
func Ref(version string) string {
	if module.IsPseudoVersion(version) {
		if rev, err := module.PseudoVersionRev(version); err == nil && rev != "" {
			return rev
		}
	}
	return "ts/" + version
}

// refArgRe matches the ARG line the Dockerfile carries the ref on, whatever
// the ref.
var refArgRe = regexp.MustCompile(`(?m)^ARG ` + RefArg + `=[^\s"'$\\]+$`)

// ArgLine is the Dockerfile line that pins the ref for an sdk/go version.
func ArgLine(version string) string {
	return "ARG " + RefArg + "=" + Ref(version)
}

// Repin rewrites the SDK ref of a rendered TypeScript business Dockerfile to
// another sdk/go version — the TypeScript half of `w17ctl codegen
// --sdk-version` (W17_SDK_VERSION). It reports whether the file carried the
// ref: a co-dev image builds the checkout's ts/business and has none.
func Repin(dockerfile []byte, version string) ([]byte, bool) {
	if !refArgRe.Match(dockerfile) {
		return dockerfile, false
	}
	return refArgRe.ReplaceAllLiteral(dockerfile, []byte(ArgLine(version))), true
}
