package codegen

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
	"google.golang.org/grpc/status"

	"github.com/wandering-compiler/w17ctl/internal/adminruntime"
	"github.com/wandering-compiler/w17ctl/internal/core"
	codegenpb "github.com/wandering-compiler/sdk/go/pb/w17compiler"
	"github.com/wandering-compiler/sdk/go/tooling/pathguard"
)

// mergeGoModReplaces splices `replace` directives from the
// on-disk go.mod at `existingPath` into `newContents`. Returns
// `newContents` unchanged when:
//   - existingPath doesn't exist (first emission),
//   - the existing file has no `replace` directives,
//   - every existing replace already appears in `newContents`
//     (server-rendered set is a superset).
//
// Existing replaces that the server didn't render get appended
// to the new go.mod's tail. This is how operator-maintained
// dev-loop paths back to the project's srcgo and the wandering-
// compiler checkout survive regen — the server has no project-
// extrinsic knowledge of those paths and would re-emit an empty
// replace block on every pass otherwise.
func mergeGoModReplaces(newContents []byte, existingPath string) ([]byte, error) {
	existingBytes, err := os.ReadFile(existingPath)
	if err != nil {
		if os.IsNotExist(err) {
			return newContents, nil
		}
		return nil, fmt.Errorf("read existing go.mod: %w", err)
	}
	existing, err := modfile.Parse(existingPath, existingBytes, nil)
	if err != nil {
		return nil, fmt.Errorf("parse existing go.mod: %w", err)
	}
	if len(existing.Replace) == 0 {
		return newContents, nil
	}
	newMod, err := modfile.Parse("go.mod", newContents, nil)
	if err != nil {
		return nil, fmt.Errorf("parse new go.mod: %w", err)
	}
	have := make(map[string]struct{}, len(newMod.Replace))
	for _, r := range newMod.Replace {
		have[r.Old.Path] = struct{}{}
	}
	// Dead module paths a stale committed go.mod must NOT resurrect on
	// regen:
	//   - <private-base>/srcgo — the generated bundle imports ZERO private
	//     srcgo (its runtime helpers moved to the public sdk/go).
	//   - <private-base>/sdk/go — the pre-rename sdk path. The SDK now lives
	//     at core.SdkModuleBase+"/sdk/go"; the old path is a dead module, so
	//     an older committed `replace <private-base>/sdk/go => …` would both
	//     dangle AND, after the fresh render already emits the new path,
	//     duplicate it.
	// Every other operator-maintained dev-loop replace still survives regen.
	deadPaths := map[string]struct{}{
		core.SrcgoModuleBase + "/srcgo":  {},
		core.SrcgoModuleBase + "/sdk/go": {},
	}
	added := false
	for _, r := range existing.Replace {
		if _, ok := have[r.Old.Path]; ok {
			continue
		}
		if _, dead := deadPaths[r.Old.Path]; dead {
			continue
		}
		newPath, newVersion := r.New.Path, r.New.Version
		if err := newMod.AddReplace(r.Old.Path, r.Old.Version, newPath, newVersion); err != nil {
			return nil, fmt.Errorf("add replace %s: %w", r.Old.Path, err)
		}
		added = true
	}
	if !added {
		return newContents, nil
	}
	newMod.Cleanup()
	out, err := newMod.Format()
	if err != nil {
		return nil, fmt.Errorf("format merged go.mod: %w", err)
	}
	return out, nil
}

// readExistingPo gathers the project's current on-disk i18n catalogs under
// `<root>/<languagesDir>/` as upload payload — the server merges them with the
// freshly harvested scaffolds (translator msgstrs survive) and bakes the
// result into i18n.ts, so the .po merge runs server-side. Relative paths are
// project-root-relative (where the server stages them). Best-effort: a missing
// tree / unreadable file yields nothing (first codegen has no catalogs yet).
func readExistingPo(root, languagesDir string) []*codegenpb.GeneratedFile {
	if languagesDir == "" {
		return nil
	}
	base := filepath.Join(root, filepath.FromSlash(languagesDir))
	var out []*codegenpb.GeneratedFile
	_ = filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".po") {
			return nil
		}
		body, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return nil
		}
		out = append(out, &codegenpb.GeneratedFile{RelativePath: filepath.ToSlash(rel), Contents: body})
		return nil
	})
	return out
}

// containedJoin joins a SERVER-SUPPLIED relative op path under root, rejecting
// anything that would escape it (absolute, or a `..` traversal). The server is
// the trust boundary the client crosses on `--console`; a compromised/buggy
// console must not be able to drive an arbitrary-disk write — or, via the
// supersede-sweep deletes, an `os.RemoveAll` — outside the project root. Cheap
// defense-in-depth applied to every write + delete op before it touches disk.
func containedJoin(root, rel string) (string, error) {
	// Canonical guard in sdk/go/tooling/pathguard (shared with the server staging
	// boundary); wrapped here with the apply-context error message. It rejects
	// empty/absolute, `..` escapes, AND `.` (the root itself — an op resolving
	// to root would let RemoveAll wipe the whole project).
	dst, err := pathguard.Join(root, rel)
	if err != nil {
		return "", fmt.Errorf("server op path %q escapes or targets the project root", rel)
	}
	return dst, nil
}

// readE2eInputs gathers the project's current on-disk e2e tree under
// `<root>/<e2eDir>/` as upload payload — the hand-written stress presets + the
// existing test skeletons the server-side e2e generator reads to build the
// e2erunner (it stages only protos otherwise → a harness-less runner that
// won't compile). Relative paths are project-root-relative (where the server
// stages them). Best-effort: a missing tree yields nothing (no e2e surface).
func readE2eInputs(root, e2eDir string) []*codegenpb.ProtoFile {
	if e2eDir == "" {
		return nil
	}
	base := filepath.Join(root, filepath.FromSlash(e2eDir))
	var out []*codegenpb.ProtoFile
	_ = filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		body, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return nil
		}
		out = append(out, &codegenpb.ProtoFile{Filename: filepath.ToSlash(rel), Contents: body})
		return nil
	})
	return out
}

// defaultGenDir is the conventions-global Go layout default per
// `docs/conventions-global/structure.md`. The client passes it as the
// request's `gen_dir`; the server applies the lock's own `generated_code`
// override when set. (The lock's standalone `gen_dir` field was consolidated
// into the `GeneratedCode` block — lock.proto §69 — so the client no longer
// reads a per-project override here; it sends the convention.)
const defaultGenDir = "srcgo"

