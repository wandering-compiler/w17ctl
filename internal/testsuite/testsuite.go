package testsuite

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"

	"golang.org/x/mod/modfile"
	"gopkg.in/yaml.v3"

	"github.com/wandering-compiler/w17ctl/internal/core"
)

// Config drives a run of the project's generated e2e suite against an
// EPHEMERAL, self-owned stack via [Config.Run]. It brings the project's
// OWN compose.yaml up — the
// real app launcher the developer maintains (their business layer, in
// any language, lives there; w17ctl never needs to know how to run it)
// — overriding only two things: a unique project name (so runs never
// collide) and the published host ports (remapped to dynamic so they
// never clash with a dev stack or a parallel run). It then discovers the
// gateway's assigned host port, builds the e2erunner as a host binary
// (cross-compiled in Docker — no local Go needed), runs it against
// http://localhost:<port> as a TRUE external client over the exposed
// port (no docker-internal networking), and tears the stack down.
type Config struct {
	ComposeFile    string
	Project        string
	GatewayService string
	GatewayPort    int
	Format         string
	Mcp            string
	Admin          string
	Domain         string
	Transport      string
	Verbose        bool
	Image          string
	GomodVolume    string
	Keep           bool
	Timeout        int
	Console        string
}

// Seams over the exec primitives so every docker-driving helper (and the
// Run lifecycle that orchestrates them) is testable without invoking
// docker. The defaults below are the genuinely-un-mockable real-exec
// leaves — they are exercised in production and intentionally left
// uncovered by unit tests (see the package tests' leaf documentation).
var (
	lookPath  = exec.LookPath
	runCmd    = func(cmd *exec.Cmd) error { return cmd.Run() }
	outputCmd = func(cmd *exec.Cmd) ([]byte, error) { return cmd.Output() }
)

