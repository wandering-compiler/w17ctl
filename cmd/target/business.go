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
// The console owns the lock. add goes through the console's
// EditLock (which parses + validates the env specs, rejects a duplicate
// domain, and re-signs); list reads through DescribeLock.
type BusinessCmd struct {
	Add    BusinessAddCmd    `cmd:"" help:"Append a business_bundles[] entry so codegen emits the domain's <domain>-business bundle (the facade tier the project's RegisterBusiness plugs into). Re-signs on save."`
	Set    BusinessSetCmd    `cmd:"" help:"Rewrite an existing business_bundles[] entry — the way back from a register path typed wrong (add refuses a second entry for the domain)."`
	List   BusinessListCmd   `cmd:"" help:"List the business_bundles[] entries declared in the lock."`
	Remove BusinessRemoveCmd `cmd:"" help:"Drop a domain's business_bundles[] entry. The generated bundle and your RegisterBusiness package stay on disk. Re-signs on save."`
}

// BusinessRemoveCmd implements `w17ctl target business remove --domain D`.
type BusinessRemoveCmd struct {
	LockPath string `name:"lock" placeholder:"PATH" default:"w17/lock.yaml" help:"Path to the lock file."`
	Console  string `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console (owns the lock). Optional — falls back to the bindings the project was initialised against."`

	Domain string `name:"domain" required:"" help:"Domain whose business_bundles[] entry to drop, as 'target business list' prints it."`
}

func (c *BusinessRemoveCmd) Run() error {
	if err := core.EditLockOnDisk("business remove", c.Console, c.LockPath, &codegenpb.LockEditIntent{
		Intent: &codegenpb.LockEditIntent_RemoveBusinessBundle{
			RemoveBusinessBundle: &codegenpb.RemoveBusinessBundleIntent{Domain: c.Domain},
		},
	}); err != nil {
		return err
	}
	fmt.Fprintf(core.Stdout, "business remove: %s no longer gets a business bundle (%s) — the generated bundle and your RegisterBusiness package stay on disk, yours to delete\n",
		c.Domain, c.LockPath)
	return nil
}

