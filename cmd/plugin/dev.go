package plugin

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	codegencmd "github.com/wandering-compiler/w17ctl/cmd/codegen"
	initcmd "github.com/wandering-compiler/w17ctl/cmd/init"
	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/pluginfetch"
)

// DevCmd runs a plugin AS ITSELF: it stands up a throwaway w17 project, turns
// this plugin on in it, and generates.
//
// # Why a plugin needed this
//
// A plugin's unit tests exercise its handlers over doubles, and they do it well.
// What they cannot exercise is the half a plugin IS: the proto producing a
// storage tier, the handlers being STAGED with their import paths rewritten,
// the bundle compiling, the surface answering. None of that exists inside
// `plugins/<name>/` — it is created by ACTIVATION in a project — so there is
// nothing there to integrate against, and a plugin module is forbidden from
// depending on anything project-shaped anyway (a repo-relative `replace` is
// refused, because the module is vendored into a tree where that path does not
// exist).
//
// Measured before building this: of four shipped plugins, two had never been
// activated by anything — never generated, never staged, never compiled in a
// project, never called. Their handlers had 124 test functions between them.
//
// # Why not an example project per plugin
//
// That was the first plan and this is better in the way that matters: it goes
// through `install` and `codegen`, which is the path a CONSUMER takes. An
// authored example tests a tree we wrote; this tests the tree they receive. It
// also works for an author who has no monorepo, the feature matrix is a flag
// rather than N copies of a project, and there is no per-plugin hand-written
// scaffolding to drift.
//
// # What it does NOT prove
//
// That the surface answers. This stops at codegen, which is where a plugin that
// has never been generated fails — and the live half needs a stack, a database
// and a console. `--up` is where that goes, and until it exists this command
// says "it generates", not "it works".
type DevCmd struct {
	Dir string `arg:"" optional:"" name:"dir" help:"Plugin tree to run — the directory holding plugin.yaml. Default: the current directory."`

	Features    string `name:"features" placeholder:"CSV" help:"Features to activate. Empty = the ones the manifest marks default:true, which is what a consumer gets by doing nothing."`
	AllFeatures bool   `name:"all-features" help:"Activate every feature the manifest declares. The widest generation the plugin can be asked for, and the one most likely to find something — features are opt-in, so a feature nothing activates has never been generated."`

	Out     string `name:"out" placeholder:"DIR" help:"Where to build the throwaway project. Empty = a temporary directory, removed on exit. A path INSIDE the plugin tree is fine and the conventional name is .w17dev — plugindigest excludes that directory, so a dev project there cannot move the digest of the plugin it is testing."`
	Keep    bool   `name:"keep" help:"Leave the project on disk and print its path, for looking at what was generated. Implied when --out is given."`
	Console string `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console. It does the generating, so this is required in practice — falls back to the binary's compile-time default."`
	Org     string `name:"org" placeholder:"SLUG" help:"Organization that owns the throwaway project. Empty = your default or your only membership."`

	SignInFrom string `name:"sign-in-from" placeholder:"DIR" help:"A plugin tree that supplies admin sign-in, for a plugin whose admin pages expect another plugin to sign people in. Empty = the sibling auth plugin (../auth) when there is one."`
}

