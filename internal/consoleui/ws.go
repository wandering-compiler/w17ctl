package consoleui

import (
	"context"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// wsPushInterval is how often the live stream re-samples docker. Two
// seconds keeps the table lively without hammering `docker stats`
// (which itself takes ~1s per sample).
const wsPushInterval = 2 * time.Second

// liveFrame is one push on the /ws stream: the project's containers, with
// live CPU/mem merged in once available. StatsReady is false on the very
// first (container-list-only) frame so the UI can show the containers
// immediately and render CPU/mem as "still loading" rather than "—"; it
// is true on every subsequent frame, which carries the `docker stats`
// sample.
type liveFrame struct {
	Project    string      `json:"project"`
	Containers []Container `json:"containers"`
	StatsReady bool        `json:"statsReady"`
	Error      string      `json:"error,omitempty"`
}

// handleWS streams a project's container list + live stats every couple
// of seconds until the client disconnects. `?project=<name>` selects the
// project; bind is loopback-only so the default same-origin check on
// Accept is the right policy.
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("project")
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		return // Accept already wrote the error response
	}
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()

	ctx := conn.CloseRead(r.Context()) // detect client close; we only write

	// Fast first frame: the container list WITHOUT `docker stats` (which
	// adds ~1s), so the table fills immediately and CPU/mem render as
	// "loading" rather than blank dashes. Every frame after carries stats.
	if err := wsjson.Write(ctx, conn, s.sampleFrame(ctx, name, false)); err != nil {
		return
	}

	ticker := time.NewTicker(wsPushInterval)
	defer ticker.Stop()

	for {
		if err := wsjson.Write(ctx, conn, s.sampleFrame(ctx, name, true)); err != nil {
			return // client gone or write failed
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// sampleFrame builds one live frame for the named project. With
// withStats=false it skips the slow `docker stats` call (the fast first
// frame); with true it merges live CPU/mem. It degrades to an error field
// (never failing the stream) when the project is unknown or docker is
// unreachable.
func (s *Server) sampleFrame(ctx context.Context, name string, withStats bool) liveFrame {
	frame := liveFrame{Project: name, StatsReady: withStats}
	cfg, err := s.loadCfg()
	if err != nil {
		frame.Error = err.Error()
		return frame
	}
	p := cfg.Projects[name]
	if p == nil {
		frame.Error = errNotRegistered(name).Error()
		return frame
	}
	containers, err := composePS(ctx, p.Path)
	if err != nil {
		frame.Error = err.Error()
		return frame
	}
	if withStats {
		if stats, serr := dockerStats(ctx); serr == nil {
			mergeStats(containers, stats)
		}
	}
	frame.Containers = containers
	return frame
}
