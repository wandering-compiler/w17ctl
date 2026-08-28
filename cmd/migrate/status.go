package migrate

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/lockfile"
)

// StatusCmd reports, per lock-listed connection, the pinned target migration
// and how many fetched artifacts sit on disk. Fully OFFLINE + read-only — the
// local counterpart absorbed from w17migrate's `status`.
type StatusCmd struct {
	MigrationsDir string `name:"migrations" placeholder:"DIR" default:"w17/migrations" help:"Directory holding fetched artifacts; status counts on-disk migrations per connection."`
}

func (c *StatusCmd) Run() error {
	// Fully offline: read the lock from disk via the local lockfile view (no
	// console). A missing root / lock is a friendly note, not an error.
	root, err := core.FindProjectRoot()
	if err != nil {
		fmt.Fprintln(core.Stdout, "no w17/lock.yaml found — run from inside a w17 project")
		return nil
	}
	lk, err := lockfile.Load(filepath.Join(root, "w17", "lock.yaml"))
	if err != nil {
		fmt.Fprintln(core.Stdout, "no w17/lock.yaml found — run from inside a w17 project")
		return nil
	}
	fmt.Fprintf(core.Stdout, "project: %s (id=%s)\n", lk.Project, lk.ProjectID)
	if len(lk.Connections) == 0 {
		fmt.Fprintln(core.Stdout, "  no connections recorded")
		return nil
	}
	for _, conn := range lk.Connections {
		target := conn.TargetMigrationID
		if target == "" {
			fmt.Fprintf(core.Stdout, "  %s: <no target pinned>\n", conn.Name)
			continue
		}
		fmt.Fprintf(core.Stdout, "  %s: target=%s\n", conn.Name, target)
		dir := filepath.Join(c.MigrationsDir, conn.Name)
		if count := countOnDiskMigrations(dir); count == 0 {
			fmt.Fprintln(core.Stdout, "    on disk: 0 (run `migrate fetch`)")
		} else {
			fmt.Fprintf(core.Stdout, "    on disk: %d migration(s) in %s\n", count, dir)
		}
	}
	return nil
}

// countOnDiskMigrations counts the *.json migration artifacts in a
// connection's fetched directory. A missing directory is 0 (legitimate: the
// connection hasn't been fetched yet).
func countOnDiskMigrations(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".json" {
			n++
		}
	}
	return n
}
