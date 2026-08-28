// Package whoami implements `w17ctl whoami` — show the stored identity (and
// org memberships) for the active console, or all of them with --all. Reads
// the machine-local credential store; no server round-trip. See
// docs/specs/plugins/auth-cli-login-and-orgs.md.
package whoami

import (
	"fmt"
	"sort"

	"github.com/wandering-compiler/w17ctl/internal/authstore"
	"github.com/wandering-compiler/w17ctl/internal/core"
)

// Cmd is `w17ctl whoami`.
type Cmd struct {
	All bool `name:"all" help:"Show every logged-in console, not just the active one."`
}

func (c *Cmd) Run() error {
	st, err := authstore.LoadDefault()
	if err != nil {
		return err
	}
	if len(st.Instances) == 0 {
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
		return nil
	}

	inst := st.ActiveInstance()
	if inst == nil {
		fmt.Fprintln(core.Stdout, "No active console. Run `w17ctl login` or pick one with --all.")
		return nil
	}
	printInstance(true, inst)
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
