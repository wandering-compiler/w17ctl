package core

import (
	"errors"
	"fmt"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wandering-compiler/sdk/go/lib/dpop/dpopclient"
	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
)

// ExplainAuthFailure turns a bare auth refusal into one that names the causes
// the CLIENT can see and the server cannot.
//
// Two refusals dominate this tool and neither can be diagnosed from the
// server's wording alone:
//
//   - `invalid credentials` has THREE causes with one signature: a token for a
//     console that no longer exists at that address, a token for a DIFFERENT
//     console that reused the address, and a console whose schema is behind
//     its code. Only the last is the server's to explain; the first two are
//     facts about this machine's credential store.
//   - `permission denied` on a caller who selected no organization is not a
//     permission problem at all. The grants exist — they are scoped to an
//     organization, the request named none, so the set narrows to nothing.
//     The server cannot say that without guessing why the set was empty; the
//     client knows exactly, because it knows what it sent.
//
// Local consoles publish EPHEMERAL ports, and docker hands a freed port to the
// next container that asks. So the second cause is not exotic: run two
// projects, let one stack go, and the other console can come up on the dead
// one's port holding its address in this store. The token is real, the address
// is right, and it belongs to a console that no longer exists.
func ExplainAuthFailure(addr string, err error) error {
	if err == nil {
		return nil
	}
	st, ok := status.FromError(err)
	if !ok {
		return err
	}
	if line := explainRevokedDevice(addr, err, st); line != "" {
		return fmt.Errorf("%w\n%s", err, line)
	}
	if line := htuMismatchHint(addr, err); line != "" {
		return fmt.Errorf("%w\n%s", err, line)
	}
	switch st.Code() {
	case codes.Unauthenticated:
		if line := explainExpiredSession(addr, st); line != "" {
			return fmt.Errorf("%w\n%s", err, line)
		}
		return fmt.Errorf("%w\n%s", err, explainUnauthenticated(addr))
	case codes.PermissionDenied:
		hint := explainPermissionDenied(addr)
		if hint == "" {
			return err
		}
		return fmt.Errorf("%w\n%s", err, hint)
	}
	return err
}

// explainRevokedDevice answers DEVICE_REVOKED: the console revoked this
// machine's device key (from its Devices page, or an administrator did), and a
// revoked device is refused for good. The key was discarded when the refusal
// arrived (revokedUnary), so the fix is a fresh login, which makes a new key —
// unless the key is the operator's own (W17_DPOP_KEY), which w17ctl never
// deletes.
func explainRevokedDevice(addr string, err error, st *status.Status) string {
	if !errors.Is(err, dpopclient.ErrDeviceRevoked) && !hasErrorDetailCode(st, string(dpopclient.ReasonDeviceRevoked)) {
		return ""
	}
	if _, explicit, _ := DeviceKeyPath(addr); explicit {
		return "  the console revoked the device key " + EnvDPoPKeyVar + " names; a revoked key is refused for good.\n" +
			"  fix: point " + EnvDPoPKeyVar + " at a new key file (w17ctl key public makes one and prints its\n" +
			"       public key), revoke the machine account's old device, and issue its token for the new key.\n"
	}
	return "  the console revoked this machine's device key; a revoked key is refused for good,\n" +
		"  so w17ctl discarded it.\n" +
		"  fix: w17ctl login " + displayAddr(addr) + " (a new key is made; the console sees a new device)\n"
}

// htuMismatchHint answers a proof the console refused because its htu — the
// origin w17ctl signed for plus the method — is not the origin the console is
// configured with. w17ctl derives the origin from the address it dialed, so
// the same console reached by another name (127.0.0.1 for a console
// configured as https://localhost:13444, an internal name behind a proxy)
// signs for the wrong one. Only this side knows what it signed for.
func htuMismatchHint(addr string, err error) string {
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.Unauthenticated || !strings.Contains(st.Message(), "htu does not match") {
		return ""
	}
	return "  the console refused the DPoP proof's origin: w17ctl signed for " + DPoPOrigin(addr) + ",\n" +
		"  and the console is configured with another (its rpc surface's DPoP origin).\n" +
		"  fix: dial the console by the name its configuration uses, or set\n" +
		"       " + EnvDPoPOriginVar + " to that origin (e.g. https://localhost:13444).\n"
}

func hasErrorDetailCode(st *status.Status, code string) bool {
	for _, d := range st.Details() {
		if ed, ok := d.(*w17pb.ErrorDetail); ok && ed.GetCode() == code {
			return true
		}
	}
	return false
}

func explainUnauthenticated(addr string) string {
	var b strings.Builder
	// An unattended caller is answered first and separately. `w17ctl login`
	// is the fix for a person at a terminal and is no fix at all in a CI job
	// or on a production host — offering it there sends the reader to a
	// command they cannot usefully run, on a machine with no home directory
	// to store the result in.
	if hint := explainEnvToken(addr); hint != "" {
		return hint
	}
	b.WriteString("  this is one of three, and they look identical from here:\n")
	inst := instanceFor(addr)
	switch {
	case inst == nil:
		b.WriteString("    · no stored credential for this console, and W17_TOKEN is not set either.\n")
		b.WriteString("      interactive: w17ctl login " + displayAddr(addr) + "\n")
		b.WriteString("      unattended:  set W17_TOKEN (a machine account's API token) and\n")
		b.WriteString("                   W17_CONSOLE_ADDR=" + displayAddr(addr) + "\n")
		return b.String()
	case isLoopback(inst.URL):
		b.WriteString("    · the token is for a console that is GONE. A dev console publishes an\n")
		b.WriteString("      ephemeral port, and docker reuses freed ports — so a second project's\n")
		b.WriteString("      console can answer on this address holding another one's token.\n")
	default:
		b.WriteString("    · the token expired, or the console's accounts were rebuilt.\n")
	}
	b.WriteString("    · the console runs code whose schema its database does not have\n")
	b.WriteString("      (a login SUCCEEDS and everything after it fails; re-login does not help).\n")
	b.WriteString("  fix, for the first two: w17ctl login " + displayAddr(addr) + "\n")
	return b.String()
}

