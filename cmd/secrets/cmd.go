// Package secrets wires `w17ctl secrets <leaf>` — the age-tier
// ("optional key") production-secrets surface. `init` mints a project
// age keypair (encrypted when the key is present, plain otherwise);
// `encrypt` turns a plain `.secrets` into a committable `.secrets.age`;
// `backend` sets the deploy-boundary materialiser. The runtime
// resolution chain (sdk/go/service/secret) consumes the result.
package secrets

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Cmd is the parent of `w17ctl secrets <leaf>`.
type Cmd struct {
	Init    InitCmd    `cmd:"" help:"Mint a project age keypair: writes the private key to a gitignored key file, records the public recipient in the lock (re-signs), and gitignores the key dir. Idempotent-ish — refuses to clobber an existing key without --force."`
	Encrypt EncryptCmd `cmd:"" help:"Encrypt a plain secrets file into a committable <file>.age for the lock's age recipient(s) (round-trips with the age CLI + the runtime resolver)."`
	Backend BackendCmd `cmd:"" help:"Set the deploy-boundary materialiser the generated deploy artefacts target: plain (in-process / env_file) | eso (ExternalSecret) | vault | cloud-csi. Re-signs the lock."`

	// An environment's values, held by the console — the only place they live.
	Set   SetCmd   `cmd:"" help:"Write values of an environment to the console: --env E SERVICE KEY…, each asked for without echo (or dotenv lines on stdin). Sealed here to a write grant (a code from the project's page, five minutes, one command); read them back with secrets pull."`
	Unset UnsetCmd `cmd:"" help:"Remove one value of an environment from the console (it is kept, marked removed, and no longer deployed)."`
	List  ListCmd  `cmd:"" help:"The keys set in an environment on the console — never the values."`
	Pull  PullCmd  `cmd:"" help:"Read one service's values of an environment back, as dotenv: --env E SERVICE, after a read code from the project's page (your password). They arrive sealed to a key made for this read; --seal-to RECIPIENT keeps what is written encrypted (age)."`
	Open  OpenCmd  `cmd:"" help:"Decrypt a secrets pull --seal-to file to stdout (the age identity file: --key or $W17_AGE_KEY_FILE)."`
	Exec  ExecCmd  `cmd:"" help:"Run a command with a secrets pull --seal-to file's values in its environment, writing nothing readable: secrets exec FILE -- CMD…"`
	Fetch FetchCmd `cmd:"" help:"A deploy's fetch: --env E --out DIR, with W17_TOKEN of a machine account given Deploy access to E. Checks the values against each service's .env.example, then writes DIR/<service>.env (--json: <service>.json). What the deploy workflows run."`

	FieldKey FieldKeyCmd `cmd:"" name:"field-key" help:"Print a field-encryption keyring for W17_FIELD_KEYS (the key of CRYPTED_SECRET columns): a fresh 1:<key>, or --rotate <current> to append the next version (new values then encrypt with it, the old ones keep decrypting). Set it on the console (w17ctl secrets set --env <env> <svc> W17_FIELD_KEYS); keep it — without it those columns are unreadable."`
}

// secretsBackends are the materialisers the lock's secrets.backend may
// name. "plain" (the default) covers the in-process age tier + the
// existing env_file/secretKeyRef wiring; the rest are deploy-boundary
// materialisers that emit a documented manifest stub — see
// production-secrets.md.
var secretsBackends = map[string]bool{
	"plain": true, "eso": true, "vault": true, "cloud-csi": true,
}

// ensureGitignore appends `pattern` to <root>/.gitignore (creating the
// file if absent) when the pattern isn't already present, prefixed by a
// `# <comment>` line. A no-op when the pattern is already ignored.
func ensureGitignore(root, pattern, comment string) error {
	path := filepath.Join(root, ".gitignore")
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, line := range strings.Split(string(existing), "\n") {
		if strings.TrimSpace(line) == pattern {
			return nil // already ignored
		}
	}
	var b strings.Builder
	b.Write(existing)
	if len(existing) > 0 && !strings.HasSuffix(string(existing), "\n") {
		b.WriteByte('\n')
	}
	if len(existing) > 0 {
		b.WriteByte('\n')
	}
	fmt.Fprintf(&b, "# %s\n%s\n", comment, pattern)
	return os.WriteFile(path, []byte(b.String()), 0o644)
}
