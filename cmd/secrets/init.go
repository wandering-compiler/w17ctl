package secrets

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/wandering-compiler/w17ctl/internal/core"
	codegenpb "github.com/wandering-compiler/sdk/go/pb/w17compiler"
	"github.com/wandering-compiler/sdk/go/service/secret"
)

// generateAgeKey is the keypair-minting seam (defaults to the runtime
// crypto). Overridable in tests to drive the keygen-failure branch —
// crypto/rand doesn't fail on demand. In-pattern with the other w17ctl
// function-var seams (core.DialClientFn, schema.LoadSchemaSet).
var generateAgeKey = secret.GenerateAgeKey

// InitCmd implements `w17ctl secrets init`.
type InitCmd struct {
	LockPath string `name:"lock" placeholder:"PATH" default:"w17/lock.yaml" help:"Path to the lock file."`
	Console  string `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console (owns the lock). Optional — falls back to the binary's compile-time default."`
	KeyFile  string `name:"key-file" placeholder:"PATH" default:"w17/keys/age.key" help:"Where to write the private age key (gitignored; mount this at deploy time via W17_SECRETS_AGE_KEY_FILE)."`
	Force    bool   `name:"force" help:"Overwrite an existing key file (mints a NEW keypair — anything encrypted for the old recipient becomes undecryptable unless that recipient is kept)."`
}

func (c *InitCmd) Run() error {
	lockBytes, err := os.ReadFile(c.LockPath)
	if err != nil {
		return fmt.Errorf("secrets init: read lock %s: %w", c.LockPath, err)
	}

	identity, recipient, err := generateAgeKey()
	if err != nil {
		return fmt.Errorf("secrets init: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(c.KeyFile), 0o700); err != nil {
		return fmt.Errorf("secrets init: mkdir key dir: %w", err)
	}
	// O_EXCL fails if the file already exists and refuses to follow a
	// pre-planted symlink — TOCTOU-safe vs the old stat-then-write race on a
	// secret-bearing file. --force explicitly mints a NEW keypair, so it
	// truncates an existing key instead.
	flags := os.O_CREATE | os.O_EXCL | os.O_WRONLY
	if c.Force {
		flags = os.O_CREATE | os.O_TRUNC | os.O_WRONLY
	}
	f, err := os.OpenFile(c.KeyFile, flags, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("secrets init: key file %s already exists (use --force to mint a new keypair)", c.KeyFile)
		}
		return fmt.Errorf("secrets init: write key file: %w", err)
	}
	keyBody := fmt.Sprintf("# w17 project age key — KEEP SECRET, never commit.\n# public recipient: %s\n%s\n", recipient, identity)
	if _, err := f.WriteString(keyBody); err != nil {
		_ = f.Close()
		return fmt.Errorf("secrets init: write key file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("secrets init: write key file: %w", err)
	}

	// Record the public recipient in the lock + default mode → "auto"
	// (idempotent server-side). The private key stays local; only the
	// recipient rides the lock.
	newBytes, err := core.EditLock(c.Console, lockBytes, &codegenpb.LockEditIntent{
		Intent: &codegenpb.LockEditIntent_InitSecretsAge{
			InitSecretsAge: &codegenpb.InitSecretsAgeIntent{Recipient: recipient},
		},
	})
	if err != nil {
		return fmt.Errorf("secrets init: %w", err)
	}
	if err := os.WriteFile(c.LockPath, newBytes, 0o644); err != nil {
		return fmt.Errorf("secrets init: write lock: %w", err)
	}

	// Gitignore the key dir (private keys never get committed). The
	// project root is the lock's grandparent (w17/lock.yaml → root).
	root := filepath.Dir(filepath.Dir(c.LockPath))
	keyDir := filepath.Dir(c.KeyFile)
	if err := ensureGitignore(root, keyDir+"/", "w17 secrets — private age keys, never commit"); err != nil {
		fmt.Fprintf(core.Stdout, "secrets init: warning: could not update .gitignore: %v\n", err)
	}

	fmt.Fprintf(core.Stdout, "secrets init: minted age keypair\n")
	fmt.Fprintf(core.Stdout, "  recipient (public, in lock): %s\n", recipient)
	fmt.Fprintf(core.Stdout, "  private key (gitignored):     %s\n", c.KeyFile)
	fmt.Fprintf(core.Stdout, "\nNext:\n")
	fmt.Fprintf(core.Stdout, "  1. put real values in a plain .secrets, then:\n")
	fmt.Fprintf(core.Stdout, "       w17ctl secrets encrypt <bundle>/.secrets   # → .secrets.age (commit this)\n")
	fmt.Fprintf(core.Stdout, "  2. at deploy, provide the key so the binary decrypts:\n")
	fmt.Fprintf(core.Stdout, "       W17_SECRETS_AGE_KEY_FILE=%s\n", c.KeyFile)
	fmt.Fprintf(core.Stdout, "     (no key set → the binary reads the plain .secrets — seamless for dev)\n")
	return nil
}
