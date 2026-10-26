// Package infra implements `w17ctl infra` — the project's infrastructure:
// where it is deployed and how, declared in w17/infra.yaml.
//
// The prompts and flags live here (UX); every change is sent to the console
// with the file's bytes — one SetInfra (the wizard's choice) or EditInfra
// (values by path) — and the console edits the file (comments kept),
// validates it and returns it; the files under deploy/ are rendered by the
// console during codegen. The client decides nothing about infrastructure.
package infra

import (
	"fmt"
	"sort"
	"strings"

	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/prompter"
	codegenpb "github.com/wandering-compiler/sdk/go/pb/w17compiler"
)

// Cmd is `w17ctl infra`.
type Cmd struct {
	Init     InitCmd     `cmd:"" help:"Choose the project's infrastructure — per environment swarm (your own server), aws-ecs or gcp-cloudrun — with its edge, TLS, database backups and server setup. Prompts; every question has a flag. Writes w17/infra.yaml (yours, commit it); the next codegen renders deploy/<env>/."`
	Update   UpdateCmd   `cmd:"" help:"Change the declared infrastructure. The same prompts, each defaulting to what w17/infra.yaml declares now; what they do not ask (workloads, the database offering, limits, your comments) stays."`
	Show     ShowCmd     `cmd:"" help:"Print the declared infrastructure. --effective: every resolved value, with where it came from (w17/infra.yaml:<line> or default)."`
	Remove   RemoveCmd   `cmd:"" help:"Remove one environment (--env) or the whole infrastructure. The next codegen deletes what it had generated for it."`
	Set      SetCmd      `cmd:"" help:"Set values of w17/infra.yaml by path: w17ctl infra set workloads.web.port=3000 'environments.prod.database.backup={mode: daily}'. --env E prefixes environments.E. Comments and order stay; the console validates the whole file."`
	Unset    UnsetCmd    `cmd:"" help:"Remove values of w17/infra.yaml by path (back to the default): w17ctl infra unset environments.prod.limits.PG_WORK_MEM."`
	Target   TargetCmd   `cmd:"" help:"Add or remove a target — a place environments deploy onto (a server, a cloud account)."`
	Workload WorkloadCmd `cmd:"" help:"Add or remove a workload: a container w17 did not generate (a frontend, a Python or Rust service), or the tuning of a generated bundle."`
	Migrate  MigrateCmd  `cmd:"" help:"Move a lock's infra block into w17/infra.yaml (once; every infra edit does it too)."`
	Plan     PlanCmd     `cmd:"" help:"What a deploy of an environment would change: the rendered stack against what the server runs (swarm), or terraform plan (aws-ecs / gcp-cloudrun)."`
	Setup    SetupCmd    `cmd:"" help:"Prepare an environment's server (swarm): copy deploy/<env>/install to the target's ssh and run setup.sh there as root, with your own SSH key."`
}

