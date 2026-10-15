package testsuite

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Device proofs (DPoP, RFC 9449) name the URL their client called, and a
// gateway whose auth method binds tokens to device keys compares that with
// its configured public origin, <PREFIX>_DPOP_ORIGIN — never with the Host
// header it is sent. The suite reaches the gateway at a port this run
// published itself, which no configured origin names (the generated
// .env.defaults says http://localhost:8080, the local stack's port). Moving
// the runner into the compose network would not help: the gateway's service
// name is no more its configured origin than the published port is.
//
// So the run keeps its ports and hands the runner the configured origins
// instead: a runner generated for a binding surface signs for them
// (dpope2e.WithOrigin) while it dials the published ports. They are read
// from the stack's merged configuration — the gateway's .env (copied from
// .env.defaults) with every override applied — so a project that moved its
// origin is followed. A runner for a surface that binds nothing reads
// neither variable.
const (
	dpopOriginEnv      = "E2E_DPOP_ORIGIN"
	adminDPoPOriginEnv = "E2E_ADMIN_DPOP_ORIGIN"

	// The variables' suffixes: <DOMAIN>_GATEWAY_DPOP_ORIGIN for the REST
	// surface (the rpc surface's is <DOMAIN>_GATEWAY_RPC_DPOP_ORIGIN, which
	// the runner does not dial) and <DOMAIN>_ADMIN_DPOP_ORIGIN for the admin.
	gatewayDPoPOriginSuffix = "_GATEWAY_DPOP_ORIGIN"
	adminDPoPOriginSuffix   = "_ADMIN_DPOP_ORIGIN"

	// The generated runner records which surfaces sign in these constants
	// (e2erunner.ftmpl); a runner that signs nothing declares neither.
	signsRESTConst  = "signsREST"
	signsAdminConst = "signsAdmin"
)

// origin is one configured public origin and the variable that set it.
// conflict is set when two variables for the surface disagree: then there
// is no value, and a suite that signs for the surface cannot run.
type origin struct {
	name, value string
	conflict    error
}

// originConflictError is two variables naming different origins for one
// surface.
type originConflictError struct{ a, b origin }

func (e *originConflictError) Error() string {
	return fmt.Sprintf("%s=%s and %s=%s disagree, and the suite dials one gateway", e.a.name, e.a.value, e.b.name, e.b.value)
}

// dpopOrigins returns the public origins the gateway service and the admin
// declare in the merged configuration: the admin's from its own service on
// a split stack (adminSvc), else from the gateway service, which a composed
// binary's admin shares. A zero origin is one the stack does not declare (a
// surface that binds nothing); one whose variables disagree carries the
// conflict. The error is a configuration that cannot be read at all.
func dpopOrigins(files []string, root, gatewaySvc, adminSvc string) (rest, admin origin, err error) {
	args := []string{"compose"}
	for _, f := range files {
		args = append(args, "-f", f)
	}
	args = append(args, "--project-directory", root, "config", "--format", "json")
	out, err := outputCmd(exec.Command("docker", args...))
	if err != nil {
		return origin{}, origin{}, fmt.Errorf("docker compose config: %w", err)
	}
	var cfg struct {
		Services map[string]struct {
			Environment map[string]*string `json:"environment"`
		} `json:"services"`
	}
	if err := json.Unmarshal(out, &cfg); err != nil {
		return origin{}, origin{}, fmt.Errorf("parse docker compose config: %w", err)
	}
	adminFrom := gatewaySvc
	if adminSvc != "" {
		adminFrom = adminSvc
	}
	return envBySuffix(cfg.Services[gatewaySvc].Environment, gatewayDPoPOriginSuffix),
		envBySuffix(cfg.Services[adminFrom].Environment, adminDPoPOriginSuffix), nil
}

// envBySuffix is the variable of env whose name ends in suffix; zero when
// there is none, and a conflict when two such variables disagree (no way to
// tell which surface is the one dialled).
func envBySuffix(env map[string]*string, suffix string) origin {
	names := make([]string, 0, len(env))
	for k := range env {
		names = append(names, k)
	}
	sort.Strings(names)
	var found origin
	for _, k := range names {
		v := env[k]
		if !strings.HasSuffix(k, suffix) || v == nil || *v == "" {
			continue
		}
		o := origin{name: k, value: *v}
		if found.value != "" && found.value != *v {
			return origin{conflict: &originConflictError{a: found, b: o}}
		}
		found = o
	}
	return found
}

