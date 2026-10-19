package secrets

import (
	"fmt"
	"os"
	"strings"

	"github.com/wandering-compiler/w17ctl/internal/core"
	codegenpb "github.com/wandering-compiler/sdk/go/pb/w17compiler"
)

// BackendCmd implements `w17ctl secrets backend <name>`. The lock mutation
// (secrets.backend) goes through the console's EditLock (the console owns
// the lock).
type BackendCmd struct {
	LockPath string `name:"lock" placeholder:"PATH" default:"w17/lock.yaml" help:"Path to the lock file."`
	Console  string `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console (owns the lock). Optional — falls back to the binary's compile-time default."`
	Name     string `arg:"" name:"name" help:"Materialiser: plain | eso | vault | cloud-csi."`
}

func (c *BackendCmd) Run() error {
	name := strings.ToLower(strings.TrimSpace(c.Name))
	if !secretsBackends[name] {
		return fmt.Errorf("secrets backend: unknown backend %q (plain|eso|vault|cloud-csi)", c.Name)
	}
	lockBytes, err := os.ReadFile(c.LockPath)
	if err != nil {
		return fmt.Errorf("secrets backend: read lock %s: %w", c.LockPath, err)
	}
	newBytes, err := core.EditLock(c.Console, lockBytes, &codegenpb.LockEditIntent{
		Intent: &codegenpb.LockEditIntent_SetSecretsBackend{
			SetSecretsBackend: &codegenpb.SetSecretsBackendIntent{Backend: name},
		},
	})
	if err != nil {
		return fmt.Errorf("secrets backend: %w", err)
	}
	if err := os.WriteFile(c.LockPath, newBytes, 0o644); err != nil {
		return fmt.Errorf("secrets backend: write lock: %w", err)
	}
	fmt.Fprintf(core.Stdout, "secrets backend: %s → %s\n", name, c.LockPath)

	switch name {
	case "eso", "vault", "cloud-csi":
		fmt.Fprintf(core.Stdout, "  next `w17ctl codegen` emits deploy/prod/secrets-%s.yaml per storage bundle —\n", name)
		fmt.Fprintf(core.Stdout, "  a manifest syncing the DSN keys into the <domain>-storage-secrets k8s Secret\n")
		fmt.Fprintf(core.Stdout, "  the Deployment references. Fill in the REPLACE_ME values (store ref / backend path).\n")
	}
	return nil
}
