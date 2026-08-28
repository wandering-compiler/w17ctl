package initcmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"google.golang.org/grpc/metadata"

	"github.com/wandering-compiler/w17ctl/cmd/connection"
	"github.com/wandering-compiler/w17ctl/internal/authstore"
	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/devconfig"
	"github.com/wandering-compiler/w17ctl/internal/prompter"
	"github.com/wandering-compiler/w17ctl/internal/scaffold"
	codegenpb "github.com/wandering-compiler/sdk/go/pb/w17compiler"
	w17registrypb "github.com/wandering-compiler/sdk/go/pb/w17registry"
	"github.com/wandering-compiler/sdk/go/tooling/certgen"
)

// Wizard prompt defaults. These are UX defaults the operator Enters through;
// the console owns the canonical lock conventions + the CI taxonomy and
// re-applies/validates them when it builds the lock (Block 2 §8.2 — the client
// holds no lock types). ciProviderHint mirrors the console's provider list for
// the prompt text only; the server rejects an unknown provider authoritatively.
const (
	defaultProtoDir     = "proto"
	defaultLanguagesDir = "w17/languages"
	defaultLanguage     = "en"
	ciProviderHint      = "github|gitlab|circleci|azure|bitbucket|jenkins|generic"
)

// Cmd is `w17ctl init` — the bootstrap wizard. Prompts
// the operator for project basics + layout + i18n + password
// + connections, registers the project with the console
// (which assigns a stable project_id), and writes a signed
// `w17/lock.yaml`.
//
// Every prompt has a matching flag so the command runs
// non-interactively when every value is supplied up front;
// otherwise the wizard prompts for missing pieces with
// e2e-project-shaped defaults the operator can Enter
// through.
type Cmd struct {
	LockPath string `name:"lock" placeholder:"PATH" default:"w17/lock.yaml" help:"Path to write the lock file."`
	W17URL   string `name:"w17-url" placeholder:"URL" env:"W17_URL" help:"URL of the console hosting this project. Required at init time so the lock pins it."`
	GoModule string `name:"go-module" placeholder:"PATH" help:"Go module path for the project's hand-written tier (<stubs-root-first-segment>/go.mod). Empty = example.com/<project>. Drives codegen's go_module + every generated bundle's import paths."`

	Name      string `name:"name" placeholder:"NAME" help:"Project name. Empty = ask. Supplying it (with the other flags) makes init fully non-interactive."`
	StubsRoot string `name:"stubs-root" placeholder:"DIR" help:"Go stub tree root (pb + grpc clients). Empty = ask (default \"srcgo/gen\")."`
	Language  string `name:"language" placeholder:"LANG" help:"Pb stubs language. Empty = ask (default \"go\"; only \"go\" supported today)."`

	ProtoDir        string `name:"proto-dir" placeholder:"DIR" help:"Hand-authored proto root. Empty = ask (default \"proto\")."`
	LanguagesDir    string `name:"languages-dir" placeholder:"DIR" help:"Shared gettext catalog tree. Empty = ask (default \"w17/languages\")."`
	Languages       string `name:"languages" placeholder:"CSV" help:"Comma-separated BCP-47 language tags. Empty = ask (default \"en\"). Example: en,cs,pt-BR."`
	E2E             string `name:"e2e" placeholder:"yes|no" help:"Generate e2e tests (REST+MCP). yes|no opts in/out non-interactively; empty = ask (default-yes). The tree path is fixed at \"w17/e2e\"."`
	CI              string `name:"ci" placeholder:"all|none|csv" help:"Generate e2e CI configs under w17/ci/<provider>/. Comma-separated providers (github|gitlab|circleci|azure|bitbucket|jenkins|generic), 'all', or 'none'. Empty = ask (default \"github\")."`
	SkipConnections bool   `name:"skip-connections" help:"Skip the connection-add loop. Operator wires connections later via \"w17ctl connection add\"."`
}

