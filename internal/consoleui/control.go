package consoleui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"

	"github.com/wandering-compiler/w17ctl/internal/devconfig"
)

// runComposeEnvFn runs `docker compose <args>` in dir with env layered on
// top of the process environment, capturing combined output for the UI.
// Indirected for tests. Mirrors the CLI's realRunComposeEnv (same
// env-injection contract) but returns the output instead of streaming to
// stdout, so the UI can show it.
var runComposeEnvFn = realRunComposeEnv

func realRunComposeEnv(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", append([]string{"compose"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

// syncPorts re-introspects a project's compose tree and (re)allocates its
// host ports in cfg — the UI mirror of the CLI's syncPorts so a freshly
// added connection/bundle gets a unique port before `up`.
func syncPorts(cfg *devconfig.Config, name, root, console string) error {
	view := loadLockViewBestEffort(console, root)
	if view == nil {
		// No lock yet (or the console is unreachable) — nothing to allocate;
		// leave any existing assignments in place.
		return nil
	}
	slots, _, err := devconfig.IntrospectSlots(root, view.GetServicesDir())
	if err != nil {
		return err
	}
	cfg.AllocatePorts(name, slots)
	return nil
}

// resolveProject loads the config and returns the named project, with a
// 404-friendly error when missing.
func (s *Server) resolveProject(name string) (*devconfig.Config, *devconfig.Project, error) {
	cfg, err := s.loadCfg()
	if err != nil {
		return nil, nil, err
	}
	p := cfg.Projects[name]
	if p == nil {
		return nil, nil, errNotRegistered(name)
	}
	return cfg, p, nil
}

type notRegisteredError struct{ name string }

func (e notRegisteredError) Error() string {
	return fmt.Sprintf("project %q is not registered", e.name)
}
func errNotRegistered(name string) error { return notRegisteredError{name} }

// respondErr writes an error response: 404 when err is a
// notRegisteredError (unregistered project), otherwise 500.
func (s *Server) respondErr(w http.ResponseWriter, err error) {
	var nre notRegisteredError
	if errors.As(err, &nre) {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	writeErr(w, http.StatusInternalServerError, err)
}

// --- up / down ------------------------------------------------------

type upRequest struct {
	Preset   string   `json:"preset"`   // empty = the project's active preset
	Services []string `json:"services"` // overrides the preset's selection
}

type actionResult struct {
	OK     bool   `json:"ok"`
	Output string `json:"output"`
	Preset string `json:"preset,omitempty"`
}

func (s *Server) handleUp(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req upRequest
	_ = decodeBody(r, &req)

	// Hold cfgMu only across the load-modify-save cycle, NOT the slow
	// `docker compose up` below — otherwise every config op would serialise
	// behind a multi-second compose run. The compose step uses the snapshot
	// (p) this critical section just persisted.
	s.cfgMu.Lock()
	cfg, p, err := s.resolveProject(name)
	if err != nil {
		s.cfgMu.Unlock()
		s.respondErr(w, err)
		return
	}

	// Re-sync host ports (a connection/bundle added since last up gets a
	// still-unique port), then persist.
	if err := syncPorts(cfg, name, p.Path, s.console); err != nil {
		s.cfgMu.Unlock()
		s.respondErr(w, err)
		return
	}
	if err := s.saveCfg(cfg); err != nil {
		s.cfgMu.Unlock()
		s.respondErr(w, err)
		return
	}
	s.cfgMu.Unlock()

	presetName := req.Preset
	if presetName == "" {
		presetName = p.ActivePreset
	}
	var preset *devconfig.Preset
	if presetName != "" {
		preset = p.Presets[presetName]
		if preset == nil {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("project %q has no preset %q", name, presetName))
			return
		}
	}

	env := devconfig.BuildUpEnv(p, preset)
	args := append([]string{"up", "-d"}, devconfig.PresetServices(req.Services, preset)...)
	out, runErr := runComposeEnvFn(r.Context(), p.Path, env, args...)
	if runErr != nil {
		writeJSON(w, http.StatusBadGateway, actionResult{OK: false, Output: out + "\n" + runErr.Error(), Preset: presetName})
		return
	}
	writeJSON(w, http.StatusOK, actionResult{OK: true, Output: out, Preset: presetName})
}

type downRequest struct {
	KeepVolumes bool `json:"keepVolumes"`
}

func (s *Server) handleDown(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req downRequest
	_ = decodeBody(r, &req)

	_, p, err := s.resolveProject(name)
	if err != nil {
		s.respondErr(w, err)
		return
	}
	args := []string{"down"}
	if !req.KeepVolumes {
		args = append(args, "-v")
	}
	out, runErr := runComposeEnvFn(r.Context(), p.Path, nil, args...)
	if runErr != nil {
		writeJSON(w, http.StatusBadGateway, actionResult{OK: false, Output: out + "\n" + runErr.Error()})
		return
	}
	writeJSON(w, http.StatusOK, actionResult{OK: true, Output: out})
}

// --- presets --------------------------------------------------------

type presetSaveRequest struct {
	Name     string            `json:"name"`
	Services []string          `json:"services"`
	Env      map[string]string `json:"env"`
}

func (s *Server) handlePresetSave(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req presetSaveRequest
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.Name == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("preset name is required"))
		return
	}
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	cfg, p, err := s.resolveProject(name)
	if err != nil {
		s.respondErr(w, err)
		return
	}
	p.SetPreset(req.Name, &devconfig.Preset{Services: req.Services, Env: req.Env})
	if err := s.saveCfg(cfg); err != nil {
		s.respondErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, buildPresetInfos(p))
}

