package infra

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/remote"
)

// RestoreCmd is `w17ctl infra restore` — one database of a healthy server
// back from the server's latest backup (or, with --to, as it was at a point
// in time: continuous backups replay the archived WAL up to it), the rest of
// the stack running and the services that use the database stopped for the
// duration. The
// work is the server's install/restore-db.sh: a side instance from the
// backup, a dump of the database from it, the current database closed and
// kept as <db>_before_restore_<ts>, the dump restored under the database's
// own name and migrated by the release running now. Asked first: what the
// backup holds and the disk it needs (restore-db.sh --check), then the
// countdown and the names typed out.
type RestoreCmd struct {
	Env          string `name:"env" required:"" help:"The environment whose database to restore."`
	DB           string `name:"db" required:"" help:"The database (its name on the server, as deploy/<env>/README.md lists it)."`
	To           string `name:"to" placeholder:"TIME" help:"Restore the database as it was at TIME (UTC: 2026-11-22T14:05:00Z, or 2026-11-22 14:05) instead of its latest backup: the last base backup finished before it, then the archived WAL replayed up to it. Needs continuous backups."`
	NoDrain      bool   `name:"no-drain" help:"Keep the services that use the database running during the restore (they fail their queries until it is back) instead of stopping them."`
	DropPrevious string `name:"drop-previous" placeholder:"TIMESTAMP" help:"Instead: drop the copy an earlier restore kept, <db>_before_restore_<TIMESTAMP>."`
	LockPath     string `name:"lock" placeholder:"PATH" default:"w17/lock.yaml" help:"Path to the lock file."`
	Console      string `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console (owns the lock). Optional — falls back to the console you are logged in to, then the binary's compile-time default."`
}

var (
	dbName    = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)
	keptStamp = regexp.MustCompile(`^[0-9]{14}$`)
)

func (c *RestoreCmd) Run() error {
	if !dbName.MatchString(c.DB) {
		return fmt.Errorf("infra restore: %q is not a database name", c.DB)
	}
	if c.DropPrevious != "" && !keptStamp.MatchString(c.DropPrevious) {
		return fmt.Errorf("infra restore: --drop-previous takes the timestamp of the kept copy (%s_before_restore_<14 digits>)", c.DB)
	}
	to, err := restoreTime(c.To, time.Now())
	if err != nil {
		return fmt.Errorf("infra restore: %w", err)
	}
	if c.DropPrevious != "" && (to != "" || c.NoDrain) {
		return fmt.Errorf("infra restore: --drop-previous takes neither --to nor --no-drain")
	}
	view, err := core.DescribeInfraAt("infra restore", c.Console, c.LockPath, false, "")
	if err != nil {
		return err
	}
	env, err := FindEnv(view, c.Env)
	if err != nil {
		return fmt.Errorf("infra restore: %w", err)
	}
	if env.GetTargetKind() != "swarm" {
		return fmt.Errorf("infra restore: restores a database of a swarm environment's server; %s deploys onto %s (%s), whose databases are the cloud's", c.Env, env.GetTarget(), env.GetTargetKind())
	}
	ssh := env.GetHost().GetSsh()
	if ssh == "" {
		return fmt.Errorf("infra restore: environment %q names no server — `w17ctl infra set targets.%s.ssh=root@<server>`", c.Env, env.GetTarget())
	}
	target, err := remote.Parse(ssh)
	if err != nil {
		return fmt.Errorf("infra restore: %w", err)
	}
	script := "/opt/stack/" + c.Env + "/install/restore-db.sh"
	args := " " + c.DB
	if c.DropPrevious != "" {
		args += " --drop-previous " + c.DropPrevious
	}
	if to != "" {
		args += " --to " + to
	}
	if c.NoDrain {
		args += " --no-drain"
	}

	// Whether the script is there, and whether it drains: the script is the
	// one the project's codegen engine rendered at its last deploy, which can
	// be older than this w17ctl. A v0.1 script takes neither --to nor
	// --no-drain and stops nothing while it restores.
	var has strings.Builder
	probe := "test -x " + script + " || { echo no; exit 0; }; if grep -q -e '--no-drain' " + script + "; then echo drains; else echo yes; fi"
	if err := target.Run(nil, &has, target.Sudo("sh -c "+remote.ShellQuote(probe))); err != nil {
		return fmt.Errorf("infra restore: reaching %s: %w", ssh, err)
	}
	f := strings.Fields(has.String())
	answer := ""
	if len(f) > 0 {
		answer = f[len(f)-1]
	}
	if answer != "yes" && answer != "drains" {
		return fmt.Errorf("infra restore: %s has no %s — environment %s backs no database up, or its last deploy predates the command; deploy it once", ssh, script, c.Env)
	}
	drains := answer == "drains"
	if !drains && (to != "" || c.NoDrain) {
		return fmt.Errorf("infra restore: the %s on %s predates --to and --no-drain (the project's codegen engine is older than 0.2) — restore the latest backup without them, or move the project forward (`w17ctl codegen-version`) and deploy", script, ssh)
	}

	step("what the restore would do, on " + ssh)
	if err := target.Run(nil, core.Stdout, target.Sudo(script+args+" --check")); err != nil {
		return fmt.Errorf("infra restore: restore-db.sh --check on %s: %w — nothing was changed", ssh, err)
	}

	var text string
	if c.DropPrevious != "" {
		text = fmt.Sprintf("\nThis drops %s_before_restore_%s from environment %s's server for good.\n\n", c.DB, c.DropPrevious, c.Env)
	} else {
		state, during := "its state in the latest backup", "the services using it are stopped (above)"
		if !drains {
			during = "the services using it keep running and fail their queries"
		}
		if to != "" {
			state = "its state at " + to
		}
		if c.NoDrain {
			during = "the services using it fail their queries"
		}
		text = fmt.Sprintf(`
This replaces database %[1]s of environment %[2]s with %[3]s.

  What it holds now is kept as %[1]s_before_restore_<timestamp> (closed), never dropped
  without --drop-previous. Until the restore ends, %[4]s.

`, c.DB, c.Env, state, during)
	}
	if err := confirm("infra restore", text, "nothing was changed",
		typed{"the environment's name", c.Env}, typed{"the database's name", c.DB}); err != nil {
		return err
	}

	step("restore-db.sh on " + ssh)
	if err := target.Run(nil, core.Stdout, target.Sudo(script+args)); err != nil {
		return fmt.Errorf("infra restore: restore-db.sh on %s: %w", ssh, err)
	}
	return nil
}

// restoreTime is --to as the server's restore-db.sh takes it: RFC 3339 in
// UTC. A time without a zone is read as UTC (the backups' clock); one in the
// future is refused — no WAL is archived for it.
func restoreTime(s string, now time.Time) (string, error) {
	if s == "" {
		return "", nil
	}
	var t time.Time
	var err error
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02T15:04", "2006-01-02 15:04"} {
		if t, err = time.Parse(layout, s); err == nil {
			break
		}
	}
	if err != nil {
		return "", fmt.Errorf("--to %q is not a time (2026-11-22T14:05:00Z, or 2026-11-22 14:05 read as UTC)", s)
	}
	if !t.Before(now) {
		return "", fmt.Errorf("--to %s is not in the past", t.UTC().Format(time.RFC3339))
	}
	return t.UTC().Format(time.RFC3339), nil
}
