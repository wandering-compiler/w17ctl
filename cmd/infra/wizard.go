package infra

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/wandering-compiler/w17ctl/internal/prompter"
	codegenpb "github.com/wandering-compiler/sdk/go/pb/w17compiler"
)

// The values the wizard offers. The console's lock validator owns the truth
// (srcgo/domains/console/lock/infra.go) and refuses anything else; these
// copies only drive the prompt, so a value missing here is one the prompt does
// not offer, never one the lock accepts silently.
var (
	targets = []string{"swarm", "aws-ecs", "gcp-cloudrun"}
	edges   = []string{"caddy", "nginx"}
	tlsAll  = []string{"acme", "custom"}
	alerts  = []string{"sentry", "none"}
	yesNo   = []string{"yes", "no"}
	onOff   = []string{"on", "off"}
)

// Defaults of a new environment (docs/decisions/infra-targets.md §4.2).
const (
	defaultEnv            = "prod"
	defaultKeepDays       = 30
	defaultBaseSchedule   = "0 3 * * *"
	defaultVerifySchedule = "0 4 * * *"
	defaultArchiveTimeout = 60
	defaultAlert          = "sentry"
)

// Answers pre-answers the wizard from flags: every non-empty field skips its
// question. Per-environment values apply to every environment the run covers,
// except Domain, which is per environment (`prod=example.com`).
type Answers struct {
	Target       string
	Envs         []string
	Domain       map[string]string
	Hosts        map[string]string // "<env>:<surface>" -> fqdn
	Edge         string
	TLS          string
	AcmeEmail    string
	Backups      string // "on" | "off"
	KeepDays     int
	BaseSchedule string
	VerifySched  string
	ArchiveTO    int
	Alert        string
	InstallDock  string // "yes" | "no"
	Firewall     string
	SSHNoPass    string
	SSH          map[string]string // env -> user@host[:port]; "none" clears
	Region       string
	Account      string
}

// Wizard asks for the infrastructure, starting from `current` (nil for `infra
// init`; the lock's block for `infra update`, so every question defaults to
// what is declared now). It returns the complete block to send as one
// SetInfra edit; the console validates it.
func Wizard(p prompter.Prompter, current *codegenpb.LockInfra, a Answers) (*codegenpb.LockInfra, error) {
	// The CI block and each environment's deploy branch belong to `w17ctl ci`;
	// this wizard carries them through unchanged (the edit replaces the block).
	out := &codegenpb.LockInfra{Ci: current.GetCi()}

	target, err := pick(p, a.Target, "Infrastructure target — swarm = your own servers (Docker Swarm), aws-ecs = ECS Fargate + RDS, gcp-cloudrun = Cloud Run + Cloud SQL",
		targets, firstNonEmpty(current.GetTarget(), "swarm"))
	if err != nil {
		return nil, err
	}
	out.Target = target

	prevEnvs := map[string]*codegenpb.LockInfraEnvironment{}
	var prevNames []string
	for _, e := range current.GetEnvironments() {
		prevEnvs[e.GetName()] = e
		prevNames = append(prevNames, e.GetName())
	}
	envs := a.Envs
	if len(envs) == 0 {
		ans, err := p.Text("Environments (comma-separated; each becomes deploy/<name>/)", firstNonEmpty(strings.Join(prevNames, ","), defaultEnv))
		if err != nil {
			return nil, err
		}
		envs = splitList(ans)
	}
	if len(envs) == 0 {
		return nil, fmt.Errorf("infra: at least one environment is needed")
	}

	for _, name := range envs {
		env, err := askEnvironment(p, target, name, prevEnvs[name], a)
		if err != nil {
			return nil, err
		}
		out.Environments = append(out.Environments, env)
	}
	return out, nil
}

