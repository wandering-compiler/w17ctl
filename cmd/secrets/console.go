package secrets

// An environment's env values and secrets held by the console instead of in
// sops files: `w17ctl secrets set|unset|list|import`. The values travel over the
// console's TLS connection; nothing here encrypts, stores or prints one —
// reading a value back is not offered.

import (
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

// SetCmd is `w17ctl secrets set --env E SERVICE KEY`, the value on stdin.
type SetCmd struct {
	consoleFlags
	Service    string `arg:"" help:"The stack service the value is for (its <service>.env on the target)."`
	Key        string `arg:"" help:"The env var name."`
	AllowEmpty bool   `name:"allow-empty" help:"Set the value to the empty string (empty stdin is refused otherwise: it is how a mistyped pipe blanks a production value)."`
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
	value, err := readSecretValue(os.Stdin)
	if err != nil {
		return fmt.Errorf("secrets set: %w", err)
	}
	if value == "" && !c.AllowEmpty {
		return fmt.Errorf("secrets set: the value on stdin is empty — pipe it in (`printf %%s \"$VALUE\" | w17ctl secrets set …`), or pass --allow-empty to set it empty on purpose")
	}
	sc, err := storageclient.DialStorageFn(c.Console)
	if err != nil {
		return err
	}
	defer sc.Close()
	ctx, cancel := core.ClientCtx()
	defer cancel()
	if _, err := sc.ES.SetSecret(ctx, &secretspb.SetSecretReq{ProjectId: project, Environment: c.Env, Service: c.Service, Key: c.Key, Value: value}); err != nil {
		return fmt.Errorf("secrets set: %w", err)
	}
	fmt.Fprintf(core.Stdout, "set %s/%s in %s\n", c.Service, c.Key, c.Env)
	return nil
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

// ImportCmd is `w17ctl secrets import --env E`: every value of the
// environment's sealed deploy/<env>/services/*/.env.enc, written to the
// console. The files stay: the deploy takes its values from the console once
// they are gone (`git rm` them in the same commit that switches over).
type ImportCmd struct {
	consoleFlags
	keyFlag
}

func (c *ImportCmd) Run() error {
	project, err := c.project()
	if err != nil {
		return fmt.Errorf("secrets import: %w", err)
	}
	root, err := projectRoot(c.LockPath)
	if err != nil {
		return fmt.Errorf("secrets import: %w", err)
	}
	if err := c.checkEnvironment(); err != nil {
		return fmt.Errorf("secrets import: %w", err)
	}
	ids, err := requireIdentities("import", c.Key)
	if err != nil {
		return err
	}
	encs, _ := filepath.Glob(filepath.Join(root, "deploy", c.Env, "services", "*", ".env.enc"))
	sort.Strings(encs)
	if len(encs) == 0 {
		return fmt.Errorf("secrets import: deploy/%s/services holds no .env.enc to import", c.Env)
	}
	type value struct{ service, key, value string }
	var values []value
	for _, enc := range encs {
		svc := filepath.Base(filepath.Dir(enc))
		b, err := os.ReadFile(enc)
		if err != nil {
			return err
		}
		lines, err := sopsenv.Decrypt(b, ids)
		if err != nil {
			return fmt.Errorf("secrets import: deploy/%s/services/%s/.env.enc: %w", c.Env, svc, err)
		}
		for _, l := range lines {
			if !l.Comment {
				values = append(values, value{svc, l.Key, envUnquote(l.Value)})
			}
		}
	}
	sc, err := storageclient.DialStorageFn(c.Console)
	if err != nil {
		return err
	}
	defer sc.Close()
	for _, v := range values {
		ctx, cancel := core.ClientCtx()
		_, err := sc.ES.SetSecret(ctx, &secretspb.SetSecretReq{ProjectId: project, Environment: c.Env, Service: v.service, Key: v.key, Value: v.value})
		cancel()
		if err != nil {
			return fmt.Errorf("secrets import: %s/%s: %w", v.service, v.key, err)
		}
	}
	fmt.Fprintf(core.Stdout, "imported %d value(s) of %d service(s) into %s.\n"+
		"The deploy takes them from the console once deploy/%s/services/*/.env.enc are gone — `git rm` them when you switch.\n",
		len(values), len(encs), c.Env, c.Env)
	return nil
}

// readSecretValue reads the value from stdin, never an argument: an argument
// lands in the shell's history and in every process listing. From a terminal
// it is read without echo. One trailing line break is the pipe's, not the
// value's; a value over the console's bound is refused, not cut.
func readSecretValue(in *os.File) (string, error) {
	var raw []byte
	var err error
	if term.IsTerminal(int(in.Fd())) {
		fmt.Fprint(os.Stderr, "value (not echoed): ")
		raw, err = term.ReadPassword(int(in.Fd()))
		fmt.Fprintln(os.Stderr)
	} else {
		raw, err = io.ReadAll(io.LimitReader(in, maxSecretValue+3))
	}
	if err != nil {
		return "", fmt.Errorf("reading the value: %w", err)
	}
	value := strings.TrimSuffix(strings.TrimSuffix(string(raw), "\n"), "\r")
	if len(value) > maxSecretValue {
		return "", fmt.Errorf("the value is longer than %d bytes — an env value, not a file", maxSecretValue)
	}
	return value, nil
}

// envUnquote is a sealed .env's value as the server's bash reads it: one pair
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
