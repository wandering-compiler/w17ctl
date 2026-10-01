package docker

import (
	"fmt"
	"os"
	"strings"
)

// DefaultCacheCap is how much build cache one project may keep.
//
// ⚠️ Deliberately SMALLER than one build's worth, which reverses the reasoning
// this constant shipped with. It used to be 10 GB, justified by "one build's
// records came to roughly 7 GB, so anything below that turns every build into a
// cold one". Both halves of that have since failed:
//
//   - the 7 GB was measured on a tree whose build CONTEXT was ~2.5 GB, because a
//     developer's `node_modules` and `.next` were being uploaded into a w17 build.
//     Measured again on a consumer's bundle from a clean checkout: one build is
//     1.6 GB of records, of which the context is 92 MB;
//   - a per-PROJECT cap does not bound a MACHINE. Twenty workspaces times any
//     number is a product, not a limit, and a shared box does not care whose
//     gigabytes they are. 20 x 10 GB is 200 GB on a 100 GB disk, which is not a
//     tuning problem, it is arithmetic that cannot come out.
//
// So the trade is taken in the other direction, on purpose and on instruction:
// a COLD BUILD IS CHEAPER THAN A FULL DISK. A colleague could not work at all
// because the machine had no space left, and no build time saved is worth that.
// At this cap a build keeps its base-image layers and little else, so expect the
// Go compile to re-run; `--cache-cap` raises it for one project that genuinely
// needs a warm loop, which is the right place for that decision because the
// person raising it is the person whose disk it is.
const DefaultCacheCap = "1GB"

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
	//
	// ⚠️ An existing builder keeps the GC policy it was CREATED with, and this
	// returns before writing a new one — so lowering DefaultCacheCap does not
	// shrink a builder somebody already has. That is survivable rather than
	// ignored: [PruneBuilderCache] passes the CURRENT cap explicitly after every
	// build, which is the half that actually bounds the disk, and the baked policy
	// only matters for a builder nothing prunes. Changing it means removing the
	// builder (`docker buildx rm w17-<project>`), which is a developer's call —
	// recreating it here would throw away a warm cache on a flag change.
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
