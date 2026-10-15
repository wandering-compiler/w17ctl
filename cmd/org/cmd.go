// Package org implements `w17ctl org` — list the organizations the
// logged-in user belongs to and choose the default one. The active org is a
// per-instance default in ~/.w17/auth.yaml; w17ctl threads it as the
// X-W17-Org selector so the console scopes requests to it (membership is
// validated server-side). `org pin` records an organization for a directory
// that is not a project, in a minimal w17/lock.yaml.
package org

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/wandering-compiler/w17ctl/internal/authstore"
	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/lockfile"
)

// refreshTimeout bounds the `org list --refresh` console round-trip so a hung
// console can't hang the command forever.
const refreshTimeout = 30 * time.Second

// Cmd is the `w17ctl org` group.
type Cmd struct {
	List ListCmd `cmd:"" help:"List the organizations you belong to on the active console (--refresh re-fetches from the console)."`
	Use  UseCmd  `cmd:"" help:"Set the default organization (by slug) for the active console — used to scope subsequent commands."`
	Pin  PinCmd  `cmd:"" help:"Record an organization for THIS directory in w17/lock.yaml — for a directory that is not a w17 project (a plugins repository), so every w17ctl run in it acts there, an unattended W17_TOKEN run included."`
}

// ListCmd is `w17ctl org list`.
type ListCmd struct {
	Refresh bool `name:"refresh" help:"Re-fetch memberships from the console and update the local cache."`
}

// targetInstance is the stored console an `org` command acts on: the one
// W17_CONSOLE_ADDR names when it is set — the variable points ONE command at
// another console without moving the pointer, as everywhere else — else the
// active one. nil, with the line to print, when there is no credential for it.
func targetInstance(st *authstore.Store) (*authstore.Instance, string) {
	if addr := os.Getenv(core.EnvConsoleAddrVar); addr != "" {
		if inst := st.Instance(addr); inst != nil {
			return inst, ""
		}
		return nil, fmt.Sprintf("Not logged in to %s (named by %s). Run `w17ctl login %s`.", addr, core.EnvConsoleAddrVar, addr)
	}
	if inst := st.ActiveInstance(); inst != nil {
		return inst, ""
	}
	return nil, "Not logged in. Run `w17ctl login <console-url>`."
}

func (c *ListCmd) Run() error {
	st, err := authstore.LoadDefault()
	if err != nil {
		return err
	}
	inst, why := targetInstance(st)
	if inst == nil {
		fmt.Fprintln(core.Stdout, why)
		return nil
	}
	if c.Refresh {
		ctx, cancel := context.WithTimeout(context.Background(), refreshTimeout)
		defer cancel()
		orgs, err := core.ListMyOrgsAs(ctx, inst.URL, inst.Token, inst.Device)
		if err != nil {
			return fmt.Errorf("org list: refresh from %s: %w", inst.URL, err)
		}
		inst.Orgs = toStoreOrgs(orgs)
		if err := authstore.SaveDefault(st); err != nil {
			return err
		}
	}
	printOrgs(inst)
	if !c.Refresh {
		fmt.Fprintln(core.Stdout, "  (the stored list — `org list --refresh` asks the console)")
	}
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
	inst, why := targetInstance(st)
	if inst == nil {
		fmt.Fprintln(core.Stdout, why)
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

// PinCmd is `w17ctl org pin <slug>`.
//
// A project's lock names its organization because the console stamps org_id
// into it at every signed write. A directory that is NOT a project — the
// plugins repository, where `plugin sign` and `plugin gen` run — has no lock
// for the console to stamp, so a run there named no organization at all. For a
// person with a default (`org use`) that went unnoticed; an environment token
// has no default, and a console facing an account in several organizations
// refuses to guess. This writes the minimal lock such a directory carries:
// org_id, the authoritative id, org_slug beside it for the reader (and as the
// key an api-realm token needs to have its memberships listed), and w17_url —
// the console whose organization it is, since an org id means nothing to any
// other console and is sent to none.
//
// Written by hand it would be two lines; it is a command because the id is a
// UUID nobody types, and because the one place it must never be written is
// inside a project, whose lock is the console's.
type PinCmd struct {
	Slug string `arg:"" help:"Organization slug to pin this directory to (one you are a member of)."`
}

func (c *PinCmd) Run() error {
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("org pin: %w", err)
	}
	if err := refuseInsideProject(); err != nil {
		return fmt.Errorf("org pin: %w", err)
	}
	st, err := authstore.LoadDefault()
	if err != nil {
		return err
	}
	inst, why := targetInstance(st)
	if inst == nil {
		return fmt.Errorf("org pin: %s", why)
	}
	// Asked, not read from the cache: the id lands in a committed file that
	// every later run trusts, and a cache from an earlier login can predate
	// the membership.
	ctx, cancel := context.WithTimeout(context.Background(), refreshTimeout)
	defer cancel()
	orgs, err := core.ListMyOrgsAs(ctx, inst.URL, inst.Token, inst.Device)
	if err != nil {
		return fmt.Errorf("org pin: list your organizations on %s: %w", inst.URL, err)
	}
	var pick *core.AuthOrg
	var slugs []string
	for i := range orgs {
		slugs = append(slugs, orgs[i].Slug)
		if orgs[i].Slug == c.Slug {
			pick = &orgs[i]
		}
	}
	if pick == nil || pick.ID == "" {
		sort.Strings(slugs)
		return fmt.Errorf("org pin: you are not a member of %q on %s (member of: %v)", c.Slug, inst.URL, slugs)
	}
	if err := os.MkdirAll(filepath.Join(cwd, "w17"), 0o755); err != nil {
		return fmt.Errorf("org pin: %w", err)
	}
	path := filepath.Join(cwd, lockfile.DefaultPath)
	body := fmt.Sprintf("# Written by `w17ctl org pin %s` against %s.\n"+
		"# This directory is not a w17 project: the lock names only the organization\n"+
		"# w17ctl acts in here (sent as the w17-org header). Commit it.\n"+
		"org_id: %s\norg_slug: %s\nw17_url: %s\n", pick.Slug, inst.URL, pick.ID, pick.Slug, inst.URL)
	if err := lockfile.WriteAtomic(path, []byte(body), 0o644); err != nil {
		return fmt.Errorf("org pin: %w", err)
	}
	fmt.Fprintf(core.Stdout, "Pinned %s to organization %s (%s) in %s — commit it.\n", cwd, pick.Slug, pick.ID, lockfile.DefaultPath)
	return nil
}

// refuseInsideProject keeps a pin out of a project. A project's lock carries
// its organization already, stamped by the console; a second lock below it
// would make every command run from there find the pin instead of the project.
// A directory already pinned may be re-pinned.
func refuseInsideProject() error {
	root, err := core.FindProjectRoot()
	if err != nil {
		return nil // not inside anything — the case this command is for
	}
	path := filepath.Join(root, lockfile.DefaultPath)
	lk, err := lockfile.Load(path)
	if err != nil {
		return fmt.Errorf("%s holds a w17/ directory without a readable lock — a project before its first signed write; "+
			"a project's organization is stamped into its lock by the console, not pinned", root)
	}
	if lk.Project != "" || lk.ProjectID != "" {
		return fmt.Errorf("this is inside the w17 project at %s, whose lock names its organization already (org_id, stamped by "+
			"the console at every signed write) — a pin is for a directory that is not a project", root)
	}
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
