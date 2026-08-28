// Package sdkupdate implements `w17ctl sdk update` — moving a project onto a
// new public sdk/go version.
//
// Why this exists: every other path was closed. The generated bundles are
// pinned by codegen, but codegen deliberately NEVER rewrites the project's
// hand-written module (that file is author-owned), and codegen's pin resolves
// from the version the project already knows — so a project stayed on its
// original pin forever. Editing go.mod by hand doesn't work either: a build
// also needs go.sum hashes, and those can't be produced without a Go
// toolchain. The result was a closed loop — a project literally could not
// accept a new SDK release (e.g. a newly added lib/principal) without running
// local `go`, which consumers are not required to have.
//
// So this does the whole job over plain HTTP against the module proxy: resolve
// the version, fetch the .mod + .zip, compute the go.sum hashes with the same
// library `go` uses (x/mod/sumdb/dirhash), and rewrite every module's go.mod +
// go.sum. No Go toolchain anywhere.
package sdkupdate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
	"golang.org/x/mod/sumdb/dirhash"

	"github.com/wandering-compiler/w17ctl/internal/core"
)

// SdkModule is the public SDK module path this command moves a project onto.
var SdkModule = core.SdkModuleBase + "/sdk/go"

// skipDirs are never walked when looking for project modules.
var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, "dist": true, ".w17": true,
}

// Run bumps every project module that requires the SDK onto version (or the
// proxy's @latest when version is empty), writing go.mod + go.sum.
func Run(stdout io.Writer, root, version string) error {
	root, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	if version != "" && !semver.IsValid(version) {
		return fmt.Errorf("sdk update: %q is not a valid version (want e.g. v0.0.0-20260716201145-36e33cc8168a)", version)
	}
	mods, err := findSdkModules(root, SdkModule)
	if err != nil {
		return err
	}
	if len(mods) == 0 {
		fmt.Fprintf(stdout, "sdk update: no module requires %s — nothing to do\n", SdkModule)
		return nil
	}

	ctx := context.Background()
	if version == "" {
		if version, err = latestVersion(ctx, SdkModule); err != nil {
			return fmt.Errorf("sdk update: %w", err)
		}
	}

	zipHash, modHash, err := moduleHashes(ctx, SdkModule, version)
	if err != nil {
		return fmt.Errorf("sdk update: %w", err)
	}
	if err := verifyAgainstSumDB(ctx, SdkModule, version, zipHash, modHash); err != nil {
		return fmt.Errorf("sdk update: %w", err)
	}

	for _, d := range mods {
		if err := applyToModule(filepath.Join(root, d), SdkModule, version, zipHash, modHash); err != nil {
			return fmt.Errorf("sdk update: %s: %w", d, err)
		}
		fmt.Fprintf(stdout, "  %s\n", filepath.ToSlash(d))
	}
	fmt.Fprintf(stdout, "sdk update: %s → %s in %d module(s)\n", SdkModule, version, len(mods))
	fmt.Fprintf(stdout, "  go.sum hashes computed from the module proxy — no Go toolchain used.\n")
	return nil
}

