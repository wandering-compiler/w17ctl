package target

import (
	"fmt"
	"strings"

	"github.com/wandering-compiler/w17ctl/internal/core"
	codegenpb "github.com/wandering-compiler/sdk/go/pb/w17compiler"
)

// PbStubsCmd is the parent of `w17ctl target pb-stubs <leaf>` — set / list.
// It manages the lock's `pb_stubs[]` surface: where the generated protobuf
// stubs land on disk, and what import prefix they carry.
//
// Those two answers used to have no writer at all. `init` does not ask for
// them and no lock edit reached them, so every project held them empty with no
// way to change that. That is survivable while a project keeps the conventional
// layout — the on-disk root derives from `stubs` and the import prefix is
// recovered from the gen dir's go.mod — and it is NOT survivable once the
// stubs root moves, because the recovered module then belongs to whatever
// directory happened to sit at the conventional path.
//
// Block 2 §8.2: the console owns the lock. This treats w17/lock.yaml as opaque
// bytes; the edit goes through EditLock (which validates + re-signs).
type PbStubsCmd struct {
	Set PbStubsSetCmd `cmd:"" help:"Set the on-disk root and/or import prefix for a language's pb stubs. Re-signs on save."`
}

// PbStubsSetCmd implements `w17ctl target pb-stubs set`.
type PbStubsSetCmd struct {
	Language   string `name:"language" default:"go" help:"Which pb_stubs entry to set."`
	OutputRoot string `name:"output-root" placeholder:"DIR" help:"Project-relative dir the stubs are written to. Empty keeps the derivation from 'stubs' (<stubs>/pb)."`
	Package    string `name:"package" placeholder:"IMPORT" help:"Go IMPORT prefix the stubs carry, e.g. github.com/acme/app/w17gen/pb. This is the module identity — set it whenever the stubs root is not the conventional one."`
	LockPath   string `name:"lock" placeholder:"PATH" default:"w17/lock.yaml" help:"Path to the lock file."`
	Console    string `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console (owns the lock). Optional."`
}

func (c *PbStubsSetCmd) Run() error {
	if strings.TrimSpace(c.OutputRoot) == "" && strings.TrimSpace(c.Package) == "" {
		return fmt.Errorf("pb-stubs set: nothing to set — pass --output-root, --package, or both\n" +
			"  --package is the one that matters when the stubs root is not the convention:\n" +
			"  it states the import prefix in the lock instead of leaving it to be derived")
	}
	if err := core.EditLockOnDisk("pb-stubs set", c.Console, c.LockPath, &codegenpb.LockEditIntent{
		Intent: &codegenpb.LockEditIntent_SetPbStub{
			SetPbStub: &codegenpb.SetPbStubIntent{
				Language:   c.Language,
				OutputRoot: c.OutputRoot,
				Package:    c.Package,
			},
		},
	}); err != nil {
		return err
	}
	fmt.Fprintf(core.Stdout, "pb-stubs set: %s → output_root=%q package=%q (%s)\n",
		c.Language, c.OutputRoot, c.Package, c.LockPath)
	fmt.Fprintf(core.Stdout, "  run `w17ctl codegen --force` to regenerate against the new paths\n")
	return nil
}