// flags are the pre-answers shared by init and update.
type flags struct {
	Target         string            `name:"target" help:"swarm | aws-ecs | gcp-cloudrun."`
	Env            []string          `name:"env" help:"Environment names (repeatable or comma-separated), e.g. prod,staging."`
	Domain         map[string]string `name:"domain" help:"Per environment: --domain prod=example.com."`
	Host           map[string]string `name:"host" help:"Override one surface's host: --host prod:app-api=api.example.com (the surface keys are <domain>-<api|rpc|mcp|admin>, listed in deploy/<env>/README.md); swarm also takes a port: --host prod:app-rpc=api.example.com:50051."`
	Edge           string            `name:"edge" help:"swarm: caddy | nginx."`
	TLS            string            `name:"tls" help:"swarm: acme | custom (custom = caddy only)."`
	AcmeEmail      string            `name:"acme-email" help:"swarm + acme: Let's Encrypt account e-mail."`
	Backups        string            `name:"backups" help:"on | off."`
	BackupKeepDays int               `name:"backup-keep-days" help:"Days of history (PITR window). Default 30."`
	BackupSchedule string            `name:"backup-schedule" help:"Daily base backup cron. Default \"0 3 * * *\"."`
	VerifySchedule string            `name:"verify-schedule" help:"Nightly restore test cron. Default \"0 4 * * *\"."`
	ArchiveTimeout int               `name:"archive-timeout-s" help:"Most seconds of data a dead server may lose. Default 60."`
	Alert          string            `name:"alert" help:"sentry | none."`
	InstallDocker  string            `name:"host-install-docker" help:"swarm: yes | no."`
	Firewall       string            `name:"host-firewall" help:"swarm: yes | no — no leaves your firewall alone."`
	SSHNoPassword  string            `name:"host-ssh-no-password" help:"swarm: yes | no."`
	SSH            map[string]string `name:"ssh" help:"swarm, per environment: --ssh prod=root@203.0.113.10 (user@host[:port]; none clears)."`
	Plugin         map[string]string `name:"plugin" help:"swarm: run a plugin activation's services beside the project, on this environment's machine. Name the image of each service its plugin leaves to you: --plugin prod:codegen.worker=bundle:app-server (or image:<name from deploy/images.custom.txt>); then --plugin prod:codegen.worker.replicas=2 (also cpus, memory, command — comma-separated argv). --plugin prod:codegen=on deploys an activation whose plugin leaves no service to you; --plugin prod:codegen=none stops deploying it. To run an activation on another machine, give it target: <another swarm target> in w17/infra.yaml."`
	Region         string            `name:"region" help:"aws-ecs / gcp-cloudrun: region."`
	Account        string            `name:"account" help:"aws-ecs: account id; gcp-cloudrun: project id."`
	LockPath       string            `name:"lock" placeholder:"PATH" default:"w17/lock.yaml" help:"Path to the lock file."`
	Console        string            `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console (owns the lock). Optional — falls back to the binary's compile-time default."`
}

func (f flags) answers() Answers {
	var envs []string
	for _, e := range f.Env {
		envs = append(envs, splitList(e)...)
	}
	return Answers{
		Target: f.Target, Envs: envs, Domain: f.Domain, Hosts: f.Host,
		Edge: f.Edge, TLS: f.TLS, AcmeEmail: f.AcmeEmail,
		Backups: f.Backups, KeepDays: f.BackupKeepDays, BaseSchedule: f.BackupSchedule,
		VerifySched: f.VerifySchedule, ArchiveTO: f.ArchiveTimeout, Alert: f.Alert,
		InstallDock: f.InstallDocker, Firewall: f.Firewall, SSHNoPass: f.SSHNoPassword, SSH: f.SSH,
		Plugins: f.Plugin,
		Region:  f.Region, Account: f.Account,
	}
}

// newPrompter is a seam for tests.
var newPrompter = prompter.NewStdinPrompter

// InitCmd is `w17ctl infra init`.
type InitCmd struct{ flags }

func (c *InitCmd) Run() error { return run("infra init", c.flags, false) }

// UpdateCmd is `w17ctl infra update`.
type UpdateCmd struct{ flags }

func (c *UpdateCmd) Run() error { return run("infra update", c.flags, true) }

func run(op string, f flags, update bool) error {
	var next *codegenpb.LockInfra
	_, err := core.EditInfraOnDisk(op, f.Console, f.LockPath, func(view *codegenpb.LockView) (*codegenpb.LockEditIntent, error) {
		current := view.GetInfra()
		switch {
		case !update && view.GetInfraSource() == "lock":
			// The lock still carries the block: move it, change nothing else.
			return &codegenpb.LockEditIntent{Intent: &codegenpb.LockEditIntent_EditInfra{EditInfra: &codegenpb.EditInfraIntent{}}}, nil
		case !update && current != nil:
			return nil, fmt.Errorf("w17/infra.yaml already declares infrastructure (%s) — change it with `w17ctl infra update`", envNames(current))
		case update && current == nil:
			return nil, fmt.Errorf("no infrastructure declared yet — start with `w17ctl infra init`")
		}
		var err error
		next, err = Wizard(newPrompter(), current, f.answers())
		if err != nil {
			return nil, err
		}
		return &codegenpb.LockEditIntent{Intent: &codegenpb.LockEditIntent_SetInfra{SetInfra: &codegenpb.SetInfraIntent{Infra: next}}}, nil
	})
	if err != nil {
		return err
	}
	if next == nil {
		fmt.Fprintf(core.Stdout, "%s: change it with `w17ctl infra update`, or edit w17/infra.yaml\n", op)
		return nil
	}
	fmt.Fprintf(core.Stdout, "%s: %s (%s)\n", op, envNames(next), core.InfraPath(f.LockPath))
	printNextSteps(next)
	return nil
}

