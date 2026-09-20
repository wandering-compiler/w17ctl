package core

import (
	"fmt"
	"os"
	"sync"
)

// EnvTokenVar is the environment variable an unattended caller sets instead of
// logging in: an API token minted for a machine account (the auth plugin's
// `service_account` feature). CI and a production deployment have no terminal
// to run `w17ctl login` on and no home directory worth persisting a credential
// into, so the credential arrives as environment.
//
// No CONSOLE_ prefix, and the split is the rule the whole family follows:
// CONSOLE_ says WHERE to connect (ADDR, ORG, CA, TLS_SKIP_VERIFY), while a
// credential says WHO you are — as W17_PASSWORD, which came first, already
// does. The production apply path reads this same variable, so an operator
// sets ONE name whichever binary runs.
const EnvTokenVar = "W17_TOKEN"

// envConsoleAddrVar is the address the env token is bound to. It is the same
// variable that already selects the console, so the pair a CI job sets is
// (W17_CONSOLE_ADDR, W17_TOKEN) — the console and the credential for it.
const envConsoleAddrVar = "W17_CONSOLE_ADDR"

// envTokenFor returns the environment-supplied bearer for the console being
// DIALED, or "" when there is none to present.
//
// The address check is the whole point, and it is the same narrowing
// instanceFor applies to the credential store (T2-5 D11-9): a bearer is
// presented to the console it was issued for, never to a host the caller
// names. Without it this variable would be the wider version of the very bug
// that narrowing fixed — and worse, because the token it leaks belongs to an
// unattended account with push rights rather than to one developer's session.
//
// What the token is bound to, in order:
//
//  1. W17_CONSOLE_ADDR — set alongside the token by whoever set the token.
//  2. DefaultConsoleAddr — the address compiled into this binary. A production
//     build is stamped with the console it belongs to, so a deployment that
//     sets only the token still works, and still only against that console.
//
// Neither set means the token is bound to nothing, and an unbound credential
// is not presented anywhere.
func envTokenFor(addr string) string {
	token := os.Getenv(EnvTokenVar)
	if token == "" {
		return ""
	}
	bound := os.Getenv(envConsoleAddrVar)
	if bound == "" {
		bound = DefaultConsoleAddr
	}
	if bound == "" {
		warnEnvTokenOnce(fmt.Sprintf(
			"%s is set but bound to no console, so it was not presented to %q.\n"+
				"  fix: set %s to the console the token was minted for.",
			EnvTokenVar, addr, envConsoleAddrVar))
		return ""
	}
	if normalizeConsoleAddr(addr) != normalizeConsoleAddr(bound) {
		warnEnvTokenOnce(fmt.Sprintf(
			"%s is bound to %q and was NOT presented to %q.\n"+
				"  a token is only sent to the console it was minted for; expect an authentication failure below.",
			EnvTokenVar, bound, addr))
		return ""
	}
	return token
}

// warnEnvTokenOnce reports a token that was set and then not used.
//
// Silence here is the expensive option. The symptom of a token that never
// leaves the process is the console's `invalid credentials`, which reads as a
// dead or mistyped token and sends you to the server, to the mint, to the
// account — anywhere except the one variable that decided not to send it. The
// same shape cost a day on the port-discovery path, where a swallowed stderr
// hid a publish race behind a wrong diagnosis.
//
// Once per process, not per RPC: the bearer is resolved on EVERY call, and a
// command that makes twenty would otherwise bury its own output.
func warnEnvTokenOnce(msg string) {
	envTokenWarnOnce.Do(func() { fmt.Fprintln(Stderr, "warning: "+msg) })
}

// A POINTER, so a test can swap in a fresh one without copying a lock.
var envTokenWarnOnce = new(sync.Once)

// EnvTokenBinding returns the console an env-supplied token is bound to, and
// whether it would be presented to addr. Both answers are for DIAGNOSTICS: a
// refusal reads completely differently depending on whether the token was
// sent and rejected, or never sent at all, and only this package can tell
// those apart.
func EnvTokenBinding(addr string) (bound string, presented bool) {
	if !EnvTokenIsSet() {
		return "", false
	}
	bound = os.Getenv(envConsoleAddrVar)
	if bound == "" {
		bound = DefaultConsoleAddr
	}
	if bound == "" {
		return "", false
	}
	return bound, normalizeConsoleAddr(addr) == normalizeConsoleAddr(bound)
}

// EnvTokenIsSet reports whether an env-supplied token is present at all,
// regardless of which console it is bound to. It exists so `whoami` can say
// that this process is acting as a machine account — with nothing in the
// credential store, whoami's "Not logged in" is true of the store and false of
// the process, and that is precisely the claim that file exists to keep honest.
func EnvTokenIsSet() bool { return os.Getenv(EnvTokenVar) != "" }
