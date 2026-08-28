package core

import (
	"context"
	"net"
	"strings"

	"github.com/wandering-compiler/w17ctl/internal/authstore"
)

// AuthTokenFn resolves the bearer token w17ctl attaches to a console gRPC
// call, for the console it is DIALING. Production reads the machine-local
// credential store (~/.w17/auth.yaml); tests override it. Returns "" when
// there is no credential for that console — in which case no auth metadata
// is attached and the behavior is identical to the pre-auth client.
//
// The address argument is the whole point. The store is keyed per instance,
// but resolving the credential from the ACTIVE instance meant a command run
// with `--console <other>` dialed one console and presented the other's
// token. Two consoles is the ordinary case here (a workspace's dev console
// and the deployed one), and every `login` moves the active pointer — so
// logging into either one silently broke commands aimed at the other, with
// `invalid credentials` as the only symptom. That reads like an expired
// session and sends you looking at the server.
var AuthTokenFn = func(addr string) string {
	if inst := instanceFor(addr); inst != nil {
		return inst.Token
	}
	return ""
}

// instanceFor resolves the stored credential for a console address.
//
// No address means "the console this machine is pointed at" — the active
// instance, which is how every command without `--console` resolves.
//
// A NAMED address is matched against the store: exactly first, then modulo
// spelling (scheme prefix, host casing, the loopback aliases localhost /
// 127.0.0.1 / ::1). An address that matches nothing gets NO credential.
//
// The narrowing is deliberate (T2-5 D11-9). This used to fall back to the
// active instance for any unknown address, on the reasoning that an unknown
// address may be another name for the same console. That reasoning is
// right, and normalization is what implements it — the fallback implemented
// something wider: `--console <anywhere>` presented the bearer for the
// console the user is logged into, to a host of the caller's choosing. An
// alias resolves; a stranger does not.
//
// Cost of the narrowing: an alias that normalization cannot see (a distinct
// hostname for the same console) now needs its own `w17ctl login`. The
// failure is the same `invalid credentials` as before, and a login fixes it
// for good.
func instanceFor(addr string) *authstore.Instance {
	st, err := authstore.LoadDefault()
	if err != nil {
		return nil
	}
	if addr == "" {
		return st.ActiveInstance()
	}
	if inst := st.Instance(addr); inst != nil {
		return inst
	}
	want := normalizeConsoleAddr(addr)
	for key, inst := range st.Instances {
		if inst == nil {
			continue
		}
		if normalizeConsoleAddr(key) == want {
			return inst
		}
	}
	return nil
}

// normalizeConsoleAddr reduces a console address to the form two spellings
// of the SAME console share: no readability scheme, lowercase host, every
// loopback spelling collapsed onto one, port preserved verbatim.
//
// The port is load-bearing and is never defaulted: a workspace's dev console
// and its deployed one commonly differ only there, and merging them is the
// wrong-token bug dc45782c0 fixed.
func normalizeConsoleAddr(addr string) string {
	_, target := splitConsoleScheme(addr)
	host, port := target, ""
	if h, p, err := net.SplitHostPort(target); err == nil {
		host, port = h, p
	}
	host = strings.ToLower(strings.TrimSuffix(strings.TrimPrefix(host, "["), "]"))
	if host == "localhost" {
		host = "127.0.0.1"
	} else if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		host = "127.0.0.1"
	}
	if port == "" {
		return host
	}
	return net.JoinHostPort(host, port)
}

// ActiveInstanceURL returns the address of the console the user is logged into
// (the authstore active instance), or "" when not logged in. Used as the
// console-address fallback so `w17ctl login <host>` makes subsequent commands
// (incl. `init`) target <host> without a flag/env or a matching compiled default.
func ActiveInstanceURL() string {
	st, err := authstore.LoadDefault()
	if err != nil {
		return ""
	}
	if inst := st.ActiveInstance(); inst != nil {
		return inst.URL
	}
	return ""
}

// bearerPerRPC attaches `authorization: Bearer <token>` to every console gRPC
// call, sourcing the token fresh per call from AuthTokenFn. With no token set
// (not logged in) it adds nothing — identical to the pre-auth behavior, so it
// is safe to wire onto every dial unconditionally. The console's auth plugin
// reads exactly this header (HeaderName "Authorization", scheme "Bearer").
//
// RequireTransportSecurity returns false on purpose: a console dial is TLS for
// a remote endpoint but PLAINTEXT on loopback (the local gateway serves
// plaintext; the kernel is the trust boundary — see ConsoleTransportCreds).
// Returning true would make gRPC refuse to send the bearer over that loopback
// hop and break local login. The token still rides the encrypted channel
// whenever the transport is TLS (remote); on loopback a bearer is only as safe
// as the local machine, which is the intended dev posture.
// addr is the console this credential set is dialing — see AuthTokenFn for
// why the credential is resolved per address rather than from the active
// instance.
type bearerPerRPC struct{ addr string }

func (b bearerPerRPC) GetRequestMetadata(_ context.Context, _ ...string) (map[string]string, error) {
	token := AuthTokenFn(b.addr)
	if token == "" {
		return nil, nil
	}
	md := map[string]string{"authorization": "Bearer " + token}
	// The ACTIVE org rides every console call, not just `init`.
	//
	// The console stamps scopes["org_id"] from this header after validating
	// membership, and a scoped model refuses a request that carries no scope.
	// Server-side there is an inference for the single-org case, so a
	// one-org user never noticed — but a user in TWO orgs got a refusal from
	// every scoped command, because `init` was the only place that ever sent
	// the header. `w17ctl org use <slug>` meanwhile printed "Default
	// organization set to …" and stored a value nothing transmitted.
	//
	// It belongs here rather than at each call site for the reason the
	// bearer does: a per-call decision is one somebody forgets.
	if slug := OrgSlugFor(b.addr); slug != "" {
		md["w17-org"] = slug
	}
	return md, nil
}

func (bearerPerRPC) RequireTransportSecurity() bool { return false }

// ActiveOrgSlug returns the slug of the active instance's default
// organization, or "" when none is set (or it cannot be resolved).
//
// The stored value is an org ID and the wire wants a SLUG — the console
// resolves the header via GetUserOrgBySlug — so this translates through the
// login-time membership cache rather than sending the id and having the
// lookup miss. Both live on the same cached Org, so no round-trip.
//
// "" is the correct answer for "no default": the console then applies its
// single-org inference, which is exactly right for the one-org case and
// deliberately refuses when there are several.
func ActiveOrgSlug() string { return OrgSlugFor("") }

// OrgSlugFor returns the chosen organization's slug for ONE console. Same
// per-address rule as AuthTokenFn: the org a user picked on the deployed
// console must not ride a call to their dev console, where that slug may
// name a different organization or none.
func OrgSlugFor(addr string) string {
	inst := instanceFor(addr)
	if inst == nil || inst.DefaultOrg == "" {
		return ""
	}
	for _, o := range inst.Orgs {
		if o != nil && o.ID == inst.DefaultOrg {
			return o.Slug
		}
	}
	return ""
}