// Run owns the full lifecycle: build → wait → run → cleanup → done.
func (c *Config) Run() (err error) {
	root, err := core.FindProjectRoot()
	if err != nil {
		return err
	}
	view, err := core.DescribeLockFromRoot(c.Console, root)
	if err != nil {
		return err
	}
	if view.GetE2EDir() == "" {
		return fmt.Errorf("e2e is not enabled for this project (w17/lock.yaml generated_code.e2e_dir is unset)")
	}
	runnerDir := filepath.Join(root, filepath.FromSlash(view.GetServicesDir()), "e2erunner")
	if _, err := os.Stat(filepath.Join(runnerDir, "main_test.go")); err != nil {
		return fmt.Errorf("no generated e2erunner module at %s — run the project's codegen first", runnerDir)
	}
	if _, err := lookPath("docker"); err != nil {
		return fmt.Errorf("w17ctl test runs the stack in Docker; `docker` was not found on PATH")
	}

	composeFile, err := c.resolveComposeFile(root)
	if err != nil {
		return err
	}
	gatewaySvc, err := c.resolveGatewayService(composeFile, root)
	if err != nil {
		return err
	}
	project := c.Project
	if project == "" {
		project = composeBaseName(composeFile, root) + "-e2e-" + randToken()
	}

	// Inspect the merged config: remap published ports → dynamic (a
	// separate overlay; compose.yaml is untouched) + learn whether any
	// service needs a build step.
	override, hasBuild, err := c.analyzeCompose(composeFile, root)
	if err != nil {
		return err
	}
	files := []string{composeFile}
	if override != "" {
		files = append(files, override)
		defer func() { _ = os.Remove(override) }()
	}

	_ = runCmd(exec.Command("docker", "volume", "create", c.GomodVolume))

	// `done` is the last thing emitted, carrying the overall outcome.
	defer func() { c.status("done", "finished", map[string]any{"ok": err == nil}) }()

	// Cleanup is registered HERE, before the build, not after `up`.
	//
	// The stack starts owning docker state the moment `compose build`
	// starts — a build that fails on the fourth service still leaves the
	// images of the three that succeeded, under this run's unique project
	// name, and nothing will ever ask for that name again. Registered
	// after `up` (where it used to be), every return between the two —
	// a failed build, an unwritable bin/ dir, a failed e2erunner
	// cross-compile — skipped cleanup entirely and leaked whatever the
	// build had reached.
	//
	// `down` on a project that never came up is a no-op for the
	// containers and networks, so covering the wider span costs nothing;
	// what it adds is that EVERY non-signalled exit from here down runs
	// the teardown. `--keep` stays the one deliberate exception.
	if !c.Keep {
		defer func() {
			c.status("cleanup", "tearing down", map[string]any{"project": project})
			c.teardown(files, project, root)
		}()
	}

	// --- build: compose images (only when a service declares build:) +
	//     the suite host binary ----------------------------------------------
	c.status("build", "building the project + suite", map[string]any{"project": project})
	if hasBuild {
		if err = c.compose(files, project, root, "build"); err != nil {
			return fmt.Errorf("docker compose build: %w", err)
		}
	}
	bin := filepath.Join(root, "bin", "e2erunner")
	if err = os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		return err
	}
	if err = c.buildRunner(root, runnerDir, bin); err != nil {
		return fmt.Errorf("build e2erunner: %w", err)
	}

	// --- waiting: bring the stack up + block until every service HEALTHY --
	// `--wait` + per-service healthchecks mean the stack is genuinely
	// ready when this returns — the suite runs exactly once, never against
	// a half-up backend.
	c.status("waiting", fmt.Sprintf("bringing up %s (project %q) — waiting for healthy", filepath.Base(composeFile), project),
		map[string]any{"project": project, "compose": filepath.Base(composeFile)})
	upArgs := []string{"up", "-d", "--wait"}
	if c.Timeout > 0 {
		upArgs = append(upArgs, "--wait-timeout", fmt.Sprint(c.Timeout))
	}
	if err = c.compose(files, project, root, upArgs...); err != nil {
		// No explicit teardown: the defer above already covers this
		// return, and a half-up stack is exactly what it exists for.
		return fmt.Errorf("docker compose up: %w (the stack did not become healthy — check the service logs)", err)
	}
	if c.Keep {
		defer c.status("keep", fmt.Sprintf("stack left running as project %q (tear down: docker compose -p %s down -v)", project, project), map[string]any{"project": project})
	}

	port, err := c.servicePort(files, project, root, gatewaySvc, c.GatewayPort)
	if err != nil {
		return err
	}
	target := fmt.Sprintf("http://localhost:%d", port)

	// Auto-discover the gateway's MCP listener (same service, its own
	// published container port) so MCP scenarios run without a manual
	// --mcp — the counterpart to discovering the REST port above. A
	// project with no gateway-native MCP surface publishes no such port,
	// so discovery fails and MCP stays off (its scenarios skip cleanly).
	// An explicit --mcp always wins.
	mcp := c.Mcp
	if mcp == "" {
		if mp, perr := c.servicePort(files, project, root, gatewaySvc, mcpContainerPort); perr == nil {
			mcp = fmt.Sprintf("http://localhost:%d", mp)
		}
	}

	// Auto-discover the admin's listener — see discoverAdmin for the two
	// topologies it has to cover. A project with no admin surface resolves
	// nothing, leaving the admin scenarios to skip cleanly. An explicit
	// --admin always wins.
	admin := c.Admin
	if admin == "" {
		admin = c.discoverAdmin(files, project, root, gatewaySvc)
	}

	// A transport the caller ASKED for and that has no endpoint would run
	// zero scenarios — the suite itself now fails on that rather than
	// reporting a green run of nothing, but failing here is better still:
	// it happens before the suite is driven, and it can name the discovery
	// that came up empty, which the suite cannot.
	if err = c.requireRequestedTransport(mcp, admin); err != nil {
		return err
	}

	// --- running: drive the suite as an external client over the port -----
	c.status("running", fmt.Sprintf("gateway %s → %s — running the suite", gatewaySvc, target),
		map[string]any{"target": target, "gateway": gatewaySvc, "mcp": mcp, "admin": admin})
	err = c.runSuite(bin, target, mcp, admin)
	return err
}

// status reports a lifecycle phase. text → a human line on stderr; json →
// an NDJSON `{type:status,phase,message,…}` record on stdout (the machine
// channel), carrying any structured extras.
func (c *Config) status(phase, message string, extra map[string]any) {
	if c.Format == "json" {
		rec := map[string]any{"type": "status", "phase": phase, "message": message}
		for k, v := range extra {
			rec[k] = v
		}
		b, _ := json.Marshal(rec)
		fmt.Fprintln(os.Stdout, string(b))
		return
	}
	fmt.Fprintf(os.Stderr, "w17ctl test: %s\n", message)
}

