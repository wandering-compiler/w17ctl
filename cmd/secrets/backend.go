package secrets

import (
	"fmt"
	"strings"

	"github.com/wandering-compiler/w17ctl/internal/core"
	codegenpb "github.com/wandering-compiler/sdk/go/pb/w17compiler"
)

// BackendCmd implements `w17ctl secrets backend <name>`. The lock mutation
// (secrets.backend) goes through the console's EditLock (the console owns
// the lock).
type BackendCmd struct {
	LockPath string `name:"lock" placeholder:"PATH" default:"w17/lock.yaml" help:"Path to the lock file."`
	Console  string `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console (owns the lock). Optional — falls back to the console you are logged in to, then the binary's compile-time default."`
	Name     string `arg:"" name:"name" help:"Materialiser: plain | eso | vault | cloud-csi."`
}

func (c *BackendCmd) Run() error {
	name := strings.ToLower(strings.TrimSpace(c.Name))
	if !secretsBackends[name] {
		return fmt.Errorf("secrets backend: unknown backend %q (plain|eso|vault|cloud-csi)", c.Name)
	}
	// Read → EditLock → atomic write under the update lock, so a concurrent
	// `push` cannot interleave and drop this edit, and a crash leaves the old
	// lock rather than a truncated one.
	if err := core.EditLockOnDisk("secrets backend", c.Console, c.LockPath, &codegenpb.LockEditIntent{
		Intent: &codegenpb.LockEditIntent_SetSecretsBackend{
			SetSecretsBackend: &codegenpb.SetSecretsBackendIntent{Backend: name},
		},
	}); err != nil {
		return err
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
