package codegen

import (
	"fmt"
	"net"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	codegenpb "github.com/wandering-compiler/sdk/go/pb/w17compiler"
)

// staleBackendAddrWarnings reports a live `.env` whose backend address names
// a service this run's composes no longer define.
//
// A bundle's `.env` is author-owned: copied once from `.env.defaults` and
// never rewritten. envDriftWarnings catches a key the defaults gained; it
// cannot catch a key whose DEFAULT moved, and that is what a topology change
// does — fold storage into a -server and every `<d>-storage:50051` becomes
// `<d>-server:50051` in the regenerated defaults, while the live `.env` keeps
// dialling a service nothing starts any more. The first sign used to be a
// DNS failure at run time.
//
// The check is on the address, not on the history of the default: a
// `*_BACKEND_ADDR` (which includes `*_COMPANION_BACKEND_ADDR`) whose host is
// a compose service name — no dot, not localhost, not an IP — and that no
// compose file defines points at nothing. "Defines" counts this run's
// composes and the project's own on disk (the root compose.yaml, its
// override, the `w17ctl test` overlay, and what they `include:`), and a name
// is defined as a service, a container_name or a network alias — any name
// the stack's DNS answers. An address naming a real host (with a domain) is
// the operator's own and is left alone, and so is a value equal to the
// regenerated default (not a stale line).
func staleBackendAddrWarnings(root string, writes []*codegenpb.GeneratedFile) []string {
	services := map[string]bool{}
	defaultsByDir := map[string][]byte{}
	written := map[string]bool{}
	var envs []string
	for _, f := range writes {
		if keptIfMissing(root, f) {
			continue
		}
		rel := filepath.ToSlash(f.GetRelativePath())
		base := path.Base(rel)
		written[path.Clean(rel)] = true
		switch {
		case base == ".env.defaults":
			defaultsByDir[path.Dir(rel)] = f.GetContents()
		case base == ".env":
			envs = append(envs, rel)
		case isComposeName(base):
			composeHosts(f.GetContents(), services)
		}
	}
	if len(services) == 0 {
		// No compose in this run: nothing to judge an address against.
		return nil
	}
	// The project's own composes run beside the generated ones, and a
	// service they define (a stand-in, a sidecar the author added) is as
	// reachable as any of ours.
	handWrittenComposeHosts(root, written, services)
	var out []string
	for _, rel := range envs {
		target, err := containedJoin(root, rel)
		if err != nil {
			continue
		}
		live, err := os.ReadFile(target)
		if err != nil {
			continue
		}
		defaults := envValues(defaultsByDir[path.Dir(rel)])
		values := envValues(live)
		keys := make([]string, 0, len(values))
		for k := range values {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if !strings.HasSuffix(k, "_BACKEND_ADDR") {
				continue
			}
			host := composeServiceHost(values[k])
			if host == "" || services[host] || values[k] == defaults[k] {
				continue
			}
			want := defaults[k]
			if want == "" {
				want = "(no default: the key is gone from .env.defaults)"
			}
			out = append(out, fmt.Sprintf(
				"%s: %s=%s names %s, which no compose service of this run defines (the topology changed?) — the default is now %s; update the line, or delete %s to regenerate it from defaults",
				rel, k, values[k], host, want, rel))
		}
	}
	sort.Strings(out)
	return out
}

// composeServiceHost is the host of a host:port address when it can only be
// a compose service name; "" otherwise (an empty value, a host with a
// domain, localhost, an IP address).
func composeServiceHost(addr string) string {
	addr = strings.TrimSpace(addr)
	if i := strings.Index(addr, "://"); i >= 0 {
		addr = strings.TrimLeft(addr[i+3:], "/")
	}
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	switch {
	case host == "", host == "localhost", strings.Contains(host, "."), strings.Contains(host, ":"), strings.ContainsAny(host, "${}"):
		return ""
	case net.ParseIP(host) != nil:
		return ""
	}
	return host
}

// envValues parses a dotenv body into KEY → VALUE (the last assignment
// wins), with envKeys' rules for blanks, comments and `export `. A value's
// surrounding quotes are dropped.
func envValues(b []byte) map[string]string {
	vals := map[string]string{}
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
		k := strings.TrimSpace(ln[:eq])
		v := strings.TrimSpace(ln[eq+1:])
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
		if k != "" {
			vals[k] = v
		}
	}
	return vals
}

