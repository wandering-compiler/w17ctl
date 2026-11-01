package plugin

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// A REQUIRED HOOK IS PART OF THE ACTIVATION, so the scaffold binds it.
//
// A hook is a call the plugin makes into a business method the PROJECT writes
// (the plugin manifest's `hooks:`). An optional one may stay unbound; a
// `required` one may not — codegen refuses the activation
// (ValidateHookCoverage). The throwaway project has no business code of its
// own, so for every required hook its enabled features turn on it writes one
// stub business method taking and returning the hook's messages as the
// activation stages them (`plugindev.app.<registered_as>.<Message>`) and binds
// it — what a consumer does, with a body nobody calls.

// devHookModule is the module the stub hook methods live in. A business
// service of the activating domain, as a hook's method must be.
const devHookModule = "hooks"

// devHook is one required hook the scaffold binds.
type devHook struct {
	plugin   string // registered_as of the activation that declares it
	slot     string
	request  string
	response string
	// imports are the staged proto paths declaring request and response.
	imports []string
}

// methodRef is the binding's `<module>.<Service>.<Method>`.
func (h devHook) methodRef() string {
	return devHookModule + ".Hooks." + h.method()
}

// method is `<Plugin>_<Slot>`. The separator keeps it unambiguous: plugin
// `x` with slot `y_z` and plugin `x_y` with slot `z` would both be `XYZ`
// without it, and neither half contains an underscore once pascal-cased.
func (h devHook) method() string {
	return pascal(h.plugin) + "_" + pascal(h.slot)
}

func pascal(s string) string {
	var b strings.Builder
	for _, part := range strings.FieldsFunc(s, func(r rune) bool { return r == '_' || r == '-' }) {
		b.WriteString(strings.ToUpper(part[:1]) + part[1:])
	}
	return b.String()
}

// devRequiredHooks lists the required hooks of the plugin under test and of
// every companion, each under the features its activation turns on.
func devRequiredHooks(dir, name string, feats []string, companions []*signInPlugin) ([]devHook, error) {
	out, err := manifestRequiredHooks(dir, name, feats)
	if err != nil {
		return nil, err
	}
	for _, p := range companions {
		hs, err := manifestRequiredHooks(p.dir, p.name, p.feats)
		if err != nil {
			return nil, fmt.Errorf("plugin %s: %w", p.name, err)
		}
		out = append(out, hs...)
	}
	// Defensive: two stubs of one name would be a proto the console refuses
	// with a message about the scaffold, not about the plugin.
	seen := map[string]devHook{}
	for _, h := range out {
		if prev, ok := seen[h.method()]; ok {
			return nil, fmt.Errorf("hooks %s.%s and %s.%s would share the stub method %s",
				prev.plugin, prev.slot, h.plugin, h.slot, h.method())
		}
		seen[h.method()] = h
	}
	return out, nil
}

// manifestRequiredHooks reads plugin.yaml's hooks and keeps the required ones
// whose feature is on (or that have none), locating each message in the
// plugin's protos.
func manifestRequiredHooks(dir, name string, feats []string) ([]devHook, error) {
	body, err := os.ReadFile(filepath.Join(dir, "plugin.yaml"))
	if err != nil {
		return nil, fmt.Errorf("reading the manifest: %w", err)
	}
	var m struct {
		Hooks []struct {
			Name     string `yaml:"name"`
			Request  string `yaml:"request"`
			Response string `yaml:"response"`
			Required bool   `yaml:"required"`
			Feature  string `yaml:"feature"`
		} `yaml:"hooks"`
	}
	if err := yaml.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("reading the manifest: %w", err)
	}
	on := map[string]bool{}
	for _, f := range feats {
		on[f] = true
	}
	var out []devHook
	for _, h := range m.Hooks {
		if !h.Required || (h.Feature != "" && !on[h.Feature]) {
			continue
		}
		h1 := devHook{plugin: name, slot: h.Name, request: h.Request, response: h.Response}
		seen := map[string]bool{}
		for _, msg := range []string{h.Request, h.Response} {
			rel, err := protoDeclaring(dir, msg)
			if err != nil {
				return nil, fmt.Errorf("hook %q: %w", h.Name, err)
			}
			// Staged under the activating domain, by registered_as, at its
			// path relative to the staging source (protoSourceDir).
			imp := "domains/app/" + name + "/" + rel
			if !seen[imp] {
				seen[imp] = true
				h1.imports = append(h1.imports, imp)
			}
		}
		out = append(out, h1)
	}
	return out, nil
}