// checkOrigin refuses an origin the runner cannot sign for. A proof's htu
// is the origin followed by the request path, and the runner's signer takes
// scheme://host[:port] alone: with a base path (a gateway served under
// https://example.com/api) it refuses every signed request, and dropping the
// path would sign for the wrong URL. Saying so here costs one sentence;
// letting the suite run costs a failure at every step that names nothing.
func checkOrigin(o origin) error {
	if o.value == "" {
		return nil
	}
	u, err := url.Parse(o.value)
	switch {
	case err != nil || u.Scheme == "" || u.Host == "":
		return fmt.Errorf("%s=%q is not an origin (scheme://host[:port]) — the e2e suite's device proofs cannot name it", o.name, o.value)
	case (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "":
		return fmt.Errorf("%s=%q has a base path, and the e2e suite signs device proofs for scheme://host[:port] alone — "+
			"a gateway served under a base path needs a newer SDK, whose suite signer takes one; until then run the suite "+
			"against an origin without a path", o.name, o.value)
	}
	return nil
}

// signingSurfaces says which surfaces of the generated runner sign device
// proofs.
type signingSurfaces struct{ rest, admin bool }

// runnerSurfaces reads the surfaces the generated runner in runnerDir signs
// for from the constants it declares; a runner that signs nothing declares
// none.
func runnerSurfaces(runnerDir string) signingSurfaces {
	main, err := os.ReadFile(filepath.Join(runnerDir, "main_test.go"))
	if err != nil {
		return signingSurfaces{}
	}
	is := func(name string) bool {
		return regexp.MustCompile(`(?m)^\s*` + name + `\s*=\s*true\s*$`).Match(main)
	}
	return signingSurfaces{rest: is(signsRESTConst), admin: is(signsAdminConst)}
}

// deviceOrigins resolves the origins the suite signs for. For each surface
// the suite signs on, it refuses an origin the suite cannot sign for (a
// base path) and two variables that disagree, and warns when the stack
// declares none; a caller's own E2E_* variable for the surface overrides the
// stack's and is the one checked. A configuration that cannot be read is a
// warning: the suite then signs for the addresses it dials.
func (c *Config) deviceOrigins(files []string, root, runnerDir, gatewaySvc, adminSvc string) (rest, admin origin, err error) {
	signs := runnerSurfaces(runnerDir)
	rest, admin, readErr := dpopOrigins(files, root, gatewaySvc, adminSvc)
	if readErr != nil {
		c.status("warning", "could not read the DPoP origins from the stack: "+readErr.Error(), nil)
	}
	adminFrom := gatewaySvc
	if adminSvc != "" {
		adminFrom = adminSvc
	}
	for _, s := range []struct {
		o            *origin
		signs        bool
		env, suffix  string
		svc, surface string
	}{
		{&rest, signs.rest, dpopOriginEnv, gatewayDPoPOriginSuffix, gatewaySvc, "gateway"},
		{&admin, signs.admin, adminDPoPOriginEnv, adminDPoPOriginSuffix, adminFrom, "admin"},
	} {
		if v := os.Getenv(s.env); v != "" {
			*s.o = origin{name: s.env, value: v}
		}
		if !s.signs {
			continue // nothing signs for this surface: its origin is not used
		}
		switch {
		case s.o.conflict != nil:
			return origin{}, origin{}, fmt.Errorf("%w — the e2e suite signs device proofs for the %s and cannot tell which origin it serves; "+
				"make them agree, or set %s to the one the proofs should name", s.o.conflict, s.surface, s.env)
		case s.o.value == "" && readErr == nil:
			c.status("warning", fmt.Sprintf("the suite signs device proofs (DPoP) for the %s, but the %s service declares no <DOMAIN>%s — "+
				"its proofs name the address this run dials, which a gateway configured with another origin refuses; "+
				"set %s to the %s's public origin", s.surface, s.svc, s.suffix, s.env, s.surface), nil)
		}
		if err := checkOrigin(*s.o); err != nil {
			return origin{}, origin{}, err
		}
	}
	return rest, admin, nil
}

// dpopEnv is what the suite's environment gains: each origin found, unless
// the caller's own environment already names it.
func dpopEnv(rest, admin string) []string {
	var env []string
	for _, kv := range [][2]string{{dpopOriginEnv, rest}, {adminDPoPOriginEnv, admin}} {
		if kv[1] != "" && os.Getenv(kv[0]) == "" {
			env = append(env, kv[0]+"="+kv[1])
		}
	}
	return env
}
