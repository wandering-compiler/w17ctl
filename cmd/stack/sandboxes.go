package stack

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/docker"
)

// sandboxLabel marks a plugin's dev-only sandbox service in compose.w17.yaml
// (composegen).
const sandboxLabel = "w17.sandbox"

// localBuild reports whether a sandbox image is one this workspace builds
// itself (`<workspace>-w17-<name>:dev`, as the cluster relay and the fake
// identity provider are) and no registry publishes — the only kind a missing
// copy cannot be pulled for. A sandbox from a registry (stripe/stripe-mock)
// is left to compose, which pulls it.
func localBuild(image string) bool {
	name, tag, ok := strings.Cut(image, ":")
	return ok && tag == "dev" && strings.Contains(name, "-w17-") && !strings.Contains(name, "/")
}

// composeService is the part of `docker compose config --format json` this
// file reads.
type composeService struct {
	Image     string            `json:"image"`
	Build     json.RawMessage   `json:"build"`
	Labels    map[string]string `json:"labels"`
	DependsOn json.RawMessage   `json:"depends_on"`
}

// composeConfigFn returns the resolved services of the project's compose
// files, under the environment the `up` gets; tests stub it.
var composeConfigFn = func(root string, env []string) (map[string]composeService, error) {
	out, err := docker.CaptureComposeEnvFn(root, env, append(docker.FileArgs(root), "config", "--format", "json")...)
	if err != nil {
		return nil, err
	}
	var cfg struct {
		Services map[string]composeService `json:"services"`
	}
	if err := json.Unmarshal(out, &cfg); err != nil {
		return nil, err
	}
	return cfg.Services, nil
}

// imagePresentFn reports whether an image is on this machine; tests stub it.
var imagePresentFn = func(ref string) bool {
	return exec.Command("docker", "image", "inspect", ref).Run() == nil
}

// filePresentFn reports whether a build's Dockerfile exists; tests stub it.
var filePresentFn = func(file string) bool {
	fi, err := os.Stat(file)
	return err == nil && fi.Mode().IsRegular()
}

// buildDockerfile is the Dockerfile the service's build reads, or "" when
// it has no build: `<context>/<dockerfile>`, `<context>/Dockerfile` when it
// names none. compose config resolves the context to an absolute path; a
// relative one is taken from the project root, and an absolute dockerfile is
// used as it is.
//
// The FILE, not the context directory, is what decides whether the build can
// run: the cluster relay builds from the plugin root (`context: .`, always
// there) with `dockerfile: sandboxes/relay/Dockerfile`, which is missing from
// a plugin vendored by a w17ctl that did not ship sandbox source.
func (s composeService) buildDockerfile(root string) string {
	if len(s.Build) == 0 {
		return ""
	}
	var b struct {
		Context    string `json:"context"`
		Dockerfile string `json:"dockerfile"`
	}
	if json.Unmarshal(s.Build, &b) != nil {
		var short string
		if json.Unmarshal(s.Build, &short) != nil {
			return ""
		}
		b.Context = short
	}
	// `docker compose config` prints the paths still compose-escaped: the
	// `$$` codegen writes for a literal `$` (composegen.composePath) comes back
	// as `$$` (measured on Compose v5.5.1), so the path on disk has one `$`.
	b.Context = strings.ReplaceAll(b.Context, "$$", "$")
	b.Dockerfile = strings.ReplaceAll(b.Dockerfile, "$$", "$")
	if b.Context == "" {
		b.Context = "."
	}
	if !filepath.IsAbs(b.Context) {
		b.Context = filepath.Join(root, b.Context)
	}
	if b.Dockerfile == "" {
		b.Dockerfile = "Dockerfile"
	}
	if filepath.IsAbs(b.Dockerfile) {
		return b.Dockerfile
	}
	return filepath.Join(b.Context, b.Dockerfile)
}

// dependsOn lists the services s depends on (short or long form).
func (s composeService) dependsOn() []string {
	if len(s.DependsOn) == 0 {
		return nil
	}
	var list []string
	if json.Unmarshal(s.DependsOn, &list) == nil {
		return list
	}
	var long map[string]json.RawMessage
	if json.Unmarshal(s.DependsOn, &long) == nil {
		for k := range long {
			list = append(list, k)
		}
	}
	return list
}

// skipMode says which command is asking [skipSandboxes], because each one
// fails on a different thing.
type skipMode int