func (c *DevCmd) Run() error {
	dir := c.Dir
	if dir == "" {
		dir = "."
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("plugin dev: %w", err)
	}
	name, err := pluginfetch.NameInDir(abs)
	if err != nil {
		return fmt.Errorf("plugin dev: %w", err)
	}
	feats, err := devFeatures(abs, c.Features, c.AllFeatures)
	if err != nil {
		return fmt.Errorf("plugin dev: %w", err)
	}

	// Resolved BEFORE the chdir below: a relative --sign-in-from is relative to
	// where the command was run, not to the throwaway project.
	signIn, err := resolveSignIn(abs, name, c.SignInFrom)
	if err != nil {
		return fmt.Errorf("plugin dev: %w", err)
	}

	proj, cleanup, err := devProjectDir(c.Out, c.Keep, name)
	if err != nil {
		return fmt.Errorf("plugin dev: %w", err)
	}
	defer cleanup()

	fmt.Fprintf(core.Stdout, "plugin dev: %s with %d feature(s): %s\n", name, len(feats), strings.Join(feats, " "))
	fmt.Fprintf(core.Stdout, "  project: %s\n", proj)

	// Every step below runs with the throwaway project as the working
	// directory, because that is how `init`, `install` and `codegen` find a
	// project — the same way a consumer runs them.
	restore, err := chdir(proj)
	if err != nil {
		return fmt.Errorf("plugin dev: %w", err)
	}
	defer restore()

	if err := devScaffold(abs, name, feats, signIn); err != nil {
		return err
	}

	fmt.Fprintln(core.Stdout, "plugin dev: installing the tree the way a consumer receives it ...")
	inst := &InstallCmd{Source: abs, Console: c.Console}
	if err := inst.Run(); err != nil {
		return fmt.Errorf("plugin dev: install: %w", err)
	}
	if signIn != nil {
		fmt.Fprintf(core.Stdout, "plugin dev: installing %s for admin sign-in ...\n", signIn.name)
		if err := (&InstallCmd{Source: signIn.dir, Console: c.Console}).Run(); err != nil {
			return fmt.Errorf("plugin dev: install %s for sign-in: %w", signIn.name, err)
		}
	}

	fmt.Fprintln(core.Stdout, "plugin dev: generating ...")
	// No update check: the throwaway project is the command's own, and a
	// prompt in the middle of a plugin run would be about a project nobody keeps.
	gen := &codegencmd.Cmd{Console: c.Console, Force: true, Gofmt: "embedded", NoUpdateCheck: true}
	if err := gen.Run(); err != nil {
		return fmt.Errorf("plugin dev: codegen: %w\n"+
			"  this is the step a plugin that has never been activated fails at: the proto has "+
			"to produce a storage tier, the handlers have to stage with their imports rewritten, "+
			"and the bundle has to compile", err)
	}

	fmt.Fprintf(core.Stdout, "plugin dev: %s generates with %s\n", name, strings.Join(feats, " "))
	fmt.Fprintln(core.Stdout, "  ⚠️ that it GENERATES, not that it answers — the live half needs a stack")
	return nil
}

// devScaffold writes the project files that turn this plugin on.
//
// Two files, and they are the whole activation: a domain sentinel carrying the
// plugin with its features, and a REST registry including the plugin's presets
// so its endpoints exist on a surface. Anything else a project has is not
// needed to find out whether a plugin generates.
func devScaffold(dir, name string, feats []string, signIn *signInPlugin) error {
	initc := &initcmd.Cmd{
		LockPath: filepath.Join("w17", "lock.yaml"),
		Name:     "plugindev",
		GoModule: "example.com/plugindev",
		// The values the documented first run uses, so this project is shaped
		// like one a person would get rather than one only this command makes.
		StubsRoot:       filepath.Join("srcgo", "gen"),
		Language:        "go",
		ProtoDir:        "proto",
		LanguagesDir:    filepath.Join("w17", "languages"),
		Languages:       "en",
		E2E:             "no",
		CI:              "none",
		SkipConnections: true,
	}
	if err := os.WriteFile("go.mod", []byte("module example.com/plugindev\n\ngo 1.26\n"), 0o644); err != nil {
		return fmt.Errorf("plugin dev: %w", err)
	}
	if err := initc.Run(); err != nil {
		return fmt.Errorf("plugin dev: init: %w", err)
	}

	domain := filepath.Join("proto", "domains", "app")
	if err := os.MkdirAll(domain, 0o755); err != nil {
		return fmt.Errorf("plugin dev: %w", err)
	}
	if err := os.WriteFile(filepath.Join(domain, "w17.proto"),
		[]byte(domainProto(name, feats, signIn)), 0o644); err != nil {
		return fmt.Errorf("plugin dev: %w", err)
	}
	// ⚠️ ONLY IF THE PLUGIN HAS A REST PRESET. Found by the dev lane's first
	// run: agent declares no presets at all, and a REST registry including a
	// plugin that publishes none is refused outright — "includes plugin
	// \"agent\", but that plugin ships no rest preset to publish". A scaffold
	// that writes the include unconditionally cannot generate such a plugin at
	// all, which is the opposite of a tool for exercising plugins.
	hasREST, err := manifestHasPreset(dir, "rest")
	if err != nil {
		return fmt.Errorf("plugin dev: %w", err)
	}
	if hasREST {
		if err := os.WriteFile(filepath.Join(domain, "rest.proto"),
			[]byte(restProto(name)), 0o644); err != nil {
			return fmt.Errorf("plugin dev: %w", err)
		}
	} else {
		// Said out loud, because it narrows what the run proves: without a
		// surface the plugin's endpoints are generated but published nowhere.
		fmt.Fprintf(core.Stdout, "  no rest preset — generating without a public surface, "+
			"so this run exercises the tiers and not the endpoints\n")
	}
	return devScaffoldMcpAndAdmin(dir, domain, name, feats, signIn)
}

