package testsuite

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// A container that restarts gets a NEW host port.
//
// Every published port of a `w17ctl test` stack is dynamic (analyzeCompose
// remaps them so parallel runs never collide), and Docker hands out a fresh
// one each time the container starts. The endpoints were resolved ONCE, right
// after `up --wait`; a gateway that crashed and came back a second later was
// then being probed on a port nothing listens on any more, and the run ended
// with "the stack did not come up, so NO test ran" — about a stack that had
// come up, gone away and come back, on a port the message did not name.
//
// So a failed connection asks docker again where the endpoint is now (a
// bounded number of times), and a run that gives up says what became of the
// SERVICE — exited with a code, restarting, unhealthy — rather than blaming a
// stack that never started.

// endpointProbe re-asks the running stack about a discovered endpoint. Nil
// on Config means "no stack to ask" (an explicit URL, or a unit test), and
// every caller treats it that way.
type endpointProbe interface {
	// resolve answers the endpoint's CURRENT URL, from docker. An error means
	// docker could not say (the service is not running, say) — not that the
	// URL is unchanged.
	resolve(name string) (string, error)
	// diagnose describes the service behind the endpoint when it is NOT
	// running normally ("service "gw" is exited with code 137 …"), and is ""
	// when it is up or nothing can be said.
	diagnose(name string) string
}

// endpointResolveEvery is the gap between two re-resolutions of a failing
// endpoint: each is a `docker compose port` round trip, so they are spaced,
// and they continue for the WHOLE wait window — a container that takes ten
// seconds to come back is found on its new port at the tenth.
//
// endpointMoveMax bounds how many times one wait FOLLOWS a move. Only a move
// counts: an answer naming the same port, or docker unable to say (the
// container is between its stop and its start), is the restart still in
// progress, not a step of a loop. Counted that way, five asks in the first
// five seconds of a fifteen-second window used the budget up before the
// container was back. A port that keeps moving is a restart loop, which the
// diagnosis then names. Vars so the tests can shorten them.
var (
	endpointResolveEvery = time.Second
	endpointMoveMax      = 5
)

// discoveredEndpoint is where discovery found an endpoint: which service, on
// which container port, under which path. Only DISCOVERED endpoints can be
// re-resolved; an explicit --mcp / --admin URL is the operator's and is
// probed as given.
type discoveredEndpoint struct {
	svc           string
	containerPort int
	path          string
}

// composeProbe is the endpointProbe over a running compose project.
type composeProbe struct {
	c         *Config
	files     []string
	project   string
	root      string
	endpoints map[string]discoveredEndpoint
}

func (p *composeProbe) resolve(name string) (string, error) {
	ep, ok := p.endpoints[name]
	if !ok {
		return "", fmt.Errorf("%s was not discovered", name)
	}
	port, _, err := p.c.servicePortOnce(p.files, p.project, p.root, ep.svc, ep.containerPort)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("http://localhost:%d%s", port, ep.path), nil
}

func (p *composeProbe) diagnose(name string) string {
	ep, ok := p.endpoints[name]
	if !ok {
		return ""
	}
	full := []string{"compose"}
	for _, f := range p.files {
		full = append(full, "-f", f)
	}
	full = append(full, "-p", p.project, "--project-directory", p.root)
	ps, err := outputCmd(exec.Command("docker", append(append([]string{}, full...), "ps", "-a", "--format", "json")...))
	if err != nil {
		return ""
	}
	state, known := serviceState(ps, ep.svc)
	switch {
	case !known:
		// The service is not in the project at all any more: its container
		// was removed (a `down`, a `docker rm` from outside).
		if len(parsePsRows(ps)) == 0 {
			return fmt.Sprintf("the compose project %q has no containers left — the stack was removed while the run was using it", p.project)
		}
		return fmt.Sprintf("service %q has no container left in compose project %q — it was removed while the run was using it", ep.svc, p.project)
	case state == "":
		return "" // running normally: the endpoint itself is the problem
	}
	msg := fmt.Sprintf("service %q is %s", ep.svc, state)
	logs, lerr := outputCmd(exec.Command("docker", append(append([]string{}, full...), "logs", "--no-color", "--no-log-prefix", "--tail", fmt.Sprint(notReadyLogTail), ep.svc)...))
	if lerr == nil && len(bytes.TrimSpace(logs)) > 0 {
		var b strings.Builder
		fmt.Fprintf(&b, "%s; the last %d lines of its log:\n", msg, notReadyLogTail)
		for _, line := range strings.Split(strings.TrimRight(string(logs), "\n"), "\n") {
			b.WriteString("  " + line + "\n")
		}
		return strings.TrimRight(b.String(), "\n")
	}
	return msg
}

