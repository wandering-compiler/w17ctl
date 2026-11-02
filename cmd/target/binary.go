package target

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"google.golang.org/protobuf/proto"

	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/lockfile"
	codegenpb "github.com/wandering-compiler/sdk/go/pb/w17compiler"
	"github.com/wandering-compiler/sdk/go/tooling/composefacts"
)

// BinaryCmd is the parent of `w17ctl target binary <leaf>` — compose /
// list / decompose. It manages the lock's `binaries[]` surface
// (binary-composition feature): folding several of a domain's
// generated components into ONE binary instead of the default
// one-binary-per-component layout.
//
// The console owns the lock. compose / decompose go
// through the console's EditLock (which validates the combo, checks the
// composition against the attached proto tree, and re-signs);
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
	Components string `name:"components" placeholder:"LIST" required:"" help:"Comma-separated component set. gateway is required: the domain's gateway, or its private one when it has no other. gateway,admin folds admin into the gateway; gateway,storage[,business] (±admin ±eventbus) folds the full tier stack into one <domain>-server with direct in-process tier calls — the only fold for a domain whose only gateway is private."`
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

	// The console validates the combo (ValidateComponentCombo) and, with the
	// proto tree attached, runs codegen's own composition check over it before
	// it re-signs — so a composition the next codegen would refuse (a domain
	// with no gateway surface, an admin fold with no admin) is refused HERE,
	// with codegen's message, instead of landing in a signed lock.
	files, attached, err := composeProto(c.LockPath)
	if err != nil {
		return fmt.Errorf("binary compose: %w", err)
	}
	intent := &codegenpb.LockEditIntent{
		Intent: &codegenpb.LockEditIntent_ComposeBinary{
			ComposeBinary: &codegenpb.ComposeBinaryIntent{
				Domain: c.Domain, Components: components, Files: files, ProtoAttached: attached,
			},
		},
	}
	// The size is decided here, before anything is sent, and never from a
	// server error. The console also answers ResourceExhausted for its rate
	// limits, and treating that as "too large" would downgrade the check. A
	// tree too large to send is refused rather than sent without its check.
	if err := refuseOversizedCompose(c.LockPath, intent, len(files)); err != nil {
		return err
	}
	if err := core.EditLockOnDisk("binary compose", c.Console, c.LockPath, intent); err != nil {
		return err
	}
	fmt.Fprintf(core.Stdout, "binary compose: %s [%s] → %s\n",
		c.Domain, strings.Join(components, ", "), c.LockPath)
	return nil
}

// maxComposeRequest is what one compose edit may weigh: the console's 4 MiB
// gRPC receive limit, minus a margin for the request envelope and for the
// lock growing between this measurement and the send.
const maxComposeRequest = 4<<20 - 256<<10

// refuseOversizedCompose refuses, before sending, a compose edit the console
// could not receive. The edit carries each domain file's markers only (a few
// bytes per file), so only a project with tens of thousands of domain files
// gets here. It is refused rather than sent without the tree, because without
// the tree the console would sign after checking the component names alone.
func refuseOversizedCompose(lockPath string, intent *codegenpb.LockEditIntent, nfiles int) error {
	lockSize := 0
	if info, err := os.Stat(lockPath); err == nil {
		lockSize = int(info.Size())
	}
	size := proto.Size(&codegenpb.EditLockRequest{Intent: intent}) + lockSize
	if size <= maxComposeRequest {
		return nil
	}
	return fmt.Errorf("binary compose: the edit would be %d bytes (the lock plus the proto markers of %d domain file(s)), "+
		"more than the console accepts in one message (%d) — nothing was sent and the lock is unchanged; "+
		"the composition cannot be checked against this proto tree in one edit", size, nfiles, maxComposeRequest)
}

// composeProto reads the domain proto files the console checks a composition
// against, for the project whose lock is lockPath (`<root>/w17/lock.yaml`).
// It takes every `.proto` under `<proto dir>/domains/`, proto-root-relative.
// Only the domains/ tree is read: the surfaces a composition needs live there,
// and a vendored plugin tree carries Go sources the check never reads.
//
// Each file goes as its composefacts.Extract, the markers the check matches,
// and not as its full text. A file with no marker contributes nothing to the
// check and is left out. This keeps the edit one small gRPC message, where a
// large project's full domain protos would exceed the console's receive limit.
//
// attached is false only when the lock is not inside a project layout, so no
// proto tree can be located; the console then checks the component
// combination alone and codegen refuses the rest, as before. A project with
// no proto tree yet is attached EMPTY: it declares no surface, and composing
// it is refused with the message codegen would print.
func composeProto(lockPath string) ([]*codegenpb.ProtoFile, bool, error) {
	abs, err := filepath.Abs(lockPath)
	if err != nil {
		return nil, false, err
	}
	if filepath.Base(filepath.Dir(abs)) != "w17" {
		return nil, false, nil
	}
	root := filepath.Dir(filepath.Dir(abs))
	protoDir := "proto"
	if lk, err := lockfile.Load(abs); err == nil {
		if d := strings.Trim(lk.GeneratedCode.ProtoDir, "/"); d != "" {
			protoDir = d
		}
	}
	if _, err := os.Stat(filepath.Join(root, protoDir)); os.IsNotExist(err) {
		return nil, true, nil
	}
	all, err := core.ReadProtoTree(root, protoDir)
	if err != nil {
		return nil, false, fmt.Errorf("read the proto tree the composition is checked against: %w", err)
	}
	var files []*codegenpb.ProtoFile
	for _, f := range all {
		if !strings.HasPrefix(filepath.ToSlash(f.GetFilename()), "domains/") {
			continue
		}
		if facts := composefacts.Extract(f.GetContents()); facts != nil {
			files = append(files, &codegenpb.ProtoFile{Filename: f.GetFilename(), Contents: facts})
		}
	}
	return files, true, nil
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
