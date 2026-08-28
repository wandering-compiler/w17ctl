package core

import (
	"fmt"
)

// DefaultConsoleAddr is the compile-time default w17ctl falls back to
// when the user passes no `--console` flag, no `W17_CONSOLE_ADDR` env
// var, and is not logged into any console.
//
// The lock's `w17_url` is NOT part of that chain — see ResolveConsoleAddr.
//
// Build pipelines inject the appropriate value per environment:
//
//	go build -ldflags "-X .../w17ctl/internal/core.DefaultConsoleAddr=localhost:13443" ...          # local dev (Caddy TLS terminator)
//	go build -ldflags "-X .../w17ctl/internal/core.DefaultConsoleAddr=console.example.com:443" ...  # deployed (LB TLS)
//
// w17ctl ALWAYS dials the console over TLS (see core.ConsoleTransportCreds).
// The console gateway itself stays a plain h2c listener; TLS is terminated in
// front of it — a load balancer with a real cert in production, the dev
// compose's `console-tls` Caddy with the self-signed dev cert locally. The
// address may carry an http(s)/grpc(s) scheme for readability; it is stripped
// for the gRPC target and the transport is TLS regardless.
//
// Empty == no compiled-in default; the resolver returns an actionable
// error pointing at every override surface.
var DefaultConsoleAddr string

// ResolveConsoleAddr walks the resolution chain so the user can run
// w17ctl without an explicit --console flag in the common case. Order
// of precedence (first non-empty wins):
//
//  1. flagValue — kong's --console flag, also auto-populated from the
//     W17_CONSOLE_ADDR env var via the kong `env` tag.
//  2. the console you're logged into — the authstore active instance URL.
//     `w17ctl login <host>` is the explicit, recorded choice of console, so
//     subsequent commands follow it without a flag/env or a matching compiled
//     default. This is what makes "log in here, then work here" just work — and
//     stops a stale compiled default from silently misrouting to the wrong port.
//  3. DefaultConsoleAddr — the binary's compile-time default injected by
//     the build pipeline (last resort, before you've logged in anywhere).
//
// All empty → an actionable error listing every surface.
//
// The lock's `w17_url` is deliberately NOT a source here (public-split §8.2 —
// the client treats the lock as opaque bytes and can't dial the console to
// learn where the console is). It still rides the lock as a stored field.
func ResolveConsoleAddr(flagValue string) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}
	if addr := ActiveInstanceURL(); addr != "" {
		return addr, nil
	}
	if DefaultConsoleAddr != "" {
		return DefaultConsoleAddr, nil
	}
	return "", fmt.Errorf("no console address configured — log in with `w17ctl login <host>`, pass --console HOST:PORT, set W17_CONSOLE_ADDR, or rebuild with -ldflags \"-X github.com/MrS1lentcz/wandering-compiler/w17ctl/internal/core.DefaultConsoleAddr=...\"")
}
