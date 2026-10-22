package secrets

// An environment's env values and secrets, held by the console:
// `w17ctl secrets set|unset|list`. A value is SEALED here,
// on this machine, to a write grant only the console can open, and sent in
// that form (decision secrets-env-and-bot-tokens-in-the-console, "Sealed
// writes"); nothing here stores or prints one, and reading a value back is not
// offered.

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/lockfile"
	"github.com/wandering-compiler/w17ctl/internal/storageclient"

	"github.com/wandering-compiler/sdk/go/service/secret/sopsenv"
	"golang.org/x/term"

	"github.com/wandering-compiler/sdk/go/lib/secretseal"
	consolerpcpb "github.com/wandering-compiler/sdk/go/pb/consoleapi/rpc"
	secretspb "github.com/wandering-compiler/sdk/go/pb/consoleapi/secrets"
)

// consoleFlags are what every console-held secrets command needs: the
// environment, the lock (the project it belongs to) and the console.
type consoleFlags struct {
	Env      string `name:"env" required:"" help:"Environment (environments.<env> in w17/infra.yaml)."`
	LockPath string `name:"lock" placeholder:"PATH" default:"w17/lock.yaml" help:"Path to the lock file (the project the values belong to)."`
	Console  string `name:"console" placeholder:"HOST:PORT" env:"CONSOLE_STORAGE_ADDR" help:"Console endpoint. Defaults to the logged-in console."`
}

// checkEnvironment refuses an environment w17/infra.yaml does not declare: a
// typo would otherwise write values no deploy ever reads.
func (f consoleFlags) checkEnvironment() error {
	view, err := core.DescribeInfraAt("", f.Console, f.LockPath, false, "")
	if err != nil {
		return err
	}
	var names []string
	for _, e := range view.GetInfra().GetEnvironments() {
		if e.GetName() == f.Env {
			return nil
		}
		names = append(names, e.GetName())
	}
	return fmt.Errorf("w17/infra.yaml declares no environment %q (it declares: %s)", f.Env, strings.Join(names, ", "))
}

func (f consoleFlags) project() (string, error) {
	lk, err := lockfile.Load(f.LockPath)
	if err != nil {
		return "", fmt.Errorf("no lock at %s — run this from the project root: %w", f.LockPath, err)
	}
	if lk.ProjectID == "" {
		return "", fmt.Errorf("%s names no project — register it with the console first (`w17ctl init`)", f.LockPath)
	}
	return lk.ProjectID, nil
}

// SetCmd is `w17ctl secrets set --env E SERVICE KEY…`: each value asked for
// without echo, or dotenv lines from stdin when no KEY is named. With --plain
// they are environment variables, which may also be given as KEY=value. With
// --dir, every <service>.env of a directory goes in one write: a write code
// is one command, and an environment is several services.
type SetCmd struct {
	consoleFlags
	Plain      bool     `name:"plain" help:"Write environment VARIABLES (shown back in the console: a domain, a port), not secrets. Only these may be given as KEY=value."`
	AllowEmpty bool     `name:"allow-empty" help:"Accept an empty value (refused otherwise: it is how a mistyped pipe blanks a production value)."`
	Dir        string   `name:"dir" placeholder:"DIR" help:"Write every DIR/<service>.env (dotenv lines) in one write, under one code — what secrets fetch writes, or a whole environment moved from elsewhere. Takes no SERVICE or KEY."`
	Service    string   `arg:"" optional:"" help:"The stack service the values are for (its <service>.env on the target)."`
	Keys       []string `arg:"" optional:"" help:"The env var names, each asked for without echo (or KEY=value with --plain). None: dotenv lines KEY=value from stdin."`
}

// maxSecretValue is the console's bound on a value (SetSecretReq.value).
const maxSecretValue = 64 * 1024

