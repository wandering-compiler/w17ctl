// Package stress wires `w17ctl stress <target>` — a thin kong adapter over
// the load runner in internal/testsuite. It parses the flags, maps them
// into a testsuite.Stress, and runs it.
package stress

import (
	"github.com/wandering-compiler/w17ctl/internal/testsuite"
)

// Cmd runs the project's generated stress/load presets against a deployed
// gateway. Unlike `test` (e2e), it owns no stack — you hand it the URL of
// an already-running target and it hammers that, so you measure a real
// deployment. One run per invocation by design (concurrency lives in each
// preset's stress.yaml plan); for a series, loop in the shell over targets.
type Cmd struct {
	Target      string `arg:"" name:"target" placeholder:"URL" help:"REST base URL of the deployed gateway to load, e.g. http://localhost:8080 (or a staging URL) — required."`
	Preset      string `name:"preset" short:"p" help:"Run only this stress preset (empty = every preset). Combine with --domain if the same preset name exists in more than one domain."`
	Domain      string `name:"domain" help:"Scope --preset to this domain; alone, run every preset in the domain (empty = any/all)."`
	Concurrency int    `name:"concurrency" short:"c" help:"Override every preset's worker count (0 = use each plan's value)."`
	Duration    string `name:"duration" short:"d" placeholder:"DUR" help:"Override every preset's run window, e.g. 30s (empty = use each plan's value)."`
	Total       int    `name:"total" short:"n" help:"Override every preset's total_requests (0 = use each plan's value)."`
	Verbose     bool   `name:"verbose" short:"v" help:"Per-preset framework output around the throughput/latency report."`
	Image       string `name:"image" default:"golang:1.26-alpine" help:"Builder image used to cross-compile the runner."`
	GomodVolume string `name:"gomod-volume" default:"w17ctl-gomod" help:"Named Docker volume for the Go module cache (shared across runs)."`
	Console     string `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console (owns the lock — read for the services dir). Optional — falls back to the binary's compile-time default."`
}

// runStress is the seam tests stub; production delegates to the real
// testsuite load lifecycle (Stress.Run, which builds + runs the runner).
var runStress = func(s *testsuite.Stress) error { return s.Run() }

func (c *Cmd) Run() error {
	return runStress(&testsuite.Stress{
		Target:      c.Target,
		Preset:      c.Preset,
		Domain:      c.Domain,
		Concurrency: c.Concurrency,
		Duration:    c.Duration,
		Total:       c.Total,
		Verbose:     c.Verbose,
		Image:       c.Image,
		GomodVolume: c.GomodVolume,
		Console:     c.Console,
	})
}