// devScaffoldMcpAndAdmin publishes the plugin's MCP and admin presets too,
// when it ships them.
//
// ⚠️ A preset no surface publishes is a preset nothing checks. The scaffold
// used to write the REST registry only, so a plugin's admin pages were never
// merged onto an `(w17.admin_api)` and the admin parser's guards over them —
// readonly_fields against what the update writes, detail.fields against what
// it persists — never ran here. auth rc.11 shipped an UpdateOrgMembership
// those guards refused, `plugin dev --all-features` said it generated, and
// the first consumer with the admin preset on could not run codegen at all.
//
// Admin needs a way to sign in. A plugin whose admin preset carries `auth`
// supplies it and the surface is complete; one that ships pages but no auth
// (it expects another plugin to sign people in) cannot be published on its
// own, and the run says what that leaves unchecked.
func devScaffoldMcpAndAdmin(dir, domain, name string, feats []string, signIn *signInPlugin) error {
	hasMCP, err := manifestHasPreset(dir, "mcp")
	if err != nil {
		return fmt.Errorf("plugin dev: %w", err)
	}
	if hasMCP {
		if err := os.WriteFile(filepath.Join(domain, "mcp.proto"),
			[]byte(mcpProto(name)), 0o644); err != nil {
			return fmt.Errorf("plugin dev: %w", err)
		}
	}
	hasAdmin, err := manifestHasPreset(dir, "admin")
	if err != nil {
		return fmt.Errorf("plugin dev: %w", err)
	}
	if !hasAdmin {
		return nil
	}
	auth, err := manifestAdminAuth(dir)
	if err != nil {
		return fmt.Errorf("plugin dev: %w", err)
	}
	if auth == nil {
		// Pages that expect ANOTHER plugin to sign people in — what a consumer
		// does is activate that plugin beside it, so the run does the same.
		if signIn == nil {
			fmt.Fprintf(core.Stdout, "  admin preset without its own sign-in, and no plugin to sign in "+
				"with (--sign-in-from) — its pages are NOT published here, so the admin guards over "+
				"them are not exercised by this run\n")
			return nil
		}
		fmt.Fprintf(core.Stdout, "  admin pages sign in through %s, activated beside it\n", signIn.name)
		return os.WriteFile(filepath.Join(domain, "admin.proto"), []byte(adminProto()), 0o644)
	}
	// The sign-in has to EXIST in this activation, not just be declared: the
	// preset's own feature and the features its two methods are gated on. A
	// partial activation (`--features rbac`) has no sign-in, and publishing an
	// admin surface there would fail the run on something the plugin is not
	// wrong about. Which features those are is read off the plugin's own
	// files; whether the activation is valid stays the console's call.
	missing, err := adminSignInMissing(dir, auth, feats)
	if err != nil {
		return fmt.Errorf("plugin dev: %w", err)
	}
	if len(missing) > 0 {
		fmt.Fprintf(core.Stdout, "  admin sign-in needs %s, not in this activation — its pages are NOT "+
			"published here, so the admin guards over them are not exercised by this run\n",
			strings.Join(missing, ", "))
		return nil
	}
	if err := os.WriteFile(filepath.Join(domain, "admin.proto"),
		[]byte(adminProto()), 0o644); err != nil {
		return fmt.Errorf("plugin dev: %w", err)
	}
	return nil
}

