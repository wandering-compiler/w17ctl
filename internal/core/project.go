package core

import (
	"bufio"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/wandering-compiler/w17ctl/internal/lockfile"
	codegenpb "github.com/wandering-compiler/sdk/go/pb/w17compiler"
)

// LockProjectIDBestEffort reads project_id from <root>/w17/lock.yaml via the
// offline lockfile reader (no console, no lockpb). Returns "" on any failure —
// the best-effort projectID fallback for commands that take an explicit
// --project / W17_PROJECT_ID first.
func LockProjectIDBestEffort() string {
	root, err := FindProjectRoot()
	if err != nil {
		return ""
	}
	lk, err := lockfile.Load(filepath.Join(root, "w17", "lock.yaml"))
	if err != nil {
		return ""
	}
	return lk.ProjectID
}

// LockOrgIDBestEffort reads org_id from the project's lock.
//
// This is what makes the DIRECTORY decide which organization a command acts
// in. The alternative — and what happened before — is one default per console
// in ~/.w17/auth.yaml, shared by every checkout on the machine: two projects
// in two organizations meant remembering to switch, and forgetting was silent,
// because the console validates the header against MEMBERSHIP rather than
// against the project. Someone in both organizations was not refused; the
// write simply landed in the other one.
//
// Best-effort by design: outside a project, or on a lock the console has not
// stamped yet, the answer is "" and the caller falls back to the machine
// default. A command run outside any project still has to work.
func LockOrgIDBestEffort() string {
	root, err := FindProjectRoot()
	if err != nil {
		return ""
	}
	lk, err := lockfile.Load(filepath.Join(root, "w17", "lock.yaml"))
	if err != nil {
		return ""
	}
	return lk.OrgID
}

// FindProjectRootFn defaults to the real walk-up; tests override it.
var FindProjectRootFn = realFindProjectRoot

// FindProjectRoot returns the project root (the directory containing a
// `w17/` directory), walking up from the current working directory.
func FindProjectRoot() (string, error) { return FindProjectRootFn() }

func realFindProjectRoot() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("getwd: %w", err)
	}
	dir := cwd
	for {
		marker := filepath.Join(dir, "w17")
		if info, err := os.Stat(marker); err == nil && info.IsDir() {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("not inside a w17 project (no w17/ directory found walking up from %s)", cwd)
		}
		dir = parent
	}
}

// ReadProtoTreeFn lets tests inject a synthetic file set.
var ReadProtoTreeFn = realReadProtoTree

// ReadProtoTree walks `<root>/<protoDir>` and returns every `.proto`
// file's contents keyed by its proto-root-relative wire filename.
func ReadProtoTree(root, protoDir string) ([]*codegenpb.ProtoFile, error) {
	return ReadProtoTreeFn(root, protoDir)
}

// realReadProtoTree walks `<root>/<protoDir>` recursively and returns
// every `.proto` file's contents + path-relative-to-proto-root. The
// relative path becomes the wire filename.
//
// Under `<protoDir>/plugins/<name>/` the file types the console daemon's
// plugin staging consumes (proto, .go, plugin.yaml, go.mod, go.sum) are
// carried verbatim so the daemon finds the full tree it needs; outside
// the plugins/ subtree only `.proto` files are included.
func realReadProtoTree(root, protoDir string) ([]*codegenpb.ProtoFile, error) {
	protoRoot := filepath.Join(root, protoDir)
	info, err := os.Stat(protoRoot)
	if err != nil {
		// A project between `init` and its first `domain add` has no proto
		// tree yet, and that is a state every adoption passes through — the
		// second command looks at it. It used to surface as a bare `stat`
		// naming an absolute path, which says what the tool tried and nothing
		// about what to do. Installing a plugin does not create this
		// directory either: a plugin is activated FROM a domain's sentinel,
		// so the domain comes first.
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no proto tree at %s yet — `w17ctl domain add <name>` "+
				"scaffolds the first domain, and codegen has something to read once it exists", protoDir)
		}
		return nil, fmt.Errorf("stat %s: %w", protoRoot, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", protoRoot)
	}
	pluginsPrefix := "plugins" + string(filepath.Separator)
	var out []*codegenpb.ProtoFile
	err = filepath.WalkDir(protoRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(protoRoot, path)
		isPlugin := strings.HasPrefix(rel, pluginsPrefix)
		if !isPlugin && !strings.HasSuffix(path, ".proto") {
			return nil
		}
		if isPlugin {
			base := filepath.Base(path)
			keep := strings.HasSuffix(path, ".proto") ||
				strings.HasSuffix(path, ".go") ||
				base == "plugin.yaml" ||
				base == "go.mod" ||
				base == "go.sum"
			if !keep {
				return nil
			}
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		out = append(out, &codegenpb.ProtoFile{
			Filename: filepath.ToSlash(rel),
			Contents: contents,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ReadGoModuleFn defaults to realReadGoModule; tests override.
var ReadGoModuleFn = realReadGoModule

// ReadGoModule returns the consumer's Go module path, looking under
// `<root>/<genDir>/go.mod` then `<root>/go.mod`. Empty when neither
// resolves.
func ReadGoModule(root, genDir string) string { return ReadGoModuleFn(root, genDir) }

func realReadGoModule(root, genDir string) string {
	candidates := []string{
		filepath.Join(root, genDir, "go.mod"),
		filepath.Join(root, "go.mod"),
	}
	for _, p := range candidates {
		if mod := parseGoModulePath(p); mod != "" {
			return mod
		}
	}
	return ""
}

// parseGoModulePath reads the go.mod at path and returns the module
// path declared on the `module` line. Empty when the file doesn't
// exist, can't be read, or uses the `module (...)` block form (which
// would need a real go.mod parser).
func parseGoModulePath(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "module") {
			continue
		}
		rest := strings.TrimSpace(line[len("module"):])
		if rest == "" || (rest[0] != '"' && (rest[0] < 'a' || rest[0] > 'z') && (rest[0] < 'A' || rest[0] > 'Z')) {
			return ""
		}
		rest = strings.Trim(rest, "\"")
		if i := strings.Index(rest, "//"); i >= 0 {
			rest = strings.TrimSpace(rest[:i])
		}
		return rest
	}
	return ""
}