// isComposeName reports whether a file name is a compose file's: compose*.y(a)ml
// or docker-compose*.y(a)ml.
// keptIfMissing reports a generate-if-missing write whose file is already on
// disk: applyWriteOps leaves it alone, so the run does not write it. The
// root compose.yaml is one — scaffolded once, then the author's — and its
// copy on disk, not the scaffold in the write list, is what the stack runs.
func keptIfMissing(root string, f *codegenpb.GeneratedFile) bool {
	if !f.GetIfMissing() {
		return false
	}
	target, err := containedJoin(root, filepath.ToSlash(f.GetRelativePath()))
	if err != nil {
		return false
	}
	_, err = os.Stat(target)
	return err == nil
}

func isComposeName(base string) bool {
	if !strings.HasSuffix(base, ".yaml") && !strings.HasSuffix(base, ".yml") {
		return false
	}
	return strings.HasPrefix(base, "compose") || strings.HasPrefix(base, "docker-compose")
}

// composeDoc is what composeHosts and handWrittenComposeHosts read of a
// compose file.
type composeDoc struct {
	Include  []yaml.Node `yaml:"include"`
	Services map[string]struct {
		ContainerName string    `yaml:"container_name"`
		Networks      yaml.Node `yaml:"networks"`
	} `yaml:"services"`
}

// composeHosts adds every name a compose file makes resolvable on its
// network: each service's name, its container_name, and the aliases it
// takes on any network (`networks: {<net>: {aliases: [...]}}`). A file that
// does not parse adds nothing.
func composeHosts(body []byte, into map[string]bool) composeDoc {
	var doc composeDoc
	if yaml.Unmarshal(body, &doc) != nil {
		return composeDoc{}
	}
	for name, svc := range doc.Services {
		into[name] = true
		if svc.ContainerName != "" {
			into[svc.ContainerName] = true
		}
		if svc.Networks.Kind != yaml.MappingNode {
			continue // the list form names networks, never aliases
		}
		for i := 1; i < len(svc.Networks.Content); i += 2 {
			var nw struct {
				Aliases []string `yaml:"aliases"`
			}
			if svc.Networks.Content[i].Decode(&nw) == nil {
				for _, a := range nw.Aliases {
					into[a] = true
				}
			}
		}
	}
	return doc
}

// handWrittenRootComposes are the project-root compose files the stack is
// driven with besides the generated ones: the developer's compose.yaml (which
// includes compose.w17.yaml), the override compose merges over it, and the
// overlay `w17ctl test` adds.
var handWrittenRootComposes = []string{
	"compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml",
	"compose.override.yaml", "compose.override.yml", "docker-compose.override.yaml", "docker-compose.override.yml",
	"compose.test.yaml",
}

// handWrittenComposeHosts adds the hosts of the project's own compose files
// on disk, and of the compose files they `include:`, to into. A file this run
// writes is skipped — its new content is already counted, and its copy on
// disk is the topology the run replaces — and so is compose.w17.yaml, which
// is the tool's. Only files inside root are read.
func handWrittenComposeHosts(root string, written map[string]bool, into map[string]bool) {
	seen := map[string]bool{}
	var visit func(rel string, depth int)
	visit = func(rel string, depth int) {
		rel = path.Clean(rel)
		if depth > 8 || seen[rel] || written[rel] || path.Base(rel) == "compose.w17.yaml" {
			return
		}
		seen[rel] = true
		target, err := containedJoin(root, rel)
		if err != nil {
			return
		}
		body, err := os.ReadFile(target)
		if err != nil {
			return
		}
		doc := composeHosts(body, into)
		for _, inc := range doc.Include {
			for _, p := range includePaths(inc) {
				if p == "" || path.IsAbs(filepath.ToSlash(p)) {
					continue
				}
				visit(path.Join(path.Dir(rel), filepath.ToSlash(p)), depth+1)
			}
		}
	}
	for _, name := range handWrittenRootComposes {
		visit(name, 0)
	}
}

// includePaths are the files one `include:` entry names: the short form (a
// path) or the long form's `path`, itself a path or a list of them.
func includePaths(n yaml.Node) []string {
	switch n.Kind {
	case yaml.ScalarNode:
		return []string{n.Value}
	case yaml.MappingNode:
		var long struct {
			Path yaml.Node `yaml:"path"`
		}
		if n.Decode(&long) != nil {
			return nil
		}
		switch long.Path.Kind {
		case yaml.ScalarNode:
			return []string{long.Path.Value}
		case yaml.SequenceNode:
			var out []string
			for _, p := range long.Path.Content {
				out = append(out, p.Value)
			}
			return out
		}
	}
	return nil
}
