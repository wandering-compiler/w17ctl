// Package secrets wires `w17ctl secrets <leaf>` — the age-tier
// ("optional key") production-secrets surface. `init` mints a project
// age keypair (encrypted when the key is present, plain otherwise);
// `encrypt` turns a plain `.secrets` into a committable `.secrets.age`;
// `backend` sets the deploy-boundary materialiser. The runtime
// resolution chain (sdk/go/service/secret) consumes the result. Spec:
// docs/specs/secrets/production-secrets.md.
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
	Backend BackendCmd `cmd:"" help:"Set the deploy-boundary materialiser the generated deploy artefacts target: plain (in-process / env_file) | sops (sops exec-env) | eso (ExternalSecret) | vault | cloud-csi. For sops it also writes a .sops.yaml for the lock's age recipients. Re-signs the lock."`
}

// secretsBackends are the materialisers the lock's secrets.backend may
// name. "plain" (the default) covers the in-process age tier + the
// existing env_file/secretKeyRef wiring; the rest are deploy-boundary
// materialisers (sops built; eso/vault/cloud-csi emit a documented
// manifest stub — see production-secrets.md).
var secretsBackends = map[string]bool{
	"plain": true, "sops": true, "eso": true, "vault": true, "cloud-csi": true,
}

// sopsConfig renders a .sops.yaml whose creation rule encrypts the
// project's `.secrets` / `.secrets.enc` files for the given age
// recipients — so `sops <file>` Just Works for every developer holding
// a matching key. sops supports age + KMS/Vault recipients; this writes
// the age set the lock tracks (add KMS lines by hand for that backend).
func sopsConfig(recipients []string) string {
	var b strings.Builder
	b.WriteString("# Managed by `w17ctl secrets backend sops`. Lets the sops CLI\n")
	b.WriteString("# encrypt/edit the project's secret files for the age recipient(s)\n")
	b.WriteString("# recorded in the lock. Add KMS/Vault recipients by hand for those\n")
	b.WriteString("# backends. Decrypt at the deploy boundary: `sops exec-env <f> -- <bin>`.\n")
	b.WriteString("creation_rules:\n")
	b.WriteString("  - path_regex: (\\.secrets(\\.enc)?$|secrets.*\\.env$)\n")
	b.WriteString("    age: " + strings.Join(recipients, ",") + "\n")
	return b.String()
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
