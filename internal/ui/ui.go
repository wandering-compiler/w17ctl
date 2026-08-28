// Package ui implements `w17ctl ui` — the local web app launcher. It is
// the w17ctl-side control plane (start/stop a detached background
// process, browser open, a ~/.w17/ui.json state file) over the actual
// web server, which lives in w17ctl/internal/consoleui. The thin cmd/ui
// layer parses flags and calls Start / Stop here.
//
// The server binds 127.0.0.1 only — the local Docker socket is
// root-equivalent on the host, so the UI must never be reachable off
// the machine.
package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/wandering-compiler/w17ctl/internal/consoleui"
	"github.com/wandering-compiler/w17ctl/internal/devconfig"
)

// Start runs `ui start`. With foreground=true it serves the blocking
// server in this process (the daemonised child); otherwise it launches a
// detached child and opens the browser. Idempotent: if a server already
// answers on the port it just opens the window.
func Start(out io.Writer, port int, noOpen, foreground bool) error {
	if foreground {
		return serveForeground(port)
	}
	return launchBackground(out, port, noOpen)
}

// Stop stops the background UI server.
func Stop(out io.Writer) error {
	st, err := readUIState()
	if err != nil {
		fmt.Fprintln(out, "w17ctl ui is not running")
		return nil
	}
	// Confirm the recorded server is actually OUR UI before signalling its
	// PID. After an ungraceful exit / reboot the state file persists with a
	// stale PID that the OS may have recycled onto an unrelated process, and
	// blindly SIGTERM-ing it would kill a bystander. probeUI checks the
	// /api/healthz marker on the recorded address, so we only ever signal a
	// live w17ctl-ui.
	if !probeUIFn(st.Addr) {
		_ = removeUIState()
		fmt.Fprintln(out, "w17ctl ui is not running (stale state cleared)")
		return nil
	}
	proc, err := os.FindProcess(st.PID)
	if err != nil {
		_ = removeUIState()
		fmt.Fprintln(out, "w17ctl ui is not running (stale state cleared)")
		return nil
	}
	if err := terminate(proc); err != nil {
		_ = removeUIState()
		fmt.Fprintln(out, "w17ctl ui is not running (stale state cleared)")
		return nil
	}
	fmt.Fprintf(out, "stopped w17ctl ui (pid %d, was on %s)\n", st.PID, st.Addr)
	return nil
}

// serveForeground is the actual server process (the daemonised child, or
// a direct --foreground run). It writes the state file once listening
// and removes it on shutdown so `ui stop` knows the pid + address.
func serveForeground(port int) error {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	srv := consoleui.New(addr)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	ready := func(bound string) {
		_ = writeUIState(uiState{PID: os.Getpid(), Addr: bound})
	}
	err := srv.Serve(ctx, ready)
	_ = removeUIState()
	return err
}

// launchBackground is the user-facing `ui start`: idempotent. If a
// server already answers on the port, it just opens the browser;
// otherwise it spawns a detached child running --foreground, waits for
// it to listen, and opens the browser.
func launchBackground(out io.Writer, port int, noOpen bool) error {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	url := "http://" + addr

	if probeUI(addr) {
		fmt.Fprintf(out, "w17ctl ui already running on %s\n", url)
		return maybeOpen(out, url, noOpen)
	}

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate w17ctl binary: %w", err)
	}
	logPath, logFile, err := openUILog()
	if err != nil {
		return err
	}
	defer func() {
		// Surface a close failure on the writable log handle rather than
		// dropping it silently. Best-effort: the child keeps its own fd,
		// so this only closes the parent's copy.
		if cerr := logFile.Close(); cerr != nil {
			fmt.Fprintf(out, "warning: closing UI log %q: %v\n", logPath, cerr)
		}
	}()

	cmd := exec.Command(exe, "ui", "start", "--foreground", "--port", strconv.Itoa(port))
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = detachAttrs() // detach from this terminal/session
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start background UI: %w", err)
	}
	_ = cmd.Process.Release()

	// Poll until it is listening (or give up and point at the log).
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if probeUI(addr) {
			fmt.Fprintf(out, "w17ctl ui started on %s\n", url)
			return maybeOpen(out, url, noOpen)
		}
		time.Sleep(150 * time.Millisecond)
	}
	return fmt.Errorf("UI did not come up within 8s — see %s", logPath)
}

func maybeOpen(out io.Writer, url string, noOpen bool) error {
	if noOpen {
		return nil
	}
	if err := openBrowserFn(url); err != nil {
		fmt.Fprintf(out, "(could not open a browser automatically: %v)\n", err)
	}
	return nil
}

// --- state file ----------------------------------------------------

// uiState is the small ~/.w17/ui.json record the running server writes
// so `ui stop` can find it and `ui start` can detect an existing one.
type uiState struct {
	PID  int    `json:"pid"`
	Addr string `json:"addr"`
}

func uiStatePath() (string, error) {
	dir, err := devconfig.DefaultDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "ui.json"), nil
}

func writeUIState(st uiState) error {
	path, err := uiStatePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func readUIState() (uiState, error) {
	var st uiState
	path, err := uiStatePath()
	if err != nil {
		return st, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return st, err
	}
	if st.PID == 0 {
		return st, fmt.Errorf("ui: empty state")
	}
	return st, nil
}

func removeUIState() error {
	path, err := uiStatePath()
	if err != nil {
		return err
	}
	err = os.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func openUILog() (string, *os.File, error) {
	dir, err := devconfig.DefaultDir()
	if err != nil {
		return "", nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", nil, err
	}
	path := filepath.Join(dir, "ui.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644) //nolint:gosec // G302: UI log is a readable status file (0644 by design), holds no secrets.
	if err != nil {
		return "", nil, fmt.Errorf("open UI log: %w", err)
	}
	return path, f, nil
}

// probeUIFn is indirected for tests (Stop's "is this PID really our server"
// guard); production uses the real probeUI.
var probeUIFn = probeUI

// probeUI reports whether OUR UI server is answering on addr — a 200
// from /api/healthz carrying the app marker. A short timeout keeps
// `ui start` snappy; a foreign server on the port fails the marker.
func probeUI(addr string) bool {
	client := &http.Client{Timeout: 400 * time.Millisecond}
	resp, err := client.Get("http://" + addr + "/api/healthz")
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
	return strings.Contains(string(body), consoleui.HealthApp)
}

// openBrowserFn is indirected for tests.
var openBrowserFn = openBrowser

// openBrowser opens url in the platform default browser. Best-effort.
func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default: // linux, *bsd
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start()
}