// w17StubsDir is the conventional Go stubs module dir (was lock.W17StubsDir; the
// client holds no lock package now). syncGoWork adds it to go.work `use`.
const w17StubsDir = "w17/stubs"

// Run is the package-level codegen entrypoint (invoked by the cmd/codegen
// kong command + stack build): it gathers the project's LOCAL inputs (proto
// tree, signed lock, go.module, go.mod dep-versions, the current gen Go tree,
// the co-dev env) and streams them to the console's
// GenerateProject RPC, which now owns ALL codegen orchestration server-side
// (surface detection, target derivation, composition, per-generator
// sequencing, file routing, the supersede-sweep — thin-client refactor Step 2).
// The client applies each streamed op under the project root with no
// derivation: write / write-if-missing / delete, plus the two LOCAL-disk
// merges it still owns (generated-go.mod replace preservation; the .po merge
// moved server-side) and go.work sync.
func Run(console string, force bool) error {
	// One window now covers the WHOLE server-side pipeline (pre-gen + main
	// Generate + every declared generator + scaffold + sweep), where the
	// former client gave each of those its own RPC deadline (60s main + 30s
	// per seam-D = a cumulative budget well past 300s). Size this for a large
	// project's full codegen incl. the docker-backed buf stub-gen, not a
	// single RPC.
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Second)
	defer cancel()

	root, err := findProjectRoot()
	if err != nil {
		return err
	}

	addr, err := core.ResolveConsoleAddr(console)
	if err != nil {
		return err
	}
	// Self-hosting codegen: dial console-app's gateway-rehosted Codegen
	// (`w17lock.console.rpc.Codegen`) — GenerateProject + DescribeLock now run
	// in the console, not the legacy cmd/console daemon (G-selfhost-codegen).
	cl, conn, err := core.DialCodegen(addr)
	if err != nil {
		return fmt.Errorf("dial %s: %w", addr, err)
	}
	defer func() { _ = conn.Close() }()

	// The raw signed lock.yaml IS the lock the client ships (uniform with every
	// Generate* RPC — proto-wire lock retired). It feeds GenerateProject and the
	// dir layout the client needs locally, which it reads back from the console's
	// DescribeLock projection (§8.2 — the client holds no lock types).
	lockYaml, err := os.ReadFile(filepath.Join(root, "w17", "lock.yaml"))
	if err != nil {
		return fmt.Errorf("codegen: read lock: %w", err)
	}
	dctx, dcancel := core.ClientCtx()
	view, err := cl.DescribeLock(dctx, &codegenpb.DescribeLockRequest{Lock: lockYaml})
	dcancel()
	if err != nil {
		return fmt.Errorf("codegen: describe lock: %w", err)
	}
	protoDir := view.GetProtoDir()
	servicesDir := view.GetServicesDir()
	languagesDir := view.GetLanguagesDir()

	genDir := defaultGenDir
	goModule := readGoModule(root, genDir)
	if goModule == "" && core.DomainsActivatePlugin(filepath.Join(root, protoDir, "domains")) {
		// readGoModule found no `module` line yet a domain activates a plugin —
		// the console would otherwise reject the request with the MISLEADING
		// "request.go_module is empty" error that blames the activation. Fail
		// here with the actionable cause.
		return fmt.Errorf("codegen: no Go module — create %s with a `module <path>` line "+
			"(e.g. `module example.com/<project>`), then re-run. `w17ctl init` scaffolds it for new projects",
			filepath.Join(root, genDir, "go.mod"))
	}

	files, err := readProtoTree(root, protoDir)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("no .proto files found under %s", filepath.Join(root, protoDir))
	}

	// dep_versions + the gen Go tree + wc_path are the ONLY inputs the server
	// can't see: the consumer's pinned library versions (read from the local
	// go.mod), the hand-written Go tree the project-map's cross-domain edges
	// parse, and the co-dev W17_WANDERING_COMPILER_PATH replace-dir env.
	depVersions, err := readDepVersions(root, genDir)
	if err != nil {
		return fmt.Errorf("read dep versions: %w", err)
	}
	// The gen-dir go.mod — the business-bundle generator reads its grpc pin +
	// sdk/go layout from it (more than dep_versions captures). Best-effort.
	genGoMod, _ := os.ReadFile(filepath.Join(root, genDir, "go.mod"))
	// The current on-disk .po catalogs — the server merges them with the
	// freshly harvested scaffolds (translator msgstrs survive) and bakes the
	// result into i18n.ts. The .po merge thus runs server-side now.
	existingPo := readExistingPo(root, languagesDir)

	stream, err := cl.GenerateProject(ctx, &codegenpb.GenerateProjectRequest{
		Files:       files,
		Lock:        lockYaml,
		GoModule:    goModule,
		GenDir:      genDir,
		ServicesDir: servicesDir,
		DepVersions: depVersions,
		WcPath:      strings.Trim(os.Getenv("W17_WANDERING_COMPILER_PATH"), "/"),
		Force:       force,
		GenFiles:    readGenGoFiles(root, genDir),
		LockYaml:    lockYaml,
		GenGoMod:    string(genGoMod),
		ExistingPo:  existingPo,
		E2EInputs:   readE2eInputs(root, view.GetE2EDir()),
	})
	if err != nil {
		return formatCodegenError(err)
	}

	// Buffer the whole op stream — the collision pre-scan (R-console-4) needs
	// the full write set before touching disk.
	var writes []*codegenpb.GeneratedFile
	var deletes, warnings []string
	for {
		op, recvErr := stream.Recv()
		if errors.Is(recvErr, io.EOF) {
			break
		}
		if recvErr != nil {
			// Flush the warnings collected so far BEFORE surfacing the error:
			// codegen streams advisories (e.g. "plugin X is active but its
			// surface is unpublished — add include:[...]") ahead of a later
			// stage that may hard-fail (e.g. an e2e test referencing that same
			// unpublished method). Dropping them here would send the developer
			// to chase the downstream error while hiding the line that fixes it.
			printCodegenWarnings(core.Stdout, warnings)
			return formatCodegenError(recvErr)
		}
		switch o := op.GetOp().(type) {
		case *codegenpb.GeneratedOp_Write:
			writes = append(writes, o.Write)
		case *codegenpb.GeneratedOp_Delete:
			deletes = append(deletes, o.Delete)
		case *codegenpb.GeneratedOp_Warning:
			warnings = append(warnings, o.Warning)
		}
	}

	// Snapshot the sdk/go pins BEFORE the write: the generator emits its
	// placeholder marker unconditionally, so once applyWriteOps lands, the
	// version the project had committed exists nowhere on disk. Without this,
	// a run that can't resolve leaves every generated module on the
	// placeholder — regressing a working tree (see resolveSdkGoPins).
	priorSdkPins := snapshotSdkGoPins(root,
		sdkGoModuleDirs(root, servicesDir, w17StubsDir, genDir),
		core.SdkModuleBase+"/sdk/go")

	if err := applyWriteOps(root, languagesDir, writes, force); err != nil {
		return err
	}

	// Vendor the admin SPA runtime (public @w17/admin-runtime, embedded in
	// this client) into <w17>/admin-runtime/ when this run emitted an admin
	// bundle — the generated spa/package.json + Dockerfile reference it there
	// via a local file: path. Delivered by the PUBLIC client, offline: no npm,
	// no private srcgo, no console round-trip. Not marker-bearing, so the
	// orphan prune below leaves it alone.
	if adminBundleEmitted(writes) {
		w17Root := "w17"
		if s := filepath.ToSlash(servicesDir); s != "" {
			w17Root = strings.SplitN(s, "/", 2)[0]
		}
		vendorRel := path.Join(w17Root, "admin-runtime")
		if err := adminruntime.WriteTo(filepath.Join(root, filepath.FromSlash(vendorRel))); err != nil {
			return fmt.Errorf("vendor admin runtime: %w", err)
		}
		fmt.Fprintf(core.Stdout, "vendored admin runtime → %s\n", vendorRel)
	}

	// The bundle `.env` is author-owned (generate-if-missing), so a regen that
	// adds/changes a key in the regenerated `.env.defaults` leaves the live
	// `.env` stale — the operator would silently run on old values. Surface the
	// divergence as a warning (the client owns the disk-diff the server can't).
	warnings = append(warnings, envDriftWarnings(root, writes)...)
	if len(writes) == 0 {
		fmt.Fprintln(core.Stdout, "no files generated")
	}
	// Deletes after writes (the server's supersede-sweep ran last too).
	// RemoveAll is idempotent: a path that never existed is a no-op, so the
	// disk-stat the server can't do is unnecessary here.
	for _, d := range deletes {
		target, err := containedJoin(root, d)
		if err != nil {
			return fmt.Errorf("refusing delete: %w", err)
		}
		// The server emits supersede-sweep deletes unconditionally (it can't
		// stat the client disk), so most are no-ops. Only announce a delete
		// that actually removed something — else every regen of a composed
		// bundle prints a phantom "removed" for a dir that never existed.
		existed := false
		if _, statErr := os.Stat(target); statErr == nil {
			existed = true
		}
		if err := os.RemoveAll(target); err != nil {
			return fmt.Errorf("remove superseded %s: %w", target, err)
		}
		if existed {
			fmt.Fprintf(core.Stdout, "removed %s\n", d)
		}
	}

	// Prune stale w17-generated files orphaned by a shrunk surface. Codegen
	// emits the COMPLETE generated set every run (no incremental delta), so a
	// marker-bearing file under a generated root that THIS run didn't emit is a
	// leftover from a prior surface (e.g. a removed service's src/rpc handler).
	// LOCAL: the stateless server can't diff against the client disk.
	kept := make(map[string]bool, len(writes))
	for _, f := range writes {
		kept[filepath.ToSlash(f.GetRelativePath())] = true
	}
	if n, pErr := pruneOrphans(root, view.GetCleanPaths(), kept, core.Stdout); pErr != nil {
		return fmt.Errorf("prune orphans: %w", pErr)
	} else if n > 0 {
		fmt.Fprintf(core.Stdout, "codegen: pruned %d stale generated file(s)\n", n)
	}

	// Add every generated bundle module + the stubs module to the project's
	// go.work and drop stale entries — LOCAL: go.work is a local file the
	// server never sees.
	if err := syncGoWork(root, servicesDir, w17StubsDir, core.Stdout); err != nil {
		return fmt.Errorf("sync go.work: %w", err)
	}
	// Pin the placeholder sdk/go require to a resolvable version so the generated
	// project builds immediately. Non-fatal: on a proxy/network hiccup the files
	// are already written, so advise the manual fix rather than fail codegen.
	if err := resolveSdkGoPins(root, servicesDir, w17StubsDir, genDir, priorSdkPins, core.Stdout); err != nil {
		fmt.Fprintf(core.Stdout, "codegen: warning: could not pin %s automatically (%v);\n"+
			"  set the version by hand in the project's go.mod files, or make a module\n"+
			"  proxy reachable (GOPROXY) and re-run codegen\n",
			core.SdkModuleBase+"/sdk/go", err)
	}
	// Refresh the platform reference (w17/specs/*) while the console is on
	// the line. It describes the COMPILER's output, so it goes stale exactly
	// when the compiler moves — and the loop that follows the compiler runs
	// CODEGEN, not `w17ctl guide`, which is the only thing that used to
	// refresh it. Non-fatal: the generated code is already on disk, and a
	// stale reference must not fail a good codegen.
	if n, gerr := refreshPlatformSpecs(cl, root); gerr != nil {
		fmt.Fprintf(core.Stdout, "codegen: note: could not refresh w17/specs/ (the platform reference): %v\n", gerr)
	} else if n > 0 {
		fmt.Fprintf(core.Stdout, "refreshed w17/specs/ (%d file(s)) — the platform reference for this compiler\n", n)
	}
	// Non-fatal advisories last — generation already succeeded.
	printCodegenWarnings(core.Stdout, warnings)
	return nil
}

