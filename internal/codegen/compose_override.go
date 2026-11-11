package codegen

import (
	"fmt"
	"os"
	"path"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	codegenpb "github.com/wandering-compiler/sdk/go/pb/w17compiler"
)

// orphanedComposeOverrideWarnings reports a service of the developer's own
// root compose file that only overrides (no image, no build, no extends) a
// service no compose of this run defines any more.
//
// The root compose.yaml is author-owned: it includes compose.w17.yaml and
// layers the project's own settings over the generated services, by name —
// an egress client's DSN on `<d>-business-companion`, a port on
// `<d>-storage`. A topology change renames the service that runs a tier:
// composing a domain folds storage and the business companion into
// `<d>-server`, so the override is left naming a service nothing defines,
// and `docker compose up` refuses it as a service with neither an image nor
// a build context, without saying where its settings belong now. The run
// says so, and names the `-server` (or gateway-only composition's gateway)
// that took the tier.
//
// "Defines" is a service with an image, a build or an `extends` in this
// run's composes or in any of the project's own compose files (and what they
// include). Only a service named as a tier this run's `-server` folds is
// judged (foldingServer), and only by a run that writes composes.
func orphanedComposeOverrideWarnings(root string, writes []*codegenpb.GeneratedFile) []string {
	generated := map[string]bool{}
	written := map[string]bool{}
	for _, f := range writes {
		if keptIfMissing(root, f) {
			continue
		}
		rel := path.Clean(strings.ReplaceAll(f.GetRelativePath(), "\\", "/"))
		written[rel] = true
		if isComposeName(path.Base(rel)) {
			for name, svc := range composeServices(f.GetContents()) {
				if svc.defines() {
					generated[name] = true
				}
			}
		}
	}
	if len(generated) == 0 {
		return nil
	}
	defined := map[string]bool{}
	overrides := map[string]string{} // service → the root file that only overrides it
	seen := map[string]bool{}
	var visit func(rel string, depth int, rootFile bool)
	visit = func(rel string, depth int, rootFile bool) {
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
		for name, svc := range composeServices(body) {
			switch {
			case svc.defines():
				defined[name] = true
			case rootFile:
				if _, ok := overrides[name]; !ok {
					overrides[name] = rel
				}
			}
		}
		var doc composeDoc
		if yaml.Unmarshal(body, &doc) != nil {
			return
		}
		for _, inc := range doc.Include {
			for _, p := range includePaths(inc) {
				if p == "" || path.IsAbs(strings.ReplaceAll(p, "\\", "/")) {
					continue
				}
				visit(path.Join(path.Dir(rel), strings.ReplaceAll(p, "\\", "/")), depth+1, false)
			}
		}
	}
	for _, name := range handWrittenRootComposes {
		visit(name, 0, true)
	}
	var out []string
	for name, file := range overrides {
		if generated[name] || defined[name] {
			continue
		}
		server := foldingServer(name, generated)
		if server == "" {
			continue
		}
		out = append(out, fmt.Sprintf(
			"%s: service %s only overrides a service, and no compose of this run defines %s any more — `docker compose up` refuses a service with neither an image nor a build; %s runs it now (the domain's composed binary folds it in) — move its settings there",
			file, name, name, server))
	}
	sort.Strings(out)
	return out
}

// foldedTiers are the first word, after the domain, of the service of a
// tier a composed binary folds in: <d>-storage, <d>-eventbus, <d>-gateway
// (and its variants), <d>-admin, <d>-business and <d>-business-companion.
var foldedTiers = map[string]bool{"storage": true, "eventbus": true, "gateway": true, "admin": true, "business": true}

// foldingServer is the generated composed binary whose domain's folded tier
// the service name is, or "" when it names no such tier: the domain's
// `<d>-server`, or its `<d>-gateway` when the composition is gateway-only
// (the gateway then folds storage, the eventbus and the business
// companion). Only that case is judged: an override-only service of any
// other name may be defined where the walk does not look (COMPOSE_FILE,
// `-f`), and a domain that is not composed leaves its tiers' services in
// place. The longest domain wins, so app-admin's tier is never attributed to
// app-server, and a domain's -server before its gateway.
func foldingServer(name string, generated map[string]bool) string {
	best, bestDomain := "", ""
	for gen := range generated {
		domain, ok := strings.CutSuffix(gen, "-server")
		if !ok {
			domain, ok = strings.CutSuffix(gen, "-gateway")
		}
		if !ok || domain == "" {
			continue
		}
		rest, ok := strings.CutPrefix(name, domain+"-")
		if !ok {
			continue
		}
		tier, _, _ := strings.Cut(rest, "-")
		if !foldedTiers[tier] || name == gen {
			continue
		}
		if len(domain) > len(bestDomain) || (len(domain) == len(bestDomain) && strings.HasSuffix(gen, "-server")) {
			best, bestDomain = gen, domain
		}
	}
	return best
}

// composeService is what orphanedComposeOverrideWarnings reads of one
// compose service: whether it defines a container or only overrides one.
type composeService struct {
	Image   string    `yaml:"image"`
	Build   yaml.Node `yaml:"build"`
	Extends yaml.Node `yaml:"extends"`
}

// defines reports whether the service stands on its own (an image, a build,
// or an `extends` of another service) rather than only overriding one.
func (s composeService) defines() bool {
	return s.Image != "" || !s.Build.IsZero() || !s.Extends.IsZero()
}

// composeServices parses a compose file's services; nil when it does not
// parse.
func composeServices(body []byte) map[string]composeService {
	var doc struct {
		Services map[string]composeService `yaml:"services"`
	}
	if yaml.Unmarshal(body, &doc) != nil {
		return nil
	}
	return doc.Services
}