func (c *Cmd) Run() error {
	if _, err := os.Stat(c.LockPath); err == nil {
		return fmt.Errorf("init: %s already exists — refusing to overwrite (each project's lock is unique by design)", c.LockPath)
	}

	prompter := prompter.NewStdinPrompter()
	answers, err := runInitWizard(prompter, initFlags{
		name:            c.Name,
		stubsRoot:       c.StubsRoot,
		language:        c.Language,
		protoDir:        c.ProtoDir,
		languagesDir:    c.LanguagesDir,
		languagesCSV:    c.Languages,
		e2e:             c.E2E,
		ci:              c.CI,
		skipConnections: c.SkipConnections,
	})
	if err != nil {
		return err
	}

	addr := c.W17URL
	if addr == "" {
		// The wizard doesn't prompt for the console URL (it's operator
		// environment, not project state). Prefer the console the user is
		// logged into — `w17ctl login <host>` is the explicit choice, so a
		// fresh `init` targets it — then fall back to the compile-time default.
		addr = core.ActiveInstanceURL()
	}
	if addr == "" {
		addr = core.DefaultConsoleAddr
	}
	if addr == "" {
		return fmt.Errorf("init: no console URL — log in with `w17ctl login <host>`, pass --w17-url, set W17_URL env, or rebuild w17ctl with -ldflags \"-X github.com/MrS1lentcz/wandering-compiler/w17ctl/internal/core.DefaultConsoleAddr=...\"")
	}

	// Which org owns the project. The console scopes the registration to the
	// gateway-verified active org; we select it here (mandatory when the caller
	// belongs to more than one) and send it as the `w17-org` header, which the
	// console validates against the caller's membership.
	orgSlug, err := chooseOrg(prompter)
	if err != nil {
		return err
	}
	// Remember the choice as the active org, so the org this project was
	// created under is the one every LATER command targets. Without this the
	// picker's answer lived for exactly one RPC: a multi-org operator picked
	// here and then had every scoped command refused for want of a scope.
	//
	// Only when no default is set yet — an operator who ran `org use` has
	// stated a preference, and creating one project elsewhere is not a
	// reason to silently move them.
	if err := rememberDefaultOrg(orgSlug); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cl, conn, err := core.DialProjectRegistry(addr)
	if err != nil {
		return fmt.Errorf("init: dial %s: %w", addr, err)
	}
	defer func() { _ = conn.Close() }()

	regCtx := ctx
	if orgSlug != "" {
		regCtx = metadata.AppendToOutgoingContext(ctx, "w17-org", orgSlug)
	}
	resp, err := cl.RegisterProject(regCtx, &w17registrypb.RegisterProjectRequest{Name: answers.projectName})
	if err != nil {
		return fmt.Errorf("init: %w", err)
	}
	projectID := resp.GetProjectId()

	// The console builds + signs the lock from the wizard answers (§8.2 — the
	// client constructs no lockpb and signs nothing). EditLock with empty bytes
	// + a bootstrap intent returns the re-signed lock to write.
	var conns []*codegenpb.LockConnection
	for _, c := range answers.connections {
		conns = append(conns, &codegenpb.LockConnection{Name: c.name, Default: c.markDefault})
	}
	lockBytes, err := core.EditLock(addr, nil, &codegenpb.LockEditIntent{
		Intent: &codegenpb.LockEditIntent_BootstrapLock{
			BootstrapLock: &codegenpb.BootstrapLockIntent{
				ProjectId:    projectID,
				Project:      answers.projectName,
				W17Url:       addr,
				ProtoDir:     answers.protoDir,
				Stubs:        answers.stubsRoot,
				LanguagesDir: answers.languagesDir,
				PbLanguage:   answers.language,
				Languages:    answers.languages,
				E2E:          answers.e2e,
				Ci:           answers.ci,
				Connections:  conns,
			},
		},
	})
	if err != nil {
		return fmt.Errorf("init: %w", err)
	}
	if dir := filepath.Dir(c.LockPath); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("init: mkdir %s: %w", dir, err)
		}
	}
	if err := os.WriteFile(c.LockPath, lockBytes, 0o644); err != nil {
		return fmt.Errorf("init: write lock: %w", err)
	}

	fmt.Fprintf(core.Stdout, "init: project %q registered (id=%s) → %s\n",
		answers.projectName, projectID, c.LockPath)

	// Scaffold the hand-written Go-module skeleton so the very next
	// `w17ctl codegen` finds a module path (readGoModule reads
	// <root>/<stubs-root-seg-1>/go.mod). Without this, codegen fails
	// with the misleading "request.go_module is empty" error that
	// blames the plugin activation. The project root is the lock's
	// grandparent dir (w17/lock.yaml → project root).
	projectRoot := filepath.Dir(filepath.Dir(c.LockPath))
	if projectRoot == "" {
		projectRoot = "."
	}
	goModule := c.GoModule
	if goModule == "" {
		goModule = "example.com/" + answers.projectName
	}
	if err := scaffoldGoModule(projectRoot, goModule, answers.stubsRoot); err != nil {
		return fmt.Errorf("init: scaffold go module: %w", err)
	}

	// Seed the local dev PKI so flipping on the internal-mesh TLS switch
	// (W17_INTERNAL_TLS=on) needs zero operator effort. The material lives
	// next to the lock (w17/certs/dev.local/), is gitignored, and is
	// idempotent — re-init never clobbers it. `w17ctl certs` refills it
	// (or a prod dir) later.
	certsDir := filepath.Join(filepath.Dir(c.LockPath), "certs", "dev.local")
	written, err := certgen.EnsureCerts(certsDir, nil)
	if err != nil {
		return fmt.Errorf("init: generate dev certs: %w", err)
	}
	if len(written) > 0 {
		fmt.Fprintf(core.Stdout, "init: generated dev TLS certs in %s (gitignored)\n", certsDir)
	}

	// Write w17/.gitignore up front so the operator never has to reverse-
	// engineer which parts of the generated w17/ tree are transient/secret
	// (dev snapshots, the dev PKI just seeded above, age keys, per-bundle
	// .env/.secrets) vs. committable. Single source of truth in scaffold;
	// the snapshot + secrets paths ensure the same file on demand.
	if wrote, gerr := scaffold.EnsureW17Gitignore(projectRoot); gerr != nil {
		// Non-fatal: a missing .gitignore is friction, not a broken init.
		fmt.Fprintf(core.Stdout, "init: warning: could not write w17/.gitignore: %v\n", gerr)
	} else if wrote {
		fmt.Fprintf(core.Stdout, "init: wrote %s\n", filepath.Join("w17", ".gitignore"))
	}

	// Register the new project in the dev-machine-local registry so
	// `w17ctl project list` sees it and `stack up` can hand it unique
	// host ports across every installed project. Port allocation is
	// lazy — it happens on the first `stack up` / `project ports`, once
	// codegen has produced the compose files to introspect. Best-effort:
	// a registry-write failure must not fail an otherwise-good init.
	registerNewProject(answers.projectName, projectRoot)

	return nil
}

