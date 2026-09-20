package target

import (
	"fmt"
	"strings"

	"github.com/wandering-compiler/w17ctl/internal/core"
	codegenpb "github.com/wandering-compiler/sdk/go/pb/w17compiler"
)

// LayoutCmd is the parent of `w17ctl target layout <leaf>` — today just `set`.
// It re-points the project's DIRECTORY fields: where proto lives, where stubs
// are written, where the language catalogues sit.
//
// Those answers used to be given once, in the `init` wizard, and never again.
// Asked for by marb, and the shape of the ask is migration onto w17: you want a
// provisional proto dir while an existing tree is still being converted, then
// the real one when the conversion lands. Changing your mind cost a re-init —
// which is the normal first week for anyone adopting w17 on an existing
// codebase, not an edge case.
//
// Block 2 §8.2: the console owns the lock. This treats w17/lock.yaml as opaque
// bytes; the edit goes through EditLock, which validates and re-signs.
type LayoutCmd struct {
	Set LayoutSetCmd `cmd:"" help:"Re-point proto/stubs/languages directories in the lock. Moves no files. Re-signs on success."`
}

// LayoutSetCmd implements `w17ctl target layout set`.
//
// Three fields, not the six that are write-once. `pb_language`, `languages` and
// `e2e` are deliberately absent: each has a different second half — what
// happens to stubs already generated in the old language is not the same
// question as what happens to a directory — and answering one in a command that
// silently leaves the others open would make the unanswered ones look settled.
type LayoutSetCmd struct {
	ProtoDir     string `name:"proto-dir" placeholder:"DIR" help:"Project-relative dir holding the project's .proto tree."`
	Stubs        string `name:"stubs" placeholder:"DIR" help:"Project-relative root the generated stub tree is written under."`
	LanguagesDir string `name:"languages-dir" placeholder:"DIR" help:"Project-relative dir holding the language catalogues."`
	LockPath     string `name:"lock" placeholder:"PATH" default:"w17/lock.yaml" help:"Path to the lock file."`
	Console      string `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console that owns this lock."`
}

func (c *LayoutSetCmd) Run() error {
	proto := strings.TrimSpace(c.ProtoDir)
	stubs := strings.TrimSpace(c.Stubs)
	langs := strings.TrimSpace(c.LanguagesDir)
	if proto == "" && stubs == "" && langs == "" {
		return fmt.Errorf("layout set: nothing to set — pass --proto-dir, --stubs, --languages-dir, or several\n" +
			"  an omitted flag leaves that directory as it stands; it does not clear it")
	}

	if err := core.EditLockOnDisk("layout set", c.Console, c.LockPath, &codegenpb.LockEditIntent{
		Intent: &codegenpb.LockEditIntent_SetDirLayout{
			SetDirLayout: &codegenpb.SetDirLayoutIntent{
				ProtoDir:     proto,
				Stubs:        stubs,
				LanguagesDir: langs,
			},
		},
	}); err != nil {
		return err
	}

	for _, f := range []struct{ name, val string }{
		{"proto_dir", proto}, {"stubs", stubs}, {"languages_dir", langs},
	} {
		if f.val != "" {
			fmt.Fprintf(core.Stdout, "layout set: %s → %q (%s)\n", f.name, f.val, c.LockPath)
		}
	}

	// ⚠️ SAYING THIS IS PART OF THE FEATURE, not politeness.
	//
	// The command re-points the lock and moves nothing, which is the right
	// default for the case it was built for — the tree is mid-conversion, so
	// moving it would fight the caller. But a re-point that does not SAY it is
	// a re-point leaves two silent hazards: sources sitting where the old
	// answer said they were, and generated output under the old directory that
	// nothing applies and nothing cleans up. That is the orphaned-artefact
	// shape, arriving through a setting instead of through a migration.
	fmt.Fprintf(core.Stdout, "\n  The LOCK changed; nothing on disk did.\n")
	fmt.Fprintf(core.Stdout, "  - move your own sources into the new directory (this command will not)\n")
	fmt.Fprintf(core.Stdout, "  - generated output under the OLD directory is now stale; delete it\n")
	fmt.Fprintf(core.Stdout, "  - then run `w17ctl codegen --force`\n")
	return nil
}
