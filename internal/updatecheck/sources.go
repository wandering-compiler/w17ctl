package updatecheck

import (
	"context"
	"path/filepath"
	"strings"
	"time"

	"github.com/wandering-compiler/w17ctl/internal/codegen"
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
			return sdkupdate.LatestForProject(ctx, root)
		},
		Plugins: func() ([]Plugin, error) { return installedPlugins(root) },
	}
}

// ProjectSdkVersion is the sdk/go version the project builds against
// (codegen.ProjectSdkVersion — the same answer codegen sends the console).
func ProjectSdkVersion(root string) string { return codegen.ProjectSdkVersion(root) }

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