// chooseOrg selects the organization that will own the new project, returning
// its slug (sent to the console as the `w17-org` header). The orgs come from
// the login-time membership cache (~/.w17/auth.yaml):
//
//   - not logged in       → error (init needs a console + a login).
//   - member of no org     → error (nothing to scope the project to).
//   - member of exactly 1  → that org, no prompt.
//   - member of >1         → MANDATORY choice (a Select with no default, so the
//     operator can't fall through to an implicit org).
//
// rememberDefaultOrg records `slug` as the active instance's default org
// when none is set yet. A no-op for an empty slug, an unknown slug, or an
// instance that already has a default — none of those is an error worth
// failing `init` over, since the registration itself carries the org
// explicitly and has already been decided by the caller.
func rememberDefaultOrg(slug string) error {
	if slug == "" {
		return nil
	}
	st, err := authstore.LoadDefault()
	if err != nil {
		return nil
	}
	inst := st.ActiveInstance()
	if inst == nil || inst.DefaultOrg != "" {
		return nil
	}
	o := inst.Org(slug)
	if o == nil || o.ID == "" {
		return nil
	}
	inst.DefaultOrg = o.ID
	return authstore.SaveDefault(st)
}

func chooseOrg(p prompter.Prompter) (string, error) {
	st, err := authstore.LoadDefault()
	if err != nil {
		return "", err
	}
	inst := st.ActiveInstance()
	if inst == nil {
		return "", fmt.Errorf("init: not logged in — run `w17ctl login <host>` first")
	}
	switch len(inst.Orgs) {
	case 0:
		return "", fmt.Errorf("init: you don't belong to any organization on %s — ask an owner to add you, then run `w17ctl login` again", inst.URL)
	case 1:
		return inst.Orgs[0].Slug, nil
	default:
		slugs := make([]string, 0, len(inst.Orgs))
		for _, o := range inst.Orgs {
			slugs = append(slugs, o.Slug)
		}
		sort.Strings(slugs)
		// Empty default → the operator MUST pick one (the stdin prompter
		// re-prompts on empty input when there's no default).
		return p.Select("Which organization should own this project?", slugs, "")
	}
}

// absFn is a test seam for filepath.Abs — its error arm fires only when
// the working directory is unresolvable, which has no portable real-FS
// trigger, so tests override this to drive the note-and-return path.
var absFn = filepath.Abs

