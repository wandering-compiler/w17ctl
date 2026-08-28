package secrets

import (
	"fmt"
	"os"
	"strings"

	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/sdk/go/service/secret"
)

// EncryptCmd implements `w17ctl secrets encrypt <file>`. Read-only on the
// lock: the age recipients come from the console's DescribeLock (Block 2
// §8.2); the file encryption stays client-side.
type EncryptCmd struct {
	LockPath string `name:"lock" placeholder:"PATH" default:"w17/lock.yaml" help:"Path to the lock file."`
	Console  string `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console (owns the lock). Optional — falls back to the binary's compile-time default."`
	File     string `arg:"" name:"file" help:"Plain secrets file to encrypt (e.g. app-business/.secrets). Output is <file>.age."`
}

func (c *EncryptCmd) Run() error {
	lockBytes, err := os.ReadFile(c.LockPath)
	if err != nil {
		return fmt.Errorf("secrets encrypt: read lock %s: %w", c.LockPath, err)
	}
	view, err := core.DescribeLock(c.Console, lockBytes)
	if err != nil {
		return fmt.Errorf("secrets encrypt: %w", err)
	}
	recipients := view.GetSecrets().GetAgeRecipients()
	if len(recipients) == 0 {
		return fmt.Errorf("secrets encrypt: no age recipients in the lock — run `w17ctl secrets init` first")
	}
	if strings.HasSuffix(c.File, ".age") {
		return fmt.Errorf("secrets encrypt: %s is already an .age file", c.File)
	}
	plain, err := os.ReadFile(c.File)
	if err != nil {
		return fmt.Errorf("secrets encrypt: read %s: %w", c.File, err)
	}
	cipher, err := secret.EncryptToAge(plain, recipients...)
	if err != nil {
		return fmt.Errorf("secrets encrypt: %w", err)
	}
	out := c.File + ".age"
	if err := os.WriteFile(out, cipher, 0o644); err != nil {
		return fmt.Errorf("secrets encrypt: write %s: %w", out, err)
	}
	fmt.Fprintf(core.Stdout, "secrets encrypt: %s → %s (%d recipient(s)) — commit the .age, keep %s gitignored\n",
		c.File, out, len(recipients), c.File)
	return nil
}
