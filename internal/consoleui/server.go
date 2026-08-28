// Package consoleui serves w17ctl's local web app — a human-facing view and
// controller over the dev-machine-local project registry (devconfig),
// each project's lock, and the local Docker daemon. It is the GUI
// counterpart to the `w17ctl project` / `stack` CLI: the CLI is the
// surface coding agents drive, this is the surface a human clicks.
//
// It introduces no new source of truth — it reads ~/.w17/config.yaml,
// w17/lock.yaml, and `docker compose ps` / `docker stats`, and (slice 2)
// drives the same compose lifecycle the CLI drives. Bind to 127.0.0.1
// only: the local Docker socket is root-equivalent on the host.
package consoleui

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/devconfig"
	codegenpb "github.com/wandering-compiler/sdk/go/pb/w17compiler"
)

// Server is the local UI HTTP server.
type Server struct {
	addr    string
	mux     *http.ServeMux
	handler http.Handler // guard(mux) — the loopback trust boundary applied to every request

	// console is the lock-projection RPC endpoint (DescribeLock); empty →
	// resolved from W17_CONSOLE_ADDR / the compiled default. The UI treats the
	// lock as opaque bytes and asks the console for its routing projection
	// (§8.2), degrading gracefully when the console is unreachable.
	console string

	// loadCfg / saveCfg are indirected for tests; production reads/writes
	// the real devconfig at ~/.w17/config.yaml.
	loadCfg func() (*devconfig.Config, error)
	saveCfg func(*devconfig.Config) error

	// cfgMu serialises the load-modify-save cycle on the shared devconfig
	// file. net/http serves each request on its own goroutine, so without
	// this two concurrent mutating handlers each load the WHOLE config,
	// edit their slice, and write it all back — the later save clobbers the
	// earlier one (a lost update, even across unrelated projects/presets,
	// since the whole file is the unit of persistence). Holding cfgMu across
	// load→mutate→save makes that read-modify-write atomic. Save itself is
	// already crash-atomic (temp+rename); this guards the logical race the
	// rename can't.
	cfgMu sync.Mutex
}

// New builds a Server bound to addr (expected to be a loopback address).
func New(addr string) *Server {
	s := &Server{
		addr:    addr,
		mux:     http.NewServeMux(),
		loadCfg: devconfig.LoadDefault,
		saveCfg: devconfig.SaveDefault,
	}
	s.routes()
	s.handler = guard(s.mux)
	return s
}

// Addr returns the address the server is configured to listen on.
func (s *Server) Addr() string { return s.addr }

// routes wires the HTTP surface. The SPA (embedded) is served at the
// root; the JSON API lives under /api.
func (s *Server) routes() {
	s.mux.HandleFunc("GET /api/healthz", s.handleHealthz)
	s.mux.HandleFunc("GET /api/projects", s.handleProjects)
	s.mux.HandleFunc("GET /api/projects/{name}", s.handleProjectDetail)
	s.mux.HandleFunc("GET /api/projects/{name}/containers", s.handleContainers)
	s.mux.HandleFunc("GET /api/projects/{name}/config", s.handleConfig)
	s.mux.HandleFunc("GET /api/projects/{name}/fixtures", s.handleFixtures)
	s.mux.HandleFunc("GET /api/stats", s.handleStats)

	// Control actions (slice 2): all mutate via devconfig + the shared
	// compose-env runner, so the UI and CLI never diverge.
	s.mux.HandleFunc("POST /api/projects/{name}/up", s.handleUp)
	s.mux.HandleFunc("POST /api/projects/{name}/down", s.handleDown)
	s.mux.HandleFunc("POST /api/projects/{name}/presets", s.handlePresetSave)
	s.mux.HandleFunc("DELETE /api/projects/{name}/presets/{preset}", s.handlePresetDelete)
	s.mux.HandleFunc("POST /api/projects/{name}/presets/{preset}/activate", s.handlePresetActivate)
	s.mux.HandleFunc("PUT /api/projects/{name}/presets/{preset}/env", s.handlePresetEnv)

	// Live container + stats push.
	s.mux.HandleFunc("GET /ws", s.handleWS)

	s.mux.Handle("/", spaHandler())
}

