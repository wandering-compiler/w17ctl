package docker

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/wandering-compiler/w17ctl/internal/core"
)

// RunDockerFn / CaptureDockerFn are the plain-`docker` seams, beside the
// `docker compose` ones above. Separate because a prune is not a compose
// operation: compose has no verb for it, and the filters below are docker's.
var (
	RunDockerFn     = realRunDocker
	CaptureDockerFn = realCaptureDocker
)

func realRunDocker(dir string, args ...string) error {
	cmd := exec.Command("docker", args...)
	cmd.Dir = dir
	cmd.Stdout = core.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func realCaptureDocker(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("docker", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	return out, err
}

// composeProjectLabel is the label docker compose stamps on every image it
// builds. Verified on this machine's daemon rather than assumed — a shared
// daemon here holds three other projects' images, and they all carry it.
const composeProjectLabel = "com.docker.compose.project"

// Reclaimed is what one post-build reclaim did and what it deliberately did
// not.
type Reclaimed struct {
	// Project is the compose project the prune was scoped to. Empty when the
	// name could not be resolved, in which case nothing was pruned: a prune
	// that cannot say whose images it is removing must not run.
	Project string
	// Images is how many untagged images belonging to Project were removed.
	Images int
	// BuildCache and BuildCacheReclaimable are docker's OWN strings, passed
	// through rather than parsed. Reporting them in docker's formatting means
	// there is no size parser here to drift from the one that produced them.
	//
	// They describe the DEFAULT builder — the shared one — and are reported only
	// when this project is not on its own builder. Once it is, its cache is
	// bounded and the machine-wide figure is somebody else's business.
	BuildCache            string
	BuildCacheReclaimable string

	// Builder is the project's own builder, empty when it is building on the
	// shared default one. BuilderCache is that builder's total after the cap
	// was applied.
	Builder      string
	BuilderCache string
	Cap          string
}

// Line renders the one-line summary a build prints, or "" when there is
// nothing true to say.
func (r Reclaimed) Line() string {
	if r.Project == "" {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "reclaim: %s — ", r.Project)
	if r.Images == 0 {
		b.WriteString("no images left over")
	} else {
		fmt.Fprintf(&b, "%d untagged image(s) removed", r.Images)
	}
	switch {
	case r.Builder != "":
		// The project has its own builder, so its cache is bounded and the
		// number is meaningful: it is this project's, and it cannot grow past
		// the cap. Nothing is said about the machine, because nothing here
		// touched it.
		fmt.Fprintf(&b, "; own build cache %s", r.BuilderCache)
		if r.Cap != "" {
			fmt.Fprintf(&b, " (capped at %s)", r.Cap)
		}
	case r.BuildCache != "":
		// No own builder — an older buildx, or it was declined. Then the cache
		// is the shared one, it cannot be scoped, and the only honest thing is
		// to say so with the numbers. A number nobody is shown is a number
		// nobody acts on.
		fmt.Fprintf(&b, "; SHARED build cache %s", r.BuildCache)
		if r.BuildCacheReclaimable != "" {
			fmt.Fprintf(&b, " (%s reclaimable, not project-scoped)", r.BuildCacheReclaimable)
		}
	}
	return b.String()
}

// ReclaimAfterBuild removes the untagged images THIS project's build just
// orphaned, and reports what it did not touch.
//
// # Why a rebuild leaves anything behind at all
//
// A generated project's compose stanzas carry `build:` with no `image:`, so
// compose names each image `<project>-<service>` and a rebuild REPLACES that
// tag. The layers the old tag pointed at survive as an untagged image that
// nothing references and nothing reclaims. Build daily and that is daily
// growth, which is the complaint this exists for.
//
// # Why it is scoped to the project, and why that is not optional
//
// `docker image prune` without a filter removes every dangling image on the
// daemon. A developer's daemon is not theirs alone — the machine this was
// written on runs three other projects' stacks, one of whose images is 3.64GB —
// and a build command that reaches outside its own project is a build command
// nobody can afford to run twice.
//
// So the filter is `label=com.docker.compose.project=<name>`, and the name comes
// from asking compose (`config --format json`) rather than deriving it. Compose
// honours COMPOSE_PROJECT_NAME and normalises directory names; a second
// implementation of that rule would disagree with the first one eventually, and
// the disagreement would be a prune aimed at the wrong project.
//
// If the name cannot be resolved, NOTHING is pruned. A prune that cannot say
// whose images it removes has no business running.
//
// # What it deliberately leaves alone
//
//   - VOLUMES. They hold the dev database. `docker rm` without `-v` once turned
//     leaked containers into leaked volumes on this machine, and the opposite
//     mistake — reclaiming a volume — costs somebody their data, which no disk
//     saving justifies.
//   - The BUILD CACHE, which is both the largest item and the one that makes the
//     next build fast. It also has no project filter, so capping it is a
//     decision about the whole machine and cannot be made from inside one
//     project's build. Reported, never touched.
//   - TAGGED images, including this project's previous versions. Unused is not
//     unwanted.
func ReclaimAfterBuild(root, builder, cap string) Reclaimed {
	var r Reclaimed
	name, err := composeProjectName(root)
	if err != nil || name == "" {
		return r
	}
	r.Project = name
	if builder != "" {
		r.Builder = builder
		r.Cap = cap
		r.BuilderCache = PruneBuilderCache(root, builder, cap)
	} else {
		r.BuildCache, r.BuildCacheReclaimable = buildCacheUsage(root)
	}

	filter := composeProjectLabel + "=" + name
	ids, err := CaptureDockerFn(root, "images", "--quiet",
		"--filter", "dangling=true", "--filter", "label="+filter)
	if err != nil {
		return r
	}
	// Counted BEFORE the prune rather than parsed out of its summary: docker's
	// "Total reclaimed space" line is prose, and a parser for it is a parser to
	// keep in step with somebody else's formatting.
	n := len(nonEmptyLines(string(ids)))
	if n == 0 {
		return r
	}
	if err := RunDockerFn(root, "image", "prune", "--force",
		"--filter", "label="+filter); err != nil {
		return r
	}
	r.Images = n
	return r
}

// ComposeProjectName asks compose what this project is called, or "" when it
// cannot be asked.
//
// Exported because the builder is named after it too: one string answers both
// "whose builder is this" and "whose images are these", so the two cannot drift
// into pruning a builder for one project and images for another.
func ComposeProjectName(root string) string {
	n, err := composeProjectName(root)
	if err != nil {
		return ""
	}
	return n
}

// composeProjectName asks compose what this project is called.
func composeProjectName(root string) (string, error) {
	out, err := CaptureComposeFn(root, append(FileArgs(root), "config", "--format", "json")...)
	if err != nil {
		return "", err
	}
	var cfg struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(out, &cfg); err != nil {
		return "", err
	}
	return strings.TrimSpace(cfg.Name), nil
}

// buildCacheUsage reads docker's own figures for the build cache. Both values
// are returned as docker printed them; see [Reclaimed.BuildCache].
func buildCacheUsage(root string) (size, reclaimable string) {
	out, err := CaptureDockerFn(root, "system", "df", "--format", "json")
	if err != nil {
		return "", ""
	}
	// One JSON object per line, one per type.
	for _, line := range nonEmptyLines(string(out)) {
		var row struct {
			Type        string `json:"Type"`
			Size        string `json:"Size"`
			Reclaimable string `json:"Reclaimable"`
		}
		if json.Unmarshal([]byte(line), &row) != nil {
			continue
		}
		if row.Type == "Build Cache" {
			return row.Size, row.Reclaimable
		}
	}
	return "", ""
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}