// sdkGoModuleDirs lists the project-root-relative module dirs that can carry an
// sdk/go require: the hand-written module, every generated bundle, and the
// w17/stubs module.
func sdkGoModuleDirs(root, servicesDir, w17Stubs, genDir string) []string {
	var dirs []string
	if genDir != "" {
		dirs = append(dirs, genDir)
	}
	if servicesDir != "" {
		entries, _ := os.ReadDir(filepath.Join(root, servicesDir))
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			if _, err := os.Stat(filepath.Join(root, servicesDir, e.Name(), "go.mod")); err == nil {
				dirs = append(dirs, filepath.Join(servicesDir, e.Name()))
			}
		}
	}
	if w17Stubs != "" {
		dirs = append(dirs, w17Stubs)
	}
	return dirs
}

// snapshotSdkGoPins records the RESOLVED sdk/go version each module currently
// pins, keyed by project-relative dir. Callers take this BEFORE writing the
// generator's output, because the generator emits the placeholder marker
// unconditionally — once written, the previously committed pin is gone from
// disk and only this snapshot can put it back. Placeholders are not recorded
// (there is nothing to preserve).
func snapshotSdkGoPins(root string, dirs []string, sdkMod string) map[string]string {
	out := map[string]string{}
	for _, d := range dirs {
		if v := sdkGoRequiredVersion(filepath.Join(root, d, "go.mod"), sdkMod); v != "" && !isPlaceholderVersion(v) {
			out[d] = v
		}
	}
	return out
}

