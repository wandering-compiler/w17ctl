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
	fmt.Fprintf(core.Stdout, "business add: %s → %s-business (RegisterBusiness=%s, %d env) → %s\n",
		c.Domain, c.Domain, strings.Trim(c.Register, "/"), len(c.Env), c.LockPath)
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
