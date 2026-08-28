package target

import (
	"fmt"
	"os"
	"strings"

	codegen "github.com/wandering-compiler/w17ctl/internal/codegen"
	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/lockfile"
	"github.com/wandering-compiler/w17ctl/internal/prompter"
	codegenpb "github.com/wandering-compiler/sdk/go/pb/w17compiler"
)

// ClientCmd is the parent of `w17ctl target client <leaf>` commands —
// add / list / generate.
//
// Block 2 §8.2: the console owns the lock. The add wizard appends one entry to
// `generated_code.clients[]` (the FE client codegen output declarations) via
// the console's EditLock (which maps the framework/language/wire tokens to the
// lock enums + re-signs); list reads through DescribeLock. (generate is codegen
// orchestration — it still loads the lock for the console address + proto dir,
// migrated with the rest of the codegen path.)
type ClientCmd struct {
	Add      ClientAddCmd      `cmd:"" help:"Wizard — append a new generated-client entry to the project's lock. Prompts for framework, language, output_root, and wire format."`
	List     ClientListCmd     `cmd:"" help:"List the generated-client entries declared in the lock."`
	Generate ClientGenerateCmd `cmd:"" help:"Generate the FE client tree(s) declared in generated_code.clients[] (formerly the standalone wcclient binary)."`
}

// ClientGenerateCmd implements `w17ctl target client generate` — the FE
// client codegen formerly shipped as the standalone `wcclient` binary. As
// a thin client it uploads the proto set + the signed lock to the
// console's GenerateClient RPC, which runs the generator server-side, and
// writes the returned trees under each client's output_root (thin-client
// refactor Step 3a).
type ClientGenerateCmd struct {
	Lock        string `name:"lock" placeholder:"PATH" default:"w17/lock.yaml" help:"Path to the lock file (relative paths resolve from cwd)."`
	ProtoRoot   string `name:"proto-root" placeholder:"DIR" help:"Directory to walk for .proto sources (relative to --project-root; default the lock's proto dir)."`
	ProjectRoot string `name:"project-root" placeholder:"DIR" help:"Project root for resolving output_root in lock.clients[] (default cwd)."`
	Console     string `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console CodegenService. Optional — falls back to console_addr in w17/lock.yaml, then the binary's compile-time default."`
}

func (c *ClientGenerateCmd) Run() error {
	root := c.ProjectRoot
	if root == "" {
		wd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("getwd: %w", err)
		}
		root = wd
	}
	lockPath := c.Lock
	if lockPath == "" {
		lockPath = "w17/lock.yaml"
	}
	protoDir := c.ProtoRoot
	if protoDir == "" {
		// Default proto dir from the console's lock projection (§8.2 — the
		// client holds no lock types).
		lockBytes, rerr := os.ReadFile(lockPath)
		if rerr != nil {
			return fmt.Errorf("client generate: read lock: %w", rerr)
		}
		view, derr := core.DescribeLock(c.Console, lockBytes)
		if derr != nil {
			return fmt.Errorf("client generate: %w", derr)
		}
		protoDir = view.GetProtoDir()
	}
	return codegen.GenerateClientViaConsole(root, c.Console, protoDir, lockPath)
}