// BusinessAddCmd implements `w17ctl target business add`. Domain + register are
// required; env is an optional repeatable declaration of the bundle's
// env-var surface (parsed + validated server-side).
type BusinessAddCmd struct {
	LockPath string `name:"lock" placeholder:"PATH" default:"w17/lock.yaml" help:"Path to the lock file."`
	Console  string `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console (owns the lock). Optional — falls back to the binary's compile-time default."`

	Domain   string   `name:"domain" required:"" help:"Domain whose business-tier facade services this bundle hosts (matches proto/domains/<domain>/.../business/)."`
	Register string   `name:"register" required:"" placeholder:"PATH" help:"Go: module-relative import path of the package exposing RegisterBusiness(cfg, registry, clients) (e.g. domains/app/modules/<module>/business). Python: project-relative directory of the package whose w17.py defines register_business(cfg, registry, clients) (e.g. business/app). TypeScript: project-relative directory whose w17.ts exports registerBusiness(cfg, registry, clients) (e.g. business/app). Rust: project-relative directory of the crate whose src/main.rs hands register_business(cfg, registry, clients) to w17_business::main! (e.g. business/app); its last segment is the crate's package name."`
	Env      []string `name:"env" placeholder:"SPEC" help:"Declared env var, repeatable. Format name[:type[:default[:description]]] — type is string|bool|int|secret (default string). 'secret' types it secret.String (redacting, OTel-excluded, → .secrets.example, no default). Note: a default containing ':' (e.g. a URL) needs hand-editing afterwards."`
	Language string   `name:"language" placeholder:"LANG" default:"go" help:"Language the business tier is written in: go (default), python, typescript or rust. A python, typescript or rust tier is generated as data (w17-business.json + descriptors.binpb) and runs as its own container on that language's business SDK, beside a generated Go companion that serves it the egress clients and file stores (folded into the composed binary when the domain has one)."`
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
				Language: c.Language,
			},
		},
	}); err != nil {
		return err
	}
	reg := strings.Trim(c.Register, "/")
	if strings.EqualFold(strings.TrimSpace(c.Language), "python") {
		// The same warning as the Go branch below, for the Python contract:
		// the directory is the author's, and nothing creates it.
		fmt.Fprintf(core.Stdout, "business add: %s → %s-business (python, register_business in %s/w17.py, %d env) → %s\n",
			c.Domain, c.Domain, reg, len(c.Env), c.LockPath)
		fmt.Fprintf(core.Stdout, "\n  Next: write %s/w17.py — it is YOUR code, not generated.\n", reg)
		fmt.Fprintf(core.Stdout, "  The bundle's image copies %s and the Python business SDK calls:\n", reg)
		fmt.Fprintf(core.Stdout, "      def register_business(cfg, registry, clients) -> None\n")
		fmt.Fprintf(core.Stdout, "  A requirements.txt beside it is installed into the image. `w17ctl codegen`\n")
		fmt.Fprintf(core.Stdout, "  writes the manifest (w17-business.json) the SDK reads at boot.\n")
		return nil
	}
	if strings.EqualFold(strings.TrimSpace(c.Language), "typescript") {
		// The TypeScript contract: Node 24 runs w17.ts as it is (type
		// stripping, no build step), and the SDK is the image's, never a
		// runtime dependency of the package.
		fmt.Fprintf(core.Stdout, "business add: %s → %s-business (typescript, registerBusiness in %s/w17.ts, %d env) → %s\n",
			c.Domain, c.Domain, reg, len(c.Env), c.LockPath)
		fmt.Fprintf(core.Stdout, "\n  Next: write %s/w17.ts — it is YOUR code, not generated.\n", reg)
		fmt.Fprintf(core.Stdout, "  The bundle's image copies %s and the TypeScript business SDK calls:\n", reg)
		fmt.Fprintf(core.Stdout, "      export function registerBusiness(cfg, registry, clients): void\n")
		fmt.Fprintf(core.Stdout, "  Node runs it with type stripping: erasable syntax only (no enum, no namespace),\n")
		fmt.Fprintf(core.Stdout, "  and relative imports name the .ts file. A package.json beside it is installed\n")
		fmt.Fprintf(core.Stdout, "  (npm ci --omit=dev). Leave @w17/business out of it: the image provides it, and\n")
		fmt.Fprintf(core.Stdout, "  a registry version fails the build even under devDependencies (for editor types,\n")
		fmt.Fprintf(core.Stdout, "  only a file: link to a local checkout's ts/business works).\n")
		fmt.Fprintf(core.Stdout, "  `w17ctl codegen` writes the manifest (w17-business.json) the SDK reads at boot.\n")
		return nil
	}
	if strings.EqualFold(strings.TrimSpace(c.Language), "rust") {
		// The Rust contract: the crate is the binary, its build.rs compiles
		// the services from the generated descriptor set, and the SDK is a
		// git dependency at the project's tag, which the image checks.
		pkg := reg[strings.LastIndexByte(reg, '/')+1:]
		fmt.Fprintf(core.Stdout, "business add: %s → %s-business (rust, crate %s in %s, %d env) → %s\n",
			c.Domain, c.Domain, pkg, reg, len(c.Env), c.LockPath)
		fmt.Fprintf(core.Stdout, "\n  Next: write the crate in %s — it is YOUR code, not generated:\n", reg)
		fmt.Fprintf(core.Stdout, "      Cargo.toml   [package] name = %q, its own [workspace], and\n", pkg)
		fmt.Fprintf(core.Stdout, "                   w17-business (+ w17-business-build under [build-dependencies])\n")
		fmt.Fprintf(core.Stdout, "                   from https://github.com/wandering-compiler/sdk at tag rs/<sdk_version>\n")
		fmt.Fprintf(core.Stdout, "      build.rs     w17_business_build::compile()\n")
		fmt.Fprintf(core.Stdout, "      src/main.rs  w17_business::main!(register_business);\n")
		fmt.Fprintf(core.Stdout, "  Commit Cargo.lock: the image builds the crate --locked and checks that it takes\n")
		fmt.Fprintf(core.Stdout, "  the SDK at the project's tag. `w17ctl codegen` writes the manifest\n")
		fmt.Fprintf(core.Stdout, "  (w17-business.json) and the descriptor set the crate compiles from.\n")
		return nil
	}
	fmt.Fprintf(core.Stdout, "business add: %s → %s-business (RegisterBusiness=%s, %d env) → %s\n",
		c.Domain, c.Domain, reg, len(c.Env), c.LockPath)

	// ⚠️ SAY WHOSE CODE THAT PATH IS. The bundle codegen renders IMPORTS the
	// register package and calls `RegisterBusiness` in it — and nothing
	// creates it, because it is the author's own service code. So an adopter
	// who runs `add` and then `codegen` gets two commands that both succeed
	// and a tree that does not build, with an error naming an import path
	// rather than a decision they made two steps earlier.
	//
	// That is the shape: a path that only fails several commands later. The path is not wrong and codegen is not wrong;
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
	Register string   `name:"register" placeholder:"PATH" help:"New register: for Go the module-RELATIVE import path of the package exposing RegisterBusiness, for Python the project-relative directory holding w17.py, for TypeScript the one holding w17.ts, for Rust the crate's directory. Empty leaves the current one (required when --language switches to or from go)."`
	Env      []string `name:"env" placeholder:"SPEC" help:"Declared env var, repeatable. Passing any --env REPLACES the whole list; pass --clear-env to empty it. Format name[:type[:default[:description]]]."`
	ClearEnv bool     `name:"clear-env" help:"Set the env list to empty. Without this, omitting --env leaves the current list alone — the two are different intents and a list that cannot be cleared is the same one-way door this command exists to open."`
	Language string   `name:"language" placeholder:"LANG" help:"New business language: go, python, typescript or rust. Empty leaves the current one. A change to or from go needs --register too: go reads it as an import path, the others as a directory."`
}