// applyToModule rewrites one module's go.mod require + go.sum entries.
func applyToModule(dir, mod, ver, zipHash, modHash string) error {
	goModPath := filepath.Join(dir, "go.mod")
	data, err := os.ReadFile(goModPath)
	if err != nil {
		return err
	}
	f, err := modfile.Parse(goModPath, data, nil)
	if err != nil {
		return fmt.Errorf("parse go.mod: %w", err)
	}
	if err := f.AddRequire(mod, ver); err != nil {
		return fmt.Errorf("set require: %w", err)
	}
	f.Cleanup()
	out, err := f.Format()
	if err != nil {
		return fmt.Errorf("format go.mod: %w", err)
	}
	if err := os.WriteFile(goModPath, out, 0o644); err != nil {
		return err
	}

	// go.sum is optional on disk (a module may not have one yet) but required
	// for the build once a require exists, so always end up with one.
	sumPath := filepath.Join(dir, "go.sum")
	prior, err := os.ReadFile(sumPath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.WriteFile(sumPath, []byte(goSumUpsert(string(prior), mod, ver, zipHash, modHash)), 0o644)
}

// goSumUpsert returns go.sum content with mod's entries replaced by the given
// version's hashes, every other line preserved verbatim, output sorted (go.sum
// is sorted, and a stable order keeps regen diffs empty).
//
// Dropping the module's OTHER-version lines is the point: a leftover line for
// the previous version is what makes `go` reject the build with a hash
// mismatch rather than simply ignore it.
func goSumUpsert(prior, mod, ver, zipHash, modHash string) string {
	var lines []string
	for _, ln := range strings.Split(prior, "\n") {
		ln = strings.TrimRight(ln, "\r")
		if strings.TrimSpace(ln) == "" {
			continue
		}
		if fields := strings.Fields(ln); len(fields) >= 1 && fields[0] == mod {
			continue // stale entry for this module — replaced below
		}
		lines = append(lines, ln)
	}
	lines = append(lines,
		mod+" "+ver+" "+zipHash,
		mod+" "+ver+"/go.mod "+modHash,
	)
	sort.Strings(lines)
	return strings.Join(lines, "\n") + "\n"
}

// findSdkModules returns project-relative dirs of every module that requires
// mod WITHOUT a local replace.
//
// Includes the hand-written module on purpose — it is the one codegen refuses
// to touch, and therefore the one that pinned the whole project in place. A
// module carrying a `replace` is co-dev: its resolution is owned by the
// checkout it points at, so bumping a version there would be meaningless
// churn.
func findSdkModules(root, mod string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() != "go.mod" {
			return nil
		}
		data, rErr := os.ReadFile(path)
		if rErr != nil {
			return nil
		}
		f, pErr := modfile.Parse(path, data, nil)
		if pErr != nil {
			return nil
		}
		for _, r := range f.Replace {
			if r.Old.Path == mod {
				return nil // co-dev replace owns resolution
			}
		}
		for _, r := range f.Require {
			if r.Mod.Path == mod {
				rel, _ := filepath.Rel(root, filepath.Dir(path))
				out = append(out, rel)
				return nil
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// proxyBase returns the first usable GOPROXY URL ("" when disabled).
func proxyBase() string {
	gp := strings.TrimSpace(os.Getenv("GOPROXY"))
	if gp == "" {
		gp = "https://proxy.golang.org,direct"
	}
	for _, p := range strings.FieldsFunc(gp, func(r rune) bool { return r == ',' || r == '|' }) {
		switch p = strings.TrimSpace(p); p {
		case "", "off", "direct", "noproxy":
			continue
		}
		return strings.TrimRight(p, "/")
	}
	return ""
}

// proxyGet fetches one proxy endpoint for mod (e.g. "@latest",
// "@v/<ver>.mod") and returns the body.
func proxyGet(ctx context.Context, mod, suffix string) ([]byte, error) {
	base := proxyBase()
	if base == "" {
		return nil, fmt.Errorf("no usable GOPROXY (unset GOPROXY=off, or pass --version and a reachable proxy)")
	}
	esc, err := module.EscapePath(mod)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/"+esc+"/"+suffix, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("proxy %s%s: %s", mod, suffix, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 256<<20))
}

// latestVersion resolves mod@latest via the proxy.
func latestVersion(ctx context.Context, mod string) (string, error) {
	body, err := proxyGet(ctx, mod, "@latest")
	if err != nil {
		return "", err
	}
	var info struct{ Version string }
	if err := json.Unmarshal(body, &info); err != nil {
		return "", fmt.Errorf("decode @latest: %w", err)
	}
	if !semver.IsValid(info.Version) {
		return "", fmt.Errorf("proxy returned an invalid @latest version %q", info.Version)
	}
	return info.Version, nil
}

// moduleHashes downloads the module's .zip + .mod and returns their go.sum
// hashes, computed with the same library `go` uses.
//
// The zip is written to a temp file because dirhash.HashZip needs a path; it
// is removed before returning.
func moduleHashes(ctx context.Context, mod, ver string) (zipHash, modHash string, err error) {
	zipBody, err := proxyGet(ctx, mod, "@v/"+ver+".zip")
	if err != nil {
		return "", "", err
	}
	tmp, err := os.CreateTemp("", "w17-sdk-*.zip")
	if err != nil {
		return "", "", err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(zipBody); err != nil {
		_ = tmp.Close()
		return "", "", err
	}
	if err := tmp.Close(); err != nil {
		return "", "", err
	}
	if zipHash, err = dirhash.HashZip(tmp.Name(), dirhash.Hash1); err != nil {
		return "", "", fmt.Errorf("hash module zip: %w", err)
	}

	modBody, err := proxyGet(ctx, mod, "@v/"+ver+".mod")
	if err != nil {
		return "", "", err
	}
	if modHash, err = goModHash(modBody); err != nil {
		return "", "", err
	}
	return zipHash, modHash, nil
}

// verifyAgainstSumDB cross-checks the hashes we computed against the public
// checksum database, and refuses to write anything on a mismatch.
//
// Two jobs. Security: the hashes come from whatever the proxy served, so
// without this we would faithfully record a tampered module — `go` would then
// trust our go.sum and never re-check. Correctness: a bug in HOW we hash
// produces a valid-looking go.sum that only fails much later, at build time,
// in someone else's project — which is exactly what happened during
// development (the go.mod entry was named wrong; see goModHash).
//
// Skipped when the module is GOPRIVATE/GONOSUMDB/GONOSUMCHECK or GONOSUMDB
// covers it, and when the sumdb is unreachable — a network outage should not
// block a bump whose hashes we can compute ourselves.
func verifyAgainstSumDB(ctx context.Context, mod, ver, zipHash, modHash string) error {
	if sumDBDisabled(mod) {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://sum.golang.org/lookup/"+mod+"@"+ver, nil)
	if err != nil {
		return nil // can't even build the request — not the consumer's problem
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil // sumdb unreachable — proceed on our own hashes
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil // not in the sumdb (private/unpublished) — nothing to compare
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil
	}
	wantZip, wantMod := "", ""
	for _, ln := range strings.Split(string(body), "\n") {
		f := strings.Fields(ln)
		if len(f) != 3 || f[0] != mod {
			continue
		}
		switch f[1] {
		case ver:
			wantZip = f[2]
		case ver + "/go.mod":
			wantMod = f[2]
		}
	}
	if wantZip != "" && wantZip != zipHash {
		return fmt.Errorf("checksum mismatch for %s@%s: proxy content hashes to %s, sum.golang.org says %s — refusing to write go.sum", mod, ver, zipHash, wantZip)
	}
	if wantMod != "" && wantMod != modHash {
		return fmt.Errorf("checksum mismatch for %s@%s/go.mod: computed %s, sum.golang.org says %s — refusing to write go.sum", mod, ver, modHash, wantMod)
	}
	return nil
}

// sumDBDisabled reports whether the checksum database is switched off for mod
// by the standard Go env knobs.
func sumDBDisabled(mod string) bool {
	if os.Getenv("GONOSUMCHECK") != "" || strings.Contains(os.Getenv("GOFLAGS"), "-insecure") {
		return true
	}
	if s := os.Getenv("GONOSUMDB"); s != "" && matchesGlobList(mod, s) {
		return true
	}
	if s := os.Getenv("GOPRIVATE"); s != "" && matchesGlobList(mod, s) {
		return true
	}
	if s := os.Getenv("GONOSUMDB"); s == "*" {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(os.Getenv("GOSUMDB")), "off")
}

// matchesGlobList reports whether mod is covered by a comma-separated
// GOPRIVATE/GONOSUMDB-style pattern list.
func matchesGlobList(mod, list string) bool {
	for _, pat := range strings.Split(list, ",") {
		pat = strings.TrimSpace(pat)
		if pat == "" {
			continue
		}
		if pat == "*" || mod == pat || strings.HasPrefix(mod, strings.TrimSuffix(pat, "/*")+"/") {
			return true
		}
		if ok, _ := path.Match(pat, mod); ok {
			return true
		}
	}
	return false
}

// goModHash returns the go.sum hash of a module's go.mod.
//
// The single hashed entry is named literally "go.mod" — NOT
// "<module>@<version>/go.mod" as the zip names its entries. This mirrors
// cmd/go's goModSum (go/src/cmd/go/internal/modfetch/fetch.go); getting it
// wrong yields a well-formed hash that `go` rejects at build time. Pinned by
// TestGoModHash_MatchesChecksumDB against a real sum.golang.org vector.
func goModHash(data []byte) (string, error) {
	h, err := dirhash.Hash1([]string{"go.mod"}, func(string) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(data)), nil
	})
	if err != nil {
		return "", fmt.Errorf("hash go.mod: %w", err)
	}
	return h, nil
}