// signInPlugin is a second plugin activated only to sign people in to an admin
// surface whose pages the plugin under test contributes.
type signInPlugin struct {
	dir, name string
	feats     []string
}

// resolveSignIn decides whether the run needs a sign-in plugin and which.
//
// Only a plugin whose admin preset ships pages and no sign-in needs one —
// cluster's backoffice pages are the case: they expect the project's auth to
// sign operators in, so without a second plugin they were never published by
// this command, and no example activates cluster at all. Those pages had never
// been generated by anything. The sign-in plugin is the sibling `auth` tree
// when there is one (this repository's layout), or --sign-in-from; it is
// activated with its own defaults, and a sign-in it cannot supply under them
// is reported rather than guessed around.
func resolveSignIn(dir, name, from string) (*signInPlugin, error) {
	hasAdmin, err := manifestHasPreset(dir, "admin")
	if err != nil || !hasAdmin {
		return nil, err
	}
	own, err := manifestAdminAuth(dir)
	if err != nil || own != nil {
		return nil, err
	}
	src, auto := from, false
	if src == "" {
		sibling := filepath.Join(filepath.Dir(dir), "auth")
		if _, statErr := os.Stat(filepath.Join(sibling, "plugin.yaml")); statErr != nil {
			return nil, nil
		}
		src, auto = sibling, true
	}
	// A sibling picked AUTOMATICALLY that cannot sign in is not the author's
	// mistake — they named nothing. The run then behaves as it did before this
	// lookup existed: the pages stay unpublished and the scaffold says so. Only
	// an explicit --sign-in-from is held to it.
	sp, err := loadSignIn(src, name)
	if err != nil {
		if auto {
			fmt.Fprintf(core.Stdout, "  %s beside it cannot sign in to admin (%v)\n", src, err)
			return nil, nil
		}
		return nil, fmt.Errorf("--sign-in-from %s: %w", src, err)
	}
	return sp, nil
}

// loadSignIn reads the plugin at src as a sign-in provider.
func loadSignIn(src, name string) (*signInPlugin, error) {
	abs, err := filepath.Abs(src)
	if err != nil {
		return nil, err
	}
	signName, err := pluginfetch.NameInDir(abs)
	if err != nil {
		return nil, err
	}
	if signName == name {
		return nil, nil
	}
	auth, err := manifestAdminAuth(abs)
	if err != nil {
		return nil, err
	}
	if auth == nil {
		return nil, fmt.Errorf("plugin %q ships no admin sign-in (presets.admin.auth)", signName)
	}
	_, defaults, err := manifestFeatures(abs)
	if err != nil {
		return nil, err
	}
	missing, err := adminSignInMissing(abs, auth, defaults)
	if err != nil {
		return nil, err
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("%q signs in only with %s, which its defaults do not turn on",
			signName, strings.Join(missing, ", "))
	}
	return &signInPlugin{dir: abs, name: signName, feats: defaults}, nil
}

// adminAuthPreset is the sign-in a plugin's admin preset wires.
type adminAuthPreset struct {
	LoginMethod string `yaml:"login_method"`
	UserLookup  string `yaml:"user_lookup"`
	Feature     string `yaml:"feature"`
}

// manifestAdminAuth returns presets.admin.auth, or nil when the plugin ships
// none.
func manifestAdminAuth(dir string) (*adminAuthPreset, error) {
	body, err := os.ReadFile(filepath.Join(dir, "plugin.yaml"))
	if err != nil {
		return nil, fmt.Errorf("reading the manifest: %w", err)
	}
	var m struct {
		Presets struct {
			Admin struct {
				Auth *adminAuthPreset `yaml:"auth"`
			} `yaml:"admin"`
		} `yaml:"presets"`
	}
	if err := yaml.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("reading the manifest: %w", err)
	}
	a := m.Presets.Admin.Auth
	if a == nil || a.LoginMethod == "" || a.UserLookup == "" {
		return nil, nil
	}
	return a, nil
}

