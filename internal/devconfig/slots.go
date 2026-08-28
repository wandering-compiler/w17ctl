package devconfig

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"

	"gopkg.in/yaml.v3"
)

// Slot is one host-published, w17ctl-overridable port found in the
// project's rendered compose tree. The override knob is an env var of
// the form `${VAR:-default}` in a service's `ports:` mapping — that is
// the single source of truth for "which ports can w17ctl rewrite".
type Slot struct {
	// Key is the stable slot identifier — equal to EnvVar, so a slot's
	// assigned port survives across re-introspection.
	Key string

	// EnvVar is the environment variable that overrides the host port
	// (e.g. SHOP_GATEWAY_HOST_PORT). w17ctl sets this in the compose
	// subprocess env on `stack up`.
	EnvVar string

	// Default is the compose default (the `:-N` value), 0 if the
	// mapping used `${VAR}` with no default.
	Default int

	// Container is the in-container port the host port maps to (for
	// display only).
	Container int

	// Service is the compose service name the port belongs to.
	Service string

	// Source is the project-relative compose file the slot came from.
	Source string
}

// portVarRE matches a managed short-form port mapping, with an OPTIONAL
// leading host-interface prefix (the bind-host feature):
//
//	${VAR:-8080}:8080                          (no prefix — legacy)
//	${W17_BIND_HOST:-127.0.0.1}:${VAR:-8080}:8080   (env-driven bind host)
//	127.0.0.1:${VAR:-8080}:8080                (literal host IP)
//
// The prefix is non-capturing, so the capture groups are unchanged:
// 1=var, 2=default (optional), 3=container. The prefix alternates a
// compose `${…}` expression, a literal IPv4, or a bracketed IPv6.
var portVarRE = regexp.MustCompile(`^(?:(?:\$\{[^}]*\}|[0-9.]+|\[[0-9A-Fa-f:]+\]):)?\$\{([A-Za-z_][A-Za-z0-9_]*)(?::-(\d+))?\}:(\d+)$`)

// composeFile is the minimal slice of a compose document we read:
// service names and their short-form `ports:` lists. Long-form port
// entries (a list of maps) are not emitted by w17 codegen; a file that
// uses them fails this unmarshal and is skipped best-effort.
type composeFile struct {
	Services map[string]struct {
		Ports []string `yaml:"ports"`
	} `yaml:"services"`
}

// IntrospectSlots scans a project's rendered compose tree for managed
// host-port slots and the full set of compose service names. It reads
// `<root>/compose.w17.yaml` (infra: DBs, NATS, tracing) plus every
// `<root>/<servicesDir>/*/compose.yaml` (the per-bundle binaries).
//
// Best-effort: a compose file that is missing or unparsable is skipped
// rather than failing the scan — slots only exist once codegen has run,
// and a half-generated tree should still let the rest be managed.
// Returns slots sorted by (Source, Service, EnvVar) and service names
// sorted alphabetically.
func IntrospectSlots(root, servicesDir string) ([]Slot, []string, error) {
	if root == "" {
		return nil, nil, fmt.Errorf("devconfig: IntrospectSlots: empty root")
	}
	var files []string
	files = append(files, filepath.Join(root, "compose.w17.yaml"))
	if servicesDir != "" {
		matches, err := filepath.Glob(filepath.Join(root, servicesDir, "*", "compose.yaml"))
		if err != nil {
			return nil, nil, fmt.Errorf("devconfig: glob bundles: %w", err)
		}
		sort.Strings(matches)
		files = append(files, matches...)
	}

	var slots []Slot
	serviceSet := map[string]bool{}
	seenVar := map[string]bool{}

	for _, abs := range files {
		data, err := os.ReadFile(abs)
		if err != nil {
			continue // missing file — skip
		}
		var cf composeFile
		if err := yaml.Unmarshal(data, &cf); err != nil {
			continue // long-form ports or non-compose YAML — skip
		}
		rel, relErr := filepath.Rel(root, abs)
		if relErr != nil {
			rel = abs
		}
		// Stable per-file service order.
		names := make([]string, 0, len(cf.Services))
		for name := range cf.Services {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			serviceSet[name] = true
			for _, raw := range cf.Services[name].Ports {
				m := portVarRE.FindStringSubmatch(raw)
				if m == nil {
					continue // literal / random-host port — not w17ctl-managed
				}
				varName := m[1]
				if seenVar[varName] {
					continue // a var drives at most one slot
				}
				seenVar[varName] = true
				def, _ := strconv.Atoi(m[2]) // empty -> 0
				container, _ := strconv.Atoi(m[3])
				slots = append(slots, Slot{
					Key:       varName,
					EnvVar:    varName,
					Default:   def,
					Container: container,
					Service:   name,
					Source:    rel,
				})
			}
		}
	}

	sort.Slice(slots, func(i, j int) bool {
		if slots[i].Source != slots[j].Source {
			return slots[i].Source < slots[j].Source
		}
		if slots[i].Service != slots[j].Service {
			return slots[i].Service < slots[j].Service
		}
		return slots[i].EnvVar < slots[j].EnvVar
	})

	services := make([]string, 0, len(serviceSet))
	for name := range serviceSet {
		services = append(services, name)
	}
	sort.Strings(services)

	return slots, services, nil
}
