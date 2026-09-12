package core

import (
	"fmt"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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
	switch st.Code() {
	case codes.Unauthenticated:
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

func explainUnauthenticated(addr string) string {
	var b strings.Builder
	b.WriteString("  this is one of three, and they look identical from here:\n")
	inst := instanceFor(addr)
	switch {
	case inst == nil:
		b.WriteString("    · no stored credential for this console — run `w17ctl login " + displayAddr(addr) + "`\n")
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

// explainPermissionDenied speaks only when this client can see a cause. A real
// permission refusal — the caller named an organization and lacks the grant —
// gets nothing added, because inventing an explanation for THAT sends the
// reader to the wrong fix.
func explainPermissionDenied(addr string) string {
	if orgSlugForRequest(addr) != "" {
		return ""
	}
	inst := instanceFor(addr)
	if inst == nil || len(inst.Orgs) == 0 {
		return ""
	}
	var slugs []string
	for _, o := range inst.Orgs {
		if o != nil && o.Slug != "" {
			slugs = append(slugs, o.Slug)
		}
	}
	if len(slugs) < 2 {
		return ""
	}
	return "  no organization was selected, and you belong to " + fmt.Sprint(len(slugs)) + ": " +
		strings.Join(slugs, ", ") + "\n" +
		"  your grants are scoped to an organization, so a request that names none\n" +
		"  narrows them to nothing — which arrives as this refusal.\n" +
		"  fix: `w17ctl org use <slug>`, or run inside a project whose lock records one\n" +
		"       (the console stamps it when it signs the lock)\n"
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
