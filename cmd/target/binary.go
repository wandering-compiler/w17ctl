package target

import (
	"fmt"
	"sort"
	"strings"

	"github.com/wandering-compiler/w17ctl/internal/core"
	codegenpb "github.com/wandering-compiler/sdk/go/pb/w17compiler"
)

// BinaryCmd is the parent of `w17ctl target binary <leaf>` — compose /
// list / decompose. It manages the lock's `binaries[]` surface
// (binary-composition feature): folding several of a domain's
// generated components into ONE binary instead of the default
// one-binary-per-component layout.
//
// Block 2 §8.2: the console owns the lock. compose / decompose go
// through the console's EditLock (which validates the combo + re-signs);
// list reads through DescribeLock. The component-combo rules + the
// derived binary name live server-side.
type BinaryCmd struct {
	Compose   BinaryComposeCmd   `cmd:"" help:"Declare a composed binary for a domain: fold its components (gateway ± admin, or the full gateway+storage+business -server) into one binary. Re-signs on save."`
	List      BinaryListCmd      `cmd:"" help:"List the composed binaries declared in the lock."`
	Decompose BinaryDecomposeCmd `cmd:"" help:"Remove a domain's composed-binary declaration (revert to standalone per-component binaries). Re-signs on save."`
}

// BinaryComposeCmd implements `w17ctl target binary compose <domain>
// --components gateway,admin`. It sets (or replaces) the domain's
// `binaries[]` entry. The binary name is DERIVED from the component
// set server-side: a storage-bearing set → `<domain>-server` (full
// tier stack), else a gateway set → `<domain>-gateway`.
type BinaryComposeCmd struct {
	Domain     string `arg:"" help:"Domain whose components to compose (matches proto/domains/<DOMAIN>/)."`
	Components string `name:"components" placeholder:"LIST" required:"" help:"Comma-separated component set. gateway is required. gateway,admin folds admin into the gateway; gateway,storage,business (±admin ±eventbus) folds the full tier stack into one <domain>-server with direct in-process tier calls."`
	LockPath   string `name:"lock" placeholder:"PATH" default:"w17/lock.yaml" help:"Path to the lock file."`
	Console    string `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console (owns the lock). Optional — falls back to the binary's compile-time default."`
}

func (c *BinaryComposeCmd) Run() error {
	if c.Domain == "" {
		return fmt.Errorf("binary compose: domain required")
	}
	components, err := parseComponents(c.Components)
	if err != nil {
		return fmt.Errorf("binary compose: %w", err)
	}

	// The console validates the combo (ValidateComponentCombo) +
	// re-signs; a bad set is rejected there with the canonical message.
	if err := core.EditLockOnDisk("binary compose", c.Console, c.LockPath, &codegenpb.LockEditIntent{
		Intent: &codegenpb.LockEditIntent_ComposeBinary{
			ComposeBinary: &codegenpb.ComposeBinaryIntent{Domain: c.Domain, Components: components},
		},
	}); err != nil {
		return err
	}
	fmt.Fprintf(core.Stdout, "binary compose: %s [%s] → %s\n",
		c.Domain, strings.Join(components, ", "), c.LockPath)
	return nil
}

// parseComponents splits + normalises the --components CSV:
// trims, lowercases, drops blanks, dedups (stable order), then
// sorts for a canonical set.
func parseComponents(csv string) ([]string, error) {
	if strings.TrimSpace(csv) == "" {
		return nil, fmt.Errorf("--components is empty")
	}
	seen := map[string]struct{}{}
	var out []string
	for _, raw := range strings.Split(csv, ",") {
		c := strings.ToLower(strings.TrimSpace(raw))
		if c == "" {
			continue
		}
		if _, dup := seen[c]; dup {
			continue
		}
		seen[c] = struct{}{}
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("--components is empty")
	}
	sort.Strings(out)
	return out, nil
}

// BinaryListCmd implements `w17ctl target binary list`.
type BinaryListCmd struct {
	LockPath string `name:"lock" placeholder:"PATH" default:"w17/lock.yaml" help:"Path to the lock file."`
	Console  string `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console (owns the lock). Optional — falls back to the binary's compile-time default."`
}

func (c *BinaryListCmd) Run() error {
	view, err := core.DescribeLockAt("binary list", c.Console, c.LockPath)
	if err != nil {
		return err
	}
	bins := view.GetBinaries()
	if len(bins) == 0 {
		fmt.Fprintln(core.Stdout, "no composed binaries declared (every component ships standalone)")
		return nil
	}
	for _, b := range bins {
		fmt.Fprintf(core.Stdout, "%s → %s [%s]\n",
			b.GetDomain(), b.GetName(), strings.Join(b.GetComponents(), ", "))
	}
	return nil
}

// BinaryDecomposeCmd implements `w17ctl target binary decompose <domain>`.
type BinaryDecomposeCmd struct {
	Domain   string `arg:"" help:"Domain whose composed-binary declaration to remove (revert to standalone)."`
	LockPath string `name:"lock" placeholder:"PATH" default:"w17/lock.yaml" help:"Path to the lock file."`
	Console  string `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console (owns the lock). Optional — falls back to the binary's compile-time default."`
}

func (c *BinaryDecomposeCmd) Run() error {
	if err := core.EditLockOnDisk("binary decompose", c.Console, c.LockPath, &codegenpb.LockEditIntent{
		Intent: &codegenpb.LockEditIntent_DecomposeBinary{
			DecomposeBinary: &codegenpb.DecomposeBinaryIntent{Domain: c.Domain},
		},
	}); err != nil {
		return err
	}
	fmt.Fprintf(core.Stdout, "binary decompose: %s reverted to standalone components (%s)\n", c.Domain, c.LockPath)
	return nil
}