// ClientAddCmd implements `w17ctl target client add`. Wizard prompts:
//
//  1. Framework — pick from CORE / REACT / REACT_NATIVE / VUE.
//  2. Language — pick from TYPESCRIPT / JAVASCRIPT.
//  3. Output root — project-relative path the wcclient binary
//     writes generated files under (convention: `ui/w17/web-client`
//     for web, `ui/w17/mobile-client` for RN).
//  4. Wire format — pick from JSON / PROTOBUF / UNSPECIFIED
//     (UNSPECIFIED defaults to JSON at codegen time, ack'd
//     historical default).
//
// Refuses if the output_root is already claimed by another
// clients[] entry (the validator on lock save enforces
// uniqueness; wizard surfaces the diag earlier with a clearer
// message).
type ClientAddCmd struct {
	LockPath string `name:"lock" placeholder:"PATH" default:"w17/lock.yaml" help:"Path to the lock file."`
	Console  string `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console (owns the lock). Optional — falls back to the binary's compile-time default."`

	// Non-interactive overrides — when every flag is set the
	// wizard skips prompting + runs straight to validation +
	// save. Useful for CI / scripts that already know the answers.
	Framework  string `name:"framework"  placeholder:"NAME" help:"Skip prompt for framework. One of: core, react, react-native, vue."`
	Language   string `name:"language"   placeholder:"NAME" help:"Skip prompt for language. One of: typescript, javascript."`
	OutputRoot string `name:"output-root" placeholder:"PATH" help:"Skip prompt for output root."`
	Wire       string `name:"wire"        placeholder:"NAME" help:"Skip prompt for wire format. One of: json, protobuf, unspecified."`
}

// clientFrameworks / clientLanguages / clientWires drive the
// wizard's selection lists. Order matches display order;
// `core` first because it's the framework-agnostic default
// every project starts with.
var clientFrameworks = []string{"core", "react", "react-native", "vue"}
var clientLanguages = []string{"typescript", "javascript"}
var clientWires = []string{"unspecified", "json", "protobuf"}

// parseClientFramework / parseClientLanguage / parseClientWire validate the
// wizard's input (alias-tolerant) and return the canonical token the EditLock
// intent carries; the console maps the token to the lock enum. They no longer
// reference lockpb (Block 2 §8.2 — the client holds no lock types).
func parseClientFramework(s string) (string, error) {
	switch strings.ToLower(s) {
	case "core":
		return "core", nil
	case "react":
		return "react", nil
	case "react-native", "react_native", "rn":
		return "react-native", nil
	case "vue":
		return "vue", nil
	}
	return "", fmt.Errorf("unknown framework %q (one of: %s)", s, strings.Join(clientFrameworks, ", "))
}

func parseClientLanguage(s string) (string, error) {
	switch strings.ToLower(s) {
	case "typescript", "ts":
		return "typescript", nil
	case "javascript", "js":
		return "javascript", nil
	}
	return "", fmt.Errorf("unknown language %q (one of: %s)", s, strings.Join(clientLanguages, ", "))
}

func parseClientWire(s string) (string, error) {
	switch strings.ToLower(s) {
	case "", "unspecified":
		return "unspecified", nil
	case "json":
		return "json", nil
	case "protobuf", "proto", "pb":
		return "pb", nil
	}
	return "", fmt.Errorf("unknown wire %q (one of: %s)", s, strings.Join(clientWires, ", "))
}

func (c *ClientAddCmd) Run() error {
	// Q58-console-1: serialise the read → EditLock → write below against
	// concurrent lock-mutating runs so the second write can't clobber the
	// first's edit. Held until this function returns.
	release, lockErr := lockfile.ForUpdate(c.LockPath)
	if lockErr != nil {
		return fmt.Errorf("client add: lock for update: %w", lockErr)
	}
	defer release()

	lockBytes, err := os.ReadFile(c.LockPath)
	if err != nil {
		return fmt.Errorf("client add: read lock %s: %w", c.LockPath, err)
	}
	view, err := core.DescribeLock(c.Console, lockBytes)
	if err != nil {
		return fmt.Errorf("client add: %w", err)
	}

	prompter := prompter.NewStdinPrompter()
	framework, language, outputRoot, wire, err := runClientAddWizard(prompter, view.GetClients(), c)
	if err != nil {
		return err
	}

	newBytes, err := core.EditLock(c.Console, lockBytes, &codegenpb.LockEditIntent{
		Intent: &codegenpb.LockEditIntent_AddClientStub{
			AddClientStub: &codegenpb.AddClientStubIntent{
				Framework:  framework,
				Language:   language,
				OutputRoot: outputRoot,
				Wire:       wire,
			},
		},
	})
	if err != nil {
		return fmt.Errorf("client add: %w", err)
	}
	if err := lockfile.WriteAtomic(c.LockPath, newBytes, 0o644); err != nil {
		return fmt.Errorf("client add: write lock: %w", err)
	}

	fmt.Fprintf(core.Stdout, "client add: %s/%s at %s (wire=%s) → %s\n",
		framework, language, outputRoot, wire, c.LockPath)
	return nil
}

