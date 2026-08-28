// Package ui wires `w17ctl ui` — a thin kong adapter over the launcher
// implementation in internal/ui. The UI is the local web app: a
// human-facing view and controller over the same project registry,
// presets, and local Docker stacks the `project` / `stack` CLI drives.
package ui

// Cmd is `w17ctl ui`. `start` runs the UI in the background (idempotent
// — re-running just opens the browser); `stop` tears it down. A bare
// `w17ctl ui` defaults to `start`.
type Cmd struct {
	Start StartCmd `cmd:"" default:"withargs" help:"Start the UI in the background, then open the browser. Idempotent: if it is already running, just opens the window. This is the default for a bare 'w17ctl ui'."`
	Stop  StopCmd  `cmd:"" help:"Stop the background UI server."`
}
