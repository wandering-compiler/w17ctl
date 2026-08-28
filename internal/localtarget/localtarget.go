// Package localtarget derives the localhost DSN for a project's PUBLISHED dev
// store connections — the zero-flag, zero-env resolution shared by `stack
// build`, `db snapshot`, and `fixtures apply` so a developer never hand-sets a
// W17_TARGET_* env for a LOCAL store. The DSN is fully derivable: the dialect
// from the connection name, the host port from the devconfig allocation (what
// `stack up` published), and the dev credentials codegen bakes into the compose
// (user=pass=db=<domain>). A REMOTE/prod target's DSN is a secret and is NOT
// derivable here — callers keep the W17_TARGET_<CONN> env override for that.
package localtarget

import (
	"fmt"
	"strings"

	"github.com/wandering-compiler/w17ctl/internal/codegen"
	"github.com/wandering-compiler/w17ctl/internal/devconfig"
)

// ResolveDSN returns the localhost DSN for one published dev store connection.
// On success skipReason is empty; otherwise dsn is empty and skipReason names
// why (no published port yet, unknown dialect, or a schemaless/file store with
// no localhost DSN form) so the caller can surface it.
func ResolveDSN(connName string, p *devconfig.Project) (dsn, skipReason string) {
	dialect, ok := codegen.DialectFromConnectionName(connName)
	if !ok {
		return "", fmt.Sprintf("%s: cannot infer a dialect from the connection name", connName)
	}
	port := 0
	if p != nil {
		port = p.Ports[HostPortSlot(connName)]
	}
	if port == 0 {
		return "", fmt.Sprintf("%s: no published host port — run 'w17ctl stack up' first", connName)
	}
	dsn = DSN(dialect, codegen.ConnectionDomain(connName), port)
	if dsn == "" {
		return "", fmt.Sprintf("%s: dialect %q has no localhost DSN (schemaless / file store)", connName, dialect)
	}
	return dsn, ""
}

// HostPortSlot is the devconfig `Ports` key for a connection's published host
// port (e.g. "core-postgres" → "W17_CORE_POSTGRES_HOST_PORT") — the same slot
// codegen writes into the dev compose's port mapping.
func HostPortSlot(connName string) string {
	return "W17_" + strings.NewReplacer("-", "_", ".", "_").Replace(strings.ToUpper(connName)) + "_HOST_PORT"
}

// DSN builds the localhost DSN for a published dev store. Mirrors the
// credentials codegen writes into the dev compose (composegen:
// user=pass=db=<domain> for postgres/mysql; redis has no auth, db 0).
func DSN(dialect, domain string, port int) string {
	switch dialect {
	case "postgres":
		return fmt.Sprintf("postgres://%s:%s@localhost:%d/%s?sslmode=disable", domain, domain, port, domain)
	case "mysql":
		return fmt.Sprintf("mysql://%s:%s@localhost:%d/%s", domain, domain, port, domain)
	case "redis":
		return fmt.Sprintf("redis://localhost:%d/0", port)
	}
	return ""
}