// sdkGoRequiredVersion returns the version the go.mod at path requires sdkMod
// at ("" when absent/unparseable).
func sdkGoRequiredVersion(goModPath, sdkMod string) string {
	data, err := os.ReadFile(goModPath)
	if err != nil {
		return ""
	}
	f, err := modfile.Parse(goModPath, data, nil)
	if err != nil {
		return ""
	}
	for _, r := range f.Require {
		if r.Mod.Path == sdkMod {
			return r.Mod.Version
		}
	}
	return ""
}

// sdkVersionFromModules returns the first RESOLVED sdk/go version already
// present in the project ("" when every module carries a placeholder). This is
// the offline fast path: the hand-written module keeps its pin across a regen
// (the generator never rewrites it), so a project that has been pinned once
// re-pins itself from its own disk — no toolchain, no network, deterministic,
// and consistent with what is committed.
func sdkVersionFromModules(root string, dirs []string, sdkMod string) string {
	for _, d := range dirs {
		if v := sdkGoRequiredVersion(filepath.Join(root, d, "go.mod"), sdkMod); v != "" && !isPlaceholderVersion(v) {
			return v
		}
	}
	return ""
}

// writeSdkPin rewrites the go.mod at path to require sdkMod at ver, textually
// via modfile. Deliberately NOT `go mod edit`: w17ctl must not depend on a Go
// toolchain being installed (and shelling out re-introduced the bug this
// replaced — a toolchain-download line on the command's output being parsed as
// data). modfile is the same library `go mod edit` uses.
func writeSdkPin(goModPath, sdkMod, ver string) error {
	data, err := os.ReadFile(goModPath)
	if err != nil {
		return err
	}
	f, err := modfile.Parse(goModPath, data, nil)
	if err != nil {
		return fmt.Errorf("parse %s: %w", goModPath, err)
	}
	if err := f.AddRequire(sdkMod, ver); err != nil {
		return fmt.Errorf("set require in %s: %w", goModPath, err)
	}
	f.Cleanup()
	out, err := f.Format()
	if err != nil {
		return fmt.Errorf("format %s: %w", goModPath, err)
	}
	return os.WriteFile(goModPath, out, 0o644)
}

// resolveSdkGoPins replaces the placeholder `<sdk/go>` require the generator
// emits in every bundle + the w17/stubs module with a resolvable
// pseudo-version, so a freshly generated project builds without a manual
// `go get` / `go mod tidy`. Go forbids a literal `latest` in go.mod and the
// stateless generator can't know the published HEAD commit, so it emits a
// placeholder marker; the client — the only place that can see THIS project's
// disk and reach the module proxy — resolves it here.
//
// Resolution order, cheapest and most faithful first:
//
//  1. A version the project ALREADY knows (any module whose sdk/go require is
//     resolved — typically the hand-written module, which the generator never
//     rewrites). Offline and deterministic.
//  2. The `prior` snapshot taken before this run's write, so a run that can't
//     reach anything still restores what was committed.
//  3. The module proxy over plain HTTP (GOPROXY), for a project that has never
//     been pinned.
//
// No step shells out to `go`: a consumer must not need a local Go toolchain to
// run codegen. Steps 1-2 are also why a proxy outage can no longer regress a
// committed pin — the failure mode that shipped `v0.0.0-00010101000000-…` into
// 7 of 8 modules and broke `stack build`.
//
// Skipped in co-dev mode (W17_WANDERING_COMPILER_PATH set): those go.mods carry a
// local `replace … => <checkout>/sdk/go` that already resolves the placeholder
// against the live monorepo checkout, and re-pinning would churn the committed
// co-dev go.mods for no gain (sdkGoNeedsPin also skips any module that replaces
// sdk/go, as a second guard).
func resolveSdkGoPins(root, servicesDir, w17Stubs, genDir string, prior map[string]string, stdout io.Writer) error {
	if strings.Trim(os.Getenv("W17_WANDERING_COMPILER_PATH"), "/") != "" {
		return nil // co-dev: the local replace resolves the placeholder
	}
	sdkMod := core.SdkModuleBase + "/sdk/go"
	dirs := sdkGoModuleDirs(root, servicesDir, w17Stubs, genDir)

	// Keep only modules carrying an unresolvable sdk/go placeholder with no local
	// replace overriding it.
	var targets []string
	for _, d := range dirs {
		if sdkGoNeedsPin(filepath.Join(root, d, "go.mod"), sdkMod) {
			targets = append(targets, d)
		}
	}
	if len(targets) == 0 {
		return nil
	}

	ver, how := sdkVersionFromModules(root, dirs, sdkMod), "project"
	if ver == "" {
		if ver = priorPin(prior, targets); ver != "" {
			how = "prior pin"
		}
	}
	if ver == "" {
		v, err := latestVersionFromProxy(context.Background(), sdkMod)
		if err != nil {
			return err
		}
		ver, how = v, "proxy"
	}
	for _, d := range targets {
		// Prefer this module's own prior pin over a project-wide answer, so a
		// deliberately divergent module isn't silently unified.
		v := ver
		if p, ok := prior[filepath.ToSlash(d)]; ok && how != "project" {
			v = p
		}
		if err := writeSdkPin(filepath.Join(root, d, "go.mod"), sdkMod, v); err != nil {
			return fmt.Errorf("pin in %s: %w", d, err)
		}
	}
	fmt.Fprintf(stdout, "codegen: pinned %s %s in %d module(s) (from %s)\n", sdkMod, ver, len(targets), how)
	return nil
}

// priorPin returns the snapshotted version for one of targets ("" when the
// snapshot knows none of them).
func priorPin(prior map[string]string, targets []string) string {
	for _, d := range targets {
		if v, ok := prior[filepath.ToSlash(d)]; ok && v != "" {
			return v
		}
	}
	return ""
}

