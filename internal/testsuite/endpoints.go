package testsuite

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
)

// A project's domains can serve their surfaces from different binaries:
// `app-gateway` for one domain, a composed `billing-server` for another.
// The suite dials one REST gateway (--target), one MCP and one admin, so
// every other domain's scenarios went to that one gateway and got its 404.
// The run therefore also hands the runner each other domain's own
// addresses, as E2E_ENDPOINTS (`<domain>/<surface>=<url>`, the runner's
// -endpoint), found in the stack itself: a gateway serves REST for the
// domain whose <DOMAIN>_GATEWAY_LISTEN_ADDR it reads, and an admin for the
// domain of its <DOMAIN>_ADMIN_LISTEN_ADDR.
//
// The environment, not a flag: a runner generated before it ignores the
// variable where it would refuse an unknown flag.
const endpointsEnv = "E2E_ENDPOINTS"

var (
	gatewayListenRe = regexp.MustCompile(`^([A-Z][A-Z0-9_]*)_GATEWAY_LISTEN_ADDR$`)
	adminListenRe   = regexp.MustCompile(`^([A-Z][A-Z0-9_]*)_ADMIN_LISTEN_ADDR$`)
)

// domainSurface is one surface of a domain served by a service the run
// does not dial at --target / --mcp / --admin.
type domainSurface struct {
	domain  string
	surface string // rest, mcp or admin
	svc     string
	port    int    // the container port
	origin  string // the public origin its device proofs name; "" = none declared
}

// key is the surface's -endpoint key.
func (s domainSurface) key() string { return s.domain + "/" + s.surface }

// composeService is what domainSurfaces reads of a merged service.
type composeService struct {
	Environment map[string]*string `json:"environment"`
	Ports       []struct {
		Target int `json:"target"`
	} `json:"ports"`
}

func (s composeService) env(name string) string {
	if v := s.Environment[name]; v != nil {
		return *v
	}
	return ""
}

func (s composeService) publishes(port int) bool {
	for _, p := range s.Ports {
		if p.Target == port {
			return true
		}
	}
	return false
}

// domainSurfaces lists the surfaces of the stack's services other than
// gatewaySvc (dialled at --target, and its MCP and absorbed admin) and
// adminSvc (dialled at --admin), sorted by key. A surface is listed only
// where its service publishes its port.
func domainSurfaces(services map[string]composeService, gatewaySvc, adminSvc string, gatewayPort int) []domainSurface {
	var out []domainSurface
	for name, svc := range services {
		if name == gatewaySvc || name == adminSvc {
			continue
		}
		for k := range svc.Environment {
			if m := gatewayListenRe.FindStringSubmatch(k); m != nil {
				d := m[1]
				for _, s := range []domainSurface{
					{surface: "rest", port: gatewayPort, origin: svc.env(d + gatewayDPoPOriginSuffix)},
					{surface: "mcp", port: mcpContainerPort},
				} {
					if svc.publishes(s.port) {
						s.domain, s.svc = strings.ToLower(d), name
						out = append(out, s)
					}
				}
				// A composed binary serves its domain's admin on a port of
				// its own; a split admin is a service of its own (below).
				if svc.env(d+"_ADMIN_LISTEN_ADDR") != "" && svc.publishes(composedAdminContainerPort) {
					out = append(out, domainSurface{domain: strings.ToLower(d), surface: "admin", svc: name,
						port: composedAdminContainerPort, origin: svc.env(d + adminDPoPOriginSuffix)})
				}
				continue
			}
			if m := adminListenRe.FindStringSubmatch(k); m != nil && strings.HasSuffix(name, "-admin") && svc.publishes(adminContainerPort) {
				out = append(out, domainSurface{domain: strings.ToLower(m[1]), surface: "admin", svc: name,
					port: adminContainerPort, origin: svc.env(m[1] + adminDPoPOriginSuffix)})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key() < out[j].key() })
	return out
}

// stackSurfaces reads the merged configuration and returns domainSurfaces
// of it. A configuration that cannot be read yields none: the run then
// dials what it always did.
func stackSurfaces(files []string, root, gatewaySvc, adminSvc string, gatewayPort int) []domainSurface {
	args := []string{"compose"}
	for _, f := range files {
		args = append(args, "-f", f)
	}
	args = append(args, "--project-directory", root, "config", "--format", "json")
	out, err := outputCmd(exec.Command("docker", args...))
	if err != nil {
		return nil
	}
	var cfg struct {
		Services map[string]composeService `json:"services"`
	}
	if json.Unmarshal(out, &cfg) != nil {
		return nil
	}
	return domainSurfaces(cfg.Services, gatewaySvc, adminSvc, gatewayPort)
}

// checkSurfaceOrigins refuses an origin the runner signs for and cannot
// (checkOrigin) — only on the surfaces the runner signs on.
func checkSurfaceOrigins(surfaces []domainSurface, signs signingSurfaces) error {
	for _, s := range surfaces {
		if (s.surface == "rest" && signs.rest) || (s.surface == "admin" && signs.admin) {
			name := strings.ToUpper(s.domain) + gatewayDPoPOriginSuffix
			if s.surface == "admin" {
				name = strings.ToUpper(s.domain) + adminDPoPOriginSuffix
			}
			if err := checkOrigin(origin{name: name, value: s.origin}); err != nil {
				return err
			}
		}
	}
	return nil
}

// resolvedSurface is a domainSurface at the URL its published port
// answers on; name keys it among the endpoints the run awaits.
type resolvedSurface struct {
	domainSurface
	name, url string
}

// resolveEndpoints finds each surface's published port. A surface whose
// port cannot be resolved is reported and left out: its scenarios then go
// where they always went.
func (c *Config) resolveEndpoints(files []string, project, root string, surfaces []domainSurface) []resolvedSurface {
	var out []resolvedSurface
	for _, s := range surfaces {
		port, err := c.servicePort(files, project, root, s.svc, s.port)
		if err != nil {
			c.status("warning", fmt.Sprintf("%s serves %s's %s, and its :%d could not be resolved: %v", s.svc, s.domain, s.surface, s.port, err), nil)
			continue
		}
		u := fmt.Sprintf("http://localhost:%d", port)
		if s.surface == "mcp" {
			u += mcpEndpointPath
		}
		out = append(out, resolvedSurface{domainSurface: s, name: s.domain + " " + s.surface, url: u})
	}
	return out
}

// endpointEntries are the E2E_ENDPOINTS entries for the resolved surfaces
// (with the origins the stack declares), each at its URL in `at` — the
// await may have followed a restarted container to a new port — and those
// URLs keyed `<domain> <surface>`.
func endpointEntries(surfaces []resolvedSurface, at map[string]string) (entries []string, urls map[string]string) {
	urls = map[string]string{}
	for _, s := range surfaces {
		u := s.url
		if v, ok := at[s.name]; ok && v != "" {
			u = v
		}
		entries = append(entries, s.key()+"="+u)
		if s.origin != "" && s.surface != "mcp" {
			entries = append(entries, s.key()+"-origin="+s.origin)
		}
		urls[s.name] = u
	}
	return entries, urls
}

// endpointsEnvVar is the E2E_ENDPOINTS the suite runs with: the stack's
// entries, then the caller's own, which the runner reads last and so win.
// Empty when there is nothing to add.
func endpointsEnvVar(entries []string) string {
	if own := strings.TrimSpace(os.Getenv(endpointsEnv)); own != "" {
		entries = append(entries, own)
	}
	if len(entries) == 0 {
		return ""
	}
	return endpointsEnv + "=" + strings.Join(entries, ",")
}