func (c *SetCmd) Run() error {
	project, err := c.project()
	if err != nil {
		return fmt.Errorf("secrets set: %w", err)
	}
	if err := c.checkEnvironment(); err != nil {
		return fmt.Errorf("secrets set: %w", err)
	}
	stdinTTY := term.IsTerminal(int(os.Stdin.Fd()))
	var values []envValue
	what := c.Service
	switch {
	case c.Dir != "" && (c.Service != "" || len(c.Keys) > 0):
		return fmt.Errorf("secrets set: --dir takes the services from its <service>.env files — name no SERVICE or KEY")
	case c.Dir != "":
		var services []string
		values, services, err = gatherDir(c.Dir, c.Plain, c.AllowEmpty)
		what = strings.Join(services, ", ")
	case c.Service == "":
		return fmt.Errorf("secrets set: name the SERVICE the values are for, or pass --dir")
	default:
		values, err = gatherValues(c.Service, c.Keys, c.Plain, c.AllowEmpty, os.Stdin, stdinTTY, promptHidden(os.Stdin))
	}
	if err != nil {
		return fmt.Errorf("secrets set: %w", err)
	}
	n, err := c.sealedWrite(project, values, stdinTTY)
	if err != nil {
		return fmt.Errorf("secrets set: %w", err)
	}
	kind := "secret"
	if c.Plain {
		kind = "variable"
	}
	fmt.Fprintf(core.Stdout, "set %d %s(s) of %s in %s\n", n, kind, what, c.Env)
	return nil
}

// gatherDir reads every <service>.env of dir as dotenv lines, the service
// named by the file. A name that is no service name is refused, not skipped:
// a value meant for it would silently stay unset.
func gatherDir(dir string, plain, allowEmpty bool) ([]envValue, []string, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.env"))
	if err != nil {
		return nil, nil, err
	}
	if len(files) == 0 {
		return nil, nil, fmt.Errorf("%s holds no <service>.env", dir)
	}
	sort.Strings(files)
	var out []envValue
	var services []string
	for _, f := range files {
		svc := strings.TrimSuffix(filepath.Base(f), ".env")
		if !reServiceName.MatchString(svc) {
			return nil, nil, fmt.Errorf("%s: %q is no service name", f, svc)
		}
		in, err := os.Open(f) // #nosec G304 -- the operator's own directory
		if err != nil {
			return nil, nil, err
		}
		vs, err := gatherValues(svc, nil, plain, allowEmpty, in, false, nil)
		_ = in.Close()
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", f, err)
		}
		out = append(out, vs...)
		services = append(services, svc)
	}
	return out, services, nil
}

// envValue is one value to write: its address and, with --plain, its kind.
type envValue struct {
	service, key, value string
	plain               bool
}

