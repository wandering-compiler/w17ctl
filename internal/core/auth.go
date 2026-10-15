package core

import (
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
//
// An environment-supplied token (W17_TOKEN — see envtoken.go) is consulted
// FIRST, and beats anything in the store. An unattended caller is the case
// that has no store at all, so the order only matters on a developer machine
// that has both: there, exporting the variable is a deliberate, per-shell act
// aimed at THIS process, while the store is a persistent default set at some
// earlier login. The narrower, more recent, more explicit signal wins.
var AuthTokenFn = func(addr string) string {
	if token := envTokenFor(addr); token != "" {
		return token
	}
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
// The narrowing is deliberate. This used to fall back to the
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
	return lookupInstance(st, addr)
}

// lookupInstance is instanceFor's matching over an already-loaded store, for
// the callers that go on to modify and save it.
func lookupInstance(st *authstore.Store, addr string) *authstore.Instance {
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
// orgSlugForRequest picks the organization THIS command acts in: the project's
// if the directory has one, the machine default otherwise.
//
// The lock wins because it is the specific answer. A default in
// ~/.w17/auth.yaml is one value for every checkout on the machine, so with two
// projects in two organizations it is right for at most one of them — and
// being wrong is silent, since the console checks the header against
// membership rather than against the project.
//
// The lock stores an ID and the wire wants a SLUG, so it is translated through
// the login-time membership cache. An id that resolves to no membership falls
// back rather than sending nothing: it means the lock names an organization
// this login cannot see, and the console's own refusal says that far better
// than a request with no scope at all, which reads as "you picked nothing".
// OrgSlugForRequestFn is indirected for the same reason AuthTokenFn is: a test
// that wants to assert the credential's SHAPE should not have to stand up an
// authstore and a project lock to do it.
var OrgSlugForRequestFn = orgSlugForRequest

func orgSlugForRequest(addr string) string {
	if orgID := LockOrgIDBestEffort(); orgID != "" {
		if inst := instanceFor(addr); inst != nil {
			for _, o := range inst.Orgs {
				if o != nil && o.ID == orgID {
					return o.Slug
				}
			}
		}
	}
	return OrgSlugFor(addr)
}

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
