// Package org implements `w17ctl org` — list the organizations the
// logged-in user belongs to and choose the default one. The active org is a
// per-instance default in ~/.w17/auth.yaml; w17ctl threads it as the
// X-W17-Org selector so the console scopes requests to it (membership is
// validated server-side). See docs/specs/plugins/auth-cli-login-and-orgs.md.
package org

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/wandering-compiler/w17ctl/internal/authstore"
	"github.com/wandering-compiler/w17ctl/internal/core"
)

// refreshTimeout bounds the `org list --refresh` console round-trip so a hung
// console can't hang the command forever.
const refreshTimeout = 30 * time.Second

// Cmd is the `w17ctl org` group.
type Cmd struct {
	List ListCmd `cmd:"" help:"List the organizations you belong to on the active console (--refresh re-fetches from the console)."`
	Use  UseCmd  `cmd:"" help:"Set the default organization (by slug) for the active console — used to scope subsequent commands."`
}

// ListCmd is `w17ctl org list`.
type ListCmd struct {
	Refresh bool `name:"refresh" help:"Re-fetch memberships from the console and update the local cache."`
}

func (c *ListCmd) Run() error {
	st, err := authstore.LoadDefault()
	if err != nil {
		return err
	}
	inst := st.ActiveInstance()
	if inst == nil {
		fmt.Fprintln(core.Stdout, "Not logged in. Run `w17ctl login <console-url>`.")
		return nil
	}
	if c.Refresh {
		ctx, cancel := context.WithTimeout(context.Background(), refreshTimeout)
		defer cancel()
		orgs, err := core.ListMyOrgs(ctx, inst.URL, inst.Token)
		if err != nil {
			return fmt.Errorf("org list: refresh from %s: %w", inst.URL, err)
		}
		inst.Orgs = toStoreOrgs(orgs)
		if err := authstore.SaveDefault(st); err != nil {
			return err
		}
	}
	printOrgs(inst)
	return nil
}

// UseCmd is `w17ctl org use <slug>`.
type UseCmd struct {
	Slug string `arg:"" help:"Organization slug to make the default."`
}

func (c *UseCmd) Run() error {
	st, err := authstore.LoadDefault()
	if err != nil {
		return err
	}
	inst := st.ActiveInstance()
	if inst == nil {
		fmt.Fprintln(core.Stdout, "Not logged in. Run `w17ctl login <console-url>`.")
		return nil
	}
	o := inst.Org(c.Slug)
	if o == nil {
		return fmt.Errorf("org use: you are not a member of %q on %s (try `w17ctl org list --refresh`)", c.Slug, inst.URL)
	}
	inst.DefaultOrg = o.ID
	if err := authstore.SaveDefault(st); err != nil {
		return err
	}
	fmt.Fprintf(core.Stdout, "Default organization for %s set to %s (%s).\n", inst.URL, o.Slug, orgName(o))
	return nil
}

func printOrgs(inst *authstore.Instance) {
	if len(inst.Orgs) == 0 {
		fmt.Fprintf(core.Stdout, "No organizations on %s yet.\n", inst.URL)
		return
	}
	orgs := append([]*authstore.Org(nil), inst.Orgs...)
	sort.Slice(orgs, func(i, j int) bool { return orgs[i].Slug < orgs[j].Slug })
	fmt.Fprintf(core.Stdout, "Organizations on %s:\n", inst.URL)
	for _, o := range orgs {
		marker := " "
		if o.ID != "" && o.ID == inst.DefaultOrg {
			marker = "*"
		}
		fmt.Fprintf(core.Stdout, "  %s %-20s %-10s %s\n", marker, o.Slug, kindOrOrg(o.Kind), o.Role)
	}
	if inst.DefaultOrg == "" {
		fmt.Fprintln(core.Stdout, "(no default — pick one with `w17ctl org use <slug>`)")
	}
}

func toStoreOrgs(orgs []core.AuthOrg) []*authstore.Org {
	if len(orgs) == 0 {
		return nil
	}
	out := make([]*authstore.Org, 0, len(orgs))
	for _, o := range orgs {
		out = append(out, &authstore.Org{ID: o.ID, Slug: o.Slug, Name: o.Name, Kind: o.Kind, Role: o.Role})
	}
	return out
}

func kindOrOrg(kind string) string {
	if kind == "" {
		return "org"
	}
	return kind
}

func orgName(o *authstore.Org) string {
	if o.Name != "" {
		return o.Name
	}
	return o.Slug
}
