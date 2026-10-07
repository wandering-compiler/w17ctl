// Package ci implements `w17ctl ci` — the automation that builds the images
// and deploys the declared infrastructure (docs/decisions/infra-targets.md
// §4.4, §9). The choice is one SetInfra lock edit (the CI block plus each
// environment's deploy branch); the workflows themselves are rendered by the
// console during codegen into .github/workflows/w17-*.yaml.
package ci

import (
	"fmt"
	"os"
	"strings"

	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/lockfile"
	"github.com/wandering-compiler/w17ctl/internal/prompter"
	codegenpb "github.com/wandering-compiler/sdk/go/pb/w17compiler"
	"google.golang.org/protobuf/proto"
)

// Cmd is `w17ctl ci`.
type Cmd struct {
	Init   InitCmd   `cmd:"" help:"Generate the GitHub workflows for the declared infrastructure: w17-ci (tests, stack checks), w17-build (images to GHCR), one w17-deploy-<env> per environment. Prompts; every question has a flag. Writes the signed lock; the next codegen writes .github/workflows/w17-*.yaml."`
	Update UpdateCmd `cmd:"" help:"Change the CI choice. The same prompts, each defaulting to what the lock declares now."`
	Show   ShowCmd   `cmd:"" help:"Print the CI choice and what to set up in GitHub."`
	Remove RemoveCmd `cmd:"" help:"Stop generating the workflows. The next codegen deletes the w17-*.yaml it wrote."`
}

type flags struct {
	Provider     string            `name:"provider" help:"github."`
	Registry     string            `name:"registry" help:"ghcr."`
	Runner       string            `name:"runner" help:"Runner label, e.g. ubuntu-latest or a self-hosted label."`
	Platforms    string            `name:"platforms" help:"Image platforms: linux/amd64 (default), linux/arm64, or both comma-separated."`
	DeployBranch map[string]string `name:"deploy-branch" help:"Per environment: --deploy-branch prod=production. 'any' = every ref may deploy."`
	LockPath     string            `name:"lock" placeholder:"PATH" default:"w17/lock.yaml" help:"Path to the lock file."`
	Console      string            `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console (owns the lock). Optional — falls back to the binary's compile-time default."`
}

var newPrompter = prompter.NewStdinPrompter

// InitCmd is `w17ctl ci init`.
type InitCmd struct{ flags }

func (c *InitCmd) Run() error { return run("ci init", c.flags, false) }

// UpdateCmd is `w17ctl ci update`.
type UpdateCmd struct{ flags }

func (c *UpdateCmd) Run() error { return run("ci update", c.flags, true) }

func run(op string, f flags, update bool) error {
	return editInfra(op, f.LockPath, f.Console, func(in *codegenpb.LockInfra) error {
		switch {
		case !update && in.GetCi() != nil:
			return fmt.Errorf("the lock already declares CI (%s) — change it with `w17ctl ci update`", in.GetCi().GetProvider())
		case update && in.GetCi() == nil:
			return fmt.Errorf("the lock declares no CI yet — start with `w17ctl ci init`")
		}
		return Wizard(newPrompter(), in, f)
	}, func(in *codegenpb.LockInfra) { printSetup(in) })
}

// editInfra reads the lock's infra block, lets mutate change a copy, and
// writes it back as one SetInfra edit the console validates.
func editInfra(op, lockPath, console string, mutate func(*codegenpb.LockInfra) error, after func(*codegenpb.LockInfra)) error {
	release, err := lockfile.ForUpdate(lockPath)
	if err != nil {
		return fmt.Errorf("%s: lock for update: %w", op, err)
	}
	defer release()
	lockBytes, err := os.ReadFile(lockPath)
	if err != nil {
		return fmt.Errorf("%s: read lock %s: %w", op, lockPath, err)
	}
	view, err := core.DescribeLock(console, lockBytes)
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	if view.GetInfra() == nil {
		return fmt.Errorf("%s: the lock declares no infrastructure — CI deploys one, so start with `w17ctl infra init`", op)
	}
	next := proto.Clone(view.GetInfra()).(*codegenpb.LockInfra)
	if err := mutate(next); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	newBytes, err := core.EditLock(console, lockBytes, &codegenpb.LockEditIntent{
		Intent: &codegenpb.LockEditIntent_SetInfra{SetInfra: &codegenpb.SetInfraIntent{Infra: next}},
	})
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	if err := lockfile.WriteAtomic(lockPath, newBytes, 0o644); err != nil {
		return fmt.Errorf("%s: write lock: %w", op, err)
	}
	if after != nil {
		after(next)
	}
	return nil
}

