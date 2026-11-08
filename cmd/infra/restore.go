package infra

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/remote"
)

// RestoreCmd is `w17ctl infra restore` — one database of a healthy server
// back from the server's latest backup, the rest of the stack running. The
// work is the server's install/restore-db.sh: a side instance from the
// backup, a dump of the database from it, the current database closed and
// kept as <db>_before_restore_<ts>, the dump restored under the database's
// own name and migrated by the release running now. Asked first: what the
// backup holds and the disk it needs (restore-db.sh --check), then the
// countdown and the names typed out.
type RestoreCmd struct {
	Env          string `name:"env" required:"" help:"The environment whose database to restore."`
	DB           string `name:"db" required:"" help:"The database (its name on the server, as deploy/<env>/README.md lists it)."`
	DropPrevious string `name:"drop-previous" placeholder:"TIMESTAMP" help:"Instead: drop the copy an earlier restore kept, <db>_before_restore_<TIMESTAMP>."`
	LockPath     string `name:"lock" placeholder:"PATH" default:"w17/lock.yaml" help:"Path to the lock file."`
	Console      string `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console (owns the lock). Optional — falls back to the binary's compile-time default."`
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

	var has strings.Builder
	if err := target.Run(nil, &has, target.Sudo("sh -c "+remote.ShellQuote("test -x "+script+" && echo yes || echo no"))); err != nil {
		return fmt.Errorf("infra restore: reaching %s: %w", ssh, err)
	}
	if f := strings.Fields(has.String()); len(f) == 0 || f[len(f)-1] != "yes" {
		return fmt.Errorf("infra restore: %s has no %s — environment %s backs no database up, or its last deploy predates the command; deploy it once", ssh, script, c.Env)
	}

	step("what the restore would do, on " + ssh)
	if err := target.Run(nil, core.Stdout, target.Sudo(script+args+" --check")); err != nil {
		return fmt.Errorf("infra restore: restore-db.sh --check on %s: %w — nothing was changed", ssh, err)
	}

	var text string
	if c.DropPrevious != "" {
		text = fmt.Sprintf("\nThis drops %s_before_restore_%s from environment %s's server for good.\n\n", c.DB, c.DropPrevious, c.Env)
	} else {
		text = fmt.Sprintf(`
This replaces database %[1]s of environment %[2]s with its state in the latest backup.

  What it holds now is kept as %[1]s_before_restore_<timestamp> (closed), never dropped
  without --drop-previous. Until the restore ends, the services using %[1]s fail their queries.

`, c.DB, c.Env)
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
