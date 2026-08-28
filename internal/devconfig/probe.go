package devconfig

import (
	"net"
	"strconv"
)

// portInUse reports whether TCP host port `port` is currently bound on
// this machine. It is the single OS-probe both the allocator (to skip a
// momentarily-occupied port when handing out a fresh slot) and the
// `stack up` preflight (to fail fast with a clear error before docker
// even tries to publish) share.
//
// Indirected through a package var so tests can make allocation
// deterministic without touching real sockets — production code never
// reassigns it.
//
// Probe strategy: attempt to bind 0.0.0.0:port (the address docker
// publishes to by default). A bind failure means something already
// holds it. This is best-effort — a port can be grabbed in the window
// between the probe and `docker compose up` — but it catches the common
// "port already allocated" case early with a readable message instead
// of an opaque docker failure deep into bring-up.
var portInUse = bindProbe

// bindProbe is the real OS probe: it can bind 0.0.0.0:port iff nothing
// else holds it. Named (rather than an inline literal) so tests can
// exercise the true bind path even while [portInUse] is stubbed.
func bindProbe(port int) bool {
	ln, err := net.Listen("tcp", ":"+strconv.Itoa(port))
	if err != nil {
		return true
	}
	_ = ln.Close()
	return false
}

// PortInUse exposes the shared OS port probe to callers outside the
// package (the `stack up` preflight). Kept as a thin wrapper over the
// same package var the allocator uses so both see identical behaviour
// (and the same test stub).
func PortInUse(port int) bool { return portInUse(port) }
