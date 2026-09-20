package target

import (
	"fmt"
	"strings"

	"github.com/wandering-compiler/w17ctl/internal/core"
	codegenpb "github.com/wandering-compiler/sdk/go/pb/w17compiler"
)

// BusinessCmd is the parent of `w17ctl target business <leaf>` — add / list.
// It closes the same lock-edit ergonomic gap that `grpc-client add`
// closed for grpc_clients[], but for the `generated_code.business_bundles[]`
// declarations codegen reads to emit a `<domain>-business` bundle: the
// generated runtime + the plugin-parallel gen package (EnvConfig /
// HandlerRegistry / ClientSet) the project's hand-written RegisterBusiness
// consumes.
//
// Block 2 §8.2: the console owns the lock. add goes through the console's
// EditLock (which parses + validates the env specs, rejects a duplicate
// domain, and re-signs); list reads through DescribeLock.
type BusinessCmd struct {
	Add  BusinessAddCmd  `cmd:"" help:"Append a business_bundles[] entry so codegen emits the domain's <domain>-business bundle (the facade tier the project's RegisterBusiness plugs into). Re-signs on save."`
	Set  BusinessSetCmd  `cmd:"" help:"Rewrite an existing business_bundles[] entry — the way back from a register path typed wrong (add refuses a second entry for the domain)."`
	List BusinessListCmd `cmd:"" help:"List the business_bundles[] entries declared in the lock."`
}

// BusinessAddCmd implements `w17ctl target business add`. Domain + register are
// required; env is an optional repeatable declaration of the bundle's
// env-var surface (parsed + validated server-side).
type BusinessAddCmd struct {
	LockPath string `name:"lock" placeholder:"PATH" default:"w17/lock.yaml" help:"Path to the lock file."`
	Console  string `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console (owns the lock). Optional — falls back to the binary's compile-time default."`

	Domain   string   `name:"domain" required:"" help:"Domain whose business-tier facade services this bundle hosts (matches proto/domains/<domain>/.../business/)."`
	Register string   `name:"register" required:"" placeholder:"IMPORT" help:"Go-module-relative import path of the package exposing RegisterBusiness(cfg, registry, clients) (e.g. domains/app/modules/<module>/business)."`
	Env      []string `name:"env" placeholder:"SPEC" help:"Declared env var, repeatable. Format name[:type[:default[:description]]] — type is string|bool|int|secret (default string). 'secret' types it secret.String (redacting, OTel-excluded, → .secrets.example, no default). Note: a default containing ':' (e.g. a URL) needs hand-editing afterwards."`
}

func (c *BusinessAddCmd) Run() error {
	// Cheap pre-flight (the console re-validates authoritatively).
	if strings.TrimSpace(c.Domain) == "" {
		return fmt.Errorf("business add: --domain required")
	}
	if strings.TrimSpace(c.Register) == "" {
		return fmt.Errorf("business add: --register required (the RegisterBusiness package import path)")
	}
	if err := core.EditLockOnDisk("business add", c.Console, c.LockPath, &codegenpb.LockEditIntent{
		Intent: &codegenpb.LockEditIntent_AddBusinessBundle{
			AddBusinessBundle: &codegenpb.AddBusinessBundleIntent{
				Domain:   c.Domain,
				Register: c.Register,
				Env:      c.Env,
			},
		},
	}); err != nil {
		return err
	}
	reg := strings.Trim(c.Register, "/")
	fmt.Fprintf(core.Stdout, "business add: %s → %s-business (RegisterBusiness=%s, %d env) → %s\n",
		c.Domain, c.Domain, reg, len(c.Env), c.LockPath)

	// ⚠️ SAY WHOSE CODE THAT PATH IS. The bundle codegen renders IMPORTS the
	// register package and calls `RegisterBusiness` in it — and nothing
	// creates it, because it is the author's own service code. So an adopter
	// who runs `add` and then `codegen` gets two commands that both succeed
	// and a tree that does not build, with an error naming an import path
	// rather than a decision they made two steps earlier.
	//
	// That is the shape a consumer reported (#24): a path that only fails
	// several commands later. The path is not wrong and codegen is not wrong;
	// what was missing is that `add` never said the package is theirs to
	// write.
	fmt.Fprintf(core.Stdout, "\n  Next: write %s — it is YOUR code, not generated.\n", reg)
	fmt.Fprintf(core.Stdout, "  The bundle imports it and calls:\n")
	fmt.Fprintf(core.Stdout, "      func RegisterBusiness(cfg *EnvConfig, registry HandlerRegistry, clients ClientSet) error\n")
	fmt.Fprintf(core.Stdout, "  (the three types come from the generated gen package for %s).\n", c.Domain)
	fmt.Fprintf(core.Stdout, "  Until it exists, `w17ctl codegen` succeeds and the bundle does not compile.\n")
	return nil
}