// adminSignInMissing returns the features the admin sign-in needs that `feats`
// lacks: the preset's own, plus each referenced method's
// `(w17.contrib.plugin_feature_rpc)` from the plugin's protos.
func adminSignInMissing(dir string, a *adminAuthPreset, feats []string) ([]string, error) {
	need := []string{}
	if a.Feature != "" {
		need = append(need, a.Feature)
	}
	for _, ref := range []string{a.LoginMethod, a.UserLookup} {
		f, err := rpcFeature(dir, ref)
		if err != nil {
			return nil, err
		}
		if f != "" {
			need = append(need, f)
		}
	}
	have := map[string]bool{}
	for _, f := range feats {
		have[f] = true
	}
	var missing []string
	seen := map[string]bool{}
	for _, f := range need {
		if !have[f] && !seen[f] {
			missing = append(missing, f)
			seen[f] = true
		}
	}
	return missing, nil
}

// rpcFeature returns the feature `<Service>.<Method>` is gated on in the
// plugin's protos, "" when it is not gated (or not found — the console then
// reports the ref).
func rpcFeature(dir, ref string) (string, error) {
	service, method, ok := strings.Cut(ref, ".")
	if !ok {
		return "", nil
	}
	rpcRe := regexp.MustCompile(`(?m)^\s*rpc\s+` + regexp.QuoteMeta(method) + `\s*\(`)
	gateRe := regexp.MustCompile(`plugin_feature_rpc\)\s*=\s*"([^"]+)"`)
	svcRe := regexp.MustCompile(`(?m)^\s*service\s+` + regexp.QuoteMeta(service) + `\s*\{`)
	var found string
	err := filepath.WalkDir(filepath.Join(dir, "proto"), func(p string, d os.DirEntry, werr error) error {
		if werr != nil || d.IsDir() || !strings.HasSuffix(p, ".proto") || found != "" {
			return werr
		}
		body, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		src := string(body)
		svc := svcRe.FindStringIndex(src)
		if svc == nil {
			return nil
		}
		at := rpcRe.FindStringIndex(src[svc[1]:])
		if at == nil {
			return nil
		}
		rest := src[svc[1]+at[1]:]
		if next := regexp.MustCompile(`(?m)^\s*rpc\s`).FindStringIndex(rest); next != nil {
			rest = rest[:next[0]]
		}
		if g := gateRe.FindStringSubmatch(rest); g != nil {
			found = g[1]
		}
		return nil
	})
	return found, err
}

// manifestHasPreset reports whether the plugin declares a preset of that kind.
//
// `presets:` is a mapping whose KEYS are the surface kinds (`rest`, `mcp`,
// `admin`), so this looks for the key at one level in — the same small scanner
// the features use, for the same reason.
func manifestHasPreset(dir, kind string) (bool, error) {
	body, err := os.ReadFile(filepath.Join(dir, "plugin.yaml"))
	if err != nil {
		return false, fmt.Errorf("reading the manifest: %w", err)
	}
	inPresets := false
	for _, raw := range strings.Split(string(body), "\n") {
		line := strings.TrimRight(raw, " \t")
		trimmed := strings.TrimSpace(line)
		if trimmed == "presets:" {
			inPresets = true
			continue
		}
		if inPresets && line != "" && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			return false, nil
		}
		if inPresets && trimmed == kind+":" {
			return true, nil
		}
	}
	return false, nil
}

func domainProto(name string, feats []string, signIn *signInPlugin) string {
	entry := func(n string, fs []string) string {
		var quoted []string
		for _, f := range fs {
			quoted = append(quoted, "\""+f+"\"")
		}
		return `    {
      source_name:   "` + n + `"
      registered_as: "` + n + `"
      channels:   [ { key: "events", value: "app" } ]
      auth_input: { header_name: "Authorization", scheme: "Bearer" }
      features:   { names: [ ` + strings.Join(quoted, ", ") + ` ] }
    }`
	}
	entries := entry(name, feats)
	if signIn != nil {
		entries += ",\n" + entry(signIn.name, signIn.feats)
	}
	return `syntax = "proto3";

// Written by ` + "`w17ctl plugin dev`" + `. A throwaway project whose only job is to
// activate one plugin and find out whether it generates.
//
// File is a SENTINEL — options only, no messages.

package plugindev.app;

import "w17/domain.proto";
import "w17/module.proto";

option (w17.domain) = {
  plugins: [
` + entries + `
  ]
};

option (w17.module) = {
  connection: { name: "app-postgres", dialect: POSTGRES, version: "18" },
  channels: [
    { name: "app", transport: TRANSPORT_NATS, retry: { max_deliver: 3 }, drain_timeout_seconds: 30 }
  ]
};
`
}

