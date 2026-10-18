package deploy

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wandering-compiler/w17ctl/cmd/secrets"
	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/lockfile"
	"github.com/wandering-compiler/w17ctl/internal/storageclient"

	"github.com/wandering-compiler/sdk/go/service/secret/sopsenv"

	secretspb "github.com/wandering-compiler/sdk/go/pb/consoleapi/secrets"
)

// consoleEnv is an environment's env files built from the values the console
// holds for it, one per service of the stack, ready to deliver as
// <stack>/env/<svc>.env.
type consoleEnv struct {
	files []consoleEnvFile
}

type consoleEnvFile struct {
	service string
	body    []byte
}

// reServiceName is the console's service-name rule; a name from the console
// that does not match it never reaches a path on the server.
var reServiceName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// fetchConsoleEnv asks the console for the environment's values and checks
// them against the stack the deploy is about to run (servicesDir, the
// rendered deploy/<env>/services): every key a service's .env.example names
// must be set, and nothing else — the check `w17ctl secrets check` makes of
// sealed files. Every service gets a file, so a value unset on the console
// leaves the target with it too.
//
// The fetch is a machine account's (the console refuses a person): W17_TOKEN
// names the environment's deploy bot, a token with the deploy-fetch role.
// ref, the commit being deployed, is recorded with the fetch.
func fetchConsoleEnv(lockPath, console, env, servicesDir, ref string) (*consoleEnv, error) {
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
		return nil, fmt.Errorf("the console holds this environment's values and hands them to a machine account only — "+
			"set W17_TOKEN to the token of the environment's deploy bot (role deploy-fetch): %w", err)
	}
	if err != nil {
		return nil, fmt.Errorf("fetching %s's values from the console: %w", env, err)
	}
	return buildConsoleEnv(env, expected, resp.GetServices())
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
		b, err := os.ReadFile(ex)
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

// buildConsoleEnv checks the fetched values against the declared keys and
// renders one file per declared service.
func buildConsoleEnv(env string, expected map[string][]string, services []*secretspb.ServiceSecrets) (*consoleEnv, error) {
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
	out := &consoleEnv{}
	for _, svc := range names {
		have := map[string]bool{}
		for _, v := range got[svc] {
			have[v.GetKey()] = true
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
		out.files = append(out.files, consoleEnvFile{service: svc, body: secrets.EnvFile(got[svc])})
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return nil, fmt.Errorf("the console's values for %s do not match the stack:\n  %s", env, strings.Join(problems, "\n  "))
	}
	return out, nil
}