// registerNewProject records a freshly-initialised project in the
// dev-machine-local registry (~/.w17/config.yaml). Idempotent +
// best-effort: a pre-existing entry is left untouched and any error is
// reported as a note, never fatal.
func registerNewProject(name, projectRoot string) {
	absRoot, err := absFn(projectRoot)
	if err != nil {
		return
	}
	cfg, err := core.LoadDevConfigFn()
	if err != nil {
		fmt.Fprintf(core.Stdout, "init: note — could not open local registry: %v\n", err)
		return
	}
	if cfg.Projects[name] != nil {
		return
	}
	cfg.Projects[name] = &devconfig.Project{Path: absRoot, Ports: map[string]int{}}
	if err := core.SaveDevConfigFn(cfg); err != nil {
		fmt.Fprintf(core.Stdout, "init: note — could not register project locally: %v\n", err)
		return
	}
	fmt.Fprintf(core.Stdout, "init: added %q to the local project registry (~/.w17/config.yaml)\n", name)
}

// scaffoldGoModule writes the project's hand-written Go-module skeleton
// — `<root>/<modDir>/go.mod` (modDir = the first segment of the stubs
// root, e.g. "srcgo" for stubs "srcgo/gen") and `<root>/go.work`. The
// go.work `use`s the project module + the wandering-compiler runtime
// modules (srcgo + sdk/go) when W17_WANDERING_COMPILER_PATH points at a
// repo checkout (co-dev mode); otherwise it `use`s only the project
// module and the require lines resolve from the module proxy. Idempotent
// — skips files that already exist so a re-init doesn't clobber edits.
func scaffoldGoModule(projectRoot, goModule, stubsRoot string) error {
	modDir := "srcgo"
	if i := strings.IndexByte(stubsRoot, '/'); i > 0 {
		modDir = stubsRoot[:i]
	} else if stubsRoot != "" {
		modDir = stubsRoot
	}

	// Co-dev replace/use paths: W17_WANDERING_COMPILER_PATH is
	// project-root-relative (codegen joins it onto the root). Empty =
	// published-module mode (no replace; proxy resolves the runtime).
	wcPath := strings.Trim(os.Getenv("W17_WANDERING_COMPILER_PATH"), "/")

	goModPath := filepath.Join(projectRoot, modDir, "go.mod")
	if _, err := os.Stat(goModPath); err != nil {
		var b strings.Builder
		// Runtime module paths come from split bases (core): sdk/go is
		// PUBLIC (SdkModuleBase, retargetable via ldflags for a published
		// w17ctl), srcgo is PRIVATE (SrcgoModuleBase) and only ever appears
		// in the inert co-dev replace below — never a require.
		srcgoMod := core.SrcgoModuleBase + "/srcgo"
		sdkMod := core.SdkModuleBase + "/sdk/go"
		fmt.Fprintf(&b, "module %s\n\ngo 1.26.1\n\n", goModule)
		if wcPath != "" {
			// go.mod replace paths are relative to the go.mod's dir
			// (<root>/<modDir>), so prepend one ".." for modDir.
			fmt.Fprintf(&b, "replace %s => %s\n", srcgoMod, filepath.ToSlash(filepath.Join("..", wcPath, "srcgo")))
			fmt.Fprintf(&b, "replace %s => %s\n\n", sdkMod, filepath.ToSlash(filepath.Join("..", wcPath, "sdk/go")))
		}
		// The project's hand-written srcgo module imports ONLY the
		// public sdk/go runtime — NEVER the private compiler `srcgo`
		// (that is closed IP and will never be publicly importable
		// [→ CLAUDE.md D4 / rule #5]). So `srcgo` gets a co-dev
		// `replace` above (inert unless a workspace sibling needs it)
		// but is deliberately absent from `require`: a require would
		// make published-mode `go build` try to fetch the private
		// module from the proxy and fail. Do NOT add it back.
		// ONLY sdk/go. The w17 ECOSYSTEM libraries (grpc / protobuf / pq /
		// redis / chi / nats) belong to the TOOL, not to the consumer's
		// project — `realReadDepVersions` says so in as many words — so the
		// scaffold must not pin them here.
		//
		// It used to pin grpc + protobuf from literals in this file, and
		// that turned the override seam inside out. `readDepVersions`
		// consults the PROJECT's go.mod FIRST precisely so a consumer CAN
		// override a tool version; pre-filling it made every fresh project
		// look like it had chosen one, and the value it "chose" was
		// whatever this source file was written against. So every bundle a
		// new project generated was pinned to that frozen version, and
		// upgrading w17ctl did not fix it — the project's own go.mod kept
		// winning through every future regen. `make audit`'s govulncheck
		// scans srcgo only, so a stale pin in generated output is invisible
		// to it, and `go mod tidy` will not move a require that is already
		// present and resolvable.
		//
		// Leaving them out is not a gap: the compiler backfills every
		// version the request leaves unset from its own embedded manifest,
		// and in co-dev the tool's srcgo/go.mod is the fallback. A consumer
		// who genuinely wants to pin one adds the require themselves — and
		// then the seam does what it was built for, because the pin is a
		// choice somebody made.
		fmt.Fprint(&b, "require ")
		fmt.Fprintf(&b, "%s v0.0.0\n", sdkMod)
		if err := os.MkdirAll(filepath.Dir(goModPath), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(goModPath, []byte(b.String()), 0o644); err != nil {
			return err
		}
		fmt.Fprintf(core.Stdout, "init: scaffolded %s (module %s)\n", goModPath, goModule)
	}

	goWorkPath := filepath.Join(projectRoot, "go.work")
	if _, err := os.Stat(goWorkPath); err != nil {
		var b strings.Builder
		fmt.Fprint(&b, "go 1.26.1\n\nuse (\n")
		if wcPath != "" {
			fmt.Fprintf(&b, "\t%s\n", filepath.ToSlash(filepath.Join(wcPath, "sdk/go")))
			fmt.Fprintf(&b, "\t%s\n", filepath.ToSlash(filepath.Join(wcPath, "srcgo")))
		}
		fmt.Fprintf(&b, "\t./%s\n)\n", modDir)
		if err := os.WriteFile(goWorkPath, []byte(b.String()), 0o644); err != nil {
			return err
		}
		fmt.Fprintf(core.Stdout, "init: scaffolded %s\n", goWorkPath)
	}
	return nil
}

// initFlags carries the pre-supplied values from CLI flags
// into the wizard so prompts get skipped where the operator
// already answered them on the command line.
type initFlags struct {
	name            string
	stubsRoot       string
	language        string
	protoDir        string
	languagesDir    string
	languagesCSV    string
	e2e             string
	ci              string
	skipConnections bool
}

// initAnswers carries the wizard's collected inputs. Pulled
// out of Run so the test path can drive runInitWizard
// directly without standing up a fake console.
type initAnswers struct {
	projectName string
	stubsRoot   string
	language    string

	protoDir     string
	languagesDir string
	languages    []string
	e2e          bool
	ci           string // raw wizard spec ("" / none / all / csv); parsed server-side
	connections  []initConnection
}

// initConnection is one connection collected during the init
// connection loop — the (name, markDefault) the lock stores.
// Dialect + version are NOT collected: they live in proto
// (`(w17.module).connection`), not the lock.
type initConnection struct {
	name        string
	markDefault bool
}

// parseLanguagesCSV splits a comma-separated language list,
// trims whitespace per entry, and rejects empty entries. An
// empty input string returns nil + no error (caller decides
// the prompt path).
func parseLanguagesCSV(s string) ([]string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		tag := strings.TrimSpace(p)
		if tag == "" {
			return nil, fmt.Errorf("empty language tag in %q", s)
		}
		out = append(out, tag)
	}
	return out, nil
}

