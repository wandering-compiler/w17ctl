package codegen

import (
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/mod/modfile"

	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/lockfile"
)

// ProjectSdkVersion is the sdk/go version the project builds against: the
// lock's pin when there is one (codegen writes it into every generated
// module), else what the project's own module requires. "" when neither says
// a real version — a co-dev tree on a local replace, or a fresh project — and
// nothing is compared then.
func ProjectSdkVersion(root string) string {
	// Co-dev: codegen resolves sdk/go through a local checkout and does not
	// floor-check at all, so neither may this — a stale pin in the lock of a
	// co-dev tree is not a build that fails.
	if strings.Trim(os.Getenv("W17_WANDERING_COMPILER_PATH"), "/") != "" {
		return ""
	}
	lk, err := lockfile.Load(filepath.Join(root, "w17", "lock.yaml"))
	modDir := "srcgo"
	if err == nil && lk.GeneratedCode.Stubs != "" {
		modDir = strings.SplitN(filepath.ToSlash(lk.GeneratedCode.Stubs), "/", 2)[0]
	}
	required, replaced := moduleSdk(filepath.Join(root, modDir, "go.mod"))
	if replaced {
		return "" // a local replace: neither the pin nor the require says what builds
	}
	if err == nil && strings.TrimSpace(lk.SdkVersion) != "" {
		return strings.TrimSpace(lk.SdkVersion)
	}
	return required
}

// moduleSdk reads what a go.mod requires sdk/go at, and whether it replaces it
// with a local tree.
func moduleSdk(path string) (required string, replaced bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	f, err := modfile.Parse(path, data, nil)
	if err != nil {
		return "", false
	}
	sdk := core.SdkModuleBase + "/sdk/go"
	for _, rep := range f.Replace {
		if rep.Old.Path == sdk {
			return "", true
		}
	}
	for _, r := range f.Require {
		if r.Mod.Path == sdk {
			return r.Mod.Version, false
		}
	}
	return "", false
}

// MetadataSdkVersion is the PlaceGenerateStart.metadata key that carries the
// sdk/go version the project builds against. The console answers a known
// vulnerability in it with an advisory notice; a console that does not know
// the key ignores it.
const MetadataSdkVersion = "sdk_version"

// buildSdkVersion is the sdk/go version this run's generated code builds
// against: --sdk-version when given, else the project's (ProjectSdkVersion).
func buildSdkVersion(root, override string) string {
	if override != "" {
		return override
	}
	return ProjectSdkVersion(root)
}
