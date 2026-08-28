package consoleui

import (
	"net"
	"net/http"
	"net/url"
	"strings"
)

// guard wraps the UI mux with a loopback-only trust boundary.
//
// The server binds 127.0.0.1, but loopback is reachable from any page the
// developer's browser loads, so a malicious site can drive the mutating
// control endpoints (`up`/`down -v` = container control + DB-volume deletion)
// via a cross-origin "simple request" — classic CSRF — and DNS rebinding can
// defeat the bind entirely by making Host/Origin a domain the attacker owns
// that resolves to 127.0.0.1. Two cheap, standard checks close both:
//
//   - Reject any request whose Host header is not a loopback host. The SPA is
//     served same-origin from 127.0.0.1, so it always passes; a rebinding
//     attacker's Host is its own domain and is rejected. This also protects the
//     read endpoints (e.g. /api/projects/{name}/config exposes lock.yaml).
//   - Reject any request that carries an Origin header that is not a loopback
//     origin. Browsers attach Origin to all cross-origin POST/PUT/DELETE
//     (including no-cors "simple requests") and the page cannot forge it, so a
//     drive-by from any external site is rejected while same-origin SPA calls
//     (Origin http://127.0.0.1:<port>) and non-browser clients (no Origin) pass.
func guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isLoopbackHost(hostOnly(r.Host)) {
			http.Error(w, "forbidden: non-loopback Host (UI is local-only)", http.StatusForbidden)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && !isLoopbackOrigin(origin) {
			http.Error(w, "forbidden: cross-origin request rejected", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// hostOnly strips the optional :port from a Host header value, tolerating a
// missing port (then the whole value is the host).
func hostOnly(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}

// isLoopbackHost reports whether h (no port; brackets allowed for IPv6) names
// the loopback interface or "localhost".
func isLoopbackHost(h string) bool {
	h = strings.TrimSuffix(strings.TrimPrefix(h, "["), "]")
	if strings.EqualFold(h, "localhost") {
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// isLoopbackOrigin reports whether an Origin header value (scheme://host[:port])
// has a loopback host. A malformed origin is treated as non-loopback (reject).
func isLoopbackOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	return isLoopbackHost(u.Hostname())
}
