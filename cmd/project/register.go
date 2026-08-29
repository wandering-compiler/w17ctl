package project

import (
	"context"
	"fmt"
	"strings"
	"time"

	"google.golang.org/grpc/metadata"

	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/lockfile"
	codegenpb "github.com/wandering-compiler/sdk/go/pb/w17compiler"
	w17registrypb "github.com/wandering-compiler/sdk/go/pb/w17registry"
)

// RegisterCmd implements `w17ctl project register` — registering a project
// that ALREADY has a lock.
//
// # Why this is not `init`
//
// `init` is the wizard: it registers a project AND builds the lock from the
// answers it collects. It refuses an existing lock, and that refusal is right
// — rerunning it would rewrite connections, pinned migration targets, plugin
// activations and the generated_code declarations from defaults. Those are the
// state the lock exists to carry, and nobody can retype them accurately.
//
// So a project generating against one console had no way onto another: `init`
// refused, `project import` is the LOCAL port registry (it never touches
// project_id), and `project import-from` folds one project into another that
// already exists. Pushing was the only thing that worked, and only because the
// console used to accept a write under an id it had never registered — the
// hole this command exists to replace.
//
// # What it does
//
//	RegisterProject on the console  →  new project_id
//	EditLock(adopt_project)         →  the console re-signs the lock
//
// Exactly one field of the lock changes. The console owns the edit and the
// signature; this command ships an intent and writes back what it gets, as the
// public split requires.
type RegisterCmd struct {
	Name     string `name:"name" placeholder:"NAME" help:"Register under this name. Empty uses the lock's project name."`
	Org      string `name:"org" placeholder:"SLUG" help:"Organization to register under. Empty lets the console infer it when you belong to exactly one; with several, it refuses rather than guess."`
	LockPath string `name:"lock" placeholder:"PATH" default:"w17/lock.yaml" help:"Path to this project's lock. Rewritten in place with the new project_id, re-signed by the console."`
	Console  string `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"Console gRPC endpoint. Falls back to the console you are logged into, then the compiled-in default."`
}

func (c *RegisterCmd) Run() error {
	addr, err := core.ResolveConsoleAddr(c.Console)
	if err != nil {
		return fmt.Errorf("project register: %w", err)
	}

	// Read the lock FIRST, so a missing one fails before anything is
	// registered. Registering and then failing to write would leave a project
	// on the console that no lock points at — the litter this command is
	// meant to stop producing.
	lk, err := lockfile.Load(c.LockPath)
	if err != nil {
		return fmt.Errorf("project register: %w\n"+
			"  This command adopts an EXISTING lock. To create one, run `w17ctl init`.", err)
	}

	name := strings.TrimSpace(c.Name)
	if name == "" {
		name = strings.TrimSpace(lk.Project)
	}
	if name == "" {
		return fmt.Errorf("project register: no project name — the lock carries none, pass --name")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cl, conn, err := core.DialProjectRegistry(addr)
	if err != nil {
		return fmt.Errorf("project register: dial %s: %w", addr, err)
	}
	defer func() { _ = conn.Close() }()

	regCtx := ctx
	if org := strings.TrimSpace(c.Org); org != "" {
		regCtx = metadata.AppendToOutgoingContext(ctx, "w17-org", org)
	}
	resp, err := cl.RegisterProject(regCtx, &w17registrypb.RegisterProjectRequest{Name: name})
	if err != nil {
		return fmt.Errorf("project register: %w", err)
	}
	projectID := resp.GetProjectId()
	if projectID == "" {
		return fmt.Errorf("project register: console returned an empty project_id")
	}

	prior := lk.ProjectID
	if err := core.EditLockOnDisk("project register", addr, c.LockPath, &codegenpb.LockEditIntent{
		Intent: &codegenpb.LockEditIntent_AdoptProject{
			AdoptProject: &codegenpb.AdoptProjectIntent{
				ProjectId: projectID,
				Project:   name,
				W17Url:    addr,
			},
		},
	}); err != nil {
		return err
	}

	fmt.Printf("✓ registered %q on %s\n", name, addr)
	fmt.Printf("  project_id: %s\n", projectID)
	if prior != "" && prior != projectID {
		// Said plainly, because the consequence is not obvious: anything the
		// console holds under the OLD id stays there and stays unreachable.
		// The id is minted by the console, so the previous one cannot be
		// re-claimed — a first push under the new id is the way forward.
		fmt.Printf("  was:        %s (anything stored under it stays there; push again under the new id)\n", prior)
	}
	return nil
}