// resolveComposeFile picks the project's launcher compose file.
func (c *Config) resolveComposeFile(root string) (string, error) {
	if c.ComposeFile != "" {
		p := c.ComposeFile
		if !filepath.IsAbs(p) {
			p = filepath.Join(root, p)
		}
		if _, err := os.Stat(p); err != nil {
			return "", fmt.Errorf("compose file %s: %w", p, err)
		}
		return p, nil
	}
	for _, name := range []string{"compose.yaml", "compose.yml", "docker-compose.yml", "docker-compose.yaml"} {
		p := filepath.Join(root, name)
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("no compose file at %s (expected compose.yaml)", root)
}

// discoverAdmin resolves the admin console's base URL from the running
// stack, covering BOTH topologies the admin ships in:
//
//   - SPLIT — the admin is its own compose service (name ending in
//     `-admin`) serving on adminContainerPort.
//   - COMPOSED — the admin is absorbed into the composed binary, so there
//     is no `-admin` service at all: it keeps its own listener inside the
//     gateway's process, on composedAdminContainerPort (port per surface,
//     see docs/specs/bundles/composed-binary-packaging.md).
//
// Only the split shape was ever looked for, so on a composed stack every
// admin scenario skipped and the run still reported PASS. Returns "" when
// neither resolves — a project with no admin surface, which is most of
// them.
func (c *Config) discoverAdmin(files []string, project, root, gatewaySvc string) string {
	if svc := resolveAdminService(files, project, root); svc != "" {
		if ap, err := c.servicePort(files, project, root, svc, adminContainerPort); err == nil {
			return fmt.Sprintf("http://localhost:%d", ap)
		}
		return ""
	}
	if gatewaySvc == "" {
		return ""
	}
	if ap, err := c.servicePort(files, project, root, gatewaySvc, composedAdminContainerPort); err == nil {
		return fmt.Sprintf("http://localhost:%d", ap)
	}
	return ""
}

// requireRequestedTransport refuses a --transport whose endpoint is not
// configured. Asking for one transport and getting a green run of zero
// scenarios is the worst outcome available here: it reads as "the admin
// suite passes" when the admin was never contacted.
//
// Only an EXPLICIT --transport is guarded. A full run over a project with
// no MCP or no admin surface is the normal case, and those scenarios
// skipping is exactly right.
func (c *Config) requireRequestedTransport(mcp, admin string) error {
	switch c.Transport {
	case "mcp":
		if mcp == "" {
			return fmt.Errorf("--transport=mcp: no MCP endpoint — the gateway service publishes no :%d, "+
				"so every MCP scenario would skip and the run would pass without testing anything; "+
				"pass --mcp http://localhost:<port> (or drop --transport to run what the stack does serve)", mcpContainerPort)
		}
	case "admin":
		if admin == "" {
			return fmt.Errorf("--transport=admin: no admin endpoint — found neither a `-admin` compose service on :%d "+
				"(split topology) nor an absorbed admin on the gateway service's :%d (composed binary), "+
				"so every admin scenario would skip and the run would pass without testing anything; "+
				"pass --admin http://localhost:<port> (or drop --transport to run what the stack does serve)",
				adminContainerPort, composedAdminContainerPort)
		}
	}
	return nil
}

// resolveAdminService finds the admin bundle's compose service by the
// `-admin` suffix the admin generator names it with. Returns "" when the
// project has no admin surface — not an error, since most don't: a
// composed binary absorbs the admin and publishes it itself (see
// discoverAdmin).
func resolveAdminService(files []string, project, root string) string {
	if len(files) == 0 {
		return ""
	}
	svcs := resolvedComposeServices(files[0], root)
	if len(svcs) == 0 {
		svcs = composeServices(files[0])
	}
	for _, s := range svcs {
		if strings.HasSuffix(s, "-admin") {
			return s
		}
	}
	return ""
}

// resolveGatewayService finds the REST gateway's compose service: the
// flag, else the service whose name ends in -gateway.
//
// Discovery reads the FULLY MERGED service list (`docker compose config
// --services`), which resolves Compose `include:` — the canonical stack
// (`compose.w17.yaml`) pulls the app-tier bundles' `w17/services/*/
// compose.yaml` in via `include:`, so the `<domain>-gateway` service is
// not defined at the launcher file's top level. Falling back to a raw
// top-level `services:` scan would miss it and force an explicit
// --gateway-service. If the merge fails (e.g. docker absent under test),
// fall back to the raw parse.
func (c *Config) resolveGatewayService(composeFile, root string) (string, error) {
	if c.GatewayService != "" {
		return c.GatewayService, c.checkGatewayService(composeFile, root, c.GatewayService)
	}
	svcs := resolvedComposeServices(composeFile, root)
	if len(svcs) == 0 {
		svcs = composeServices(composeFile)
	}
	for _, s := range svcs {
		if strings.HasSuffix(s, "-gateway") {
			return s, nil
		}
	}
	for _, s := range svcs {
		if strings.Contains(s, "gateway") {
			return s, nil
		}
	}
	// Composed topology: the gateway is absorbed into a binary the author
	// named (`app-server`, …), so no service name carries the word at all.
	// What still holds is the SURFACE — that binary is the process
	// publishing the REST port — so ask the merged config who does. Only
	// when exactly one service does: two candidates is a stack this
	// heuristic has no business picking from.
	if pub := servicesPublishing(composeFile, root, c.GatewayPort); len(pub) == 1 {
		return pub[0], nil
	}
	return "", fmt.Errorf("could not find a gateway service in %s — no service is named `*-gateway` and none uniquely publishes :%d "+
		"(a composed binary serves REST itself); pass --gateway-service (services: %s)",
		filepath.Base(composeFile), c.GatewayPort, strings.Join(svcs, ", "))
}

// checkGatewayService vets an EXPLICIT --gateway-service against the
// merged config, before the stack is brought up.
//
// The flag is an override, so it is never second-guessed on what it picks
// — but a name the stack does not have (a typo, a bundle renamed by a
// regen) or one that serves no REST port cannot possibly work: port
// discovery fails minutes later, after a full build + up, with docker's
// "not running" for a service that was never going to run. Naming it here
// costs one already-cached compose parse and turns that into an
// instant, precise message that also lists the services that DO publish
// the port.
//
// Both checks stay silent when the merged config is unreadable (docker
// absent, an invalid compose): an unknown stack must not be reported as a
// bad flag.
func (c *Config) checkGatewayService(composeFile, root, svc string) error {
	svcs := resolvedComposeServices(composeFile, root)
	if len(svcs) == 0 {
		return nil
	}
	if !slices.Contains(svcs, svc) {
		return fmt.Errorf("--gateway-service %q is not a service in %s (services: %s)",
			svc, filepath.Base(composeFile), strings.Join(svcs, ", "))
	}
	// Only when the stack is KNOWN to serve the port elsewhere. "Nobody
	// publishes it" is the ambiguous case — an unreadable config looks the
	// same — and the port call downstream reports it either way.
	pub := servicesPublishing(composeFile, root, c.GatewayPort)
	if len(pub) > 0 && !slices.Contains(pub, svc) {
		return fmt.Errorf("--gateway-service %q publishes no :%d, so the suite has no gateway to drive "+
			"(services publishing :%d: %s) — name one of those, or pass --gateway-port for a gateway on another port",
			svc, c.GatewayPort, c.GatewayPort, strings.Join(pub, ", "))
	}
	return nil
}

// servicesPublishing lists the merged config's services that publish
// `port` as a container port, sorted so the answer never depends on map
// order. Returns nil on any error (docker missing / invalid config) — the
// caller treats "don't know" and "nobody" alike.
func servicesPublishing(composeFile, root string, port int) []string {
	out, err := outputCmd(exec.Command("docker", "compose", "-f", composeFile, "--project-directory", root, "config", "--format", "json"))
	if err != nil {
		return nil
	}
	var cfg struct {
		Services map[string]struct {
			Ports []struct {
				Target int `json:"target"`
			} `json:"ports"`
		} `json:"services"`
	}
	if json.Unmarshal(out, &cfg) != nil {
		return nil
	}
	var names []string
	for name, svc := range cfg.Services {
		for _, p := range svc.Ports {
			if p.Target == port {
				names = append(names, name)
				break
			}
		}
	}
	sort.Strings(names)
	return names
}

// reclaimLabelKey / workspace mirror srcgo/tests/containers/reclaim.go, which
// labels the OTHER family of ephemeral containers this repo starts (the Go
// tests' detached databases). Both families have to answer to one sweep —
// `make docker-sweep`, which filters
// `--filter label=wc-test-workspace=$(WC_WORKSPACE)` — so the key and the
// value have to be identical on both sides.
//
// Duplicated rather than imported: w17ctl is the PUBLIC thin client and its
// private-srcgo closure is 0 (D4). srcgo/tests/dockernames gates the two
// spellings against each other so the duplication cannot drift silently.
const reclaimLabelKey = "wc-test-workspace"

// workspace mirrors the Makefile's WC_WORKSPACE (which it exports) and falls
// back to the Makefile's own default. A bare `w17ctl test` outside make still
// labels its stack — an UNLABELLED container is the one thing this must never
// produce, because it is invisible to every sweep by construction.
func workspace() string {
	if w := strings.TrimSpace(os.Getenv("WC_WORKSPACE")); w != "" {
		return w
	}
	return "wc"
}

// analyzeCompose reads the merged compose config once and returns (a) a temp
// override applied on top of the project's own compose.yaml, and (b) whether
// any service has a `build:` directive (so the caller can skip
// `docker compose build` — and its "No services to build" warning — for an
// image-only stack). The developer's compose.yaml is never edited.
//
// The override carries TWO things:
//
//   - every published port remapped to a DYNAMIC host port (`ports: !override`
//     with the container port only → Docker assigns a free one, so parallel
//     runs never collide);
//   - the reclaim LABEL on every service.
//
// The label is why this is now emitted unconditionally, where it used to
// return "" for a stack that publishes no ports. `w17ctl test` tears its
// stack down in a `defer`, and a defer does not survive a signalled death —
// Ctrl-C, a CI timeout, an OOM kill. What is left then is a full compose
// stack (databases, brokers, gateways) that `make docker-sweep` cannot see,
// because the sweep collects by this label and nothing here applied it.
// That is not hypothetical: two such stacks, fifteen containers, were found
// alive 46 hours after the run that started them, seven still healthy, while
// the sweep reported "no leftovers".
//
// The label matches `srcgo/tests/containers` (reclaim.go), which has carried
// it for the Go-test container family all along — same key, same workspace
// value, so one sweep collects both. It is spelled out rather than imported:
// w17ctl is the public thin client and imports zero private srcgo (D4), so
// the agreement is pinned by a gate instead (srcgo/tests/dockernames).
func (c *Config) analyzeCompose(composeFile, root string) (override string, hasBuild bool, err error) {
	out, err := outputCmd(exec.Command("docker", "compose", "-f", composeFile, "--project-directory", root, "config", "--format", "json"))
	if err != nil {
		return "", false, fmt.Errorf("docker compose config: %w", err)
	}
	var cfg struct {
		Services map[string]struct {
			Build json.RawMessage `json:"build"`
			Ports []struct {
				Target   int    `json:"target"`
				Protocol string `json:"protocol"`
			} `json:"ports"`
		} `json:"services"`
	}
	if err := json.Unmarshal(out, &cfg); err != nil {
		return "", false, fmt.Errorf("parse compose config: %w", err)
	}
	names := make([]string, 0, len(cfg.Services))
	for n := range cfg.Services {
		names = append(names, n)
	}
	sort.Strings(names)

	var b strings.Builder
	b.WriteString("# generated by w17ctl test — reclaim label + published ports → dynamic\nservices:\n")
	for _, n := range names {
		svc := cfg.Services[n]
		builds := len(svc.Build) > 0 && string(svc.Build) != "null"
		if builds {
			hasBuild = true
		}
		fmt.Fprintf(&b, "  %s:\n    labels:\n      %s: %q\n", n, reclaimLabelKey, workspace())
		// The same label on the IMAGE, for the services that build one.
		// The container label above makes a leaked stack sweepable; it
		// says nothing about the `<project>-<service>` image compose
		// built for it, and an image carries no trace of the containers
		// that once ran it. Teardown removes those images with
		// `--rmi local`, but teardown is a `defer` — a signalled death
		// skips it and leaves the images behind with the containers.
		// This is what makes that residue collectable by the same
		// workspace-scoped `make docker-sweep` as everything else.
		if builds {
			fmt.Fprintf(&b, "    build:\n      labels:\n        %s: %q\n", reclaimLabelKey, workspace())
		}
		if len(svc.Ports) == 0 {
			continue
		}
		fmt.Fprintf(&b, "    ports: !override\n")
		for _, p := range svc.Ports {
			proto := ""
			if p.Protocol != "" && p.Protocol != "tcp" {
				proto = "/" + p.Protocol
			}
			fmt.Fprintf(&b, "      - \"%d%s\"\n", p.Target, proto)
		}
	}
	if len(names) == 0 {
		return "", hasBuild, nil
	}
	f := filepath.Join(os.TempDir(), "w17ctl-ports-"+randToken()+".yaml")
	if err := os.WriteFile(f, []byte(b.String()), 0o644); err != nil {
		return "", hasBuild, err
	}
	return f, hasBuild, nil
}

// compose runs a `docker compose` subcommand (with every -f file)
// streamed to the user.
func (c *Config) compose(files []string, project, root string, args ...string) error {
	full := []string{"compose"}
	for _, f := range files {
		full = append(full, "-f", f)
	}
	full = append(full, "-p", project, "--project-directory", root)
	full = append(full, args...)
	cmd := exec.Command("docker", full...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return runCmd(cmd)
}

// teardown brings the stack down + removes its volumes AND the images
// compose built for it (best-effort). The caller emits the "cleanup"
// status; docker's own output streams to stderr.
//
// ⚠️ `--rmi local` is not optional here, it is the counterpart of the
// per-run project name. Every run gets a fresh `<base>-e2e-<token>`
// project, so compose builds a fresh `<project>-<service>` image set for
// it — and `down -v --remove-orphans` reclaims containers, networks and
// volumes but NEVER images. Nothing else collects them either: the host's
// nightly reclaim runs `docker image prune -f` WITHOUT `-a` (by design —
// `-a` would delete every idle workspace's tagged images), and these are
// tagged. So the leak is unbounded, and it was: 142 `platform-e2e-*`
// images, ~11 GB, accumulated over three weeks on the shared host before
// anyone looked.
//
// `local`, not `all`: it removes only images compose named itself (a
// service with no `image:` key), which is exactly this run's build output.
// `all` would additionally drop the images the compose file NAMES —
// postgres, nats, caddy — which are shared with every other workspace on
// the box and would be re-pulled by whoever needs them next.
func (c *Config) teardown(files []string, project, root string) {
	_ = c.compose(files, project, root, "down", "-v", "--remove-orphans", "--rmi", "local")
}

// mcpContainerPort is the gateway bundle's MCP listener container port —
// the gateway serves REST (GatewayPort, 8080) + MCP concurrently as one
// binary, publishing the MCP transport on :8081 (the codegen default
// `defaultMCPListen`, published by the gateway compose as
// `${..._MCP_HOST_PORT:-8081}:8081`). Used to auto-discover the MCP
// endpoint so MCP scenarios run without a manual --mcp.
const mcpContainerPort = 8081

// adminContainerPort is the admin bundle's HTTP listener container port in
// the SPLIT topology, where the admin is its OWN compose service (unlike
// MCP, which shares the gateway service) — so discovery matches a service
// name before probing this port.
const adminContainerPort = 8080

// composedAdminContainerPort is where an ABSORBED admin listens when the
// project ships a composed binary: the admin keeps its own HTTP listener
// inside the shared process and the composed bundle publishes that port
// (`admin.AbsorbedListenPort`, docs/specs/bundles/composed-binary-packaging.md
// §"port per surface"). Mirrored — not imported — because the client holds
// no compiler types; a change to the platform's absorbed port has to be
// mirrored here, which is why the composed lane asserts the admin answers
// (`w17ctl test --transport=admin` against the composed example).
const composedAdminContainerPort = 9090

// portPublishWait bounds how long [Config.servicePort] waits for a
// container's published ports to become VISIBLE to `docker compose port`,
// and portPublishDelay is the gap between attempts.
//
// `up --wait` returning with every service Healthy does NOT order against
// port publication: a `port` query issued immediately after can still see a
// container with ZERO published ports, and the run then dies during
// discovery on a stack that is in fact fully up. Measured at ~50% of runs
// on a loaded shared box (16 cores, load ~12, ~50 foreign containers —
// deinvo, 2026-07-28); it does not reproduce on an idle machine, which is
// why it read as a config error for a week.
//
// Waiting costs nothing when the ports are already up — the first attempt
// answers — and it is only ever entered on the not-ready signature below,
// never on a real misconfiguration.
var (
	portPublishWait  = 10 * time.Second
	portPublishDelay = 250 * time.Millisecond
)

// servicePort asks compose for the dynamically-assigned host port mapped
// to a service's given container port, waiting out the publish race above.
func (c *Config) servicePort(files []string, project, root, svc string, containerPort int) (int, error) {
	deadline := time.Now().Add(portPublishWait)
	for attempt := 0; ; attempt++ {
		port, notReady, err := c.servicePortOnce(files, project, root, svc, containerPort)
		if err == nil || !notReady {
			return port, err
		}
		if !time.Now().Before(deadline) {
			return 0, fmt.Errorf("%w — the container still published NO ports after %s (this is a publish race, not a bad service/port)", err, portPublishWait)
		}
		if attempt == 0 {
			// A silent multi-second pause here is indistinguishable from a
			// hang; say what is being waited for.
			c.status("waiting", fmt.Sprintf("%s reports no published ports yet — waiting up to %s for its :%d", svc, portPublishWait, containerPort),
				map[string]any{"service": svc, "container_port": containerPort})
		}
		time.Sleep(portPublishDelay)
	}
}

// servicePortOnce is one `docker compose port` attempt. notReady is true
// only for the publish-race signature — docker reporting that the
// container has no published ports AT ALL — so a genuinely wrong service
// or port still fails on the first attempt, as it should.
//
// Carries --project-directory like every other compose call here: without
// it compose takes the FIRST -f file's directory as the project root, so a
// --compose-file living in a subdirectory would resolve build contexts and
// env_file paths against a different root than the `up` that started the
// stack — a discovery failure with no relation to the ports.
func (c *Config) servicePortOnce(files []string, project, root, svc string, containerPort int) (port int, notReady bool, err error) {
	full := []string{"compose"}
	for _, f := range files {
		full = append(full, "-f", f)
	}
	full = append(full, "-p", project, "--project-directory", root, "port", svc, fmt.Sprint(containerPort))
	out, err := outputCmd(exec.Command("docker", full...))
	if err != nil {
		// Docker says exactly what went wrong here — "no port 8080/tcp for
		// container X: 4222/tcp, …" or `service "X" is not running` — and
		// dropping it left a bare "exit status 1" that names neither the
		// cause nor the fix (deinvo, 2026-07-28). It is the only evidence
		// there is: the caller cannot re-derive it from the exit code.
		if msg := dockerStderr(err); msg != "" {
			return 0, portsUnpublished(msg), fmt.Errorf("discover host port (%s:%d): %w — docker: %s", svc, containerPort, err, msg)
		}
		return 0, false, fmt.Errorf("discover host port (%s:%d): %w", svc, containerPort, err)
	}
	s := strings.TrimSpace(string(out))
	i := strings.LastIndex(s, ":")
	if i < 0 {
		return 0, false, fmt.Errorf("unexpected `docker compose port` output %q", s)
	}
	if _, err := fmt.Sscanf(s[i+1:], "%d", &port); err != nil || port == 0 {
		return 0, false, fmt.Errorf("could not parse host port from %q", s)
	}
	return port, false, nil
}

// portsUnpublished reports whether a failed `docker compose port` said the
// container has NO published ports at all — the not-ready signature, worth
// retrying:
//
//	no port 8080/tcp for container proj-core-server-1:      ← empty list
//	no port 9090/tcp for container proj-nats-1: 4222/tcp    ← wrong port
//
// The second shape means the service does not publish what was asked for.
// That is a config error with a different fix and it will never resolve
// itself, so it must keep failing immediately.
func portsUnpublished(stderr string) bool {
	if !strings.Contains(stderr, "no port ") {
		return false
	}
	_, tail, ok := strings.Cut(stderr, " for container ")
	if !ok {
		return false
	}
	// Container names carry no colon, so the first one ends the name and
	// everything after it is docker's list of what the container DOES
	// publish.
	_, published, ok := strings.Cut(tail, ":")
	return ok && strings.TrimSpace(published) == ""
}

// dockerStderr returns what a failed docker command wrote to stderr,
// flattened to a single line (compose writes multi-line diagnostics, and
// this ends up inside a one-line error). Empty when the failure carried no
// stderr — a docker that could not be started at all, or a stubbed
// outputCmd under test.
func dockerStderr(err error) string {
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return ""
	}
	return strings.Join(strings.Fields(string(ee.Stderr)), " ")
}

// buildRunner cross-compiles the e2erunner test binary for the HOST
// platform inside the builder image, writing it to bin (a host path
// under the bind-mounted workspace).
func (c *Config) buildRunner(root, runnerDir, bin string) error {
	return buildRunnerBin(root, runnerDir, bin, c.Image, c.GomodVolume)
}

// buildRunnerBin is the shared cross-compile step behind both `test` (e2e)
// and `stress` (load): it builds the e2erunner host binary in the builder
// image and writes it to bin (a host path under the bind-mounted
// workspace). Both surfaces produce the SAME binary — the mode is chosen at
// run time by the flags passed to it (-stress / -target).
func buildRunnerBin(root, runnerDir, bin, image, gomodVolume string) error {
	mountRoot := workspaceMountRoot(root, runnerDir)
	script := fmt.Sprintf("apk add --no-cache git >/dev/null 2>&1; go test -c -o %s .", shSingleQuote(bin))
	cmd := exec.Command("docker", "run", "--rm",
		"-v", mountRoot+":"+mountRoot,
		"-v", gomodVolume+":/go/pkg/mod",
		"-w", runnerDir,
		"-e", "CGO_ENABLED=0",
		"-e", "GOOS="+runtime.GOOS,
		"-e", "GOARCH="+runtime.GOARCH,
		image, "sh", "-c", script,
	)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return runCmd(cmd)
}

// runSuite runs the host binary against target exactly once — the stack
// is already healthy (docker compose up --wait guaranteed it), so any
// failure is a real one. text → stream the suite's output straight
// through. json → pass the suite `-format json` and forward only its
// valid NDJSON records to stdout, dropping the `go test` framework noise
// (=== RUN / PASS / ok …) so the stream stays clean.
func (c *Config) runSuite(bin, target, mcp, admin string) error {
	args := []string{"-target", target}
	switch {
	case c.Format == "json":
		// -test.v so the testing framework doesn't buffer the runner's
		// stdout away; the JSON filter below strips the framework lines.
		args = append(args, "-format", "json", "-test.v")
	case c.Verbose:
		args = append(args, "-test.v")
	}
	// mcp is the resolved endpoint (explicit --mcp, else auto-discovered);
	// empty when the project has no MCP surface, leaving those scenarios to
	// skip.
	if mcp != "" {
		args = append(args, "-mcp", mcp)
	}
	// admin is the resolved admin-bundle endpoint; empty when the project
	// has no admin surface, leaving those scenarios to skip.
	if admin != "" {
		args = append(args, "-admin", admin)
	}
	if c.Domain != "" {
		args = append(args, "-domain", c.Domain)
	}
	if c.Transport != "" {
		args = append(args, "-transport", c.Transport)
	}

	cmd := exec.Command(bin, args...)
	cmd.Stderr = os.Stderr
	if c.Format != "json" {
		cmd.Stdout = os.Stdout
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("e2e suite failed")
		}
		return nil
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024) // a fail record's error string can be large
	for sc.Scan() {
		line := sc.Bytes()
		if json.Valid(line) && bytes.Contains(line, []byte(`"type"`)) {
			fmt.Fprintln(os.Stdout, sc.Text())
		}
	}
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("e2e suite failed")
	}
	return nil
}

