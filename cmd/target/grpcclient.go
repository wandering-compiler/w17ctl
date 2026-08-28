package target

import (
	"fmt"
	"os"
	"strings"

	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/lockfile"
	"github.com/wandering-compiler/w17ctl/internal/prompter"
	codegenpb "github.com/wandering-compiler/sdk/go/pb/w17compiler"
)

// GrpcClientCmd is the parent of `w17ctl target grpc-client <leaf>` —
// add / list. Manages the `generated_code.grpc_clients[]` declarations
// codegen reads to render the project's Go gRPC `clients` package (the
// dependency-inversion seam a hand-written business binary builds its
// downstream clients from — see srcgo/gen/clients/<domain>/<tier>).
//
// Block 2 §8.2: the console owns the lock. The add goes through the
// console's EditLock (which re-signs); the list + the wizard's
// duplicate-output_root check read through DescribeLock.
type GrpcClientCmd struct {
	Add  GrpcClientAddCmd  `cmd:"" help:"Append a grpc_clients[] entry to the project's lock so codegen renders the Go gRPC clients package. Re-signs on save."`
	List GrpcClientListCmd `cmd:"" help:"List the grpc_clients[] entries declared in the lock."`
}

// grpcClientLanguages drives the wizard's language selection.
// Iter-1 ships "go" only, mirroring the GrpcClient pb comment.
var grpcClientLanguages = []string{"go"}

func parseGrpcClientLanguage(s string) (string, error) {
	switch strings.ToLower(s) {
	case "", "go":
		return "go", nil
	}
	return "", fmt.Errorf("unknown language %q (one of: %s)", s, strings.Join(grpcClientLanguages, ", "))
}

// GrpcClientAddCmd implements `w17ctl target grpc-client add`. The
// GrpcClient lock message carries only language + output_root;
// for Go leave output_root empty so it derives from the lock's
// `stubs` (<stubs>/clients). Set it only to override that
// default.
type GrpcClientAddCmd struct {
	LockPath string `name:"lock" placeholder:"PATH" default:"w17/lock.yaml" help:"Path to the lock file."`
	Console  string `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console (owns the lock). Optional — falls back to the binary's compile-time default."`

	// Non-interactive overrides — when --language is set the
	// wizard skips prompting + runs straight to validation + save.
	Language   string `name:"language"    placeholder:"NAME" help:"Skip prompt for language. One of: go. Empty = ask (default go)."`
	OutputRoot string `name:"output-root" placeholder:"PATH" help:"Project-relative output root. For Go leave empty: derives from the lock's stubs (<stubs>/clients)."`
}

func (c *GrpcClientAddCmd) Run() error {
	// Q58-console-1: serialise the read → EditLock → write below against
	// concurrent lock-mutating runs so the second write can't clobber the
	// first's edit. Held until this function returns.
	release, lockErr := lockfile.ForUpdate(c.LockPath)
	if lockErr != nil {
		return fmt.Errorf("grpc-client add: lock for update: %w", lockErr)
	}
	defer release()

	lockBytes, err := os.ReadFile(c.LockPath)
	if err != nil {
		return fmt.Errorf("grpc-client add: read lock %s: %w", c.LockPath, err)
	}
	view, err := core.DescribeLock(c.Console, lockBytes)
	if err != nil {
		return fmt.Errorf("grpc-client add: %w", err)
	}

	prompter := prompter.NewStdinPrompter()
	language, outputRoot, err := runGrpcClientAddWizard(prompter, view.GetGrpcClients(), c)
	if err != nil {
		return err
	}

	newBytes, err := core.EditLock(c.Console, lockBytes, &codegenpb.LockEditIntent{
		Intent: &codegenpb.LockEditIntent_AddGrpcClient{
			AddGrpcClient: &codegenpb.AddGrpcClientIntent{Language: language, OutputRoot: outputRoot},
		},
	})
	if err != nil {
		return fmt.Errorf("grpc-client add: %w", err)
	}
	if err := lockfile.WriteAtomic(c.LockPath, newBytes, 0o644); err != nil {
		return fmt.Errorf("grpc-client add: write lock: %w", err)
	}

	root := outputRoot
	if root == "" {
		root = "<stubs>/clients (default)"
	}
	fmt.Fprintf(core.Stdout, "grpc-client add: %s at %s → %s\n", language, root, c.LockPath)
	return nil
}

// runGrpcClientAddWizard is the interactive wizard body, pulled out of Run so
// the test harness can drive it without disk IO. When --language is set the
// prompter is bypassed (CI / scripts). `existing` is the lock's current
// grpc_clients[] (via DescribeLock) for the duplicate-output_root check; the
// console re-validates authoritatively on EditLock.
func runGrpcClientAddWizard(p prompter.Prompter, existing []*codegenpb.LockGrpcClient, c *GrpcClientAddCmd) (
	language string,
	outputRoot string,
	err error,
) {
	fmt.Fprintln(core.Stdout, "Adding a new grpc-clients entry to the project...")
	fmt.Fprintln(core.Stdout)

	languageStr := c.Language
	if languageStr == "" {
		languageStr, err = p.Select("Language", grpcClientLanguages, "go")
		if err != nil {
			return "", "", err
		}
	}
	language, err = parseGrpcClientLanguage(languageStr)
	if err != nil {
		return "", "", err
	}

	outputRoot = c.OutputRoot
	// Output root is optional for Go (derives from stubs); only
	// prompt when the override flag wasn't supplied AND we're
	// interactive. A blank answer keeps the derive-from-stubs
	// default, so an empty result is valid here.
	if outputRoot == "" && c.Language == "" {
		outputRoot, err = p.Text("Output root (project-relative; blank = derive from stubs)", "")
		if err != nil {
			return "", "", err
		}
	}
	for _, e := range existing {
		if e.GetOutputRoot() == outputRoot {
			where := outputRoot
			if where == "" {
				where = "<derive-from-stubs default>"
			}
			return "", "", fmt.Errorf("grpc-client add: output_root %q already claimed by another grpc_clients[] entry", where)
		}
	}
	return language, outputRoot, nil
}

// GrpcClientListCmd implements `w17ctl target grpc-client list`.
type GrpcClientListCmd struct {
	LockPath string `name:"lock" placeholder:"PATH" default:"w17/lock.yaml" help:"Path to the lock file."`
	Console  string `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console (owns the lock). Optional — falls back to the binary's compile-time default."`
}

func (c *GrpcClientListCmd) Run() error {
	view, err := core.DescribeLockAt("grpc-client list", c.Console, c.LockPath)
	if err != nil {
		return err
	}
	gcs := view.GetGrpcClients()
	if len(gcs) == 0 {
		fmt.Fprintln(core.Stdout, "no grpc_clients[] entries declared in the lock")
		return nil
	}
	for _, gc := range gcs {
		root := gc.GetOutputRoot()
		if root == "" {
			root = "<stubs>/clients (default)"
		}
		fmt.Fprintf(core.Stdout, "%s at %s\n", gc.GetLanguage(), root)
	}
	return nil
}