// Serve listens and serves until ctx is cancelled, then shuts down
// gracefully. Returns the bound address via the optional ready callback
// (so callers can print the real port when addr used :0).
func (s *Server) Serve(ctx context.Context, ready func(addr string)) error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("ui: listen on %s: %w", s.addr, err)
	}
	s.addr = ln.Addr().String()
	if ready != nil {
		ready(s.addr)
	}
	srv := &http.Server{
		Handler:           s.handler, // guard(mux): loopback Host + Origin enforcement
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() { //nolint:gosec // G118: the shutdown deadline must derive from Background, not the already-cancelled serve ctx.
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// --- JSON helpers ---------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

// --- API payloads ---------------------------------------------------

// projectSummary is one row of GET /api/projects.
type projectSummary struct {
	Name         string `json:"name"`
	Path         string `json:"path"`
	ActivePreset string `json:"activePreset"`
	PortCount    int    `json:"portCount"`
	PresetCount  int    `json:"presetCount"`
}

// portInfo is one assigned host port with the service/container it maps to.
type portInfo struct {
	EnvVar    string `json:"envVar"`
	HostPort  int    `json:"hostPort"`
	Container int    `json:"container,omitempty"`
	Service   string `json:"service,omitempty"`
}

// presetInfo mirrors a devconfig.Preset for the wire.
type presetInfo struct {
	Name     string            `json:"name"`
	Active   bool              `json:"active"`
	Services []string          `json:"services"`
	Env      map[string]string `json:"env"`
}

// lockSummary is a small, read-only digest of a project's lock for the
// detail view.
type lockSummary struct {
	Project     string   `json:"project"`
	Connections []string `json:"connections"`
}

// projectDetail is GET /api/projects/{name}.
type projectDetail struct {
	Name         string        `json:"name"`
	Path         string        `json:"path"`
	ActivePreset string        `json:"activePreset"`
	Ports        []portInfo    `json:"ports"`
	Presets      []presetInfo  `json:"presets"`
	Services     []string      `json:"services"`
	Lock         *lockSummary  `json:"lock,omitempty"`
	Links        []ServiceLink `json:"links"`
}

// --- handlers -------------------------------------------------------

// HealthApp is the marker an `/api/healthz` response carries so the
// `w17ctl ui start` launcher can tell OUR server apart from some other
// process that happens to hold the port.
const HealthApp = "w17ctl-ui"

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "app": HealthApp})
}

func (s *Server) handleProjects(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.loadCfg()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	out := make([]projectSummary, 0, len(cfg.Projects))
	for name, p := range cfg.Projects {
		out = append(out, projectSummary{
			Name:         name,
			Path:         p.Path,
			ActivePreset: p.ActivePreset,
			PortCount:    len(p.Ports),
			PresetCount:  len(p.Presets),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleProjectDetail(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	cfg, err := s.loadCfg()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	p := cfg.Projects[name]
	if p == nil {
		writeErr(w, http.StatusNotFound, fmt.Errorf("project %q is not registered", name))
		return
	}

	// Best-effort: introspect the compose tree for service names +
	// per-port service/container mapping. A half-generated project just
	// yields fewer slots; it must not fail the detail view.
	slotByVar := map[string]devconfig.Slot{}
	var services []string
	if view := loadLockViewBestEffort(s.console, p.Path); view != nil {
		if slots, svcs, ierr := devconfig.IntrospectSlots(p.Path, view.GetServicesDir()); ierr == nil {
			services = svcs
			for _, sl := range slots {
				slotByVar[sl.EnvVar] = sl
			}
		}
	}

	portInfos := buildPortInfos(p, slotByVar)
	detail := projectDetail{
		Name:         name,
		Path:         p.Path,
		ActivePreset: p.ActivePreset,
		Ports:        portInfos,
		Presets:      buildPresetInfos(p),
		Services:     services,
		Lock:         buildLockSummary(s.console, p.Path),
		Links:        buildServiceLinks(portInfos),
	}
	writeJSON(w, http.StatusOK, detail)
}

func (s *Server) handleContainers(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	cfg, err := s.loadCfg()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	p := cfg.Projects[name]
	if p == nil {
		writeErr(w, http.StatusNotFound, fmt.Errorf("project %q is not registered", name))
		return
	}
	containers, err := composePS(r.Context(), p.Path)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	if stats, serr := dockerStats(r.Context()); serr == nil {
		mergeStats(containers, stats)
	}
	writeJSON(w, http.StatusOK, containers)
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	stats, err := dockerStats(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

// --- builders -------------------------------------------------------

func buildPortInfos(p *devconfig.Project, slotByVar map[string]devconfig.Slot) []portInfo {
	out := make([]portInfo, 0, len(p.Ports))
	for v, hp := range p.Ports {
		pi := portInfo{EnvVar: v, HostPort: hp}
		if sl, ok := slotByVar[v]; ok {
			pi.Container = sl.Container
			pi.Service = sl.Service
		}
		out = append(out, pi)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].EnvVar < out[j].EnvVar })
	return out
}

func buildPresetInfos(p *devconfig.Project) []presetInfo {
	out := make([]presetInfo, 0, len(p.Presets))
	for name, pr := range p.Presets {
		out = append(out, presetInfo{
			Name:     name,
			Active:   name == p.ActivePreset,
			Services: pr.Services,
			Env:      pr.Env,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func buildLockSummary(console, root string) *lockSummary {
	view := loadLockViewBestEffort(console, root)
	if view == nil {
		return nil
	}
	conns := make([]string, 0, len(view.GetConnections()))
	for _, c := range view.GetConnections() {
		conns = append(conns, c.GetName())
	}
	sort.Strings(conns)
	return &lockSummary{Project: view.GetProject(), Connections: conns}
}

// loadLockViewBestEffort reads <root>/w17/lock.yaml and asks the console for its
// routing projection, returning nil on any error (no lock yet, or the console
// unreachable) — the UI degrades gracefully (§8.2: the client holds no lock
// types; the console interprets the opaque bytes).
func loadLockViewBestEffort(console, root string) *codegenpb.LockView {
	view, err := core.DescribeLockFromRoot(console, root)
	if err != nil {
		return nil
	}
	return view
}
