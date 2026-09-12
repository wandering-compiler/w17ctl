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