// --- compose / workspace helpers ---------------------------------------

// resolvedComposeServices returns the fully merged service list from
// `docker compose config --services`, which resolves `include:` +
// overrides (unlike a raw top-level `services:` scan). Returns nil on any
// error (docker missing / invalid config) so the caller can fall back to
// the raw parse.
func resolvedComposeServices(composeFile, root string) []string {
	out, err := outputCmd(exec.Command("docker", "compose", "-f", composeFile, "--project-directory", root, "config", "--services"))
	if err != nil {
		return nil
	}
	var svcs []string
	for _, ln := range strings.Split(string(out), "\n") {
		if s := strings.TrimSpace(ln); s != "" {
			svcs = append(svcs, s)
		}
	}
	return svcs
}

func composeServices(composeFile string) []string {
	data, err := os.ReadFile(composeFile)
	if err != nil {
		return nil
	}
	var doc struct {
		Services map[string]any `yaml:"services"`
	}
	if yaml.Unmarshal(data, &doc) != nil {
		return nil
	}
	out := make([]string, 0, len(doc.Services))
	for s := range doc.Services {
		out = append(out, s)
	}
	return out
}

// composeBaseName derives a readable base for the run's project name:
// the compose if set, else the project directory's name.
func composeBaseName(composeFile, root string) string {
	data, err := os.ReadFile(composeFile)
	if err == nil {
		var doc struct {
			Name string `yaml:"name"`
		}
		if yaml.Unmarshal(data, &doc) == nil && doc.Name != "" {
			return doc.Name
		}
	}
	return filepath.Base(root)
}

