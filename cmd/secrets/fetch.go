package secrets

// A deploy's values: what the console holds for an environment, fetched by
// the environment's deploy bot and checked against the stack the deploy runs.
// The console is the only source of an environment's values (decision
// secrets-env-and-bot-tokens-in-the-console, "the console is the only
// source"); `w17ctl deploy` and the generated deploy workflows both take them
// from here.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/lockfile"
	"github.com/wandering-compiler/w17ctl/internal/storageclient"

	"github.com/wandering-compiler/sdk/go/service/secret/sopsenv"

	secretspb "github.com/wandering-compiler/sdk/go/pb/consoleapi/secrets"
)

// DeployEnv is an environment's values, one file per service of the stack.
type DeployEnv struct {
	Files []DeployEnvFile
}

// DeployEnvFile is one service's values: Body is its <service>.env as the
// server's bash reads it, Values the same values unquoted.
type DeployEnvFile struct {
	Service string
	Body    []byte
	Values  map[string]string
}

// reServiceName is the console's service-name rule; a name from the console
// that does not match it never reaches a path.
var reServiceName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// FetchDeployEnv asks the console for the environment's values and checks
// them against the stack about to run (servicesDir, the rendered
// deploy/<env>/services): every key a service's .env.example names must be
// set, and nothing else; and every value passes valuesProblems. Every service
// gets a file, so a value unset on the console leaves the target with it too.
//
// The fetch is a machine account's (the console refuses a person): W17_TOKEN
// names the environment's deploy bot, given Deploy access to the environment
// on the project's page. ref, the commit being deployed, is recorded with the
// fetch.
func FetchDeployEnv(lockPath, console, env, servicesDir, ref string) (*DeployEnv, error) {
	lk, err := lockfile.Load(lockPath)
	if err != nil {
		return nil, err
	}
	if lk.ProjectID == "" {
		return nil, fmt.Errorf("%s names no project", lockPath)
	}
	expected, err := expectedKeys(servicesDir)
	if err != nil {
		return nil, err
	}
	sc, err := storageclient.DialStorageFn(console)
	if err != nil {
		return nil, err
	}
	defer sc.Close()
	ctx, cancel := core.ClientCtx()
	defer cancel()
	resp, err := sc.ES.FetchEnvironmentSecrets(ctx, &secretspb.FetchEnvironmentSecretsReq{ProjectId: lk.ProjectID, Environment: env, DeployRef: ref})
	if status.Code(err) == codes.PermissionDenied || status.Code(err) == codes.Unauthenticated {
		return nil, fmt.Errorf("the console hands an environment's values to its deploy bot only — set W17_TOKEN (W17_FETCH_TOKEN "+
			"under w17ctl deploy) to the token of a machine account given Deploy access to %s on the project's page: %w", env, err)
	}
	if err != nil {
		return nil, fmt.Errorf("fetching %s's values from the console: %w", env, err)
	}
	return buildDeployEnv(env, expected, resp.GetServices())
}

// expectedKeys reads the keys each service of the stack declares in its
// rendered .env.example.
func expectedKeys(servicesDir string) (map[string][]string, error) {
	examples, _ := filepath.Glob(filepath.Join(servicesDir, "*", ".env.example"))
	if len(examples) == 0 {
		return nil, fmt.Errorf("%s holds no service .env.example to check the console's values against — `w17ctl codegen` renders them", servicesDir)
	}
	out := map[string][]string{}
	for _, ex := range examples {
		b, err := os.ReadFile(ex) // #nosec G304 -- the project's own rendered file
		if err != nil {
			return nil, err
		}
		lines, err := sopsenv.Parse(b)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", ex, err)
		}
		svc := filepath.Base(filepath.Dir(ex))
		out[svc] = []string{}
		for _, l := range lines {
			if !l.Comment {
				out[svc] = append(out[svc], l.Key)
			}
		}
	}
	return out, nil
}