func envNames(in *codegenpb.LockInfra) string {
	var names []string
	for _, e := range in.GetEnvironments() {
		names = append(names, e.GetName()+" on "+firstNonEmpty(e.GetTargetKind(), in.GetTarget()))
	}
	return strings.Join(names, ", ")
}

func printNextSteps(in *codegenpb.LockInfra) {
	swarm := false
	for _, e := range in.GetEnvironments() {
		swarm = swarm || firstNonEmpty(e.GetTargetKind(), in.GetTarget()) == "swarm"
	}
	fmt.Fprintln(core.Stdout, "next:")
	fmt.Fprintln(core.Stdout, "  1. w17ctl codegen                       — renders deploy/<env>/ (commit w17/infra.yaml; deploy/ is generated)")
	if swarm {
		fmt.Fprintln(core.Stdout, "     w17ctl infra setup --env <env>       — prepares the server named by --ssh, with your SSH key")
	}
	fmt.Fprintln(core.Stdout, "  2. w17ctl secrets set --env <env> …     — each service's values, as its .env.example names them (or the project's page)")
	fmt.Fprintln(core.Stdout, "  3. a machine account with Deploy access — the project's page → Deploy access to <env>; the deploy fetches with its token")
	fmt.Fprintln(core.Stdout, "  4. w17ctl ci init                       — the build and deploy workflows")
}

// ShowCmd is `w17ctl infra show`.
type ShowCmd struct {
	Effective bool   `name:"effective" help:"Every resolved value, with where it came from (w17/infra.yaml:<line>, or default)."`
	Env       string `name:"env" help:"Only this environment (with --effective)."`
	LockPath  string `name:"lock" placeholder:"PATH" default:"w17/lock.yaml" help:"Path to the lock file."`
	Console   string `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console (owns the lock). Optional — falls back to the binary's compile-time default."`
}