// sdkGoNeedsPin reports whether the go.mod at path requires sdkMod at an
// unresolvable placeholder version (`v0.0.0` or the zero pseudo-version) and
// carries no `replace` redirecting sdkMod to a local checkout.
func sdkGoNeedsPin(goModPath, sdkMod string) bool {
	data, err := os.ReadFile(goModPath)
	if err != nil {
		return false
	}
	f, err := modfile.Parse(goModPath, data, nil)
	if err != nil {
		return false
	}
	for _, r := range f.Replace {
		if r.Old.Path == sdkMod {
			return false // co-dev / operator replace owns resolution
		}
	}
	for _, r := range f.Require {
		if r.Mod.Path == sdkMod {
			return isPlaceholderVersion(r.Mod.Version)
		}
	}
	return false
}

// isPlaceholderVersion matches the two markers the generator emits for a
// not-yet-resolved sdk/go require: the bare `v0.0.0` and the zero pseudo-version
// `v0.0.0-00010101000000-000000000000`.
func isPlaceholderVersion(v string) bool {
	return v == "v0.0.0" || strings.HasPrefix(v, "v0.0.0-00010101000000")
}

// parseModuleVersion extracts a version from command/HTTP output that may
// carry leading noise. It takes the LAST non-blank line and requires it to be
// valid semver.
//
// Both halves are load-bearing. The Go toolchain interleaves progress
// ("go: downloading go1.26.1 (linux/amd64)") with the value it was asked for;
// taking the whole output as the version concatenated that line into the
// version string, which then flowed into a go.mod require. Validating the
// result means anything unexpected fails LOUDLY here instead of being written
// to disk as a corrupt pin.
func parseModuleVersion(out []byte) (string, error) {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		v := strings.TrimSpace(lines[i])
		if v == "" {
			continue
		}
		if !semver.IsValid(v) {
			return "", fmt.Errorf("not a version: %q", v)
		}
		return v, nil
	}
	return "", fmt.Errorf("empty version output")
}

// goProxyBase returns the first usable proxy URL from GOPROXY ("" when the
// module proxy is disabled or only `direct` is configured — w17ctl speaks HTTP
// to a proxy, it does not implement VCS fetching).
func goProxyBase() string {
	gp := strings.TrimSpace(os.Getenv("GOPROXY"))
	if gp == "" {
		gp = "https://proxy.golang.org,direct"
	}
	for _, p := range strings.FieldsFunc(gp, func(r rune) bool { return r == ',' || r == '|' }) {
		p = strings.TrimSpace(p)
		switch p {
		case "", "off", "direct", "noproxy":
			continue
		}
		return strings.TrimRight(p, "/")
	}
	return ""
}

