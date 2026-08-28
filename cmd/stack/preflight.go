package stack

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/wandering-compiler/w17ctl/internal/devconfig"
	"github.com/wandering-compiler/w17ctl/internal/docker"
)

// preflightPorts fails fast, with a readable error, when a host port the
// stack is about to publish is already held by a foreign process — so
// `stack up` reports "these ports are taken" up front instead of letting
// docker crash deep into bring-up with an opaque "port is already
// allocated".
//
// Ports already published by THIS project's own running containers are
// excluded, so re-running `stack up` on a live stack stays idempotent
// (docker reconciles those; they are not a conflict).
//
// `services` is the resolved compose-service selection (empty = whole
// stack). `slots` are the project's managed host-port slots (from
// SyncPorts); `p.Ports` maps each slot key to its assigned host port.
func preflightPorts(root string, p *devconfig.Project, slots []devconfig.Slot, services []string) error {
	sel := make(map[string]bool, len(services))
	for _, s := range services {
		sel[s] = true
	}

	type target struct {
		port int
		svc  string
		env  string
	}
	var targets []target
	seen := map[int]bool{}
	for _, s := range slots {
		if len(sel) > 0 && !sel[s.Service] {
			continue // starting a subset that doesn't include this service
		}
		port := p.Ports[s.Key]
		if port == 0 || seen[port] {
			continue
		}
		seen[port] = true
		targets = append(targets, target{port: port, svc: s.Service, env: s.EnvVar})
	}
	if len(targets) == 0 {
		return nil
	}

	own := ownPublishedPorts(root)

	var conflicts []string
	for _, t := range targets {
		if own[t.port] {
			continue // our own stack already publishes it — not a conflict
		}
		if devconfig.PortInUse(t.port) {
			conflicts = append(conflicts, fmt.Sprintf("  %-5d  %s  (%s)", t.port, t.svc, t.env))
		}
	}
	if len(conflicts) == 0 {
		return nil
	}
	sort.Strings(conflicts)
	return fmt.Errorf(
		"refusing to start: host port(s) already in use by another process:\n%s\n\n"+
			"free them, stop the conflicting process, or re-base this project's ports\n"+
			"with `w17ctl project ports --rebase`, then run `w17ctl stack up` again",
		strings.Join(conflicts, "\n"))
}

// ownPublishedPorts returns the set of host ports currently published by
// this project's own compose containers, via `docker compose ps --format
// json`. Best-effort: on any error (docker missing, stack down, format
// change) it returns an empty set — the caller then treats every
// in-scope port as needing a free-port check, which is the safe default.
func ownPublishedPorts(root string) map[int]bool {
	out := map[int]bool{}
	raw, err := docker.CaptureComposeFn(root, "ps", "--format", "json")
	if err != nil || len(raw) == 0 {
		return out
	}
	for _, port := range parsePublishedPorts(raw) {
		out[port] = true
	}
	return out
}

// parsePublishedPorts extracts every PublishedPort from `docker compose
// ps --format json` output. Docker emits either a single JSON array or
// newline-delimited JSON objects depending on version, so this handles
// both: it tries a whole-blob array first, then falls back to per-line
// objects.
func parsePublishedPorts(raw []byte) []int {
	type publisher struct {
		PublishedPort int `json:"PublishedPort"`
	}
	type psEntry struct {
		Publishers []publisher `json:"Publishers"`
	}
	collect := func(entries []psEntry) []int {
		var ports []int
		for _, e := range entries {
			for _, pub := range e.Publishers {
				if pub.PublishedPort > 0 {
					ports = append(ports, pub.PublishedPort)
				}
			}
		}
		return ports
	}

	// Whole-output JSON array form.
	var arr []psEntry
	if err := json.Unmarshal(raw, &arr); err == nil {
		return collect(arr)
	}

	// NDJSON form — one object per line.
	var entries []psEntry
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var e psEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue // tolerate a stray non-JSON line
		}
		entries = append(entries, e)
	}
	return collect(entries)
}