// gatherValues collects what one command writes. Named keys are asked for one
// by one (a terminal is needed; a secret is never echoed), KEY=value is taken
// only for a variable — an argument lands in the shell's history and, while
// the command runs, in every local user's `ps` — and with no key named the
// values are dotenv lines on stdin.
func gatherValues(service string, keys []string, plain, allowEmpty bool, stdin io.Reader, stdinTTY bool, ask func(label string) (string, error)) ([]envValue, error) {
	var out []envValue
	seen := map[string]bool{}
	add := func(key, value string) error {
		if seen[key] {
			return fmt.Errorf("%s is named twice", key)
		}
		seen[key] = true
		if value == "" && !allowEmpty {
			return fmt.Errorf("the value of %s is empty — pass --allow-empty to set it empty on purpose", key)
		}
		if len(value) > maxSecretValue {
			return fmt.Errorf("the value of %s is longer than %d bytes — an env value, not a file", key, maxSecretValue)
		}
		out = append(out, envValue{service: service, key: key, value: value, plain: plain})
		return nil
	}
	if len(keys) == 0 {
		if stdinTTY {
			return nil, fmt.Errorf("name the keys (`w17ctl secrets set --env E %s KEY …`), or pipe dotenv lines KEY=value on stdin", service)
		}
		raw, err := io.ReadAll(io.LimitReader(stdin, 4<<20))
		if err != nil {
			return nil, fmt.Errorf("reading stdin: %w", err)
		}
		lines, err := sopsenv.Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("stdin: %w", err)
		}
		for _, l := range lines {
			if l.Comment {
				continue
			}
			if err := add(l.Key, envUnquote(l.Value)); err != nil {
				return nil, err
			}
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("stdin holds no KEY=value line")
		}
		return out, nil
	}
	for _, k := range keys {
		if key, value, ok := strings.Cut(k, "="); ok {
			if !plain {
				return nil, fmt.Errorf("%s=… gives a secret as an argument, which lands in the shell's history and in `ps` — name the key alone and type the value when asked, pipe dotenv lines on stdin, or pass --plain for a variable that is not secret", key)
			}
			if err := add(key, value); err != nil {
				return nil, err
			}
			continue
		}
		if !stdinTTY {
			return nil, fmt.Errorf("%s has no value: with no terminal, give the values as dotenv lines on stdin and name no keys", k)
		}
		v, err := ask(service + "/" + k)
		if err != nil {
			return nil, err
		}
		if err := add(k, v); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// promptHidden asks for one value on the terminal, without echo.
func promptHidden(in *os.File) func(label string) (string, error) {
	return func(label string) (string, error) {
		fmt.Fprintf(os.Stderr, "%s (not echoed): ", label)
		raw, err := term.ReadPassword(int(in.Fd()))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", fmt.Errorf("reading %s: %w", label, err)
		}
		return strings.TrimSuffix(string(raw), "\r"), nil
	}
}

// writeCode is the write grant's code: W17_SECRETS_CODE, for an agent with no
// terminal, or asked for on the terminal — /dev/tty when stdin carried the
// values.
func writeCode(env string, stdinTTY bool) (string, error) {
	return grantCode(env, stdinTTY, "Write code (the console: the project's page → %s → Write from the command line → Create write code): ")
}

