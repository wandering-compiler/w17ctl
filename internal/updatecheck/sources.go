package updatecheck

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/mod/modfile"

	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/lockfile"
	"github.com/wandering-compiler/w17ctl/internal/pluginfetch"
	"github.com/wandering-compiler/w17ctl/internal/sdkupdate"
)

// ProjectSources fills the project half of [Sources] from disk: the SDK the
// project builds against and its installed plugins. The client half (its own
// newest release, which only the installer can answer) and the floor are the
// caller's.
func ProjectSources(root string) Sources {
	return Sources{
		SdkCurrent: ProjectSdkVersion(root),
		SdkLatest: func() (string, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			return sdkupdate.LatestVersion(ctx)
		},
		Plugins: func() ([]Plugin, error) { return installedPlugins(root) },
	}
}

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

// installedPlugins reads the lock's plugins and asks each one's OWN registry
// (the repository its pin records) for the newest release. A plugin installed
// from a directory has no registry and is skipped; a registry that cannot be
// reached is reported, not guessed.
func installedPlugins(root string) ([]Plugin, error) {
	lk, err := lockfile.Load(filepath.Join(root, "w17", "lock.yaml"))
	if err != nil {
		return nil, nil
	}
	var out []Plugin
	var firstErr error
	for _, p := range lk.Plugins {
		if p.Git == nil || p.Git.Repo == "" {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		v, verr := pluginfetch.LatestVersion(ctx, p.Git.Repo, p.Name)
		cancel()
		if verr != nil {
			if firstErr == nil {
				firstErr = verr
			}
			continue
		}
		out = append(out, Plugin{Name: p.Name, Installed: p.Version, Served: strings.TrimPrefix(v, "v")})
	}
	return out, firstErr
}
