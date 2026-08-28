package stack

import (
	"fmt"
	"sort"

	project "github.com/wandering-compiler/w17ctl/cmd/project"
	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/devconfig"
	"github.com/wandering-compiler/w17ctl/internal/remotesync"
)

// Remote-mode registry + mode-selection verbs (docs/experiments/remote-stack.md).
// All state lives in `~/.w17/config.yaml` (devconfig, per-user), never the
// signed lock — a remote docker host + which mode a project runs in are
// dev-machine concerns, exactly like the port map and run presets. These
// verbs only edit config; the rsync / remote-compose / tunnel machinery that
// consumes them lands in later slices.

// RemoteCmd is `stack remote` — manage the registry of remote docker hosts
// AND pin THIS project to one (`remote use <name>`). The local counterpart
// is the top-level `stack local`.
type RemoteCmd struct {
	Add    RemoteAddCmd    `cmd:"" help:"Register a remote docker host (global): --ssh user@host --path /srv/w17."`
	List   RemoteListCmd   `cmd:"" help:"List registered remote docker hosts."`
	Remove RemoteRemoveCmd `cmd:"" help:"Unregister a remote docker host (clears it from the default + any project pins)."`
	Use    UseRemoteCmd    `cmd:"" help:"Pin THIS project to remote mode on a named remote host."`
}

// RemoteAddCmd — register (or overwrite) a named remote docker host.
type RemoteAddCmd struct {
	Name string `arg:"" help:"Name for this remote (a Remotes key you reference with --remote / 'stack remote use')."`
	SSH  string `name:"ssh" required:"" placeholder:"user@host[:port]" help:"SSH destination for ssh/rsync. An ssh_config alias is fine."`
	Path string `name:"path" required:"" placeholder:"/srv/w17" help:"Base dir on the server; each project gets a <path>/<project> subdir."`
}

func (c *RemoteAddCmd) Run() error {
	cfg, err := core.LoadDevConfigFn()
	if err != nil {
		return err
	}
	_, existed := cfg.Remotes[c.Name]
	cfg.SetRemote(c.Name, &devconfig.Remote{SSH: c.SSH, Path: c.Path})
	if err := core.SaveDevConfigFn(cfg); err != nil {
		return err
	}
	verb := "registered"
	if existed {
		verb = "updated"
	}
	fmt.Fprintf(core.Stdout, "%s remote %q (%s:%s)\n", verb, c.Name, c.SSH, c.Path)
	return nil
}

// RemoteListCmd — list registered remotes, marking the default.
type RemoteListCmd struct{}

func (c *RemoteListCmd) Run() error {
	cfg, err := core.LoadDevConfigFn()
	if err != nil {
		return err
	}
	if len(cfg.Remotes) == 0 {
		fmt.Fprintln(core.Stdout, "no remotes registered — `w17ctl stack remote add <name> --ssh user@host --path /srv/w17`")
		return nil
	}
	names := make([]string, 0, len(cfg.Remotes))
	for n := range cfg.Remotes {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		r := cfg.Remotes[n]
		marker := ""
		if n == cfg.DefaultRemote {
			marker = " (default)"
		}
		fmt.Fprintf(core.Stdout, "%s%s\n  ssh:  %s\n  path: %s\n", n, marker, r.SSH, r.Path)
	}
	return nil
}

// RemoteRemoveCmd — unregister a remote (cascades to default + pins).
type RemoteRemoveCmd struct {
	Name string `arg:"" help:"Remote name to unregister."`
}

func (c *RemoteRemoveCmd) Run() error {
	cfg, err := core.LoadDevConfigFn()
	if err != nil {
		return err
	}
	if !cfg.DeleteRemote(c.Name) {
		return fmt.Errorf("remote %q is not registered", c.Name)
	}
	if err := core.SaveDevConfigFn(cfg); err != nil {
		return err
	}
	fmt.Fprintf(core.Stdout, "unregistered remote %q\n", c.Name)
	return nil
}

