// Package whoami implements `w17ctl whoami` — show the STORED identity (and
// org memberships) for the active console, or all of them with --all. Reads
// the machine-local credential store; no server round-trip unless --verify.
//
// The offline read is deliberate and stays. What changed is what it CLAIMS:
// printing an identity and a list of organizations reads as "you are logged
// in", and what it actually knows is "this file says so". deinvo took that as
// confirmation before a regeneration and found out at the first real call that
// the token was dead (2026-09-12).
//
// A recycled port makes it worse than stale. A dev console publishes an
// ephemeral port, docker hands freed ports on, and this store keys credentials
// by address — so the identity and organizations printed here can belong to a
// console that no longer exists, while some OTHER project's console answers at
// that address now. Same confident output, no relation to what is listening.
//
// See docs/specs/plugins/auth-cli-login-and-orgs.md.
package whoami

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/wandering-compiler/w17ctl/internal/authstore"
	"github.com/wandering-compiler/w17ctl/internal/core"
)

// Cmd is `w17ctl whoami`.
type Cmd struct {
	All    bool `name:"all" help:"Show every console with a stored credential, not just the active one."`
	Verify bool `name:"verify" help:"Ask the console whether this credential still works (one round-trip)."`
}

func (c *Cmd) Run() error {
	st, err := authstore.LoadDefault()
	if err != nil {
		return err
	}
	// A machine account leaves no trace in the store, so this branch RETURNS
	// rather than falling through to it.
	//
	// It used to print the two lines below and then continue into the store
	// read, which ended at "Not logged in. Run `w17ctl login <console-url>`"
	// — advice a CI runner must not take. `login` is the wrong act for a
	// machine account and would not even work: it is `kind = BOT`, and the
	// plugin refuses a password sign-in for one. Reported from a real CI run
	// where the token was valid (deinvo, 2026-09-21), and AGENTS.md points at
	// this very command as the arbiter of "is the credential the problem".
	if core.EnvTokenIsSet() {
		return c.reportMachineAccount()
	}

	if len(st.Instances) == 0 {
		// "Not logged in" is exact here: with nothing stored there is no
		// credential that could be valid. The claim only outruns the evidence
		// when a record EXISTS, which is what the note below addresses.
		fmt.Fprintln(core.Stdout, "Not logged in. Run `w17ctl login <console-url>`.")
		return nil
	}

	if c.All {
		urls := make([]string, 0, len(st.Instances))
		for u := range st.Instances {
			urls = append(urls, u)
		}
		sort.Strings(urls)
		for _, u := range urls {
			printInstance(u == st.DefaultInstance, st.Instances[u])
		}
		if !c.Verify {
			unverifiedNote()
		}
		return nil
	}

	inst := st.ActiveInstance()
	if inst == nil {
		fmt.Fprintln(core.Stdout, "No active console. Run `w17ctl login` or pick one with --all.")
		return nil
	}
	printInstance(true, inst)
	if c.Verify {
		return verify(inst)
	}
	unverifiedNote()
	return nil
}

// unverifiedNote says what this output is, once, rather than letting the shape
// of it imply something stronger.
func unverifiedNote() {
	fmt.Fprintln(core.Stdout, "  (stored locally, NOT checked against the console — `whoami --verify` asks it)")
}

// verify spends one round-trip to turn "the file says this" into "the console
// agrees". ListMyOrgs is the probe because it needs a valid bearer AND returns
// the truth about organizations, so a credential that belongs to a DIFFERENT
// console shows up as a mismatch rather than as a pass.
func verify(inst *authstore.Instance) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	orgs, err := core.ListMyOrgs(ctx, inst.URL, inst.Token)
	if err != nil {
		// Returned, not printed: the top-level handler decorates an auth
		// refusal with the causes only this machine can see.
		return err
	}
	fmt.Fprintln(core.Stdout, "  verified: the console accepts this credential")

	stored := map[string]bool{}
	for _, o := range inst.Orgs {
		if o != nil {
			stored[o.Slug] = true
		}
	}
	var missing []string
	for _, o := range orgs {
		if !stored[o.Slug] {
			missing = append(missing, o.Slug)
		}
	}
	if len(missing) > 0 || len(orgs) != len(inst.Orgs) {
		sort.Strings(missing)
		fmt.Fprintf(core.Stdout, "  ⚠ the console reports %d organization(s), this file has %d",
			len(orgs), len(inst.Orgs))
		if len(missing) > 0 {
			fmt.Fprintf(core.Stdout, " — not in the file: %s", strings.Join(missing, ", "))
		}
		fmt.Fprintln(core.Stdout, "\n    the stored record is out of date, or it belongs to a different console that once answered at this address")
		fmt.Fprintln(core.Stdout, "    fix: w17ctl login "+inst.URL)
	}
	return nil
}