func (c *BusinessSetCmd) Run() error {
	if strings.TrimSpace(c.Domain) == "" {
		return fmt.Errorf("business set: --domain required")
	}
	replaceEnv := len(c.Env) > 0 || c.ClearEnv
	if strings.TrimSpace(c.Register) == "" && !replaceEnv && strings.TrimSpace(c.Language) == "" {
		return fmt.Errorf("business set: nothing to set — pass --register, --env, --clear-env, or --language")
	}
	if err := core.EditLockOnDisk("business set", c.Console, c.LockPath, &codegenpb.LockEditIntent{
		Intent: &codegenpb.LockEditIntent_SetBusinessBundle{
			SetBusinessBundle: &codegenpb.SetBusinessBundleIntent{
				Domain:     c.Domain,
				Register:   c.Register,
				Env:        c.Env,
				ReplaceEnv: replaceEnv,
				Language:   c.Language,
			},
		},
	}); err != nil {
		return err
	}
	if lang := strings.TrimSpace(c.Language); lang != "" {
		fmt.Fprintf(core.Stdout, "business set: %s → language %s → %s\n", c.Domain, strings.ToLower(lang), c.LockPath)
	}
	switch {
	case strings.TrimSpace(c.Register) != "" && replaceEnv:
		fmt.Fprintf(core.Stdout, "business set: %s → RegisterBusiness=%s, %d env → %s\n",
			c.Domain, strings.Trim(c.Register, "/"), len(c.Env), c.LockPath)
	case strings.TrimSpace(c.Register) != "":
		fmt.Fprintf(core.Stdout, "business set: %s → RegisterBusiness=%s → %s\n",
			c.Domain, strings.Trim(c.Register, "/"), c.LockPath)
	case replaceEnv:
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
		if lang := b.GetLanguage(); lang != "" && lang != "go" {
			fmt.Fprintf(core.Stdout, "%s → %s-business (%s, register=%s, %d env)\n",
				b.GetDomain(), b.GetDomain(), lang, b.GetRegister(), b.GetEnvCount())
			continue
		}
		fmt.Fprintf(core.Stdout, "%s → %s-business (RegisterBusiness=%s, %d env)\n",
			b.GetDomain(), b.GetDomain(), b.GetRegister(), b.GetEnvCount())
	}
	return nil
}
