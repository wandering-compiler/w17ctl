package core

import (
	"crypto/tls"
	"crypto/x509"
	_ "embed"
	"net"
	"os"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// w17ctl ALWAYS dials the console over TLS. A real console is reached over a
// TLS endpoint (a deployed instance behind a load balancer with a real cert);
// for local development the console is fronted by a TLS-terminating proxy
// (Caddy in the dev compose) holding the self-signed dev cert. There is no
// plaintext path — the console gateway itself stays a plain h2c listener, but
// w17ctl never talks to it directly; it talks to the terminator.
//
// gRPC has no native https:// — the scheme in a grpc.NewClient target is a
// NAME RESOLVER, and TLS is a separate dial option. So a console address may
// optionally carry an http(s)/grpc(s) scheme for readability; w17ctl strips it
// for the dial target (ConsoleTarget) and the transport is always TLS
// regardless.
//
// Verification is ZERO-CONFIG:
//
//   - Remote dials verify against the system roots (a real public cert). No
//     env, no flags — a deployed console just works.
//   - Loopback dials (localhost / 127.0.0.1 / ::1) additionally trust the
//     compiled-in dev CA (devca.crt, committed alongside dev/certs), so the
//     dev TLS terminator's self-signed cert verifies with no env either. The
//     dev CA is trust-scoped to loopback ONLY — it is never added to the root
//     set for a remote host, so a leaked dev CA key cannot MITM a real domain.
//
// The sole remaining knob is a DEV escape hatch:
//
//	W17_CONSOLE_TLS_SKIP_VERIFY "true" encrypts without verifying the server
//	                            cert at all (for ad-hoc terminators the dev CA
//	                            doesn't cover).

// devCACert is the committed dev CA certificate (public — the private key is
// dev-only and lives in dev/certs). It is trusted ONLY for loopback dials so a
// zero-env `w17ctl login localhost:…` verifies the dev terminator's cert. Kept
// a copy in this package because go:embed can't reach outside the module.
//
//go:embed devca.crt
var devCACert []byte

// ConsoleTarget returns the bare gRPC dial target (host:port) for
// grpc.NewClient, stripping an http(s)/grpc(s) scheme. A scheme that gRPC
// itself resolves (dns://, unix://, passthrough://) is left attached so the
// resolver still receives it.
func ConsoleTarget(addr string) string {
	_, target := splitConsoleScheme(addr)
	return target
}

// ConsoleTransportCreds returns the transport credentials for a console dial.
// Always TLS (see the package note above). addr selects the trust set: a
// loopback target additionally trusts the compiled-in dev CA.
func ConsoleTransportCreds(addr string) grpc.DialOption {
	return grpc.WithTransportCredentials(credentials.NewTLS(consoleTLSConfig(os.Getenv, addr)))
}

// splitConsoleScheme splits an optional readability scheme off addr. It only
// recognizes the http(s)/grpc(s) schemes; any other "scheme://" (a gRPC
// resolver target) is left whole so the resolver still receives it.
func splitConsoleScheme(addr string) (scheme, target string) {
	for _, s := range []string{"https", "grpcs", "http", "grpc"} {
		if rest, ok := strings.CutPrefix(addr, s+"://"); ok {
			return s, rest
		}
	}
	return "", addr
}

// consoleTLSConfig builds the client tls.Config for a dial to addr. Default is
// verify-against-system-roots; a loopback target additionally trusts the
// compiled-in dev CA (see the package note). W17_CONSOLE_TLS_SKIP_VERIFY=true
// is the last-resort DEV escape hatch.
func consoleTLSConfig(getenv func(string) string, addr string) *tls.Config {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if strings.EqualFold(getenv("W17_CONSOLE_TLS_SKIP_VERIFY"), "true") {
		cfg.InsecureSkipVerify = true //nolint:gosec // G402: explicit DEV escape hatch, gated behind a named env knob; default verifies against system roots.
		return cfg
	}
	// Loopback dials add the compiled-in dev CA to the root set so the dev TLS
	// terminator's self-signed cert verifies with zero env. Scoped to loopback
	// so the dev CA can never validate a remote host.
	if isLoopbackTarget(addr) {
		cfg.RootCAs = loopbackRootPool()
	}
	return cfg
}

// loopbackRootPool returns the system roots plus the compiled-in dev CA. It
// clones the system pool so real certs still verify on a loopback dial; if the
// system pool is unavailable it falls back to a pool holding only the dev CA.
func loopbackRootPool() *x509.CertPool {
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	pool.AppendCertsFromPEM(devCACert)
	return pool
}

// isLoopbackTarget reports whether addr's host is a loopback address/name
// (localhost, 127.0.0.1, ::1). The scheme and port are stripped first.
func isLoopbackTarget(addr string) bool {
	_, target := splitConsoleScheme(addr)
	host := target
	if h, _, err := net.SplitHostPort(target); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}
