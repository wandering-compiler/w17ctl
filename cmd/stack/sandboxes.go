package stack

import (
	"encoding/json"
	"fmt"
	"os/exec"
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

// withoutUnbuiltSandboxes is the service list a full `stack up` starts when
// some plugin sandbox has no image on this machine and is a local build
// ([localBuild]: the cluster plugin's relay, the auth plugin's fake identity
// provider) that no registry publishes, so compose failed pulling it — and
// a failed pull aborts the whole `up`: the services that did not need the
// sandbox, the private proxy among them, were never started either, and what
// a developer then saw was another service's DNS failure. Each skipped
// sandbox, and every service that depends on one, is named.
//
// It returns nil (start everything, as before) when nothing is missing or
// the configuration cannot be read.
func withoutUnbuiltSandboxes(root string, env []string) []string {
	svcs, err := composeConfigFn(root, env)
	if err != nil || len(svcs) == 0 {
		return nil
	}
	skip := map[string]bool{}
	var missing []string
	for name, s := range svcs {
		if s.Labels[sandboxLabel] == "" || len(s.Build) > 0 || !localBuild(s.Image) || imagePresentFn(s.Image) {
			continue
		}
		skip[name] = true
		missing = append(missing, name)
	}
	if len(missing) == 0 {
		return nil
	}
	// Whatever depends on a skipped service would make compose pull it again.
	for changed := true; changed; {
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
	var dependents, start []string
	for name := range svcs {
		switch {
		case !skip[name]:
			start = append(start, name)
		case svcs[name].Labels[sandboxLabel] == "":
			dependents = append(dependents, name)
		}
	}
	sort.Strings(start)
	sort.Strings(dependents)
	for _, name := range missing {
		fmt.Fprintf(core.Stderr, "stack up: sandbox %s is not started: its image %s is not on this machine, and no registry publishes it (a plugin's dev sandbox is a local build). Build the image, or start the stack without the plugin feature that needs it.\n",
			name, svcs[name].Image)
	}
	if len(dependents) > 0 {
		fmt.Fprintf(core.Stderr, "stack up: not started either, because they depend on a skipped sandbox: %s\n", strings.Join(dependents, ", "))
	}
	return start
}
