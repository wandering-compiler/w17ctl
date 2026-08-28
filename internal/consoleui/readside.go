package consoleui

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// --- service clickthroughs ------------------------------------------

// ServiceLink is a friendly, typed link to a running service derived from
// a host-port slot — the "jump to the admin / gateway / Jaeger" affordance.
type ServiceLink struct {
	Label   string `json:"label"`
	URL     string `json:"url"`
	Kind    string `json:"kind"` // gateway | mcp | admin | trace | db | bus | other
	Service string `json:"service"`
	Web     bool   `json:"web"` // browser-navigable (http) vs raw TCP (db/bus)
}

// buildServiceLinks classifies each port slot into a typed, labelled link.
// Classification is by container port + service name — the same heuristics
// a developer applies when reading a compose file. Only http surfaces are
// marked Web (clickable); DB/NATS ports are shown for reference.
func buildServiceLinks(ports []portInfo) []ServiceLink {
	links := make([]ServiceLink, 0, len(ports))
	for _, p := range ports {
		label, kind, web := classifyPort(p.Service, p.Container)
		links = append(links, ServiceLink{
			Label:   label,
			URL:     "http://localhost:" + strconv.Itoa(p.HostPort),
			Kind:    kind,
			Service: p.Service,
			Web:     web,
		})
	}
	// Stable, useful order: web surfaces first (admin, gateway, mcp,
	// trace), then infra; alphabetical within a group.
	sort.SliceStable(links, func(i, j int) bool {
		if links[i].Web != links[j].Web {
			return links[i].Web // web first
		}
		return links[i].Label < links[j].Label
	})
	return links
}

func classifyPort(service string, container int) (label, kind string, web bool) {
	s := strings.ToLower(service)
	switch {
	case strings.Contains(s, "admin"):
		return "Admin UI", "admin", true
	case strings.Contains(s, "gateway") && container == 8081:
		return "MCP", "mcp", true
	case strings.Contains(s, "gateway"):
		return "REST / OpenAPI", "gateway", true
	case strings.Contains(s, "tracing") || container == 16686:
		return "Jaeger (traces)", "trace", true
	case container == 5432 || strings.Contains(s, "postgres"):
		return "Postgres", "db", false
	case container == 6379 || strings.Contains(s, "redis"):
		return "Redis", "db", false
	case strings.Contains(s, "nats"):
		return "NATS", "bus", false
	default:
		if service == "" {
			return "service", "other", true
		}
		return service, "other", true
	}
}

// --- config preview -------------------------------------------------

// configPreview is GET /api/projects/{name}/config — the read-only
// project digest: the raw signed lock, the AI project-map overview, and
// AGENTS.md. All best-effort; a missing file yields an empty string.
type configPreview struct {
	LockYAML string `json:"lockYaml"`
	MapIndex string `json:"mapIndex"`
	Agents   string `json:"agents"`
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	_, p, err := s.resolveProject(name)
	if err != nil {
		s.respondErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, configPreview{
		LockYAML: readFileBestEffort(filepath.Join(p.Path, "w17", "lock.yaml")),
		MapIndex: readFileBestEffort(filepath.Join(p.Path, "w17", "map", "index.map")),
		Agents:   readFileBestEffort(filepath.Join(p.Path, "w17", "AGENTS.md")),
	})
}

func readFileBestEffort(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}

// --- fixtures -------------------------------------------------------

// fixtureRow is one seeded record, rendered human-readably (the model,
// its primary key, and its fields).
type fixtureRow struct {
	PK     any            `json:"pk"`
	Fields map[string]any `json:"fields"`
}

// fixtureModel groups a fixture file's rows by their `model`.
type fixtureModel struct {
	Model string       `json:"model"`
	Rows  []fixtureRow `json:"rows"`
}

// fixtureFile is one parsed fixtures JSON file.
type fixtureFile struct {
	Connection string         `json:"connection"`
	File       string         `json:"file"`
	Models     []fixtureModel `json:"models"`
}

// djangoFixture is the on-disk shape (Django-style dumpdata) w17 seeds use.
type djangoFixture struct {
	Model  string         `json:"model"`
	PK     any            `json:"pk"`
	Fields map[string]any `json:"fields"`
}

func (s *Server) handleFixtures(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	_, p, err := s.resolveProject(name)
	if err != nil {
		s.respondErr(w, err)
		return
	}
	files, _ := filepath.Glob(filepath.Join(p.Path, "fixtures", "*", "*.json"))
	sort.Strings(files)
	out := make([]fixtureFile, 0, len(files))
	for _, abs := range files {
		ff, ok := parseFixtureFile(p.Path, abs)
		if ok {
			out = append(out, ff)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// parseFixtureFile reads one fixtures JSON file and groups its records by
// model, preserving file order within each model. Returns ok=false for an
// unreadable/non-array file (skipped best-effort).
func parseFixtureFile(root, abs string) (fixtureFile, bool) {
	data, err := os.ReadFile(abs)
	if err != nil {
		return fixtureFile{}, false
	}
	var recs []djangoFixture
	if err := json.Unmarshal(data, &recs); err != nil {
		return fixtureFile{}, false
	}
	rel, relErr := filepath.Rel(root, abs)
	if relErr != nil {
		rel = abs
	}
	conn := filepath.Base(filepath.Dir(abs)) // fixtures/<conn>/<file>.json

	order := []string{}
	byModel := map[string][]fixtureRow{}
	for _, rec := range recs {
		if _, seen := byModel[rec.Model]; !seen {
			order = append(order, rec.Model)
		}
		byModel[rec.Model] = append(byModel[rec.Model], fixtureRow{PK: rec.PK, Fields: rec.Fields})
	}
	models := make([]fixtureModel, 0, len(order))
	for _, m := range order {
		models = append(models, fixtureModel{Model: m, Rows: byModel[m]})
	}
	return fixtureFile{Connection: conn, File: rel, Models: models}, true
}
