package docker

import (
	"fmt"
	"os"
	"strings"
)

// DefaultCacheCap is how much build cache one project may keep.
//
// Derived from a measurement, and it is a FLOOR rather than an optimum: on a
// real consumer's project one build's records came to roughly 7 GB — the build
// context upload (~3 GB, the whole project root), the `COPY . .` layer (2.29 GB)
// and the `go build` exec mount (1.79 GB). A cap below one build's worth would
// evict what the next build is about to ask for and turn every build into a cold
// one, so the default has to clear that with room. It is not swept; `--cache-cap`
// exists because the right number is a property of the project, not of this file.
const DefaultCacheCap = "10GB"

// builderPrefix names the per-project builder. Prefixed so a `docker buildx ls`
// says whose it is, and so it cannot collide with a builder somebody made by
// hand.
const builderPrefix = "w17-"

// BuilderName is the builder a project builds on.
func BuilderName(project string) string { return builderPrefix + project }

// EnsureBuilder makes sure this project has its OWN buildx builder, and returns
// its name — or "" when one could not be had, in which case the caller builds on
// the default builder exactly as before.
//
// # Why a project gets its own builder
//
// Because the shared one cannot say whose cache is whose, and that is not a gap
// in docker. Measured on a live daemon: a cache record carries an id, parents,
// timestamps, a size, a usage count and a `shared` flag — and no owner.
// `buildx prune --filter` has no label key. So on the shared builder there is no
// filter that reclaims a project's own cache and nothing else.
//
// The first instinct — leave it shared, because pruning a shared cache would
// throw away layers other projects would hit — turned out to be wrong in the
// important direction. Broken down by size, the cache is:
//
//	3.07 GB + 2.49 GB + 2.49 GB   "local source for context"   ← ONE project's
//	2.29 GB                        [build 4/6] COPY . .        ← that project's
//	1.79 GB                        go build exec mount         ← that project's
//	kB … 67 MB                     WORKDIR, base-image bits    ← cross-project
//
// The heavily reused records (16, 29, 31 uses) are the TRIVIAL ones. The
// gigabytes are per-project, and they are marked `shared` only because one w17
// project builds several bundles from one context — sharing WITHIN a project,
// which a per-project builder keeps completely. What is lost is the base-image
// layers: tens of MB, pulled once per builder.
//
// So: ownership of gigabytes, at a cost of tens of megabytes and one buildkit
// container.
//
// # The cap is set twice, on purpose
//
// The builder is created with a buildkit GC policy (`gckeepstorage`), which
// bounds growth even for builds that never go through w17ctl — a plain
// `docker compose build` still lands on this builder if BUILDX_BUILDER is set in
// the shell.
//
// That policy is NOT a hard ceiling: measured against a 2 GB target, repeated
// builds read 0.85 → 1.9 → 3.16 → 2.32 → 4.0 GB. It trims lazily and overshoots
// between passes. So [PruneBuilderCache] also runs an explicit
// `prune --max-used-space` after each build, which brought that same 4.0 GB to
// 1.69 GB — under the cap, synchronously, at a point the developer controls.
func EnsureBuilder(root, project, cap string) string {
	if project == "" {
		return ""
	}
	name := BuilderName(project)
	// Already there? `inspect` is the cheap existence check and it does not
	// boot the builder.
	if _, err := CaptureDockerFn(root, "buildx", "inspect", name); err == nil {
		return name
	}
	cfg, err := writeBuildkitConfig(cap)
	if err != nil {
		return ""
	}
	defer func() { _ = os.Remove(cfg) }()

	if err := RunDockerFn(root, "buildx", "create",
		"--name", name,
		"--driver", "docker-container",
		"--buildkitd-config", cfg,
		"--bootstrap"); err != nil {
		// Older buildx, no docker-container driver, a daemon that refuses — all
		// the same outcome for the caller: build the way we always did. Not an
		// error, because a build must not fail over housekeeping.
		return ""
	}
	return name
}

// writeBuildkitConfig writes the GC policy the builder is created with.
func writeBuildkitConfig(cap string) (string, error) {
	f, err := os.CreateTemp("", "w17-buildkitd-*.toml")
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	// gckeepstorage is what bounds an unattended builder. `gc = true` is the
	// default in recent buildkit and is stated anyway: a config file that relies
	// on a default is a config file that changes meaning when the default does.
	if _, err := fmt.Fprintf(f, "[worker.oci]\n  gc = true\n  gckeepstorage = %q\n", cap); err != nil {
		return "", err
	}
	return f.Name(), nil
}

// PruneBuilderCache holds one project's builder to its cap, and reports the
// size afterwards.
//
// Only ever the project's OWN builder: `--builder` is not optional here, and
// without a name this does nothing. `docker buildx prune` with no builder acts
// on the default one, which is shared with every other project on the machine —
// see [EnsureBuilder] for why that is the one thing this must never do.
func PruneBuilderCache(root, builder, cap string) string {
	if builder == "" {
		return ""
	}
	if err := RunDockerFn(root, "buildx", "prune",
		"--builder", builder,
		"--max-used-space", cap,
		"--force"); err != nil {
		return ""
	}
	return builderCacheSize(root, builder)
}

// builderCacheSize reads the builder's own total, as buildx prints it.
func builderCacheSize(root, builder string) string {
	out, err := CaptureDockerFn(root, "buildx", "du", "--builder", builder)
	if err != nil {
		return ""
	}
	// The last `Total:` line is the builder's whole cache.
	var total string
	for _, l := range nonEmptyLines(string(out)) {
		if strings.HasPrefix(l, "Total:") {
			total = strings.TrimSpace(strings.TrimPrefix(l, "Total:"))
		}
	}
	return total
}

// BuilderEnv is what a compose build runs with so it lands on the project's own
// builder.
func BuilderEnv(builder string) []string {
	if builder == "" {
		return nil
	}
	return []string{"BUILDX_BUILDER=" + builder}
}