// latestVersionFromProxy resolves `<mod>@latest` to a concrete version over
// plain HTTP against GOPROXY.
//
// Deliberately not `go list -m <mod>@latest`: that requires a local Go
// toolchain, which a w17 consumer must not need (and the client is already the
// process that owns transport). This is the last-resort arm — a project that
// has been pinned once resolves from its own disk and never gets here.
func latestVersionFromProxy(ctx context.Context, mod string) (string, error) {
	base := goProxyBase()
	if base == "" {
		return "", fmt.Errorf("resolve %s@latest: no usable GOPROXY (set GOPROXY, or pin the version in the project's go.mod)", mod)
	}
	esc, err := module.EscapePath(mod)
	if err != nil {
		return "", fmt.Errorf("resolve %s@latest: %w", mod, err)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/"+esc+"/@latest", nil)
	if err != nil {
		return "", fmt.Errorf("resolve %s@latest: %w", mod, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("resolve %s@latest: %w", mod, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("resolve %s@latest: proxy returned %s", mod, resp.Status)
	}
	var info struct{ Version string }
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&info); err != nil {
		return "", fmt.Errorf("resolve %s@latest: decode proxy response: %w", mod, err)
	}
	return parseModuleVersion([]byte(info.Version))
}

// envDriftWarnings compares each bundle's live on-disk `.env` against the
// `.env.defaults` regenerated in the SAME run and returns a warning per
// bundle whose live `.env` is missing keys the defaults now carry. The `.env`
// is generate-if-missing (author-owned) so codegen never overwrites it; this
// is the non-destructive nudge that a new/renamed setting (e.g. a freshly
// added `<PREFIX>_BUSINESS_BACKEND_ADDR`) hasn't reached the live file, which
// otherwise surfaces only as a runtime failure. Values are never compared —
// only key presence — so operator-tuned values never trip it.
// adminBundleEmitted reports whether the write set includes an admin
// bundle's SPA package.json — the signal that this project needs the
// vendored @w17/admin-runtime dropped alongside it.
func adminBundleEmitted(writes []*codegenpb.GeneratedFile) bool {
	for _, f := range writes {
		if strings.Contains(filepath.ToSlash(f.GetRelativePath()), "-admin/spa/package.json") {
			return true
		}
	}
	return false
}

func envDriftWarnings(root string, writes []*codegenpb.GeneratedFile) []string {
	defaultsByDir := map[string][]byte{}
	for _, f := range writes {
		rel := filepath.ToSlash(f.GetRelativePath())
		if path.Base(rel) == ".env.defaults" {
			defaultsByDir[path.Dir(rel)] = f.GetContents()
		}
	}
	var out []string
	for _, f := range writes {
		rel := filepath.ToSlash(f.GetRelativePath())
		if path.Base(rel) != ".env" {
			continue
		}
		defaults, ok := defaultsByDir[path.Dir(rel)]
		if !ok {
			continue
		}
		target, err := containedJoin(root, rel)
		if err != nil {
			continue
		}
		live, err := os.ReadFile(target)
		if err != nil {
			continue // no live .env (fresh) → applyWriteOps writes it; no drift
		}
		liveKeys := envKeys(live)
		var missing []string
		for k := range envKeys(defaults) {
			if !liveKeys[k] {
				missing = append(missing, k)
			}
		}
		if len(missing) == 0 {
			continue
		}
		sort.Strings(missing)
		out = append(out, fmt.Sprintf(
			"%s is missing key(s) present in .env.defaults: %s — add them, or delete %s to regenerate it from defaults",
			rel, strings.Join(missing, ", "), rel))
	}
	sort.Strings(out)
	return out
}

// envKeys extracts the KEY names from a dotenv body (KEY=VALUE lines),
// skipping blanks, comments, and `export ` prefixes. Value-agnostic.
func envKeys(b []byte) map[string]bool {
	keys := map[string]bool{}
	for _, ln := range strings.Split(string(b), "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" || strings.HasPrefix(ln, "#") {
			continue
		}
		ln = strings.TrimPrefix(ln, "export ")
		eq := strings.IndexByte(ln, '=')
		if eq <= 0 {
			continue
		}
		if k := strings.TrimSpace(ln[:eq]); k != "" {
			keys[k] = true
		}
	}
	return keys
}

// applyWriteOps writes each streamed write op under the project root. The
// server already routed every op to its FINAL project-root-relative path, so
// the client only joins root + applies the LOCAL-disk semantics it still owns:
// generate-if-missing, the non-force collision pre-scan (buffer-then-write,
// ZERO side effects on conflict), and per-bundle go.mod replace preservation
// (the .po merge moved server-side). Two-pass (plan then write) so a collision
// aborts with the full conflict list and never a half-regenerated tree.
func applyWriteOps(root, languagesDir string, writes []*codegenpb.GeneratedFile, force bool) error {
	type plannedWrite struct {
		target   string
		contents []byte
	}
	var planned []plannedWrite
	var collisions []string
	langPrefix := strings.TrimSuffix(languagesDir, "/") + "/"
	for _, f := range writes {
		rel := filepath.ToSlash(f.GetRelativePath())
		target, err := containedJoin(root, rel)
		if err != nil {
			return fmt.Errorf("refusing write: %w", err)
		}
		// .po catalogs carry translator edits — never collide / abort on them.
		// The server already merged them (scaffold + the existing catalog the
		// client uploaded), so the op carries the FINAL merged body; the client
		// just overwrites the on-disk file with it.
		isPoCatalog := strings.HasPrefix(rel, langPrefix) && strings.HasSuffix(rel, ".po")
		// The committed lock.yaml comes back re-signed from the server (the
		// console is the signer — public-split boundary §4). Like .po, the op
		// carries the authoritative body, so it always overwrites: the client
		// uploaded it, the server re-signed it, the client writes it back (and,
		// once the client stops signing, this is how the lock gets signed at all).
		isLock := rel == "w17/lock.yaml"
		// Generate-if-missing files are written ONCE then owned by the author —
		// never overwritten, even with --force (delete to regenerate).
		if f.GetIfMissing() {
			if _, statErr := os.Stat(target); statErr == nil {
				continue
			}
		}
		if !isPoCatalog && !isLock && !force {
			if _, statErr := os.Stat(target); statErr == nil {
				collisions = append(collisions, target)
				continue
			}
		}
		contents := f.GetContents()
		// Every generated go.mod (bundle, e2erunner, stubs, …) may carry
		// operator-maintained `replace` directives the server has no
		// project-extrinsic knowledge of (co-dev paths to the project srcgo +
		// the wandering-compiler checkout) — splice them back in from the
		// existing file so regen preserves them. Codegen never emits the
		// project's own go.mod, so every go.mod write op is a generated module.
		// Merged HERE in the planning pass (not the write pass) so a malformed
		// existing go.mod aborts BEFORE any file is written — preserving the
		// all-or-nothing guarantee below.
		if filepath.Base(rel) == "go.mod" {
			merged, mErr := mergeGoModReplaces(contents, target)
			if mErr != nil {
				return fmt.Errorf("preserve replace directives in %s: %w", target, mErr)
			}
			contents = merged
		}
		planned = append(planned, plannedWrite{target: target, contents: contents})
	}
	if len(collisions) > 0 {
		sort.Strings(collisions)
		return fmt.Errorf("%d target file(s) already exist (use --force to overwrite):\n  %s",
			len(collisions), strings.Join(collisions, "\n  "))
	}
	// Write pass — every body is final (go.mod already merged), so nothing here
	// can fail on input parsing: a half-regenerated tree is reachable only via a
	// filesystem error (mkdir/write), not a malformed input.
	for _, p := range planned {
		if err := os.MkdirAll(filepath.Dir(p.target), 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", filepath.Dir(p.target), err)
		}
		mode := os.FileMode(0o644)
		if filepath.Ext(p.target) == ".sh" {
			mode = 0o755
		}
		if err := os.WriteFile(p.target, p.contents, mode); err != nil {
			return fmt.Errorf("write %s: %w", p.target, err)
		}
		fmt.Fprintf(core.Stdout, "wrote %s (%d bytes)\n", p.target, len(p.contents))
	}
	return nil
}

// syncGoWork ensures every generated bundle module (each
// `<servicesDir>/<bundle>/go.mod` + the `<w17Stubs>/go.mod` stubs
// module) appears in the project's go.work `use` block. Idempotent:
// only missing entries are appended, existing ones (incl. the
// hand-written tier + co-dev runtime modules) are preserved. A project
// with no go.work is left alone — `w17ctl init` owns its creation; this
// only keeps an existing one current.
func syncGoWork(root, servicesDir, w17Stubs string, stdout io.Writer) error {
	workPath := filepath.Join(root, "go.work")
	data, err := os.ReadFile(workPath)
	if err != nil {
		return nil // no go.work (e.g. published-module build) — nothing to keep in sync
	}

	// Collect generated module dirs (relative to root, slash-form +
	// "./"-prefixed to match go.work use syntax).
	var mods []string
	if servicesDir != "" {
		entries, _ := os.ReadDir(filepath.Join(root, servicesDir))
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			if _, statErr := os.Stat(filepath.Join(root, servicesDir, e.Name(), "go.mod")); statErr == nil {
				mods = append(mods, "./"+filepath.ToSlash(filepath.Join(servicesDir, e.Name())))
			}
		}
	}
	if w17Stubs != "" {
		if _, statErr := os.Stat(filepath.Join(root, w17Stubs, "go.mod")); statErr == nil {
			mods = append(mods, "./"+filepath.ToSlash(w17Stubs))
		}
	}
	if len(mods) == 0 {
		return nil
	}

	wf, err := modfile.ParseWork(workPath, data, nil)
	if err != nil {
		return fmt.Errorf("parse %s: %w", workPath, err)
	}
	existing := map[string]bool{}
	for _, u := range wf.Use {
		existing[u.Path] = true
	}
	added := 0
	for _, m := range mods {
		if existing[m] {
			continue
		}
		if err := wf.AddUse(m, ""); err != nil {
			return fmt.Errorf("go.work add use %s: %w", m, err)
		}
		existing[m] = true
		added++
	}

	// Reconcile: drop `use` entries under servicesDir whose bundle dir no
	// longer has a go.mod — e.g. a bundle removed by a composition-mode
	// change (sweepSupersededGatewayBundles). Only servicesDir-prefixed
	// entries are candidates; the hand-written tier, co-dev runtime
	// modules, the e2e tree, and the stubs module are never touched.
	removed := 0
	if servicesDir != "" {
		valid := map[string]bool{}
		for _, m := range mods {
			valid[m] = true
		}
		servicesPrefix := "./" + filepath.ToSlash(servicesDir) + "/"
		var stale []string
		for _, u := range wf.Use {
			if strings.HasPrefix(u.Path, servicesPrefix) && !valid[u.Path] {
				stale = append(stale, u.Path)
			}
		}
		for _, p := range stale {
			if err := wf.DropUse(p); err != nil {
				return fmt.Errorf("go.work drop use %s: %w", p, err)
			}
			removed++
		}
	}

	if added == 0 && removed == 0 {
		return nil
	}
	wf.Cleanup()
	if err := os.WriteFile(workPath, modfile.Format(wf.Syntax), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", workPath, err)
	}
	switch {
	case added > 0 && removed > 0:
		fmt.Fprintf(stdout, "codegen: go.work — added %d, dropped %d stale module(s)\n", added, removed)
	case removed > 0:
		fmt.Fprintf(stdout, "codegen: go.work — dropped %d stale module(s)\n", removed)
	default:
		fmt.Fprintf(stdout, "codegen: go.work — added %d generated module(s)\n", added)
	}
	return nil
}

// printCodegenWarnings renders the server's non-fatal advisories as
// a prominent boxed ⚠ banner. No-op when there are none.
func printCodegenWarnings(w io.Writer, warnings []string) {
	if len(warnings) == 0 {
		return
	}
	const rule = "────────────────────────────────────────────────────────────────────"
	fmt.Fprintf(w, "\n⚠  CODEGEN WARNING(S): %d\n%s\n", len(warnings), rule)
	for i, msg := range warnings {
		if i > 0 {
			fmt.Fprintf(w, "%s\n", rule)
		}
		for _, line := range strings.Split(strings.TrimRight(msg, "\n"), "\n") {
			fmt.Fprintf(w, "  %s\n", line)
		}
	}
	fmt.Fprintf(w, "%s\n", rule)
}

// readDepVersionsFn is a package var production code keeps at
// realReadDepVersions; tests override to inject synthetic
// versions without touching disk.
var readDepVersionsFn = realReadDepVersions

func readDepVersions(root, genDir string) (*codegenpb.DepVersions, error) {
	return readDepVersionsFn(root, genDir)
}

// realReadDepVersions resolves the library versions templated
// into each per-bundle go.mod's `require` block. These are the
// w17 *ecosystem* libraries the generated bundles import (the
// pq / redis / chi / nats drivers + grpc/protobuf) — they belong
// to the TOOL, not the consumer's project, so a consumer never
// has to pin them in its own manifest to make codegen work.
//
// Resolution mirrors e2egen: the consumer's `<root>/<genDir>/go.mod`
// is consulted FIRST (so a project CAN override a version if it
// genuinely wants to), then the wandering-compiler's own
// `srcgo/go.mod` is the authoritative fallback for anything the
// consumer didn't pin. In co-dev the tool is located via
// W17_WANDERING_COMPILER_PATH (project-root-relative, same env the
// replace-directive plumbing uses). The generator still never
// guesses versions from training data
// (`feedback_verify_tool_versions`) — every value flows from a
// real manifest, just the tool's rather than demanding the
// consumer mirror it.
//
// Off-repo correctness does NOT depend on this client-side fallback:
// the compiler (server) backfills every version the request leaves
// unset from its OWN embedded manifest (compiler_versions_embed.go), so
// a standalone w17ctl with no W17_WANDERING_COMPILER_PATH can send empty
// pins for libraries the project hasn't adopted yet (e.g. a fresh
// Postgres project with no lib/pq pin) and codegen still succeeds. This
// client read only lets a project OVERRIDE a tool version it genuinely
// wants to pin itself; the server's `pick(client, compiler)` merge keeps
// that override winning.
func realReadDepVersions(root, genDir string) (*codegenpb.DepVersions, error) {
	primary, err := parseGoModRequires(filepath.Join(root, genDir, "go.mod"))
	if err != nil {
		return nil, err
	}
	var secondary map[string]string
	if wcPath := strings.Trim(os.Getenv("W17_WANDERING_COMPILER_PATH"), "/"); wcPath != "" {
		// Best-effort: a missing/unreadable tool go.mod just leaves
		// the fallback empty (the validator surfaces any still-unset
		// required version with a clear message).
		secondary, _ = parseGoModRequires(filepath.Join(root, wcPath, "srcgo", "go.mod"))
	}
	pick := func(mod string) string {
		if v := primary[mod]; v != "" {
			return v
		}
		return secondary[mod]
	}
	return &codegenpb.DepVersions{
		Grpc:           pick("google.golang.org/grpc"),
		Protobuf:       pick("google.golang.org/protobuf"),
		LibPq:          pick("github.com/lib/pq"),
		GoMysql:        pick("github.com/go-sql-driver/mysql"),
		ModerncSqlite:  pick("modernc.org/sqlite"),
		GoRedis:        pick("github.com/redis/go-redis/v9"),
		OtelGrpc:       pick("go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"),
		CoderWebsocket: pick("github.com/coder/websocket"),
		GoChi:          pick("github.com/go-chi/chi/v5"),
		NatsGo:         pick("github.com/nats-io/nats.go"),
	}, nil
}

// parseGoModRequires reads a go.mod and returns a map of
// `<module> → <version>` for every entry inside the file's
// `require` blocks (single-line or grouped). Skips not-exist
// (returns empty map) so projects without a go.mod don't
// crash the codegen — RenderGoMod's validator surfaces the
// missing fields downstream with a precise error.
func parseGoModRequires(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}, nil
		}
		return nil, err
	}
	out := map[string]string{}
	inBlock := false
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if i := strings.Index(line, "//"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		switch {
		case line == "require (":
			inBlock = true
			continue
		case line == ")" && inBlock:
			inBlock = false
			continue
		case strings.HasPrefix(line, "require "):
			fields := strings.Fields(strings.TrimPrefix(line, "require "))
			if len(fields) >= 2 {
				out[fields[0]] = fields[1]
			}
		case inBlock:
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				out[fields[0]] = fields[1]
			}
		}
	}
	return out, nil
}