// Wizard asks for the CI choice, defaulting to what is declared (or the
// conventions), and writes it into in.
func Wizard(p prompter.Prompter, in *codegenpb.LockInfra, f flags) error {
	cur := in.GetCi()
	ci := &codegenpb.LockInfraCI{}
	var err error
	if ci.Provider, err = choose(p, f.Provider, "CI provider (the workflows go to .github/workflows/w17-*.yaml)", []string{"github"}, firstNonEmpty(cur.GetProvider(), "github")); err != nil {
		return err
	}
	if ci.Registry, err = choose(p, f.Registry, "Image registry", []string{"ghcr"}, firstNonEmpty(cur.GetRegistry(), "ghcr")); err != nil {
		return err
	}
	if ci.Runner, err = text(p, f.Runner, "Runner label (ubuntu-latest, or a self-hosted label)", firstNonEmpty(cur.GetRunner(), "ubuntu-latest")); err != nil {
		return err
	}
	if ci.Platforms, err = text(p, f.Platforms, "Image platforms (linux/amd64, linux/arm64, or both comma-separated)", firstNonEmpty(cur.GetPlatforms(), "linux/amd64")); err != nil {
		return err
	}
	in.Ci = ci
	for _, env := range in.GetEnvironments() {
		def := env.GetDeployBranch()
		if def == "" {
			if env.GetName() == "prod" || env.GetName() == "production" {
				def = "production"
			} else {
				def = "any"
			}
		}
		b, err := text(p, f.DeployBranch[env.GetName()], fmt.Sprintf("[%s] Branch it deploys from ('any' = every ref)", env.GetName()), def)
		if err != nil {
			return err
		}
		if b == "any" {
			b = ""
		}
		env.DeployBranch = b
	}
	return nil
}

func choose(p prompter.Prompter, flag, question string, options []string, def string) (string, error) {
	if flag != "" {
		for _, o := range options {
			if o == flag {
				return flag, nil
			}
		}
		return "", fmt.Errorf("%q is not one of %v", flag, options)
	}
	return p.Select(question, options, def)
}