// protoSourceDir is the directory staging copies a plugin's protos from:
// `<dir>/proto` when it exists, else the plugin root (the flat layout).
// It is the rule the console stages a plugin's protos by, so the import path
// computed here is the path the file is staged at.
func protoSourceDir(dir string) string {
	if info, err := os.Stat(filepath.Join(dir, "proto")); err == nil && info.IsDir() {
		return filepath.Join(dir, "proto")
	}
	return dir
}

// protoDeclaring returns the path, relative to the staging source, of the
// file declaring the top-level message `msg`.
func protoDeclaring(dir, msg string) (string, error) {
	root := protoSourceDir(dir)
	var found string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, werr error) error {
		if werr != nil || d.IsDir() || !strings.HasSuffix(p, ".proto") || found != "" {
			return werr
		}
		body, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if declaresTopLevelMessage(string(body), msg) {
			rel, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			found = filepath.ToSlash(rel)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if found == "" {
		return "", fmt.Errorf("no proto under %s declares a top-level message %s", root, msg)
	}
	return found, nil
}

// declaresTopLevelMessage reports whether src declares `message <name> {` at
// file scope. A small tokenizer rather than a full parse: comments and string
// literals are skipped, braces are counted so a nested message of the same
// name does not count, and indentation does not matter. (Plugin protos carry
// staging placeholders, so they are not always valid proto before staging.)
func declaresTopLevelMessage(src, name string) bool {
	var toks []string
	depth := 0
	flush := func(word string) {
		if word != "" {
			toks = append(toks, word)
		}
	}
	word := ""
	for i := 0; i < len(src); i++ {
		c := src[i]
		switch {
		case c == '/' && i+1 < len(src) && src[i+1] == '/':
			flush(word)
			word = ""
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			flush(word)
			word = ""
			i += 2
			for i+1 < len(src) && !(src[i] == '*' && src[i+1] == '/') {
				i++
			}
			i++
		case c == '"' || c == '\'':
			flush(word)
			word = ""
			q := c
			for i++; i < len(src) && src[i] != q; i++ {
				if src[i] == '\\' {
					i++
				}
			}
			toks = append(toks, "\"\"")
		case c == '{':
			flush(word)
			word = ""
			if depth == 0 && len(toks) >= 2 && toks[len(toks)-2] == "message" && toks[len(toks)-1] == name {
				return true
			}
			depth++
			toks = append(toks, "{")
		case c == '}':
			flush(word)
			word = ""
			if depth > 0 {
				depth--
			}
			toks = append(toks, "}")
		case c == '_' || c == '.' || (c >= '0' && c <= '9') || (c|0x20 >= 'a' && c|0x20 <= 'z'):
			word += string(c)
		default:
			flush(word)
			word = ""
			if c == ';' || c == '=' || c == '(' || c == ')' || c == '[' || c == ']' || c == ',' || c == '<' || c == '>' {
				toks = append(toks, string(c))
			}
		}
	}
	return false
}

// writeHookModule writes the stub module the required hooks are bound to;
// nothing when there are none.
func writeHookModule(domain string, hooks []devHook) error {
	if len(hooks) == 0 {
		return nil
	}
	mod := filepath.Join(domain, devHookModule)
	if err := os.MkdirAll(filepath.Join(mod, "business"), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(mod, "w17.proto"), []byte(hookModuleSentinel()), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(mod, "business", "hooks.proto"), []byte(hookServiceProto(hooks)), 0o644)
}

func hookModuleSentinel() string {
	return `syntax = "proto3";

// Written by ` + "`w17ctl plugin dev`" + `: the module the activations' required
// hooks are bound to (see ../w17.proto).
package plugindev.app.` + devHookModule + `;
`
}

func hookServiceProto(hooks []devHook) string {
	imports := map[string]bool{}
	for _, h := range hooks {
		for _, i := range h.imports {
			imports[i] = true
		}
	}
	sorted := make([]string, 0, len(imports))
	for i := range imports {
		sorted = append(sorted, i)
	}
	sort.Strings(sorted)
	var b strings.Builder
	b.WriteString(`syntax = "proto3";

package plugindev.app.` + devHookModule + `;

`)
	for _, i := range sorted {
		b.WriteString(`import "` + i + `";` + "\n")
	}
	b.WriteString(`
// Hooks holds one stub per required hook: the method a project would write,
// typed by the hook's messages as the activation stages them. A REQUIRED hook
// left unbound is refused, so the dev project binds each — the body is never
// what this run is about.
service Hooks {
`)
	for _, h := range hooks {
		pkg := "plugindev.app." + h.plugin + "."
		fmt.Fprintf(&b, "  // %s's %q hook.\n  rpc %s(%s%s) returns (%s%s);\n", h.plugin, h.slot, h.method(), pkg, h.request, pkg, h.response)
	}
	b.WriteString("}\n")
	return b.String()
}