func (s *Server) handlePresetDelete(w http.ResponseWriter, r *http.Request) {
	name, preset := r.PathValue("name"), r.PathValue("preset")
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	cfg, p, err := s.resolveProject(name)
	if err != nil {
		s.respondErr(w, err)
		return
	}
	if !p.DeletePreset(preset) {
		writeErr(w, http.StatusNotFound, fmt.Errorf("project %q has no preset %q", name, preset))
		return
	}
	if err := s.saveCfg(cfg); err != nil {
		s.respondErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, buildPresetInfos(p))
}

type activateRequest struct {
	Clear bool `json:"clear"`
}

func (s *Server) handlePresetActivate(w http.ResponseWriter, r *http.Request) {
	name, preset := r.PathValue("name"), r.PathValue("preset")
	var req activateRequest
	_ = decodeBody(r, &req)

	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	cfg, p, err := s.resolveProject(name)
	if err != nil {
		s.respondErr(w, err)
		return
	}
	if req.Clear {
		p.ClearActivePreset()
	} else if !p.Activate(preset) {
		writeErr(w, http.StatusNotFound, fmt.Errorf("project %q has no preset %q", name, preset))
		return
	}
	if err := s.saveCfg(cfg); err != nil {
		s.respondErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, buildPresetInfos(p))
}

type envRequest struct {
	Env map[string]string `json:"env"`
}

func (s *Server) handlePresetEnv(w http.ResponseWriter, r *http.Request) {
	name, preset := r.PathValue("name"), r.PathValue("preset")
	var req envRequest
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	cfg, p, err := s.resolveProject(name)
	if err != nil {
		s.respondErr(w, err)
		return
	}
	pr := p.Presets[preset]
	if pr == nil {
		writeErr(w, http.StatusNotFound, fmt.Errorf("project %q has no preset %q", name, preset))
		return
	}
	pr.Env = req.Env
	if err := s.saveCfg(cfg); err != nil {
		s.respondErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, buildPresetInfos(p))
}

// decodeBody decodes an optional JSON request body. An empty body is not
// an error (POSTs with no payload are allowed); a present-but-malformed
// body is.
func decodeBody(r *http.Request, v any) error {
	if r.Body == nil || r.ContentLength == 0 {
		return nil
	}
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("ui: parse request body: %w", err)
	}
	return nil
}