func printInstance(active bool, inst *authstore.Instance) {
	marker := " "
	if active {
		marker = "*"
	}
	fmt.Fprintf(core.Stdout, "%s %s\n", marker, inst.URL)
	if inst.User != nil && inst.User.ID != "" {
		id := inst.User.ID
		if inst.User.Email != "" {
			id = inst.User.Email + " (" + id + ")"
		}
		fmt.Fprintf(core.Stdout, "    identity: %s\n", id)
	}
	if len(inst.Orgs) == 0 {
		return
	}
	orgs := append([]*authstore.Org(nil), inst.Orgs...)
	sort.Slice(orgs, func(i, j int) bool { return orgs[i].Slug < orgs[j].Slug })
	fmt.Fprintf(core.Stdout, "    organizations (%d):\n", len(orgs))
	for _, o := range orgs {
		def := ""
		if o.ID != "" && o.ID == inst.DefaultOrg {
			def = " [default]"
		}
		fmt.Fprintf(core.Stdout, "      - %s — %s%s\n", o.Slug, o.Role, def)
	}
}

// reportMachineAccount answers "who am I" for a token-bearing caller.
//
// The store cannot answer it — nothing about a machine account is written
// there — so the console is asked instead, which is also the only source that
// can say whether the token still works. No `login` is offered on any path
// here: for this caller it is not a fix, it is a wrong turn.
func (c *Cmd) reportMachineAccount() error {
	addr, aerr := core.ResolveConsoleAddr("")
	fmt.Fprintf(core.Stdout, "Acting as a machine account: %s is set in this environment.\n", core.EnvTokenVar)
	if aerr != nil || addr == "" {
		fmt.Fprintln(core.Stdout, "  but no console is configured, so the token is bound to nothing and is sent nowhere.")
		fmt.Fprintf(core.Stdout, "  fix: set %s to the console the token was minted for.\n", core.EnvConsoleAddrVar)
		return nil
	}
	bound, presented := core.EnvTokenBinding(addr)
	fmt.Fprintf(core.Stdout, "  console: %s\n", addr)
	if !presented {
		// Bound elsewhere is NOT "refused": nothing was sent. Saying so here
		// stops the reader debugging a credential that never left the process.
		fmt.Fprintf(core.Stdout, "  ⚠ the token is bound to %s, so it is NOT sent to this console.\n", bound)
		fmt.Fprintf(core.Stdout, "  fix: point %s at the console being dialed, or dial the one it names.\n", core.EnvConsoleAddrVar)
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	orgs, err := core.ListMyOrgs(ctx, addr, core.EnvToken())
	if err != nil {
		// Returned, not printed: the top-level handler decorates an auth
		// refusal with the causes only this machine can see — including
		// whether the token was presented at all.
		return err
	}
	fmt.Fprintln(core.Stdout, "  verified: the console accepts this token")
	if len(orgs) == 0 {
		// A bot with no membership resolves no org-scoped grant, which is the
		// difference between "the token works" and "the token can do
		// anything". Worth saying, because the two look identical until the
		// first real call.
		fmt.Fprintln(core.Stdout, "  organizations: none — an org-scoped role grants this token nothing")
		return nil
	}
	fmt.Fprintf(core.Stdout, "  organizations (%d):\n", len(orgs))
	for _, o := range orgs {
		fmt.Fprintf(core.Stdout, "    - %s\n", o.Slug)
	}
	return nil
}