func restProto(name string) string {
	return `syntax = "proto3";

package plugindev.app;

import "w17/rest.proto";

// The plugin's own presets supply the surface. Including them is what makes the
// plugin's endpoints exist at all — presets are opt-in, so a plugin generated
// without this has had only half of it exercised.
option (w17.rest_api) = {
  name:        "public",
  version:     "v1",
  prefix:      "/api/v1",
  description: "plugin dev — one plugin, generated as itself.",
  include: [ { plugin: "` + name + `" } ]
};
`
}

func mcpProto(name string) string {
	return `syntax = "proto3";

package plugindev.app;

import "w17/mcp.proto";

// The plugin's MCP preset, published — see devScaffoldMcpAndAdmin.
option (w17.mcp_api) = {
  name:        "default",
  version:     "v1",
  description: "plugin dev — the plugin's MCP tools.",
  include:     [ "` + name + `" ]
};
`
}

func adminProto() string {
	return `syntax = "proto3";

package plugindev.app;

import "w17/admin.proto";

// The admin surface the plugin's admin preset merges its pages, widgets and
// sign-in onto (presets.admin is on by default) — see devScaffoldMcpAndAdmin.
option (w17.admin_api) = {
  name:   "admin",
  prefix: "/admin"
};
`
}

// devFeatures resolves which features to activate.
//
// The default is what the MANIFEST marks `default: true` — which is what a
// consumer gets by doing nothing, and therefore the run that matters most.
//
// ⚠️ `--all-features` is the one most likely to find something, and the reason
// is measured rather than guessed: features are opt-in, so a feature nothing
// activates has never been generated. Of auth's seventeen, sixteen are
// activated somewhere in this repository and one — `password_reset` — is
// declared by nothing at all. Its handler has tests. Its generated half has
// never existed.
func devFeatures(dir, csv string, all bool) ([]string, error) {
	declared, defaults, err := manifestFeatures(dir)
	if err != nil {
		return nil, err
	}
	// ⚠️ AN EXPLICIT LIST IS CHECKED FOR NAMES AND NOT FOR COMPATIBILITY, and
	// the asymmetry is deliberate. A name the manifest does not carry is a fact
	// about the MANIFEST, which this command reads, and a typo would otherwise
	// activate nothing and report success. Whether two features may be enabled
	// together is enforced by the COMPILER, which owns that rule — re-checking
	// it here would be a second copy of it, and the refusal an author gets from
	// the compiler is the authoritative one.
	switch {
	case csv != "":
		var out []string
		known := map[string]bool{}
		for _, f := range declared {
			known[f] = true
		}
		for _, f := range strings.Split(csv, ",") {
			f = strings.TrimSpace(f)
			if f == "" {
				continue
			}
			if !known[f] {
				return nil, fmt.Errorf(
					"this plugin declares no feature %q\n  it declares: %s\n"+
						"  why refuse rather than pass it on: a feature name the manifest does not "+
						"carry generates nothing, and a run that quietly activated nothing would "+
						"report success over an empty exercise",
					f, strings.Join(declared, " "))
			}
			out = append(out, f)
		}
		return out, nil
	case all:
		return widestCompatibleSet(dir, declared)
	default:
		return defaults, nil
	}
}

