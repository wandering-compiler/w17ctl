package infra

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/wandering-compiler/w17ctl/internal/core"
	codegenpb "github.com/wandering-compiler/sdk/go/pb/w17compiler"
)

// The value-by-value editors of w17/infra.yaml. Each builds one EditInfra
// intent — paths and YAML values — and the console applies it to the file's
// YAML tree and validates the whole; nothing here knows what a value means
// beyond spelling it.

type fileFlags struct {
	LockPath string `name:"lock" placeholder:"PATH" default:"w17/lock.yaml" help:"Path to the lock file (w17/infra.yaml sits beside it)."`
	Console  string `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console. Optional — falls back to the binary's compile-time default."`
}

func editValues(op string, f fileFlags, set []*codegenpb.InfraValue, unset []string) error {
	_, err := core.EditInfraOnDisk(op, f.Console, f.LockPath, func(*codegenpb.LockView) (*codegenpb.LockEditIntent, error) {
		return &codegenpb.LockEditIntent{Intent: &codegenpb.LockEditIntent_EditInfra{
			EditInfra: &codegenpb.EditInfraIntent{Set: set, Unset: unset},
		}}, nil
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(core.Stdout, "%s: %s updated — the next `w17ctl codegen` renders it\n", op, core.InfraPath(f.LockPath))
	return nil
}

func envPath(env, path string) string {
	if env == "" {
		return path
	}
	return "environments." + env + "." + path
}

// SetCmd is `w17ctl infra set`.
type SetCmd struct {
	Values []string `arg:"" name:"path=value" sep:"none" help:"A dotted path and a YAML value: workloads.web.port=3000, environments.prod.limits.PG_WORK_MEM=16MB, 'workloads.web.command=[serve, --port, \"3000\"]'."`
	Env    string   `name:"env" help:"Prefix every path with environments.<env>."`
	fileFlags
}

func (c *SetCmd) Run() error {
	var set []*codegenpb.InfraValue
	for _, kv := range c.Values {
		p, v, ok := strings.Cut(kv, "=")
		if !ok || p == "" {
			return fmt.Errorf("infra set: %q is not <path>=<value>", kv)
		}
		set = append(set, &codegenpb.InfraValue{Path: envPath(c.Env, p), Value: v})
	}
	return editValues("infra set", c.fileFlags, set, nil)
}

// UnsetCmd is `w17ctl infra unset`.
type UnsetCmd struct {
	Paths []string `arg:"" name:"path" help:"Dotted paths to remove (back to the default)."`
	Env   string   `name:"env" help:"Prefix every path with environments.<env>."`
	fileFlags
}

func (c *UnsetCmd) Run() error {
	var paths []string
	for _, p := range c.Paths {
		paths = append(paths, envPath(c.Env, p))
	}
	return editValues("infra unset", c.fileFlags, nil, paths)
}

// MigrateCmd is `w17ctl infra migrate`.
type MigrateCmd struct{ fileFlags }

func (c *MigrateCmd) Run() error { return editValues("infra migrate", c.fileFlags, nil, nil) }

// mapping spells a YAML flow mapping of the set fields, in order. Values are
// double-quoted YAML strings unless they are a number, a bool or already a
// list / mapping (the console decodes them against the schema).
type mapping []struct{ key, value string }

func (m *mapping) str(key, v string) {
	if v != "" {
		*m = append(*m, struct{ key, value string }{key, strconv.Quote(v)})
	}
}

func (m *mapping) raw(key, v string) {
	if v != "" {
		*m = append(*m, struct{ key, value string }{key, v})
	}
}

func (m *mapping) yesNo(key, v string) error {
	switch v {
	case "":
		return nil
	case "yes", "true":
		m.raw(key, "true")
	case "no", "false":
		m.raw(key, "false")
	default:
		return fmt.Errorf("--%s takes yes or no, not %q", strings.ReplaceAll(key, "_", "-"), v)
	}
	return nil
}

func (m mapping) String() string {
	parts := make([]string, 0, len(m))
	for _, kv := range m {
		parts = append(parts, kv.key+": "+kv.value)
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

func list(vs []string) string {
	q := make([]string, len(vs))
	for i, v := range vs {
		q[i] = strconv.Quote(v)
	}
	return "[" + strings.Join(q, ", ") + "]"
}

func kvMapping(kvs map[string]string) string {
	keys := make([]string, 0, len(kvs))
	for k := range kvs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var m mapping
	for _, k := range keys {
		m.str(k, kvs[k])
	}
	return m.String()
}

// TargetCmd is `w17ctl infra target`.
type TargetCmd struct {
	Add    TargetAddCmd    `cmd:"" help:"Add (or replace) a target: w17ctl infra target add eu-box --kind swarm --ssh root@203.0.113.10; w17ctl infra target add aws-prod --kind aws-ecs --region eu-central-1 --account 123456789012. Point an environment at it with --env, or later: w17ctl infra set environments.prod.target=eu-box."`
	Remove TargetRemoveCmd `cmd:"" help:"Remove a target no environment deploys onto."`
}

// TargetAddCmd is `w17ctl infra target add`.
type TargetAddCmd struct {
	Name          string   `arg:"" help:"The target's name (lower-case letters, digits, dashes)."`
	Kind          string   `name:"kind" required:"" help:"swarm | aws-ecs | gcp-cloudrun."`
	SSH           string   `name:"ssh" help:"swarm: user@host[:port]."`
	InstallDocker string   `name:"install-docker" help:"swarm: yes | no (default yes)."`
	Firewall      string   `name:"manage-firewall" help:"swarm: yes | no (default yes)."`
	SSHNoPassword string   `name:"ssh-no-password" help:"swarm: yes | no (default yes)."`
	Region        string   `name:"region" help:"aws-ecs / gcp-cloudrun: the region."`
	Account       string   `name:"account" help:"aws-ecs: the account id; gcp-cloudrun: the project id."`
	Env           []string `name:"env" help:"Point these environments at it (repeatable)."`
	fileFlags
}

func (c *TargetAddCmd) Run() error {
	var m mapping
	m.str("kind", c.Kind)
	m.str("ssh", c.SSH)
	for _, f := range []struct{ key, v string }{{"install_docker", c.InstallDocker}, {"manage_firewall", c.Firewall}, {"ssh_no_password", c.SSHNoPassword}} {
		if err := m.yesNo(f.key, f.v); err != nil {
			return fmt.Errorf("infra target add: %w", err)
		}
	}
	m.str("region", c.Region)
	m.str("account", c.Account)
	set := []*codegenpb.InfraValue{{Path: "targets." + c.Name, Value: m.String()}}
	for _, e := range c.Env {
		set = append(set, &codegenpb.InfraValue{Path: "environments." + e + ".target", Value: strconv.Quote(c.Name)})
	}
	return editValues("infra target add", c.fileFlags, set, nil)
}

// TargetRemoveCmd is `w17ctl infra target remove`.
type TargetRemoveCmd struct {
	Name string `arg:"" help:"The target to remove."`
	fileFlags
}

func (c *TargetRemoveCmd) Run() error {
	return editValues("infra target remove", c.fileFlags, nil, []string{"targets." + c.Name})
}

// WorkloadCmd is `w17ctl infra workload`.
type WorkloadCmd struct {
	Add    WorkloadAddCmd    `cmd:"" help:"Add (or replace) a workload. An external one needs --image or --build-context: w17ctl infra workload add web --build-context frontend --port 3000 --route-host www --health-http /healthz. A generated bundle's name with only --replicas / --cpus / --memory tunes it."`
	Remove WorkloadRemoveCmd `cmd:"" help:"Remove a workload (with --env: only that environment's settings for it)."`
}

// WorkloadAddCmd is `w17ctl infra workload add`.
type WorkloadAddCmd struct {
	Name         string            `arg:"" help:"The workload's name: a generated bundle's to tune it, any other for a container of your own."`
	Image        string            `name:"image" help:"A prebuilt image (ghcr.io/acme/web:1.4, …@sha256:…)."`
	BuildContext string            `name:"build-context" help:"Build the image from this directory (project-relative)."`
	Dockerfile   string            `name:"dockerfile" help:"The Dockerfile (default <context>/Dockerfile)."`
	BuildTarget  string            `name:"build-target" help:"A multi-stage build's stage."`
	BuildArg     map[string]string `name:"build-arg" mapsep:"none" help:"Build arguments: --build-arg VITE_API_URL='${surfaces.app-api.url}'."`
	Command      []string          `name:"command" sep:"none" help:"The container command, one argument per flag (repeatable; --command=-x for one starting with a dash)."`
	Port         int               `name:"port" help:"The port it listens on."`
	Protocol     string            `name:"protocol" help:"http (default) | grpc."`
	HealthHTTP   string            `name:"health-http" help:"An HTTP path answering 2xx when healthy."`
	HealthCmd    []string          `name:"health-cmd" sep:"none" help:"A health check command, one argument per flag (repeatable)."`
	RouteHost    string            `name:"route-host" help:"Publish it on the edge: a label (www → www.<domain>) or a host name."`
	PathPrefix   string            `name:"path-prefix" help:"Only this path of the route's host."`
	EnvVar       map[string]string `name:"env-var" mapsep:"none" help:"Environment variables in clear: --env-var LOG_LEVEL=info."`
	Secret       []string          `name:"secret" sep:"none" help:"Keys its sealed .env.enc must carry (repeatable)."`
	Database     bool              `name:"database" help:"Its own database and role in the environment's Postgres (DATABASE_URL)."`
	Migrate      []string          `name:"migrate" sep:"none" help:"The command that migrates its database before the switch, one argument per flag (repeatable; --migrate=-c for one starting with a dash)."`
	Replicas     int               `name:"replicas" help:"How many tasks run."`
	CPUs         string            `name:"cpus" help:"CPU limit per task: 1, 0.5."`
	Memory       string            `name:"memory" help:"Memory limit per task: 512M, 2G."`
	Env          string            `name:"env" help:"Only in this environment (environments.<env>.workloads), merged over the top-level entry."`
	fileFlags
}

func (c *WorkloadAddCmd) Run() error {
	var m mapping
	if c.BuildContext != "" || c.Dockerfile != "" || c.BuildTarget != "" || len(c.BuildArg) > 0 {
		var b mapping
		b.str("context", c.BuildContext)
		b.str("dockerfile", c.Dockerfile)
		b.str("target", c.BuildTarget)
		if len(c.BuildArg) > 0 {
			b.raw("args", kvMapping(c.BuildArg))
		}
		m.raw("build", b.String())
	}
	m.str("image", c.Image)
	if len(c.Command) > 0 {
		m.raw("command", list(c.Command))
	}
	if c.Port != 0 {
		m.raw("port", strconv.Itoa(c.Port))
	}
	m.str("protocol", c.Protocol)
	if c.HealthHTTP != "" || len(c.HealthCmd) > 0 {
		var h mapping
		h.str("http", c.HealthHTTP)
		if len(c.HealthCmd) > 0 {
			h.raw("command", list(c.HealthCmd))
		}
		m.raw("health", h.String())
	}
	if c.RouteHost != "" || c.PathPrefix != "" {
		var r mapping
		r.str("host", c.RouteHost)
		r.str("path_prefix", c.PathPrefix)
		m.raw("route", r.String())
	}
	if len(c.EnvVar) > 0 {
		m.raw("env", kvMapping(c.EnvVar))
	}
	if len(c.Secret) > 0 {
		m.raw("secrets", list(c.Secret))
	}
	if c.Database {
		m.raw("database", "true")
	}
	if len(c.Migrate) > 0 {
		m.raw("migrate", list(c.Migrate))
	}
	if c.Replicas != 0 {
		m.raw("replicas", strconv.Itoa(c.Replicas))
	}
	if c.CPUs != "" || c.Memory != "" {
		var r mapping
		r.str("cpus", c.CPUs)
		r.str("memory", c.Memory)
		m.raw("resources", r.String())
	}
	return editValues("infra workload add", c.fileFlags,
		[]*codegenpb.InfraValue{{Path: envPath(c.Env, "workloads."+c.Name), Value: m.String()}}, nil)
}

// WorkloadRemoveCmd is `w17ctl infra workload remove`.
type WorkloadRemoveCmd struct {
	Name string `arg:"" help:"The workload to remove."`
	Env  string `name:"env" help:"Only this environment's settings for it."`
	fileFlags
}

func (c *WorkloadRemoveCmd) Run() error {
	return editValues("infra workload remove", c.fileFlags, nil, []string{envPath(c.Env, "workloads."+c.Name)})
}