func (c *ShowCmd) Run() error {
	view, err := core.DescribeInfraAt("infra show", c.Console, c.LockPath, c.Effective, c.Env)
	if err != nil {
		return err
	}
	in := view.GetInfra()
	if in == nil {
		fmt.Fprintln(core.Stdout, "no infrastructure declared — `w17ctl infra init` chooses one")
		return nil
	}
	if view.GetInfraSource() == "lock" {
		fmt.Fprintln(core.Stdout, "# declared in the lock's infra block — `w17ctl infra migrate` moves it into w17/infra.yaml")
	}
	if c.Effective {
		fmt.Fprint(core.Stdout, view.GetInfraEffective())
		return nil
	}
	w := core.Stdout
	for _, e := range in.GetEnvironments() {
		fmt.Fprintf(w, "environment %s:\n", e.GetName())
		fmt.Fprintf(w, "  target: %s (%s)\n", e.GetTarget(), e.GetTargetKind())
		fmt.Fprintf(w, "  domain: %s\n", e.GetDomain())
		if len(e.GetHosts()) > 0 {
			keys := make([]string, 0, len(e.GetHosts()))
			for k := range e.GetHosts() {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				fmt.Fprintf(w, "  host %s: %s\n", k, e.GetHosts()[k])
			}
		}
		if e.GetEdge() != "" {
			fmt.Fprintf(w, "  edge: %s, tls: %s", e.GetEdge(), e.GetTls())
			if e.GetAcmeEmail() != "" {
				fmt.Fprintf(w, " (%s)", e.GetAcmeEmail())
			}
			fmt.Fprintln(w)
		}
		if c := e.GetCloud(); c != nil {
			fmt.Fprintf(w, "  region: %s, account: %s\n", c.GetRegion(), c.GetAccount())
		}
		if b := e.GetBackups(); b.GetEnabled() {
			fmt.Fprintf(w, "  backups: keep %d days\n", b.GetKeepDays())
		} else {
			fmt.Fprintln(w, "  backups: off")
		}
		if h := e.GetHost(); h != nil {
			fmt.Fprintf(w, "  server setup: install docker %v, manage firewall %v, no ssh passwords %v\n",
				h.GetInstallDocker(), h.GetManageFirewall(), h.GetSshNoPassword())
			if h.GetSsh() != "" {
				fmt.Fprintf(w, "  ssh: %s\n", h.GetSsh())
			}
		}
		for _, d := range e.GetPlugins() {
			names := make([]string, 0, len(d.GetServices()))
			for n := range d.GetServices() {
				names = append(names, n)
			}
			sort.Strings(names)
			for _, n := range names {
				svc := d.GetServices()[n]
				img := "bundle " + svc.GetBundle()
				if svc.GetImage() != "" {
					img = "image " + svc.GetImage()
				}
				fmt.Fprintf(w, "  plugin %s.%s: %s", d.GetActivation(), n, img)
				if svc.GetReplicas() > 1 {
					fmt.Fprintf(w, ", %d replicas", svc.GetReplicas())
				}
				if svc.GetCpus() != "" {
					fmt.Fprintf(w, ", cpus %s", svc.GetCpus())
				}
				if svc.GetMemory() != "" {
					fmt.Fprintf(w, ", memory %s", svc.GetMemory())
				}
				if len(svc.GetCommand()) > 0 {
					fmt.Fprintf(w, ", command %q", svc.GetCommand())
				}
				fmt.Fprintln(w)
			}
		}
	}
	fmt.Fprintln(w, "(workloads, the database and every default: `w17ctl infra show --effective`)")
	return nil
}

// RemoveCmd is `w17ctl infra remove`.
type RemoveCmd struct {
	Env      string `name:"env" help:"Remove only this environment. Without it the whole infrastructure is removed."`
	LockPath string `name:"lock" placeholder:"PATH" default:"w17/lock.yaml" help:"Path to the lock file."`
	Console  string `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console (owns the lock). Optional — falls back to the binary's compile-time default."`
}

func (c *RemoveCmd) Run() error {
	const op = "infra remove"
	what := "the whole infrastructure"
	_, err := core.EditInfraOnDisk(op, c.Console, c.LockPath, func(view *codegenpb.LockView) (*codegenpb.LockEditIntent, error) {
		current := view.GetInfra()
		if current == nil {
			return nil, fmt.Errorf("no infrastructure declared")
		}
		intent := &codegenpb.SetInfraIntent{Clear: true}
		if c.Env != "" {
			// The CI block belongs to the whole infrastructure, not the
			// environment going.
			kept := &codegenpb.LockInfra{Target: current.GetTarget(), Ci: current.GetCi()}
			found := false
			for _, e := range current.GetEnvironments() {
				if e.GetName() == c.Env {
					found = true
					continue
				}
				kept.Environments = append(kept.Environments, e)
			}
			if !found {
				return nil, fmt.Errorf("no environment %q (declared: %s)", c.Env, envNames(current))
			}
			what = "environment " + c.Env
			// The last environment going takes the infrastructure with it.
			if len(kept.GetEnvironments()) > 0 {
				intent = &codegenpb.SetInfraIntent{Infra: kept}
			}
		}
		return &codegenpb.LockEditIntent{Intent: &codegenpb.LockEditIntent_SetInfra{SetInfra: intent}}, nil
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(core.Stdout, "%s: removed %s — the next `w17ctl codegen` deletes its generated files; its values on the console stay\n", op, what)
	return nil
}