// psRow is one service row of `docker compose ps --format json`.
type psRow struct {
	Service  string
	State    string
	Health   string
	ExitCode int
}

// parsePsRows reads `docker compose ps --format json` — one JSON object per
// line, or one array on older Compose. Unparseable input yields no rows.
func parsePsRows(ps []byte) []psRow {
	var rows []psRow
	if trimmed := bytes.TrimSpace(ps); bytes.HasPrefix(trimmed, []byte("[")) {
		_ = json.Unmarshal(trimmed, &rows)
	} else {
		for _, line := range bytes.Split(trimmed, []byte("\n")) {
			var r psRow
			if json.Unmarshal(line, &r) == nil && r.Service != "" {
				rows = append(rows, r)
			}
		}
	}
	return rows
}

// notUpState describes a row that is not up — "exited with code 137",
// "restarting", "running (unhealthy)" — or "" for a row that is: running
// (and healthy, when it has a healthcheck), or a one-shot that exited 0.
func notUpState(r psRow) string {
	switch {
	case r.State == "running" && (r.Health == "" || r.Health == "healthy"):
		return ""
	case r.State == "exited" && r.ExitCode == 0:
		return ""
	}
	state := r.State
	if r.Health != "" {
		state += " (" + r.Health + ")"
	}
	if r.State == "exited" {
		state += fmt.Sprintf(" with code %d", r.ExitCode)
	}
	return state
}

// serviceState is notUpState for one service; known is false when the
// project has no container for it. A service with several replicas reports
// the first that is not up, in a stable order.
func serviceState(ps []byte, svc string) (state string, known bool) {
	var states []string
	for _, r := range parsePsRows(ps) {
		if r.Service != svc {
			continue
		}
		known = true
		if s := notUpState(r); s != "" {
			states = append(states, s)
		}
	}
	sort.Strings(states)
	if len(states) > 0 {
		return states[0], true
	}
	return "", known
}

// stackChangedDuringRun is what a FAILED suite run appends when the stack
// under it moved: an endpoint whose port is not the one the suite was given
// (its container restarted mid-run), or a service that is no longer up. The
// failures after that point are the stack's, and a list of ✗ alone reads as
// broken tests. "" when nothing changed — the failures are the tests'.
func stackChangedDuringRun(probe endpointProbe, endpoints map[string]string) string {
	if probe == nil {
		return ""
	}
	names := make([]string, 0, len(endpoints))
	for name, u := range endpoints {
		if u != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	var notes []string
	for _, name := range names {
		if d := probe.diagnose(name); d != "" {
			notes = append(notes, fmt.Sprintf("the %s endpoint went away during the run: %s", name, d))
			continue
		}
		if now, err := probe.resolve(name); err == nil && now != endpoints[name] {
			notes = append(notes, fmt.Sprintf("the %s endpoint moved from %s to %s during the run — its container restarted, "+
				"so the checks that failed after the restart were talking to a port nothing served", name, endpoints[name], now))
		}
	}
	if len(notes) == 0 {
		return ""
	}
	return "the stack changed under the suite (this is not a test failure):\n  " + strings.Join(notes, "\n  ")
}