// runClientAddWizard is the interactive wizard body, pulled out of Run so the
// test harness can drive it without disk IO. When every override flag is set
// the prompter is bypassed (CI / scripts). It returns the canonical tokens the
// EditLock intent carries; the console validates them authoritatively. `existing`
// is the lock's current clients[] (via DescribeLock) for the early
// duplicate-output_root diag.
func runClientAddWizard(p prompter.Prompter, existing []*codegenpb.LockClientStub, c *ClientAddCmd) (
	framework string,
	language string,
	outputRoot string,
	wire string,
	err error,
) {
	fmt.Fprintln(core.Stdout, "Adding a new generated-client entry to the project...")
	fmt.Fprintln(core.Stdout)

	// Framework
	frameworkStr := c.Framework
	if frameworkStr == "" {
		frameworkStr, err = p.Select("Framework", clientFrameworks, "core")
		if err != nil {
			return "", "", "", "", err
		}
	}
	framework, err = parseClientFramework(frameworkStr)
	if err != nil {
		return "", "", "", "", err
	}

	// Language
	languageStr := c.Language
	if languageStr == "" {
		languageStr, err = p.Select("Language", clientLanguages, "typescript")
		if err != nil {
			return "", "", "", "", err
		}
	}
	language, err = parseClientLanguage(languageStr)
	if err != nil {
		return "", "", "", "", err
	}

	// Output root
	outputRoot = c.OutputRoot
	if outputRoot == "" {
		defaultRoot := "ui/w17/web-client"
		if framework == "react-native" {
			defaultRoot = "ui/w17/mobile-client"
		}
		outputRoot, err = p.Text("Output root (project-relative path)", defaultRoot)
		if err != nil {
			return "", "", "", "", err
		}
	}
	if outputRoot == "" {
		return "", "", "", "", fmt.Errorf("output root is required")
	}
	for _, e := range existing {
		if e.GetOutputRoot() == outputRoot {
			return "", "", "", "", fmt.Errorf("client add: output_root %q already claimed by another clients[] entry", outputRoot)
		}
	}

	// Wire
	wireStr := c.Wire
	if wireStr == "" {
		wireStr, err = p.Select("Wire format", clientWires, "unspecified")
		if err != nil {
			return "", "", "", "", err
		}
	}
	wire, err = parseClientWire(wireStr)
	if err != nil {
		return "", "", "", "", err
	}
	return framework, language, outputRoot, wire, nil
}

// ClientListCmd implements `w17ctl target client list`. One client
// per line, framework + language + output root + wire format
// surfaced in the friendly shape.
type ClientListCmd struct {
	LockPath string `name:"lock" placeholder:"PATH" default:"w17/lock.yaml" help:"Path to the lock file."`
	Console  string `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console (owns the lock). Optional — falls back to the binary's compile-time default."`
}

func (c *ClientListCmd) Run() error {
	view, err := core.DescribeLockAt("client list", c.Console, c.LockPath)
	if err != nil {
		return err
	}
	clients := view.GetClients()
	if len(clients) == 0 {
		fmt.Fprintln(core.Stdout, "no generated-client entries declared in the lock")
		return nil
	}
	for _, cl := range clients {
		fmt.Fprintf(core.Stdout, "%s/%s at %s (wire=%s)\n",
			cl.GetFramework(), cl.GetLanguage(), cl.GetOutputRoot(), cl.GetWire())
	}
	return nil
}