// explainEnvToken answers the two refusals an unattended caller actually
// gets, and returns "" when no token is in play so the interactive
// explanation stands.
//
// The distinction it draws is the whole value. "Set and refused" and "set and
// never sent" arrive as the same `invalid credentials`, and they have nothing
// in common: the first is a question for the console (revoked? minted
// elsewhere?), the second is a missing line in a pipeline file. A deployment
// debugging the second one against the first reads as the token being wrong,
// and the token is fine.
func explainEnvToken(addr string) string {
	bound, presented := EnvTokenBinding(addr)
	if !EnvTokenIsSet() {
		return ""
	}
	if presented {
		return "  " + EnvTokenVar + " WAS presented to this console and refused.\n" +
			"    · the token was revoked, or its account was disabled.\n" +
			"    · the token was minted on a DIFFERENT console than " + displayAddr(addr) + ".\n" +
			"    · the console runs code whose schema its database does not have.\n" +
			"  a machine account's token does not expire by sitting unused, so an\n" +
			"  unchanged pipeline that suddenly fails here points at the account.\n"
	}
	if bound == "" {
		return "  " + EnvTokenVar + " is set but bound to no console, so NOTHING was sent.\n" +
			"  fix: set " + envConsoleAddrVar + "=" + displayAddr(addr) + "\n"
	}
	return "  " + EnvTokenVar + " is bound to " + bound + " and was NOT sent to " + displayAddr(addr) + ".\n" +
		"  a token is only presented to the console it was minted for, so this\n" +
		"  call went out unauthenticated — the token itself is not implicated.\n" +
		"  fix: point " + envConsoleAddrVar + " at the console being dialed, or dial the one it names.\n"
}

// explainPermissionDenied speaks only when this client can see a cause. A real
// permission refusal — the caller named an organization and lacks the grant —
// gets nothing added, because inventing an explanation for THAT sends the
// reader to the wrong fix.
//
// The cause it can see is "no organization was sent, and the account has
// several". The memberships come from the login cache, or — for an
// environment token, which has none — from the console. A console that will
// not list them without an organization is itself the answer: the request had
// no organization and the account's grants need one, which is what an account
// in several organizations looks like to an api-realm token.
func explainPermissionDenied(addr string) string {
	if slug, err := orgSlugForRequest(addr); slug != "" || err != nil {
		return ""
	}
	var slugs []string
	switch inst := instanceFor(addr); {
	case inst != nil && len(inst.Orgs) > 0:
		for _, o := range inst.Orgs {
			if o != nil && o.Slug != "" {
				slugs = append(slugs, o.Slug)
			}
		}
	case AuthTokenFn(addr) != "":
		l := listOrgsOnce(addr, AuthTokenFn(addr), "")
		switch {
		case l.err == nil:
			slugs = orgSlugs(l.orgs)
		case status.Code(l.err) == codes.PermissionDenied:
			return "  no organization was sent with this request, and the console would not even list\n" +
				"  " + accountFor(addr) + "'s organizations without one: its grants are all scoped\n" +
				"  to an organization, so a request that names none narrows them to nothing.\n" +
				noOrgFix(addr)
		default:
			return ""
		}
	default:
		return ""
	}
	if len(slugs) < 2 {
		return ""
	}
	return "  no organization was sent with this request, and " + accountFor(addr) + " belongs to " +
		fmt.Sprint(len(slugs)) + ": " + strings.Join(slugs, ", ") + "\n" +
		"  the grants are scoped to an organization, so a request that names none\n" +
		"  narrows them to nothing — which arrives as this refusal.\n" +
		noOrgFix(addr)
}

// noOrgFix says how to name the organization, starting with the way that works
// for every caller: a lock in the directory.
func noOrgFix(addr string) string {
	var b strings.Builder
	b.WriteString("  fix: run inside a project whose lock records its organization\n" +
		"       (the console stamps org_id when it signs the lock); a directory that is not a\n" +
		"       project — a plugins repository — records one with `w17ctl org pin <slug>`.\n")
	if envTokenFor(addr) != "" {
		b.WriteString("       " + EnvTokenVar + " carries no organization of its own: the lock is how an\n" +
			"       unattended run names one.\n")
	} else {
		b.WriteString("       Or set this machine's default: `w17ctl org use <slug>`.\n")
	}
	return b.String()
}

func isLoopback(url string) bool {
	for _, h := range []string{"localhost", "127.0.0.1", "[::1]", "::1"} {
		if strings.Contains(url, h) {
			return true
		}
	}
	return false
}

func displayAddr(addr string) string {
	if addr == "" {
		return "<console>"
	}
	return addr
}
