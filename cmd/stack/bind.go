package stack

import (
	"fmt"
	"strings"

	project "github.com/wandering-compiler/w17ctl/cmd/project"
	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/devconfig"
	"github.com/wandering-compiler/w17ctl/internal/prompter"
)

// Host-bind selection + the public-exposure guard (docs/experiments/remote-stack.md
// §9). Docker publishes this project's ports on the loopback interface by
// default (secure — reachable off-box only through the SSH tunnel in remote
// mode). `stack bind public` opts into 0.0.0.0 (all interfaces); on a remote
// host with a public IP that is an internet exposure, so the dangerous
// public+remote combination is gated behind a typed confirmation.

// publicExposurePhrase is the exact sentence the developer must type to
// acknowledge that the project's containers become network-reachable. A
// deliberate high-friction confirm (not a bare y/N) for a footgun.
const publicExposurePhrase = "I understand these containers are exposed to the network"

// newPrompter is the seam tests stub; production reads the real stdin.
var newPrompter = prompter.NewStdinPrompter

// BindCmd is `w17ctl stack bind loopback|public` — choose the host
// interface docker publishes this project's ports on. Per-machine
// (devconfig), like the port map; NOT a codegen/lock input.
type BindCmd struct {
	Mode string `arg:"" enum:"loopback,public" help:"Host bind: loopback (127.0.0.1, secure default — reachable off-box only via the SSH tunnel) | public (0.0.0.0, all interfaces)."`
}

func (c *BindCmd) Run() error {
	root, err := core.FindProjectRoot()
	if err != nil {
		return err
	}
	cfg, err := core.LoadDevConfigFn()
	if err != nil {
		return err
	}
	name, err := project.EnsureRegistered(cfg, root)
	if err != nil {
		return err
	}
	p := cfg.Projects[name]

	bind, err := devconfig.ParseBind(c.Mode)
	if err != nil {
		return err
	}
	if bind == devconfig.BindPublic {
		p.Bind = devconfig.BindPublic
		// Gate only when this project is (or defaults to) remote — public
		// on the local box is the historical, non-internet-facing default.
		remote, err := isRemoteProject(cfg, root)
		if err != nil {
			return err
		}
		if remote {
			if err := confirmPublicExposure(p, newPrompter()); err != nil {
				return err
			}
		}
	} else {
		p.Bind = devconfig.BindLoopback
		p.PublicAck = false // leaving public re-arms the guard for next time
	}
	if err := core.SaveDevConfigFn(cfg); err != nil {
		return err
	}
	fmt.Fprintf(core.Stdout, "%q now binds %s (%s)\n", name, bind, devconfig.BindHost(bind))
	return nil
}

// isRemoteProject reports whether the project at root resolves to remote
// mode (pinned or via the global default).
func isRemoteProject(cfg *devconfig.Config, root string) (bool, error) {
	mode, err := cfg.ResolveMode(root, "")
	if err != nil {
		return false, err
	}
	return mode == devconfig.ModeRemote, nil
}

// confirmPublicExposure is the guard for the public+remote combination. A
// no-op when the project isn't public-bound or the dev has already
// acknowledged. Otherwise it prints a strong warning and requires the
// developer to type publicExposurePhrase verbatim; on success it records
// PublicAck on the project (the caller persists cfg). A mismatch aborts.
func confirmPublicExposure(p *devconfig.Project, pr prompter.Prompter) error {
	if p.Bind != devconfig.BindPublic || p.PublicAck {
		return nil
	}
	fmt.Fprintln(core.Stdout, strings.TrimSpace(`
⚠️  PUBLIC EXPOSURE WARNING
This project is set to run on a REMOTE docker host with the `+"`public`"+` bind
(0.0.0.0). Its containers — including databases — will be reachable on EVERY
network interface of that server. On a box with a public IP and no firewall,
that means the open internet. Docker also bypasses ufw/iptables, so a naive
firewall does NOT protect these ports; use a cloud security group / DOCKER-USER
rule restricting inbound to SSH, or switch back with `+"`stack bind loopback`"+`.`))
	answer, err := pr.Text("To proceed, type exactly: \""+publicExposurePhrase+"\"", "")
	if err != nil {
		return err
	}
	if strings.TrimSpace(answer) != publicExposurePhrase {
		return fmt.Errorf("public exposure not confirmed (phrase did not match) — aborted; the bind stays recorded but run `stack bind loopback` to revert")
	}
	p.PublicAck = true
	fmt.Fprintln(core.Stdout, "public exposure acknowledged.")
	return nil
}