func askEnvironment(p prompter.Prompter, target, name string, prev *codegenpb.LockInfraEnvironment, a Answers) (*codegenpb.LockInfraEnvironment, error) {
	env := &codegenpb.LockInfraEnvironment{Name: name, DeployBranch: prev.GetDeployBranch()}
	q := func(s string) string { return fmt.Sprintf("[%s] %s", name, s) }

	domain := a.Domain[name]
	if domain == "" {
		var err error
		domain, err = p.Text(q("Domain — each published surface gets a host under it (api., rpc., admin., one per web client)"), prev.GetDomain())
		if err != nil {
			return nil, err
		}
	}
	env.Domain = strings.TrimSpace(domain)
	for k, v := range prev.GetHosts() {
		if env.Hosts == nil {
			env.Hosts = map[string]string{}
		}
		env.Hosts[k] = v
	}
	for key, fqdn := range a.Hosts {
		envName, surface, ok := strings.Cut(key, ":")
		if !ok || envName != name {
			continue
		}
		if env.Hosts == nil {
			env.Hosts = map[string]string{}
		}
		env.Hosts[surface] = fqdn
	}

	var err error
	if target == "swarm" {
		if env.Edge, err = pick(p, a.Edge, q("Edge proxy"), edges, firstNonEmpty(prev.GetEdge(), "caddy")); err != nil {
			return nil, err
		}
		// Custom certificates are a Caddy feature here; with nginx the only
		// offer is ACME, so the prompt cannot lead into a refused lock.
		tlsOptions := tlsAll
		if env.GetEdge() == "nginx" {
			tlsOptions = []string{"acme"}
		}
		if env.Tls, err = pick(p, a.TLS, q("TLS — acme = Let's Encrypt (staging CA outside prod), custom = your certificate files (caddy only)"),
			tlsOptions, firstNonEmpty(prev.GetTls(), "acme")); err != nil {
			return nil, err
		}
		if env.GetTls() == "acme" {
			env.AcmeEmail = a.AcmeEmail
			if env.GetAcmeEmail() == "" {
				if env.AcmeEmail, err = p.Text(q("E-mail for the Let's Encrypt account (expiry notices)"), prev.GetAcmeEmail()); err != nil {
					return nil, err
				}
			}
		}
	} else {
		cloud := &codegenpb.LockInfraCloud{Region: a.Region, Account: a.Account}
		if cloud.GetRegion() == "" {
			if cloud.Region, err = p.Text(q("Region"), prev.GetCloud().GetRegion()); err != nil {
				return nil, err
			}
		}
		if cloud.GetAccount() == "" {
			label := "AWS account id (optional)"
			if target == "gcp-cloudrun" {
				label = "GCP project id"
			}
			if cloud.Account, err = p.Text(q(label), prev.GetCloud().GetAccount()); err != nil {
				return nil, err
			}
		}
		env.Cloud = cloud
	}

	backups, err := askBackups(p, q, target, prev.GetBackups(), a)
	if err != nil {
		return nil, err
	}
	env.Backups = backups

	if target == "swarm" {
		host, err := askHost(p, q, name, prev.GetHost(), a)
		if err != nil {
			return nil, err
		}
		env.Host = host
	}
	return env, nil
}