// SetModeCmd — set the GLOBAL default execution mode.
type SetModeCmd struct {
	Mode   string `arg:"" enum:"local,remote" help:"Default mode for projects that don't pin one: local | remote."`
	Remote string `name:"remote" placeholder:"NAME" help:"When mode=remote, also set the default remote to this registered name."`
}

func (c *SetModeCmd) Run() error {
	cfg, err := core.LoadDevConfigFn()
	if err != nil {
		return err
	}
	mode, err := devconfig.ParseMode(c.Mode)
	if err != nil {
		return err
	}
	cfg.DefaultMode = string(mode)
	if c.Remote != "" {
		if mode != devconfig.ModeRemote {
			return fmt.Errorf("--remote only applies with mode=remote")
		}
		if cfg.Remotes[c.Remote] == nil {
			return fmt.Errorf("unknown remote %q — register it first with `w17ctl stack remote add`", c.Remote)
		}
		cfg.DefaultRemote = c.Remote
	}
	if err := core.SaveDevConfigFn(cfg); err != nil {
		return err
	}
	if mode == devconfig.ModeRemote && cfg.DefaultRemote != "" {
		fmt.Fprintf(core.Stdout, "default mode set to remote (default remote: %q)\n", cfg.DefaultRemote)
	} else {
		fmt.Fprintf(core.Stdout, "default mode set to %s\n", mode)
	}
	return nil
}

// UseRemoteCmd — pin THIS project to remote mode + a specific remote.
type UseRemoteCmd struct {
	Name string `arg:"" help:"Registered remote to pin this project to."`
}

func (c *UseRemoteCmd) Run() error {
	cfg, err := core.LoadDevConfigFn()
	if err != nil {
		return err
	}
	root, err := core.FindProjectRoot()
	if err != nil {
		return err
	}
	if cfg.Remotes[c.Name] == nil {
		return fmt.Errorf("unknown remote %q — register it first with `w17ctl stack remote add`", c.Name)
	}
	name, err := project.EnsureRegistered(cfg, root)
	if err != nil {
		return err
	}
	p := cfg.Projects[name]
	p.Mode = string(devconfig.ModeRemote)
	p.Remote = c.Name
	// Pinning to remote turns the (public) bind into a real network
	// exposure — gate it behind the typed confirmation before saving.
	if p.Bind == devconfig.BindPublic {
		if err := confirmPublicExposure(p, newPrompter()); err != nil {
			return err
		}
	}
	if err := core.SaveDevConfigFn(cfg); err != nil {
		return err
	}
	fmt.Fprintf(core.Stdout, "%q pinned to remote mode on %q\n", name, c.Name)
	// Guard: remote mode rsyncs the tree, so it wants the generated rsync
	// ignore list. Warn (don't fail) when it is absent — the built-in
	// excludes cover the MVP until codegen produces it.
	if _, baseFound := rsyncExcludeFiles(root); !baseFound {
		fmt.Fprintf(core.Stdout,
			"note: no generated rsync ignore (%s) yet — remote sync uses built-in excludes until you run `w17ctl codegen`.\n      add an optional %s at the project root to extend it.\n",
			remotesync.BaseIgnoreFile, remotesync.CustomIgnoreFile)
	}
	return nil
}

// UseLocalCmd — pin THIS project back to local mode.
type UseLocalCmd struct{}

func (c *UseLocalCmd) Run() error {
	cfg, err := core.LoadDevConfigFn()
	if err != nil {
		return err
	}
	root, err := core.FindProjectRoot()
	if err != nil {
		return err
	}
	name, err := project.EnsureRegistered(cfg, root)
	if err != nil {
		return err
	}
	p := cfg.Projects[name]
	p.Mode = string(devconfig.ModeLocal)
	p.Remote = ""
	if err := core.SaveDevConfigFn(cfg); err != nil {
		return err
	}
	fmt.Fprintf(core.Stdout, "%q pinned to local mode\n", name)
	return nil
}