// randRead is the seam tests stub to exercise randToken's fallback;
// production reads crypto/rand (which effectively never fails).
var randRead = rand.Read

func randToken() string {
	var b [4]byte
	if _, err := randRead(b[:]); err != nil {
		return "run"
	}
	return hex.EncodeToString(b[:])
}

// workspaceMountRoot returns the directory to bind-mount for the build:
// the smallest dir containing the project root, the e2erunner, and every
// module a go.work above the runner references. No go.work → the project
// root.
func workspaceMountRoot(projectRoot, runnerDir string) string {
	wf := findGoWork(runnerDir)
	if wf == "" {
		return projectRoot
	}
	data, err := os.ReadFile(wf)
	if err != nil {
		return projectRoot
	}
	work, err := modfile.ParseWork(wf, data, nil)
	if err != nil {
		return projectRoot
	}
	workDir := filepath.Dir(wf)
	dirs := []string{projectRoot, workDir}
	for _, u := range work.Use {
		dirs = append(dirs, filepath.Clean(filepath.Join(workDir, filepath.FromSlash(u.Path))))
	}
	return commonAncestor(dirs)
}

func findGoWork(start string) string {
	dir := start
	for {
		p := filepath.Join(dir, "go.work")
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

func commonAncestor(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	sep := string(filepath.Separator)
	common := strings.Split(filepath.Clean(paths[0]), sep)
	for _, p := range paths[1:] {
		parts := strings.Split(filepath.Clean(p), sep)
		n := len(common)
		if len(parts) < n {
			n = len(parts)
		}
		i := 0
		for i < n && common[i] == parts[i] {
			i++
		}
		common = common[:i]
	}
	res := strings.Join(common, sep)
	if res == "" {
		return sep
	}
	return res
}

func shSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
