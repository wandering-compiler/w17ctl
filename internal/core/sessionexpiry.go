package core

import (
	"sync"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/status"

	"github.com/wandering-compiler/w17ctl/internal/authstore"
	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
)

// SessionExpiredReason is the code the console's auth plugin puts on the
// refusal of a session token that EXISTS but has expired — both as the
// w17.ErrorDetail code and as the google.rpc.ErrorInfo reason. An unknown
// token gets the generic refusal and neither detail.
const SessionExpiredReason = "SESSION_EXPIRED"

// sessionExpiredDomain is the ErrorInfo domain the auth plugin stamps. The
// reason alone is not enough: another service may use the same word for its
// own kind of session, and only the console's says THIS credential expired.
const sessionExpiredDomain = "auth.w17"

// sessionExpiresAtKey is the ErrorInfo metadata key carrying the expiry,
// RFC3339 in UTC.
const sessionExpiresAtKey = "expires_at"

// nowFn is the clock the stored-expiry checks read; a var so a test can stand
// on either side of an expiry without sleeping.
var nowFn = time.Now

// presentedTokens is the bearer this process last sent to each console, keyed
// by normalized address.
//
// The explainer needs the token that was REFUSED, not the one the store holds
// now: a `w17ctl login` in another shell can replace the stored token between
// the call and its refusal, and stamping the old session's expiry onto that
// fresh token would call it expired on every later refusal and in whoami.
var (
	presentedMu     sync.Mutex
	presentedTokens = map[string]string{}
)

func notePresentedToken(addr, token string) {
	if token == "" {
		return
	}
	presentedMu.Lock()
	presentedTokens[normalizeConsoleAddr(addr)] = token
	presentedMu.Unlock()
}

// presentedToken returns the bearer last sent to addr, or "" when this process
// sent none (or addr is empty: "the active console" names no dial).
func presentedToken(addr string) string {
	if addr == "" {
		return ""
	}
	presentedMu.Lock()
	defer presentedMu.Unlock()
	return presentedTokens[normalizeConsoleAddr(addr)]
}

// FormatSessionTime renders a session expiry the way every line that mentions
// one does: minutes, UTC, said out loud.
func FormatSessionTime(t time.Time) string {
	return t.UTC().Format("2006-01-02 15:04 UTC")
}

// sessionExpiredDetail reads the console's own verdict off a refusal.
//
// ErrorInfo is preferred because it alone carries the date. The ErrorDetail
// alone still settles the question — the session expired — and leaves the
// date unknown, as does an expiry this client cannot parse: the verdict is the
// part that changes the advice, the date is a courtesy.
func sessionExpiredDetail(st *status.Status) (expired bool, at time.Time) {
	for _, d := range st.Details() {
		switch dd := d.(type) {
		case *errdetails.ErrorInfo:
			if dd.GetReason() != SessionExpiredReason || dd.GetDomain() != sessionExpiredDomain {
				continue
			}
			if t, err := time.Parse(time.RFC3339, dd.GetMetadata()[sessionExpiresAtKey]); err == nil {
				return true, t.UTC()
			}
			expired = true
		case *w17pb.ErrorDetail:
			if dd.GetCode() == SessionExpiredReason {
				expired = true
			}
		}
	}
	return expired, time.Time{}
}

// explainExpiredSession returns the single line that replaces the three-cause
// explanation when this refusal is KNOWN to be an expired session, and ""
// otherwise.
//
// Two sources know it. The console says so, on a refusal that carries the
// SESSION_EXPIRED detail; and this machine knows it when the stored record has
// an expiry in the past, which covers a console too old to send the detail.
// Either way there is one cause and one fix, and listing two others beside it
// is what hid a thirty-day expiry behind "this is one of three".
//
// A presented W17_TOKEN is not this function's to explain: the stored session
// was not what was sent, and `login` is no fix for an unattended caller.
func explainExpiredSession(addr string, st *status.Status) string {
	if _, presented := EnvTokenBinding(addr); presented {
		return ""
	}
	expired, at := sessionExpiredDetail(st)
	sent := presentedToken(addr)
	inst := instanceFor(addr)
	stored, known := inst.SessionExpiry()
	if inst != nil && sent != "" && inst.Token != sent {
		// The record now holds a different session than the one refused;
		// its expiry says nothing about this refusal.
		known = false
	}
	pastStored := known && !stored.After(nowFn())
	switch {
	case expired && !at.IsZero():
		recordSessionExpiry(addr, sent, at)
	case expired:
		// The console said expired without a date; a stored one that agrees
		// supplies it.
		if pastStored {
			at = stored
		}
	case pastStored:
		at = stored
	default:
		return ""
	}
	console := addr
	if console == "" && inst != nil {
		console = inst.URL
	}
	return expiredSessionLine(displayAddr(console), at)
}

// expiredSessionLine is the one-line verdict; a zero time drops the date.
func expiredSessionLine(console string, at time.Time) string {
	when := ""
	if !at.IsZero() {
		when = " " + FormatSessionTime(at)
	}
	return "login to " + console + " expired" + when + "; run: w17ctl login " + console
}

// recordSessionExpiry writes an expiry the console reported into the stored
// record for addr, so `whoami` can show it and the next refusal is explained
// even by a console that stops sending the detail.
//
// Only onto the session that was refused: the store is re-read here, right
// before the save, and written only when its record still holds the token
// that was sent. Anything else — no token known, a login in another shell
// that replaced it — leaves the file alone, so a fresh token is never marked
// expired and a whole stale store is never saved over a newer one.
//
// Best effort: failing to remember a date must not change the error being
// reported.
func recordSessionExpiry(addr, sent string, at time.Time) {
	st, err := authstore.LoadDefault()
	if err != nil {
		return
	}
	inst := LookupInstance(st, addr)
	if inst == nil || inst.Token == "" || inst.Token != sent || !inst.RecordSessionExpiry(at) {
		return
	}
	_ = authstore.SaveDefault(st)
}