// findProjectRoot / readProtoTree forward to internal/core (the
// shared infra layer). They stay as package-local shims so the many
// in-package callers keep compiling during the cmd/internal layering
// migration; new cmd/* and internal/* packages call core directly.
func findProjectRoot() (string, error) { return core.FindProjectRoot() }

func readProtoTree(root, protoDir string) ([]*codegenpb.ProtoFile, error) {
	return core.ReadProtoTree(root, protoDir)
}

// formatCodegenError extracts the typed CodegenError detail (if
// present) from a gRPC status and renders a multi-line error
// the user can act on. Falls back to the bare gRPC message
// when no typed detail is attached.
func formatCodegenError(err error) error {
	st, ok := status.FromError(err)
	if !ok {
		return err
	}
	for _, d := range st.Details() {
		ce, ok := d.(*codegenpb.CodegenError)
		if !ok {
			continue
		}
		var b strings.Builder
		fmt.Fprintf(&b, "codegen %s [%s]: %s", st.Code(), stageLabel(ce.GetStage()), st.Message())
		if ce.GetFilename() != "" {
			fmt.Fprintf(&b, "\n  in %s", ce.GetFilename())
			if ce.GetServiceName() != "" {
				fmt.Fprintf(&b, " · %s", ce.GetServiceName())
				if ce.GetMethodName() != "" {
					fmt.Fprintf(&b, ".%s", ce.GetMethodName())
				}
			}
		}
		for _, diag := range ce.GetDiagnostics() {
			if diag.GetMessage() != "" {
				fmt.Fprintf(&b, "\n  - %s", diag.GetMessage())
			}
			if diag.GetWhy() != "" {
				fmt.Fprintf(&b, "\n      why: %s", diag.GetWhy())
			}
			if diag.GetFix() != "" {
				fmt.Fprintf(&b, "\n      fix: %s", diag.GetFix())
			}
		}
		return fmt.Errorf("%s", b.String())
	}
	return err
}

