// Package certs wires `w17ctl certs` — seed a directory with a local
// dev PKI (a self-signed CA + a CA-signed server leaf) for the opt-in
// internal-mesh TLS switch (W17_INTERNAL_TLS=on). The implementation it
// delegates to is sdk/go/tooling/certgen.
package certs

import (
	"fmt"

	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/sdk/go/tooling/certgen"
)

// Cmd is `w17ctl certs` — the standalone twin of the cert seeding `init`
// does. `init` writes `w17/certs/dev.local/` once at bootstrap; this
// command refills a directory whose files are missing — the same
// idempotent EnsureCerts call. Two uses:
//
//   - Re-create dev certs an operator deleted (or never had, on a fresh
//     clone — the certs are gitignored, not committed).
//   - Scaffold a prod cert directory's MISSING material
//     (`--dir deploy/prod/certs`). It never overwrites existing files,
//     so a real CA / leaf already in place stays put. Production renewal
//     / rotation is devops' job — this only fills gaps.
type Cmd struct {
	Dir  string   `name:"dir" placeholder:"DIR" default:"w17/certs/dev.local" help:"Directory to fill with ca.crt/ca.key + server.crt/server.key. Idempotent — existing files are never overwritten."`
	SANs []string `name:"san" placeholder:"NAME" help:"Extra SubjectAltName entries (DNS name or IP) for the server leaf — typically the compose/k8s service names. localhost + loopback are always included."`
}

func (c *Cmd) Run() error {
	written, err := certgen.EnsureCerts(c.Dir, c.SANs)
	if err != nil {
		return fmt.Errorf("certs: %w", err)
	}
	if len(written) == 0 {
		fmt.Fprintf(core.Stdout, "certs: %s already complete — nothing to do\n", c.Dir)
		return nil
	}
	for _, f := range written {
		fmt.Fprintf(core.Stdout, "certs: wrote %s\n", f)
	}
	return nil
}