// BusinessSetCmd implements `w17ctl target business set` — the missing half
// of `add`.
//
// `add` allows one entry per domain and nothing could edit it, so the first
// `register` a project typed was final: a consumer who passed a full import
// path where a module-relative one belongs had no way back short of
// reinitialising the project. They named it as one of a family — decisions
// taken at `init` that `init` gives no way to revise — of which `pb_stubs`
// was the previous one.
type BusinessSetCmd struct {
	LockPath string `name:"lock" placeholder:"PATH" default:"w17/lock.yaml" help:"Path to the lock file."`
	Console  string `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console (owns the lock). Optional — falls back to the bindings the project was initialised against."`

	Domain   string   `name:"domain" required:"" help:"Domain whose entry to rewrite. Must already exist — 'target business add' creates one."`
	Register string   `name:"register" placeholder:"IMPORT" help:"New Go-module-RELATIVE import path of the package exposing RegisterBusiness. Empty leaves the current one."`
	Env      []string `name:"env" placeholder:"SPEC" help:"Declared env var, repeatable. Passing any --env REPLACES the whole list; pass --clear-env to empty it. Format name[:type[:default[:description]]]."`
	ClearEnv bool     `name:"clear-env" help:"Set the env list to empty. Without this, omitting --env leaves the current list alone — the two are different intents and a list that cannot be cleared is the same one-way door this command exists to open."`
}

func (c *BusinessSetCmd) Run() error {
	if strings.TrimSpace(c.Domain) == "" {
		return fmt.Errorf("business set: --domain required")
	}
	replaceEnv := len(c.Env) > 0 || c.ClearEnv
	if strings.TrimSpace(c.Register) == "" && !replaceEnv {
		return fmt.Errorf("business set: nothing to set — pass --register, --env, or --clear-env")
	}
	if err := core.EditLockOnDisk("business set", c.Console, c.LockPath, &codegenpb.LockEditIntent{
		Intent: &codegenpb.LockEditIntent_SetBusinessBundle{
			SetBusinessBundle: &codegenpb.SetBusinessBundleIntent{
				Domain:     c.Domain,
				Register:   c.Register,
				Env:        c.Env,
				ReplaceEnv: replaceEnv,
			},
		},
	}); err != nil {
		return err
	}
	switch {
	case strings.TrimSpace(c.Register) != "" && replaceEnv:
		fmt.Fprintf(core.Stdout, "business set: %s → RegisterBusiness=%s, %d env → %s\n",
			c.Domain, strings.Trim(c.Register, "/"), len(c.Env), c.LockPath)
	case strings.TrimSpace(c.Register) != "":
		fmt.Fprintf(core.Stdout, "business set: %s → RegisterBusiness=%s → %s\n",
			c.Domain, strings.Trim(c.Register, "/"), c.LockPath)
	default:
		fmt.Fprintf(core.Stdout, "business set: %s → %d env → %s\n", c.Domain, len(c.Env), c.LockPath)
	}
	return nil
}

// BusinessListCmd implements `w17ctl target business list`.
type BusinessListCmd struct {
	LockPath string `name:"lock" placeholder:"PATH" default:"w17/lock.yaml" help:"Path to the lock file."`
	Console  string `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console (owns the lock). Optional — falls back to the binary's compile-time default."`
}

func (c *BusinessListCmd) Run() error {
	view, err := core.DescribeLockAt("business list", c.Console, c.LockPath)
	if err != nil {
		return err
	}
	bundles := view.GetBusinessBundles()
	if len(bundles) == 0 {
		fmt.Fprintln(core.Stdout, "no business_bundles[] entries declared in the lock")
		return nil
	}
	for _, b := range bundles {
		fmt.Fprintf(core.Stdout, "%s → %s-business (RegisterBusiness=%s, %d env)\n",
			b.GetDomain(), b.GetDomain(), b.GetRegister(), b.GetEnvCount())
	}
	return nil
}
