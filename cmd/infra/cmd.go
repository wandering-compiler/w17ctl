// Package infra implements `w17ctl infra` — the project's infrastructure:
// where it is deployed and how (docs/decisions/infra-targets.md).
//
// The prompts live here (UX); the choice is sent to the console as ONE
// SetInfra lock edit and validated there; the files are rendered by the
// console during codegen. The client decides nothing about infrastructure
// (docs/specs/w17ctl/public-split-architecture.md §8.2).
package infra

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/lockfile"
	"github.com/wandering-compiler/w17ctl/internal/prompter"
	codegenpb "github.com/wandering-compiler/sdk/go/pb/w17compiler"
)

// Cmd is `w17ctl infra`.
type Cmd struct {
	Init   InitCmd   `cmd:"" help:"Choose the project's infrastructure — swarm (your own servers), aws-ecs or gcp-cloudrun — and its environments, edge, TLS, database backups and server setup. Prompts; every question has a flag. Writes the signed lock; the next codegen renders deploy/<env>/."`
	Update UpdateCmd `cmd:"" help:"Change the declared infrastructure. The same prompts, each defaulting to what the lock declares now."`
	Show   ShowCmd   `cmd:"" help:"Print the declared infrastructure."`
	Remove RemoveCmd `cmd:"" help:"Remove one environment (--env) or the whole infrastructure. The next codegen deletes what it had generated for it."`
	Setup  SetupCmd  `cmd:"" help:"Prepare an environment's server (swarm): copy deploy/<env>/install to the lock's host.ssh and run setup.sh there as root, with your own SSH key."`
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
	Plugin         map[string]string `name:"plugin" help:"swarm: run a plugin activation's services beside the project, on this environment's machine. Name the image of each service its plugin leaves to you: --plugin prod:codegen.worker=bundle:app-server (or image:<name from deploy/images.custom.txt>); then --plugin prod:codegen.worker.replicas=2 (also cpus, memory, command — comma-separated argv). --plugin prod:codegen=on deploys an activation whose plugin leaves no service to you; --plugin prod:codegen=none stops deploying it."`
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
	release, err := lockfile.ForUpdate(f.LockPath)
	if err != nil {
		return fmt.Errorf("%s: lock for update: %w", op, err)
	}
	defer release()
	lockBytes, err := os.ReadFile(f.LockPath)
	if err != nil {
		return fmt.Errorf("%s: read lock %s: %w", op, f.LockPath, err)
	}
	view, err := core.DescribeLock(f.Console, lockBytes)
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	current := view.GetInfra()
	switch {
	case !update && current != nil:
		return fmt.Errorf("%s: the lock already declares infrastructure (%s) — change it with `w17ctl infra update`", op, current.GetTarget())
	case update && current == nil:
		return fmt.Errorf("%s: the lock declares no infrastructure yet — start with `w17ctl infra init`", op)
	}

	next, err := Wizard(newPrompter(), current, f.answers())
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	newBytes, err := core.EditLock(f.Console, lockBytes, &codegenpb.LockEditIntent{
		Intent: &codegenpb.LockEditIntent_SetInfra{SetInfra: &codegenpb.SetInfraIntent{Infra: next}},
	})
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	if err := lockfile.WriteAtomic(f.LockPath, newBytes, 0o644); err != nil {
		return fmt.Errorf("%s: write lock: %w", op, err)
	}
	fmt.Fprintf(core.Stdout, "%s: %s with %s (%s)\n", op, next.GetTarget(), envNames(next), f.LockPath)
	printNextSteps(next)
	return nil
}

func envNames(in *codegenpb.LockInfra) string {
	var names []string
	for _, e := range in.GetEnvironments() {
		names = append(names, e.GetName())
	}
	return strings.Join(names, ", ")
}

func printNextSteps(in *codegenpb.LockInfra) {
	fmt.Fprintln(core.Stdout, "next:")
	fmt.Fprintln(core.Stdout, "  1. w17ctl codegen                       — renders deploy/<env>/")
	if in.GetTarget() == "swarm" {
		fmt.Fprintln(core.Stdout, "     w17ctl infra setup --env <env>       — prepares the server named by --ssh, with your SSH key")
	}
	fmt.Fprintln(core.Stdout, "  2. fill deploy/<env>/**/.env            — from the .env.example beside each")
	fmt.Fprintln(core.Stdout, "  3. w17ctl secrets keygen --env <env>    — once per environment; keep the private key")
	fmt.Fprintln(core.Stdout, "     w17ctl secrets seal                  — writes the .env.enc you commit")
	fmt.Fprintln(core.Stdout, "  4. w17ctl ci init                       — the build and deploy workflows")
}

// ShowCmd is `w17ctl infra show`.
type ShowCmd struct {
	LockPath string `name:"lock" placeholder:"PATH" default:"w17/lock.yaml" help:"Path to the lock file."`
	Console  string `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console (owns the lock). Optional — falls back to the binary's compile-time default."`
}

func (c *ShowCmd) Run() error {
	view, err := core.DescribeLockAt("infra show", c.Console, c.LockPath)
	if err != nil {
		return err
	}
	in := view.GetInfra()
	if in == nil {
		fmt.Fprintln(core.Stdout, "no infrastructure declared — `w17ctl infra init` chooses one")
		return nil
	}
	w := core.Stdout
	fmt.Fprintf(w, "target: %s\n", in.GetTarget())
	for _, e := range in.GetEnvironments() {
		fmt.Fprintf(w, "environment %s:\n", e.GetName())
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
			fmt.Fprintf(w, "  backups: keep %d days, base %q, restore test %q, max loss %ds, alert %s\n",
				b.GetKeepDays(), b.GetBaseSchedule(), b.GetVerifySchedule(), b.GetArchiveTimeoutS(), b.GetAlert())
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
	release, err := lockfile.ForUpdate(c.LockPath)
	if err != nil {
		return fmt.Errorf("%s: lock for update: %w", op, err)
	}
	defer release()
	lockBytes, err := os.ReadFile(c.LockPath)
	if err != nil {
		return fmt.Errorf("%s: read lock %s: %w", op, c.LockPath, err)
	}
	view, err := core.DescribeLock(c.Console, lockBytes)
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	current := view.GetInfra()
	if current == nil {
		return fmt.Errorf("%s: the lock declares no infrastructure", op)
	}
	intent := &codegenpb.SetInfraIntent{Clear: true}
	what := "the whole infrastructure"
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
			return fmt.Errorf("%s: no environment %q (declared: %s)", op, c.Env, envNames(current))
		}
		what = "environment " + c.Env
		// The last environment going takes the infrastructure with it — an
		// infrastructure with no environment is refused by the lock anyway.
		if len(kept.GetEnvironments()) > 0 {
			intent = &codegenpb.SetInfraIntent{Infra: kept}
		}
	}
	newBytes, err := core.EditLock(c.Console, lockBytes, &codegenpb.LockEditIntent{
		Intent: &codegenpb.LockEditIntent_SetInfra{SetInfra: intent},
	})
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	if err := lockfile.WriteAtomic(c.LockPath, newBytes, 0o644); err != nil {
		return fmt.Errorf("%s: write lock: %w", op, err)
	}
	fmt.Fprintf(core.Stdout, "%s: removed %s (%s) — the next `w17ctl codegen` deletes its generated files; your .env / .env.enc stay\n", op, what, c.LockPath)
	return nil
}