// runInitWizard prompts for every piece of init-time state.
// Prompts get skipped wherever the matching CLI flag was
// supplied — that's how `--proto-dir=… --languages-dir=…
// --skip-connections` produces a fully non-interactive init
// without changing the function shape.
func runInitWizard(p prompter.Prompter, flags initFlags) (initAnswers, error) {
	fmt.Fprintln(core.Stdout, "Welcome to w17. Bootstrapping a new project...")
	fmt.Fprintln(core.Stdout)

	var err error
	name := flags.name
	if name == "" {
		name, err = p.Text("Project name (e.g. orders-api)", "")
		if err != nil {
			return initAnswers{}, err
		}
	}
	if name == "" {
		return initAnswers{}, fmt.Errorf("project name is required")
	}

	// Note: the bundle services dir is a fixed convention
	// (lock.ServicesDir = "w17/services"), not an init prompt — w17
	// owns the path, so there is nothing to ask.

	stubsRoot := flags.stubsRoot
	if stubsRoot == "" {
		stubsRoot, err = p.Text("Go stub tree root (pb + grpc clients)", "srcgo/gen")
		if err != nil {
			return initAnswers{}, err
		}
	}

	// Language is a Select today even though only "go" is
	// supported — when Python / Rust ship, the prompt grows
	// without changing the call shape.
	language := flags.language
	if language == "" {
		language, err = p.Select("Pb stubs language", []string{"go"}, "go")
		if err != nil {
			return initAnswers{}, err
		}
	}

	// --- Layout ---

	protoDir := flags.protoDir
	if protoDir == "" {
		protoDir, err = p.Text("Proto directory", defaultProtoDir)
		if err != nil {
			return initAnswers{}, err
		}
	}

	// --- i18n ---

	languagesDir := flags.languagesDir
	if languagesDir == "" {
		languagesDir, err = p.Text("Languages directory", defaultLanguagesDir)
		if err != nil {
			return initAnswers{}, err
		}
	}

	var languages []string
	if flags.languagesCSV != "" {
		languages, err = parseLanguagesCSV(flags.languagesCSV)
		if err != nil {
			return initAnswers{}, fmt.Errorf("--languages: %w", err)
		}
	} else {
		raw, err := p.Text("Languages (comma-separated BCP-47 tags, e.g. en,cs,pt-BR)", defaultLanguage)
		if err != nil {
			return initAnswers{}, err
		}
		languages, err = parseLanguagesCSV(raw)
		if err != nil {
			return initAnswers{}, err
		}
	}

	// --- e2e tests (opt-in, default-yes) ---

	// Flag "yes"/"no" opts in/out non-interactively; empty runs the
	// default-yes prompt. The tree path is fixed (lock carries only the
	// bool); decline → feature off (no skeletons, no bundle `test`).
	var e2e bool
	switch strings.ToLower(strings.TrimSpace(flags.e2e)) {
	case "yes", "y", "true":
		e2e = true
	case "no", "n", "false":
		e2e = false
	case "":
		yn, err := p.Select("Generate e2e tests for your gateways (REST + MCP)?", []string{"yes", "no"}, "yes")
		if err != nil {
			return initAnswers{}, err
		}
		e2e = yn == "yes"
	default:
		return initAnswers{}, fmt.Errorf("--e2e: want yes|no, got %q", flags.e2e)
	}

	// --- CI configs (opt-in, default github) ---

	// Flag carries a comma-separated provider list / "all" / "none";
	// empty runs the prompt with a github default. The configs run the
	// e2e suite via `w17ctl test` under w17/ci/<provider>/. The raw spec is
	// passed through to the console, which parses + validates it (the provider
	// taxonomy lives server-side).
	ciRaw := strings.TrimSpace(flags.ci)
	if ciRaw == "" {
		ciRaw, err = p.Text("Generate e2e CI configs? Providers ("+ciProviderHint+"), 'all', or 'none'", "github")
		if err != nil {
			return initAnswers{}, err
		}
	}

	// --- Connections ---

	var connections []initConnection
	if !flags.skipConnections {
		connections, err = runInitConnectionLoop(p)
		if err != nil {
			return initAnswers{}, err
		}
	}

	return initAnswers{
		projectName:  name,
		stubsRoot:    stubsRoot,
		language:     language,
		protoDir:     protoDir,
		languagesDir: languagesDir,
		languages:    languages,
		e2e:          e2e,
		ci:           ciRaw,
		connections:  connections,
	}, nil
}

// runInitConnectionLoop runs the "Add a connection? y/N" gate
// in a loop, reusing the connection-add wizard from
// `w17ctl connection add` for each iteration. The
// accumulated list is fed back as a synthetic Lock so the
// inner wizard's duplicate-name check works.
func runInitConnectionLoop(p prompter.Prompter) ([]initConnection, error) {
	var collected []initConnection
	for {
		more, err := p.Select("Add a connection?", []string{"yes", "no"}, "no")
		if err != nil {
			return collected, err
		}
		if more == "no" {
			return collected, nil
		}
		// Feed the accumulated connections back as the wizard's "existing"
		// set so its duplicate-name + default-resolution checks work. (The
		// init flow assembles the whole lock locally today; migrating init
		// itself onto EditLock is a later Block-2 slice.)
		var existing []*codegenpb.LockConnection
		for _, c := range collected {
			existing = append(existing, &codegenpb.LockConnection{Name: c.name, Default: c.markDefault})
		}
		name, markDefault, err := connection.RunAddWizard(p, existing, false, "")
		if err != nil {
			return collected, fmt.Errorf("connection #%d: %w", len(collected)+1, err)
		}
		collected = append(collected, initConnection{name: name, markDefault: markDefault})
	}
}
