package secrets

// Reading an environment's values back from the console (decision
// secrets-env-and-bot-tokens-in-the-console, S2): `w17ctl secrets pull`, with
// `secrets open` and `secrets exec` for a pull kept encrypted on disk.

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"

	"filippo.io/age"
	"golang.org/x/term"

	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/storageclient"

	"github.com/wandering-compiler/sdk/go/lib/secretseal"
	"github.com/wandering-compiler/sdk/go/service/secret/sopsenv"

	consolerpcpb "github.com/wandering-compiler/sdk/go/pb/consoleapi/rpc"
	secretspb "github.com/wandering-compiler/sdk/go/pb/consoleapi/secrets"
)

// PullCmd is `w17ctl secrets pull --env E SERVICE`: one service's values as
// dotenv, after a read code from the project's page (the password step-up).
// This CLI makes a key pair for the one read; the console answers every value
// sealed to it, so the values exist readable only in this process's memory —
// and, without --seal-to, in what it writes.
type PullCmd struct {
	consoleFlags
	Service string `arg:"" help:"The stack service whose values to read (its <service>.env on the target)."`
	Out     string `name:"out" short:"o" placeholder:"FILE" help:"Write to FILE (mode 0600) instead of stdout."`
	SealTo  string `name:"seal-to" placeholder:"RECIPIENT" help:"Encrypt what is written to this age recipient (age1…, or a file of recipients): the values never touch the disk readable. Read it back with secrets open / secrets exec."`
}

func (c *PullCmd) Run() error {
	project, err := c.project()
	if err != nil {
		return fmt.Errorf("secrets pull: %w", err)
	}
	if err := c.checkEnvironment(); err != nil {
		return fmt.Errorf("secrets pull: %w", err)
	}
	var recipients []age.Recipient
	if c.SealTo != "" {
		if recipients, err = parseRecipients(c.SealTo); err != nil {
			return fmt.Errorf("secrets pull: --seal-to: %w", err)
		}
	}
	code, err := grantCode(c.Env, term.IsTerminal(int(os.Stdin.Fd())),
		"Read code (the console: the project's page → %s → Read from the command line → Create read code): ")
	if err != nil {
		return fmt.Errorf("secrets pull: %w", err)
	}
	sc, err := storageclient.DialStorageFn(c.Console)
	if err != nil {
		return err
	}
	defer sc.Close()
	ctx, cancel := core.ClientCtx()
	defer cancel()
	body, n, err := readAndOpen(ctx, sc.ES, project, c.Env, c.Service, code)
	if err != nil {
		return fmt.Errorf("secrets pull: %w", err)
	}
	if recipients != nil {
		var sealed bytes.Buffer
		w, err := age.Encrypt(&sealed, recipients...)
		if err != nil {
			return fmt.Errorf("secrets pull: %w", err)
		}
		if _, err := w.Write(body); err != nil {
			return err
		}
		if err := w.Close(); err != nil {
			return err
		}
		body = sealed.Bytes()
	} else if c.Out == "" && term.IsTerminal(int(os.Stdout.Fd())) {
		fmt.Fprintln(os.Stderr, "(the values follow, readable — -o FILE or --seal-to keep them off the screen)")
	}
	if c.Out != "" {
		if err := os.WriteFile(c.Out, body, 0o600); err != nil {
			return fmt.Errorf("secrets pull: %w", err)
		}
		fmt.Fprintf(os.Stderr, "read %d value(s) of %s in %s into %s\n", n, c.Service, c.Env, c.Out)
		return nil
	}
	_, err = core.Stdout.Write(body)
	return err
}

// readAndOpen makes the key pair, reads, and opens every value at its own
// address; the dotenv of the service, keys in order.
func readAndOpen(ctx context.Context, es consolerpcpb.EnvironmentSecretsClient, project, env, service, code string) ([]byte, int, error) {
	priv, pub, err := secretseal.NewKeyPair()
	if err != nil {
		return nil, 0, err
	}
	resp, err := es.ReadSealedSecrets(ctx, &secretspb.ReadSealedSecretsReq{ProjectId: project, Environment: env, Code: code,
		PublicKey: base64.StdEncoding.EncodeToString(pub), Service: service})
	if err != nil {
		return nil, 0, err
	}
	var values []*secretspb.SecretValue
	for _, v := range resp.GetValues() {
		if v.GetService() != service {
			return nil, 0, fmt.Errorf("the console answered %s/%s for service %s", v.GetService(), v.GetKey(), service)
		}
		raw, err := base64.StdEncoding.DecodeString(v.GetSealed())
		if err != nil {
			return nil, 0, fmt.Errorf("%s: unreadable", v.GetKey())
		}
		plain, err := secretseal.Open(priv, resp.GetGrantId(), v.GetService(), v.GetKey(), raw)
		if err != nil {
			return nil, 0, fmt.Errorf("%s: %w", v.GetKey(), err)
		}
		values = append(values, &secretspb.SecretValue{Key: v.GetKey(), Value: string(plain)})
	}
	sort.Slice(values, func(i, j int) bool { return values[i].GetKey() < values[j].GetKey() })
	return EnvFile(values), len(values), nil
}