// grantCode asks for a grant's code with prompt (one %s: the environment).
func grantCode(env string, stdinTTY bool, prompt string) (string, error) {
	if c := strings.TrimSpace(os.Getenv("W17_SECRETS_CODE")); c != "" {
		return c, nil
	}
	in := os.Stdin
	if !stdinTTY {
		tty, err := os.Open("/dev/tty")
		if err != nil {
			return "", fmt.Errorf("no terminal to ask for the code on — set W17_SECRETS_CODE")
		}
		defer func() { _ = tty.Close() }()
		in = tty
	}
	fmt.Fprintf(os.Stderr, prompt, env)
	raw, err := term.ReadPassword(int(in.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", fmt.Errorf("reading the code: %w", err)
	}
	return strings.TrimSpace(string(raw)), nil
}

// sealedWrite trades the code for the write grant's public key, seals every
// value to it for its own address and sends them in one call, which ends the
// grant.
func (f consoleFlags) sealedWrite(project string, values []envValue, stdinTTY bool) (int, error) {
	code, err := writeCode(f.Env, stdinTTY)
	if err != nil {
		return 0, err
	}
	sc, err := storageclient.DialStorageFn(f.Console)
	if err != nil {
		return 0, err
	}
	defer sc.Close()
	ctx, cancel := core.ClientCtx()
	defer cancel()
	return sealAndSend(ctx, sc.ES, project, f.Env, code, values)
}

// sealAndSend is sealedWrite past the dial: the half a test drives.
func sealAndSend(ctx context.Context, es consolerpcpb.EnvironmentSecretsClient, project, env, code string, values []envValue) (int, error) {
	grant, err := es.OpenSecretWriteGrant(ctx, &secretspb.OpenSecretWriteGrantReq{ProjectId: project, Environment: env, Code: code})
	if err != nil {
		return 0, err
	}
	pub, err := base64.StdEncoding.DecodeString(grant.GetPublicKey())
	if err != nil {
		return 0, fmt.Errorf("the console answered an unreadable public key")
	}
	req := &secretspb.SetSealedSecretsReq{ProjectId: project, Environment: env, GrantId: grant.GetGrantId(), Code: code}
	for _, v := range values {
		sealed, err := secretseal.Seal(pub, grant.GetGrantId(), v.service, v.key, []byte(v.value))
		if err != nil {
			return 0, err
		}
		req.Values = append(req.Values, &secretspb.SealedSecret{Service: v.service, Key: v.key, Sealed: base64.StdEncoding.EncodeToString(sealed), Plain: v.plain})
	}
	out, err := es.SetSealedSecrets(ctx, req)
	if err != nil {
		return 0, err
	}
	return int(out.GetCount()), nil
}

// UnsetCmd is `w17ctl secrets unset --env E SERVICE KEY`.
type UnsetCmd struct {
	consoleFlags
	Service string `arg:"" help:"The stack service."`
	Key     string `arg:"" help:"The env var name."`
}

func (c *UnsetCmd) Run() error {
	project, err := c.project()
	if err != nil {
		return fmt.Errorf("secrets unset: %w", err)
	}
	sc, err := storageclient.DialStorageFn(c.Console)
	if err != nil {
		return err
	}
	defer sc.Close()
	ctx, cancel := core.ClientCtx()
	defer cancel()
	if _, err := sc.ES.UnsetSecret(ctx, &secretspb.UnsetSecretReq{ProjectId: project, Environment: c.Env, Service: c.Service, Key: c.Key}); err != nil {
		return fmt.Errorf("secrets unset: %w", err)
	}
	fmt.Fprintf(core.Stdout, "unset %s/%s in %s\n", c.Service, c.Key, c.Env)
	return nil
}

// ListCmd is `w17ctl secrets list --env E`: the keys set, never the values.
type ListCmd struct {
	consoleFlags
}

func (c *ListCmd) Run() error {
	project, err := c.project()
	if err != nil {
		return fmt.Errorf("secrets list: %w", err)
	}
	sc, err := storageclient.DialStorageFn(c.Console)
	if err != nil {
		return err
	}
	defer sc.Close()
	ctx, cancel := core.ClientCtx()
	defer cancel()
	resp, err := sc.ES.ListSecretKeys(ctx, &secretspb.ListSecretKeysReq{ProjectId: project, Environment: c.Env})
	if err != nil {
		return fmt.Errorf("secrets list: %w", err)
	}
	if len(resp.GetKeys()) == 0 {
		fmt.Fprintf(core.Stdout, "no values set in %s\n", c.Env)
		return nil
	}
	for _, k := range resp.GetKeys() {
		at := ""
		if t := k.GetUpdatedAt(); t != nil {
			at = t.AsTime().UTC().Format("2006-01-02 15:04 UTC")
		}
		fmt.Fprintf(core.Stdout, "%s/%s\t%s\n", k.GetService(), k.GetKey(), at)
	}
	return nil
}

// envUnquote is a dotenv line's value as the server's bash reads it: one pair
// of surrounding double quotes is syntax, not part of the value (valueProblem
// allows nothing else).
func envUnquote(v string) string {
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		return v[1 : len(v)-1]
	}
	return v
}

// EnvQuote is how a value from the console is written into a target's
// <service>.env: wrapped in "…" when the server's bash would otherwise split
// or interpret it — the rule the infra generator writes its own values by
// (swarm quoteEnv). The console refused at SetSecret every value no quoting
// can carry ($, backtick, ", a line break).
func EnvQuote(v string) string {
	if v == "" || !strings.ContainsAny(v, " &;|<>()#'\t~") {
		return v
	}
	return `"` + v + `"`
}

// EnvFile renders one service's values as its <service>.env, in key order.
func EnvFile(values []*secretspb.SecretValue) []byte {
	var b strings.Builder
	sorted := append([]*secretspb.SecretValue(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].GetKey() < sorted[j].GetKey() })
	for _, v := range sorted {
		b.WriteString(v.GetKey() + "=" + EnvQuote(v.GetValue()) + "\n")
	}
	return []byte(b.String())
}