func askBackups(p prompter.Prompter, q func(string) string, target string, prev *codegenpb.LockInfraBackups, a Answers) (*codegenpb.LockInfraBackups, error) {
	prevOn := "on"
	if prev != nil && !prev.GetEnabled() {
		prevOn = "off"
	}
	label := "Database backups — WAL streamed to S3, a daily base backup, a nightly restore test"
	if target != "swarm" {
		label = "Database backups — the managed database's automated backups + point-in-time recovery"
	}
	on, err := pick(p, a.Backups, q(label), onOff, prevOn)
	if err != nil {
		return nil, err
	}
	if on == "off" {
		return &codegenpb.LockInfraBackups{Enabled: false}, nil
	}
	b := &codegenpb.LockInfraBackups{Enabled: true}
	if b.KeepDays, err = askInt(p, a.KeepDays, q("Days of history to keep (the point-in-time-recovery window)"), int(prev.GetKeepDays()), defaultKeepDays); err != nil {
		return nil, err
	}
	// The managed databases schedule themselves; the fields still carry the
	// defaults so the block reads the same everywhere.
	b.BaseSchedule, b.VerifySchedule, b.ArchiveTimeoutS, b.Alert = defaultBaseSchedule, defaultVerifySchedule, defaultArchiveTimeout, defaultAlert
	if target != "swarm" {
		return b, nil
	}
	if b.BaseSchedule, err = text(p, a.BaseSchedule, q("Daily base backup (cron)"), firstNonEmpty(prev.GetBaseSchedule(), defaultBaseSchedule)); err != nil {
		return nil, err
	}
	if b.VerifySchedule, err = text(p, a.VerifySched, q("Nightly restore test (cron)"), firstNonEmpty(prev.GetVerifySchedule(), defaultVerifySchedule)); err != nil {
		return nil, err
	}
	if b.ArchiveTimeoutS, err = askInt(p, a.ArchiveTO, q("Most seconds of data a dead server may lose (WAL switch interval)"), int(prev.GetArchiveTimeoutS()), defaultArchiveTimeout); err != nil {
		return nil, err
	}
	if b.Alert, err = pick(p, a.Alert, q("Report a failed backup or restore test to"), alerts, firstNonEmpty(prev.GetAlert(), defaultAlert)); err != nil {
		return nil, err
	}
	return b, nil
}

func askHost(p prompter.Prompter, q func(string) string, env string, prev *codegenpb.LockInfraHost, a Answers) (*codegenpb.LockInfraHost, error) {
	def := func(v bool) string {
		if prev == nil || v {
			return "yes"
		}
		return "no"
	}
	h := &codegenpb.LockInfraHost{}
	var ans string
	var err error
	if ans, err = pick(p, a.InstallDock, q("Server setup: install Docker CE and initialise Swarm (skipped when already present)"), yesNo, def(prev.GetInstallDocker())); err != nil {
		return nil, err
	}
	h.InstallDocker = ans == "yes"
	if ans, err = pick(p, a.Firewall, q("Server setup: manage the firewall (no = your own rules stay; the ports to open are printed)"), yesNo, def(prev.GetManageFirewall())); err != nil {
		return nil, err
	}
	h.ManageFirewall = ans == "yes"
	if ans, err = pick(p, a.SSHNoPass, q("Server setup: disable SSH password login (only after key login is confirmed to work)"), yesNo, def(prev.GetSshNoPassword())); err != nil {
		return nil, err
	}
	h.SshNoPassword = ans == "yes"
	// Who reaches the server: `w17ctl infra setup` and the rendered commands
	// use it with the operator's own key. Optional — "none" leaves it unset.
	ssh, err := text(p, a.SSH[env], q("Server SSH (user@host or user@host:port; none = set it later)"), firstNonEmpty(prev.GetSsh(), "none"))
	if err != nil {
		return nil, err
	}
	if ssh != "none" {
		h.Ssh = ssh
	}
	return h, nil
}

// pick returns the flag value when set (it must be one of the options),
// otherwise asks.
func pick(p prompter.Prompter, flag, question string, options []string, def string) (string, error) {
	if flag != "" {
		for _, o := range options {
			if o == flag {
				return flag, nil
			}
		}
		return "", fmt.Errorf("%q is not one of %v (%s)", flag, options, question)
	}
	return p.Select(question, options, def)
}

func text(p prompter.Prompter, flag, question, def string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	return p.Text(question, def)
}

func askInt(p prompter.Prompter, flag int, question string, prev, def int) (int32, error) {
	if flag != 0 {
		return int32(flag), nil
	}
	if prev == 0 {
		prev = def
	}
	ans, err := p.Text(question, strconv.Itoa(prev))
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseInt(strings.TrimSpace(ans), 10, 32)
	if err != nil {
		return 0, fmt.Errorf("%s: %q is not a number", question, ans)
	}
	return int32(n), nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func splitList(s string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' }) {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}