// buildDeployEnv checks the fetched values against the declared keys and the
// value checks, and renders one file per declared service.
func buildDeployEnv(env string, expected map[string][]string, services []*secretspb.ServiceSecrets) (*DeployEnv, error) {
	got := map[string][]*secretspb.SecretValue{}
	var problems []string
	for _, s := range services {
		if !reServiceName.MatchString(s.GetService()) {
			return nil, fmt.Errorf("the console answered a service named %q, which is no service name", s.GetService())
		}
		if _, ok := expected[s.GetService()]; !ok {
			problems = append(problems, fmt.Sprintf("%s: the console holds values for it, and the stack has no such service (`w17ctl secrets unset --env %s %s KEY`)", s.GetService(), env, s.GetService()))
			continue
		}
		got[s.GetService()] = s.GetValues()
	}
	names := make([]string, 0, len(expected))
	for svc := range expected {
		names = append(names, svc)
	}
	sort.Strings(names)
	out := &DeployEnv{}
	values := map[string]map[string]string{}
	for _, svc := range names {
		have := map[string]bool{}
		values[svc] = map[string]string{}
		for _, v := range got[svc] {
			have[v.GetKey()] = true
			values[svc][v.GetKey()] = v.GetValue()
		}
		declared := map[string]bool{}
		for _, k := range expected[svc] {
			declared[k] = true
			if !have[k] {
				problems = append(problems, fmt.Sprintf("%s: %s is not set (`w17ctl secrets set --env %s %s %s`)", svc, k, env, svc, k))
			}
		}
		for _, v := range got[svc] {
			if !declared[v.GetKey()] {
				problems = append(problems, fmt.Sprintf("%s: %s is set on the console and %s's .env.example does not declare it", svc, v.GetKey(), svc))
			}
		}
		out.Files = append(out.Files, DeployEnvFile{Service: svc, Body: EnvFile(got[svc]), Values: values[svc]})
	}
	problems = append(problems, valuesProblems(values)...)
	if len(problems) > 0 {
		sort.Strings(problems)
		return nil, fmt.Errorf("the console's values for %s are not ready for a deploy:\n  %s", env, strings.Join(problems, "\n  "))
	}
	return out, nil
}

// WriteDir writes every service's values into dir: as <service>.env, or as
// <service>.json (one flat object) with asJSON — what the cloud deploy
// scripts read. dir is created 0700 when it does not exist, and must be empty
// when it does: a file an earlier run left there would be delivered with
// these. Each file is created new, mode 0600; a failure removes what this
// call wrote, so the directory never holds part of an environment.
func (e *DeployEnv) WriteDir(dir string, asJSON bool) (err error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if entries, err := os.ReadDir(dir); err != nil {
		return err
	} else if len(entries) > 0 {
		return fmt.Errorf("%s is not empty — write the values into a directory of their own", dir)
	}
	var written []string
	defer func() {
		if err != nil {
			for _, p := range written {
				_ = os.Remove(p)
			}
		}
	}()
	for _, f := range e.Files {
		name, body := f.Service+".env", f.Body
		if asJSON {
			js, err := json.Marshal(f.Values)
			if err != nil {
				return err
			}
			name, body = f.Service+".json", js
		}
		p := filepath.Join(dir, name)
		out, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- a service name of the rendered stack
		if err != nil {
			return err
		}
		written = append(written, p)
		_, werr := out.Write(body)
		if cerr := out.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			return werr
		}
	}
	return nil
}

// FetchCmd is `w17ctl secrets fetch --env E --out DIR`: the deploy bot's
// fetch, for a deploy workflow. Nothing is written unless every value passes.
type FetchCmd struct {
	Env      string `name:"env" required:"" help:"Environment (environments.<env> in w17/infra.yaml)."`
	Out      string `name:"out" short:"o" required:"" placeholder:"DIR" help:"Directory to write <service>.env (mode 0600) into."`
	JSON     bool   `name:"json" help:"Write <service>.json (one flat object of the values) instead — the cloud deploy scripts' form."`
	Ref      string `name:"ref" env:"GITHUB_SHA" help:"The commit being deployed, recorded with the fetch."`
	LockPath string `name:"lock" placeholder:"PATH" default:"w17/lock.yaml" help:"Path to the lock file (the project the values belong to)."`
	Console  string `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console. Optional — falls back to the binary's compile-time default."`
}

func (c *FetchCmd) Run() error {
	abs, err := filepath.Abs(c.LockPath)
	if err != nil {
		return err
	}
	servicesDir := filepath.Join(filepath.Dir(filepath.Dir(abs)), "deploy", c.Env, "services")
	env, err := FetchDeployEnv(c.LockPath, c.Console, c.Env, servicesDir, c.Ref)
	if err != nil {
		return fmt.Errorf("secrets fetch: %w", err)
	}
	if err := env.WriteDir(c.Out, c.JSON); err != nil {
		return fmt.Errorf("secrets fetch: %w", err)
	}
	fmt.Fprintf(core.Stdout, "secrets fetch: %d service(s) of %s → %s\n", len(env.Files), c.Env, c.Out)
	return nil
}