const (
	// skipForUp: a plain `stack up`. Compose pulls a missing image, or builds
	// it when the service has a build — so a sandbox is a failure only when
	// its image is missing AND there is nothing to build it from.
	skipForUp skipMode = iota
	// skipForUpBuild: `stack up --build`. Every service with a build is
	// BUILT, whether or not its image is present, so a build context the
	// project lacks fails the `up` even with the image on the machine.
	skipForUpBuild
	// skipForBuild: `stack build`. Only the builds matter — a pull never
	// happens — and nothing is started, so a dependent of a skipped sandbox
	// is still built.
	skipForBuild
)

// withoutUnbuiltSandboxes is the service list a full `stack up` starts when
// some plugin sandbox cannot be had ([skipSandboxes], [skipForUp]).
func withoutUnbuiltSandboxes(root string, env []string) []string {
	return skipSandboxes(root, env, skipForUp)
}

// skipSandboxes is the service list a full `stack up`, `stack up --build` or
// `stack build` acts on when some plugin sandbox cannot be had:
//
//   - its image is a local build ([localBuild]: the cluster plugin's relay,
//     the auth plugin's fake identity provider) that no registry publishes,
//     it is not on this machine, and the service has no compose `build:` —
//     so compose fails pulling it (not for `stack build`, which never pulls);
//   - or it has a `build:` whose context is not in this project — a plugin
//     vendored by a w17ctl that did not ship its sandbox source — so the
//     build fails (for a plain `up`, only when the image is missing too).
//
// Either failure aborts the whole command: the services that did not need
// the sandbox, the private proxy among them, were never started (or built),
// and what a developer then saw was another service's error. Each skipped
// sandbox — and, for an `up`, every service that depends on one — is named.
//
// It returns nil (everything, as before) when nothing is skipped or the
// configuration cannot be read.
func skipSandboxes(root string, env []string, mode skipMode) []string {
	verb := "stack up"
	if mode == skipForBuild {
		verb = "stack build"
	}
	svcs, err := composeConfigFn(root, env)
	if err != nil || len(svcs) == 0 {
		return nil
	}
	skip := map[string]bool{}
	var missing []string
	// noContext: sandboxes compose WOULD build, but whose Dockerfile is not in
	// this project.
	noContext := map[string]string{}
	for name, s := range svcs {
		if s.Labels[sandboxLabel] == "" {
			continue
		}
		if file := s.buildDockerfile(root); file != "" {
			if filePresentFn(file) || (mode == skipForUp && imagePresentFn(s.Image)) {
				continue // compose builds it, or a plain up runs the image it has
			}
			noContext[name] = file
		} else if mode == skipForBuild || !localBuild(s.Image) || imagePresentFn(s.Image) {
			continue // nothing to build, or compose can pull or run it
		}
		skip[name] = true
		missing = append(missing, name)
	}
	if len(missing) == 0 {
		return nil
	}
	// Whatever depends on a skipped service would make compose pull or build
	// it again. A build starts nothing, so its dependents build as they are.
	for changed := mode != skipForBuild; changed; {
		changed = false
		for name, s := range svcs {
			if skip[name] {
				continue
			}
			for _, d := range s.dependsOn() {
				if skip[d] {
					skip[name], changed = true, true
					break
				}
			}
		}
	}
	sort.Strings(missing)
	var dependents, keep []string
	for name := range svcs {
		switch {
		case !skip[name]:
			keep = append(keep, name)
		case svcs[name].Labels[sandboxLabel] == "":
			dependents = append(dependents, name)
		}
	}
	sort.Strings(keep)
	sort.Strings(dependents)
	what := "started"
	if mode == skipForBuild {
		what = "built"
	}
	for _, name := range missing {
		if ctx, ok := noContext[name]; ok {
			fmt.Fprintf(core.Stderr, "%s: sandbox %s is not %s: its build's %s is not in this project (image %s). The plugin that brings it was installed without its sandbox source: re-install it (w17ctl plugin update <plugin>), or start the stack without the plugin feature that needs it.\n",
				verb, name, what, ctx, svcs[name].Image)
			continue
		}
		fmt.Fprintf(core.Stderr, "%s: sandbox %s is not %s: its image %s is not on this machine, and no registry publishes it (a plugin's dev sandbox is a local build). Build the image, or start the stack without the plugin feature that needs it.\n",
			verb, name, what, svcs[name].Image)
	}
	if len(dependents) > 0 {
		fmt.Fprintf(core.Stderr, "%s: not started either, because they depend on a skipped sandbox: %s\n", verb, strings.Join(dependents, ", "))
	}
	return keep
}
