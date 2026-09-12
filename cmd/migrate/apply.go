package migrate

import (
	"fmt"
	"strings"

	"github.com/wandering-compiler/w17ctl/internal/lockfile"
	migratesdk "github.com/wandering-compiler/sdk/go/tooling/migrate"
	"github.com/wandering-compiler/sdk/go/tooling/migrate/factory"
)

// The apply / fetch / rollback machinery that used to live here is gone: it
// moved into the generated binary, which is the process that can reach the
// database (sdk/go/tooling/migrate/migratecli). What is left is DSN
// resolution, still used by the commands this client kept.

// resolveDSNs builds the per-connection DSN list from the
// W17_TARGET_<CONN_UPPER> env-var convention. A connection with a target
// pinned in the lock but no DSN is a clear error listing the connection
// name + the env var the operator should set.
//
// envLookup is a parameterised getenv so tests drive it without
// t.Setenv (production wires os.Getenv). DSNs are always env-only —
// never a flag — so credentials don't leak into shell history or process
// listings.
func resolveDSNs(lk *lockfile.Lock, envLookup func(string) string) ([]factory.TargetSpec, error) {
	var specs []factory.TargetSpec
	var missing []string
	for _, conn := range lk.Connections {
		if conn.TargetMigrationID == "" {
			continue
		}
		name := conn.Name
		dsn := envLookup(envVarName(name))
		if dsn == "" {
			missing = append(missing, name)
			continue
		}
		specs = append(specs, factory.TargetSpec{Connection: name, DSN: dsn})
	}
	if len(missing) > 0 {
		first := missing[0]
		return nil, fmt.Errorf(
			"apply: no DSN for connection(s) %s — set env var %s",
			strings.Join(missing, ", "), envVarName(first),
		)
	}
	return specs, nil
}

// envVarName turns a schema-declared connection name into the
// W17_TARGET_<CONN_UPPER> convention. Hyphens collapse to underscores so
// connection name `read-replica` resolves via `W17_TARGET_READ_REPLICA`.
//
// The rule itself lives in the SDK: the generated binary reads the same
// variables, and an operator who sets the one THIS side names while the other
// side looks for a different spelling gets "no DSN for connection" pointing at
// a variable they can see is set.
func envVarName(connection string) string {
	return migratesdk.TargetEnvVar(connection)
}