func text(p prompter.Prompter, flag, question, def string) (string, error) {
	if flag != "" {
		return strings.TrimSpace(flag), nil
	}
	v, err := p.Text(question, def)
	return strings.TrimSpace(v), err
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

// printSetup is the one manual step: what to create in GitHub. Nothing the
// workflows need is in this list unless they really read it.
func printSetup(in *codegenpb.LockInfra) {
	w := core.Stdout
	swarm := in.GetTarget() == "swarm"
	if swarm {
		fmt.Fprintln(w, "next: `w17ctl codegen` writes .github/workflows/w17-ci.yaml, w17-build.yaml and")
	} else {
		fmt.Fprintln(w, "next: `w17ctl codegen` writes .github/workflows/w17-ci.yaml and")
	}
	fmt.Fprintln(w, "      one w17-deploy-<env>.yaml per environment. Then in GitHub:")
	fmt.Fprintln(w, "  repository secret  W17_TOKEN          a `ci-push` machine account's token (console: auth/bots)")
	for _, env := range in.GetEnvironments() {
		fmt.Fprintf(w, "  environment %s:\n", env.GetName())
		if b := env.GetDeployBranch(); b != "" {
			fmt.Fprintf(w, "    deployment branches: only %s (Settings → Environments) — the workflow refuses other refs too\n", b)
		}
		fmt.Fprintln(w, "    secret    SOPS_AGE_KEY    the environment's age private key (`w17ctl secrets keygen` printed it)")
		switch in.GetTarget() {
		case "swarm":
			fmt.Fprintln(w, "    secret    STACK_SSH_KEY   a private SSH key root@server accepts (deploy only)")
			fmt.Fprintln(w, "    variable  STACK_HOST_KEY  the line `setup.sh` printed (pins the server's SSH host key)")
			if env.GetHost().GetSsh() == "" {
				fmt.Fprintf(w, "    ⚠ no server in the lock — `w17ctl infra update --ssh %s=root@<server>`; the deploy workflow targets it\n", env.GetName())
			}
			fmt.Fprintln(w, "    variable  STACK_LIMIT_OVERRIDES (optional)  e.g. COLOR_WATCH_S=20")
		case "aws-ecs":
			fmt.Fprintln(w, "    variable  AWS_ROLE_ARN    deploy/"+env.GetName()+"/terraform/bootstrap's role_arn output (GitHub OIDC, no AWS key)")
			fmt.Fprintln(w, "    variable  TF_STATE_BUCKET the bootstrap's bucket")
		case "gcp-cloudrun":
			fmt.Fprintln(w, "    variable  GCP_WORKLOAD_IDENTITY_PROVIDER, GCP_SERVICE_ACCOUNT  deploy/"+env.GetName()+"/terraform/bootstrap's outputs")
			fmt.Fprintln(w, "    variable  TF_STATE_BUCKET the bootstrap's bucket")
		}
	}
	if swarm {
		fmt.Fprintln(w, "Images are pushed with the workflow's own GITHUB_TOKEN (packages: write); the server pulls with")
		fmt.Fprintln(w, "the deploy run's token for the length of the run — no registry secret to create or rotate.")
	} else {
		fmt.Fprintln(w, "Images go to the environment's own registry (ECR / Artifact Registry), pushed by the deploy")
		fmt.Fprintln(w, "workflow; deploy/<env>/README.md walks the first install. The target is a RELEASE CANDIDATE.")
	}
}

// ShowCmd is `w17ctl ci show`.
type ShowCmd struct {
	LockPath string `name:"lock" placeholder:"PATH" default:"w17/lock.yaml" help:"Path to the lock file."`
	Console  string `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console (owns the lock). Optional — falls back to the binary's compile-time default."`
}

func (c *ShowCmd) Run() error {
	view, err := core.DescribeLockAt("ci show", c.Console, c.LockPath)
	if err != nil {
		return err
	}
	ci := view.GetInfra().GetCi()
	if ci == nil {
		fmt.Fprintln(core.Stdout, "no CI declared — `w17ctl ci init`")
		return nil
	}
	fmt.Fprintf(core.Stdout, "provider: %s, registry: %s, runner: %s, platforms: %s\n", ci.GetProvider(), ci.GetRegistry(), ci.GetRunner(), ci.GetPlatforms())
	for _, env := range view.GetInfra().GetEnvironments() {
		fmt.Fprintf(core.Stdout, "environment %s: deploys from %s\n", env.GetName(), firstNonEmpty(env.GetDeployBranch(), "any ref"))
	}
	printSetup(view.GetInfra())
	return nil
}

// RemoveCmd is `w17ctl ci remove`.
type RemoveCmd struct {
	LockPath string `name:"lock" placeholder:"PATH" default:"w17/lock.yaml" help:"Path to the lock file."`
	Console  string `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console (owns the lock). Optional — falls back to the binary's compile-time default."`
}

func (c *RemoveCmd) Run() error {
	return editInfra("ci remove", c.LockPath, c.Console, func(in *codegenpb.LockInfra) error {
		if in.GetCi() == nil {
			return fmt.Errorf("the lock declares no CI")
		}
		in.Ci = nil
		return nil
	}, func(*codegenpb.LockInfra) {
		fmt.Fprintln(core.Stdout, "ci remove: the next `w17ctl codegen` deletes .github/workflows/w17-*.yaml")
	})
}