// stageLabel renders the Stage enum in human-friendly form for
// CLI output.
func stageLabel(s codegenpb.Stage) string {
	name := s.String()
	name = strings.TrimPrefix(name, "STAGE_")
	return strings.ToLower(name)
}

// readGoModule forwards to internal/core (shared infra). Kept as a
// package-local shim during the cmd/internal layering migration.
func readGoModule(root, genDir string) string { return core.ReadGoModule(root, genDir) }

// DialectFromConnectionName picks the dialect from the
// connection-name `<domain>-<dialect>` suffix — the same
// convention the connection name follows everywhere (the
// dialect itself is declared in proto's (w17.module).connection,
// never stored in the lock). Excludes nats / s3, which don't
// appear in compose.w17.yaml as per-connection services (nats
// has its own block, s3 is versionless managed infrastructure).
func DialectFromConnectionName(name string) (string, bool) {
	for _, suffix := range []string{"postgres", "mysql", "redis"} {
		if strings.HasSuffix(name, "-"+suffix) {
			return suffix, true
		}
	}
	return "", false
}

// ConnectionDomain returns the segment before the first `-`
// in a `<domain>-<role>` connection name (e.g. "forge-postgres"
// → "forge"). Used for the PG init-dir mount path + the
// default user/db credentials in the dev compose.
func ConnectionDomain(name string) string {
	if i := strings.IndexByte(name, '-'); i > 0 {
		return name[:i]
	}
	return name
}

// generatedMarkerRe matches the Go-convention "generated code" banner
// (`// Code generated <tool>. DO NOT EDIT.`) in any comment style — w17's
// own variants (`Code generated by w17 / w17ctl / wandering-compiler …`) and
// the protoc-gen-go pb banner all share the `Code generated … DO NOT EDIT`
// shape. The leading `.{0,16}` bounds the match to a comment prefix at line
// start (`//`, `#`, `--`, `<!--`, `/*` + space), so prose that merely mentions
// the phrase mid-line never matches.
var generatedMarkerRe = regexp.MustCompile(`(?m)^.{0,16}Code generated .*DO NOT EDIT`)

// hasGeneratedMarker reports whether the file's head carries the
// generated-code banner. Only the first 4 KiB is read — the banner is a
// top-of-file convention. An unreadable file is treated as unmarked (never
// pruned).
func hasGeneratedMarker(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	head := make([]byte, 4096)
	n, _ := f.Read(head)
	return generatedMarkerRe.Match(head[:n])
}

// pruneOrphans deletes stale w17-generated files left behind when a surface
// shrinks (a removed service, a renamed generated file). Codegen emits the
// COMPLETE generated set every run, so any file under a generated root that
// (1) the current run did NOT emit and (2) still carries the w17 generated-code
// marker is an orphan from a prior surface. All THREE guards — generated-root
// scope, absent-from-write-set, generated-marker — must hold before a file is
// removed, so hand-written facade code (lives in srcgo/, never under a generated
// root), locally produced artifacts (go.sum / go.work.sum — no marker), and
// write-if-missing scaffolds (in the write set) are never touched. Returns the
// number of files removed.
func pruneOrphans(root string, roots []string, kept map[string]bool, stdout io.Writer) (int, error) {
	removed := 0
	for _, r := range roots {
		// The clean roots are SERVER-SUPPLIED (view.GetCleanPaths()); guard each
		// before walking + removing under it, consistent with the explicit
		// delete ops above. ValidateRel permits `.` (the project root is a
		// legitimate clean root) but rejects `..`/absolute escapes, so the
		// marker-gated os.Remove sweep can never reach outside the project.
		if err := pathguard.ValidateRel(r); err != nil {
			return removed, fmt.Errorf("server clean-root %q escapes the project root: %w", r, err)
		}
		base := filepath.Join(root, filepath.FromSlash(r))
		info, err := os.Stat(base)
		if err != nil || !info.IsDir() {
			continue
		}
		walkErr := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			rel, relErr := filepath.Rel(root, p)
			if relErr != nil {
				return nil
			}
			relSlash := filepath.ToSlash(rel)
			if kept[relSlash] || !hasGeneratedMarker(p) {
				return nil
			}
			if rmErr := os.Remove(p); rmErr != nil {
				return fmt.Errorf("prune orphan %s: %w", relSlash, rmErr)
			}
			removed++
			fmt.Fprintf(stdout, "pruned stale %s\n", relSlash)
			return nil
		})
		if walkErr != nil {
			return removed, walkErr
		}
	}
	return removed, nil
}