// parseRecipients is an age1… recipient, or a file of them (one per line).
func parseRecipients(s string) ([]age.Recipient, error) {
	if strings.HasPrefix(s, "age1") {
		r, err := age.ParseX25519Recipient(s)
		if err != nil {
			return nil, err
		}
		return []age.Recipient{r}, nil
	}
	f, err := os.Open(s)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }() // read only
	return age.ParseRecipients(f)
}

// OpenCmd is `w17ctl secrets open FILE`: a --seal-to pull, decrypted to stdout.
type OpenCmd struct {
	keyFlag
	File string `arg:"" help:"The file secrets pull --seal-to wrote."`
}

func (c *OpenCmd) Run() error {
	body, err := openSealed("open", c.File, c.Key)
	if err != nil {
		return err
	}
	_, err = core.Stdout.Write(body)
	return err
}

// ExecCmd is `w17ctl secrets exec FILE -- CMD…`: CMD runs with the file's
// values in its environment, and nothing readable is written anywhere.
type ExecCmd struct {
	keyFlag
	File string   `arg:"" help:"The file secrets pull --seal-to wrote."`
	Cmd  []string `arg:"" passthrough:"" help:"The command and its arguments (after --)."`
}

func (c *ExecCmd) Run() error {
	// The passthrough keeps the `--` that ends w17ctl's own flags.
	if len(c.Cmd) > 0 && c.Cmd[0] == "--" {
		c.Cmd = c.Cmd[1:]
	}
	if len(c.Cmd) == 0 {
		return errors.New("secrets exec: name a command after --")
	}
	body, err := openSealed("exec", c.File, c.Key)
	if err != nil {
		return err
	}
	lines, err := sopsenv.Parse(body)
	if err != nil {
		return fmt.Errorf("secrets exec: %s: %w", c.File, err)
	}
	env := os.Environ()
	for _, l := range lines {
		if !l.Comment {
			env = append(env, l.Key+"="+envUnquote(l.Value))
		}
	}
	cmd := exec.Command(c.Cmd[0], c.Cmd[1:]...)
	cmd.Env, cmd.Stdin, cmd.Stdout, cmd.Stderr = env, os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			os.Exit(ee.ExitCode())
		}
		return fmt.Errorf("secrets exec: %w", err)
	}
	return nil
}

// keyFlag names the age identity file a --seal-to pull is opened with.
type keyFlag struct {
	Key string `name:"key" placeholder:"FILE" env:"W17_AGE_KEY_FILE" help:"The age identity file (age-keygen's) holding the key --seal-to's recipient belongs to."`
}

// requireIdentities reads the identities of the --key file.
func requireIdentities(verb, key string) ([]age.Identity, error) {
	if key == "" {
		return nil, fmt.Errorf("secrets %s: no age identity — pass --key FILE or set W17_AGE_KEY_FILE", verb)
	}
	f, err := os.Open(key) // #nosec G304 -- the operator's own key file
	if err != nil {
		return nil, fmt.Errorf("secrets %s: %w", verb, err)
	}
	defer func() { _ = f.Close() }() // read only
	ids, err := age.ParseIdentities(f)
	if err != nil {
		return nil, fmt.Errorf("secrets %s: %s: %w", verb, key, err)
	}
	return ids, nil
}

// openSealed decrypts an age file with the identities of the --key file.
func openSealed(verb, file, key string) ([]byte, error) {
	ids, err := requireIdentities(verb, key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(file)
	if err != nil {
		return nil, fmt.Errorf("secrets %s: %w", verb, err)
	}
	defer func() { _ = f.Close() }() // read only
	r, err := age.Decrypt(f, ids...)
	if err != nil {
		return nil, fmt.Errorf("secrets %s: %s: %w", verb, file, err)
	}
	return io.ReadAll(r)
}