// widestCompatibleSet is the largest activation a plugin actually supports.
//
// ⚠️ "EVERY FEATURE" IS NOT NECESSARILY AN ACTIVATION, and the first version of
// this command assumed it was. Measured on the first run of the dev lane: auth
// declares `tenant_scope` as incompatible with `oauth`, so asking for all
// seventeen is refused by the compiler — `features "oauth" and "tenant_scope"
// cannot be enabled together`. A flag whose whole job is to reach features
// nothing else turns on cannot be a flag that never generates.
//
// So it walks the manifest IN ORDER and takes each feature unless it conflicts
// with one already taken. Manifest order rather than a search for the true
// maximum: it is deterministic, it is the order a reader sees, and the answer
// is reproducible — a set that changed between runs would make a failure
// impossible to reason about.
//
// Features a taken one REQUIRES are pulled in, because a plugin that declares
// `oauth` needing `email_verification` will not generate with one and not the
// other, and a run that dropped the dependency would fail for a reason that
// has nothing to do with the feature being tested.
//
// What is left out is REPORTED by the caller, not swallowed: a coverage tool
// that quietly narrowed its own coverage would be the thing this whole slab
// exists to prevent.
func widestCompatibleSet(dir string, declared []string) ([]string, error) {
	conflicts, requires, err := manifestFeatureGraph(dir)
	if err != nil {
		return nil, err
	}
	taken := map[string]bool{}
	var out []string

	// clashWith reports what f is incompatible with among the taken set, in
	// EITHER direction — a manifest declares the incompatibility on one of the
	// pair and the reader must not care which.
	clashWith := func(f string) string {
		for _, c := range conflicts[f] {
			if taken[c] {
				return c
			}
		}
		for t := range taken {
			for _, c := range conflicts[t] {
				if c == f {
					return t
				}
			}
		}
		return ""
	}

	for _, f := range declared {
		if c := clashWith(f); c != "" {
			fmt.Fprintf(core.Stdout, "  leaving out %s — the manifest declares it incompatible with %s\n", f, c)
			continue
		}
		// ⚠️ A REQUIRED FEATURE IS CHECKED TOO, and the first version of this
		// did not check it: `add` pulled in everything a taken feature requires
		// unconditionally, so a requirement that conflicts with something
		// already taken would have produced a set the COMPILER refuses — and a
		// refused set means the widest run does not run at all, which is
		// exactly how auth's failure looked before it was diagnosed.
		//
		// No manifest here has that shape today; the check is not waiting for
		// one to appear before being right.
		blocked := ""
		for _, r := range requires[f] {
			if taken[r] {
				continue
			}
			if c := clashWith(r); c != "" {
				blocked = r + " (incompatible with " + c + ")"
				break
			}
		}
		if blocked != "" {
			fmt.Fprintf(core.Stdout, "  leaving out %s — it requires %s\n", f, blocked)
			continue
		}
		for _, r := range requires[f] {
			if !taken[r] {
				taken[r] = true
				out = append(out, r)
			}
		}
		if !taken[f] {
			taken[f] = true
			out = append(out, f)
		}
	}
	return out, nil
}

// manifestFeatureGraph reads the `conflicts_with` and `requires` lists.
//
// The same small scanner as manifestFeatures and for the same reason — the
// console validates a manifest and a second parser here would be a second
// opinion about what one IS — and checked the same way, against a real YAML
// parse in the test, because a scanner that silently reads FEWER conflicts
// produces an activation the compiler then refuses.
func manifestFeatureGraph(dir string) (conflicts, requires map[string][]string, err error) {
	body, rerr := os.ReadFile(filepath.Join(dir, "plugin.yaml"))
	if rerr != nil {
		return nil, nil, fmt.Errorf("reading the manifest: %w", rerr)
	}
	conflicts, requires = map[string][]string{}, map[string][]string{}
	inFeatures, current, list := false, "", ""
	for _, raw := range strings.Split(string(body), "\n") {
		line := strings.TrimRight(raw, " \t")
		trimmed := strings.TrimSpace(line)
		if trimmed == "features:" {
			inFeatures = true
			continue
		}
		if inFeatures && line != "" && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			inFeatures, current, list = false, "", ""
		}
		if !inFeatures {
			continue
		}
		if n, ok := cutFeatureName(trimmed); ok {
			current, list = n, ""
			continue
		}
		switch trimmed {
		case "conflicts_with:":
			list = "conflicts"
			continue
		case "requires:":
			list = "requires"
			continue
		}
		if item, ok := strings.CutPrefix(trimmed, "- "); ok && current != "" && list != "" {
			v := strings.Trim(strings.TrimSpace(item), `"'`)
			if v == "" || strings.Contains(v, ":") {
				// A nested mapping item, not a bare name: the list ended.
				list = ""
				continue
			}
			if list == "conflicts" {
				conflicts[current] = append(conflicts[current], v)
			} else {
				requires[current] = append(requires[current], v)
			}
			continue
		}
		if trimmed != "" && !strings.HasPrefix(trimmed, "#") && !strings.HasPrefix(trimmed, "- ") {
			// Any other key under this feature ends whichever list was open.
			list = ""
		}
	}
	return conflicts, requires, nil
}

