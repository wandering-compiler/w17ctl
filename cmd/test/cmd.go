// Package test wires `w17ctl test` — a thin kong adapter over the e2e
// suite runner in internal/testsuite. It parses the flags, maps them
// into a testsuite.Config, and runs it.
package test

import (
	"github.com/wandering-compiler/w17ctl/internal/testsuite"
)

// Cmd runs the project's generated e2e suite against an EPHEMERAL,
// self-owned stack: it brings the project's OWN compose.yaml up (unique
// project name + dynamic host ports so runs never collide), discovers
// the gateway's assigned host port, builds the e2erunner as a host
// binary (cross-compiled in Docker — no local Go needed), runs it
// against http://localhost:<port> as a true external client, and tears
// the stack down.
type Cmd struct {
	ComposeFile    string `name:"compose-file" placeholder:"PATH" help:"Compose file that launches the stack (default: compose.yaml at the project root)."`
	Project        string `name:"project" help:"COMPOSE_PROJECT_NAME for the run (default: <compose-name>-e2e-<random> so runs never collide)."`
	GatewayService string `name:"gateway-service" help:"Compose service that serves the REST gateway (default: the service whose name ends in -gateway)."`
	GatewayPort    int    `name:"gateway-port" default:"8080" help:"Container port the gateway listens on (the host port is assigned dynamically + discovered)."`
	Format         string `name:"format" enum:"text,json" default:"text" help:"Output: text (human ✓/✗ checklist + progress) or json (NDJSON — lifecycle status records + one result record per step, for machine consumption)."`
	Mcp            string `name:"mcp" help:"MCP endpoint URL (set when running MCP scenarios)."`
	Admin          string `name:"admin" help:"Admin bundle endpoint URL (default: auto-discovered from the -admin compose service)."`
	Domain         string `name:"domain" help:"Run only this domain (empty = all)."`
	Transport      string `name:"transport" help:"Run only this transport: rest|mcp|admin (empty = all)."`
	Verbose        bool   `name:"verbose" help:"Per-step progress output (passes -test.v to the suite)."`
	Image          string `name:"image" default:"golang:1.26-alpine" help:"Builder image used to cross-compile the suite."`
	GomodVolume    string `name:"gomod-volume" default:"w17ctl-gomod" help:"Named Docker volume for the Go module cache (shared across runs)."`
	Keep           bool   `name:"keep" help:"Leave the stack running after the suite (skip teardown) for debugging."`
	Timeout        int    `name:"timeout" default:"300" help:"Seconds to wait for the stack to become healthy (docker compose up --wait-timeout)."`
	Console        string `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console (owns the lock — read for the e2e + services dirs). Optional — falls back to the binary's compile-time default."`
}

// runConfig is the seam tests stub; production delegates to the real
// testsuite lifecycle (Config.Run, which shells out to docker).
var runConfig = func(cfg *testsuite.Config) error { return cfg.Run() }

func (c *Cmd) Run() error {
	return runConfig(&testsuite.Config{
		ComposeFile:    c.ComposeFile,
		Project:        c.Project,
		GatewayService: c.GatewayService,
		GatewayPort:    c.GatewayPort,
		Format:         c.Format,
		Mcp:            c.Mcp,
		Admin:          c.Admin,
		Domain:         c.Domain,
		Transport:      c.Transport,
		Verbose:        c.Verbose,
		Image:          c.Image,
		GomodVolume:    c.GomodVolume,
		Keep:           c.Keep,
		Timeout:        c.Timeout,
		Console:        c.Console,
	})
}