// manifestFeatures reads the declared features and the default-on subset.
//
// Deliberately a small scanner rather than a YAML parse: this walks one list of
// `- name:` / `default:` pairs, the console is the side that validates a
// manifest, and a dependency on a parser here would be a second opinion about
// what a manifest is.
func manifestFeatures(dir string) (declared, defaults []string, err error) {
	body, rerr := os.ReadFile(filepath.Join(dir, "plugin.yaml"))
	if rerr != nil {
		return nil, nil, fmt.Errorf("reading the manifest: %w", rerr)
	}
	inFeatures := false
	current := ""
	for _, raw := range strings.Split(string(body), "\n") {
		line := strings.TrimRight(raw, " \t")
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "features:":
			inFeatures = true
			continue
		case inFeatures && line != "" && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t"):
			// A new top-level key ends the list.
			inFeatures = false
			current = ""
		}
		if !inFeatures {
			continue
		}
		if n, ok := cutFeatureName(trimmed); ok {
			current = n
			declared = append(declared, n)
			continue
		}
		if current != "" && trimmed == "default: true" {
			defaults = append(defaults, current)
		}
	}
	if len(declared) == 0 {
		// Not an error. A plugin may have no features at all — cluster has
		// none — and "activate nothing" is then the only correct answer.
		return nil, nil, nil
	}
	return declared, defaults, nil
}

func cutFeatureName(trimmed string) (string, bool) {
	rest, ok := strings.CutPrefix(trimmed, "- name:")
	if !ok {
		return "", false
	}
	return strings.Trim(strings.TrimSpace(rest), `"'`), true
}

// devProjectDir picks where the throwaway project lives.
//
// Temporary and REMOVED by default, because the point is the generation and not
// the tree: a dev loop that left a project behind on every run would fill a
// working copy with directories nobody asked for. `--keep` and `--out` are how
// somebody who wants to look at the output says so.
func devProjectDir(out string, keep bool, name string) (string, func(), error) {
	if out != "" {
		abs, err := filepath.Abs(out)
		if err != nil {
			return "", func() {}, err
		}
		if err := os.MkdirAll(abs, 0o755); err != nil {
			return "", func() {}, err
		}
		return abs, func() {
			fmt.Fprintf(core.Stdout, "plugin dev: project left at %s\n", abs)
		}, nil
	}
	dir, err := os.MkdirTemp("", "w17-plugindev-"+name+"-*")
	if err != nil {
		return "", func() {}, err
	}
	if keep {
		return dir, func() {
			fmt.Fprintf(core.Stdout, "plugin dev: project left at %s\n", dir)
		}, nil
	}
	return dir, func() { _ = os.RemoveAll(dir) }, nil
}

// chdir moves into dir and returns the way back.
//
// In-process rather than a subprocess: `init`, `install` and `codegen` are all
// in this binary and finding a project is what they do with the working
// directory, so composing them here runs exactly the path a consumer runs. The
// restore is deferred because a command that left the process somewhere else
// would break everything after it in the same run.
func chdir(dir string) (func(), error) {
	prev, err := os.Getwd()
	if err != nil {
		return func() {}, err
	}
	if err := os.Chdir(dir); err != nil {
		return func() {}, err
	}
	return func() { _ = os.Chdir(prev) }, nil
}
