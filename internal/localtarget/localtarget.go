// Package localtarget derives the localhost DSN for a project's PUBLISHED dev
// store connections — the zero-flag, zero-env resolution shared by `stack
// build`, `db snapshot`, and `fixtures apply` so a developer never hand-sets a
// W17_TARGET_* env for a LOCAL store. The DSN is fully derivable: the dialect
// from the connection name, the host port from the devconfig allocation (what
// `stack up` published), and the dev credentials codegen bakes into the compose
// (user=pass=db=<domain>). A REMOTE/prod target's DSN is a secret and is NOT
// derivable here — callers keep the W17_TARGET_<CONN> env override for that.
package localtarget

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/wandering-compiler/w17ctl/internal/codegen"
	"github.com/wandering-compiler/w17ctl/internal/devconfig"
	"github.com/wandering-compiler/w17ctl/internal/docker"
)

// ResolveDSN returns the localhost DSN for one published dev store connection.
// On success skipReason is empty; otherwise dsn is empty and skipReason names
// why (no published port yet, unknown dialect, or a schemaless/file store with
// no localhost DSN form) so the caller can surface it.
func ResolveDSN(connName string, p *devconfig.Project) (dsn, skipReason string) {
	dialect, ok := codegen.DialectFromConnectionName(connName)
	if !ok {
		return "", fmt.Sprintf("%s: cannot infer a dialect from the connection name", connName)
	}
	port := 0
	if p != nil {
		port = p.Ports[HostPortSlot(connName)]
	}
	if port == 0 {
		return "", fmt.Sprintf("%s: no published host port — run 'w17ctl stack up' first", connName)
	}
	dsn = DSN(dialect, codegen.ConnectionDomain(connName), port)
	if dsn == "" {
		return "", fmt.Sprintf("%s: dialect %q has no localhost DSN (schemaless / file store)", connName, dialect)
	}
	return dsn, ""
}

// HostPortSlot is the devconfig `Ports` key for a connection's published host
// port (e.g. "core-postgres" → "W17_CORE_POSTGRES_HOST_PORT") — the same slot
// codegen writes into the dev compose's port mapping.
func HostPortSlot(connName string) string {
	return "W17_" + strings.NewReplacer("-", "_", ".", "_").Replace(strings.ToUpper(connName)) + "_HOST_PORT"
}

// DSN builds the localhost DSN for a published dev store. Mirrors the
// credentials codegen writes into the dev compose (composegen:
// user=pass=db=<domain> for postgres/mysql; redis has no auth, db 0).
func DSN(dialect, domain string, port int) string {
	switch dialect {
	case "postgres":
		return fmt.Sprintf("postgres://%s:%s@localhost:%d/%s?sslmode=disable", domain, domain, port, domain)
	case "mysql":
		return fmt.Sprintf("mysql://%s:%s@localhost:%d/%s", domain, domain, port, domain)
	case "redis":
		return fmt.Sprintf("redis://localhost:%d/0", port)
	}
	return ""
}

// EnginePort is the port a store engine LISTENS on inside its container.
//
// One table, because this mapping was written in four places — the in-container
// dump route, the test suite's compose reader, the console UI's read-side and
// here, implicitly, in the DSN shapes. Four copies of a rule drift, and the drift
// here is silent: a wrong container port makes a published-port lookup find
// nothing, which reads exactly like "the store is not up".
//
// Redis is here because it is ASKED: DSN builds a localhost form for it,
// DialectFromConnectionName recognises `-redis`, and a `cache-redis` connection
// resolves through this table like any other store. (An earlier version of this
// comment said the opposite — that redis was listed for completeness and nothing
// asked it — which would have told the next change it was safe to drop.)
//
// The dialects genuinely absent are the ones with no localhost DSN at all: NATS,
// S3, the file stores. ResolveDSN skips those before reaching here.
func EnginePort(dialect string) (int, bool) {
	switch dialect {
	case "postgres":
		return 5432, true
	case "mysql":
		return 3306, true
	case "redis":
		return 6379, true
	}
	return 0, false
}

// DSNFor is ResolveDSN with the host port supplied rather than remembered — the
// form `stack build` uses once compose has said what it actually publishes.
// Empty when the dialect carries no localhost DSN.
func DSNFor(connName string, port int) string {
	dialect, ok := codegen.DialectFromConnectionName(connName)
	if !ok {
		return ""
	}
	return DSN(dialect, codegen.ConnectionDomain(connName), port)
}

// PortOf reads the host port back out of a DSN this package built.
//
// Used to compare a remembered port with the live one. Zero when the DSN has no
// port or is not one of ours, which the caller reads as "cannot compare" rather
// than as a mismatch — reporting a correction that did not happen is its own way
// of being wrong.
func PortOf(dsn string) int {
	u, err := url.Parse(dsn)
	if err != nil {
		return 0
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		return 0
	}
	return port
}

// DialectByEnginePort is EnginePort inverted: which engine listens on a container
// port. The test suite reads a compose file and has only the port.
//
// Both directions from one table, because the pair is where a mapping drifts:
// forward says postgres is 5432 and backward says 5432 is postgres, and nothing
// makes two hand-written copies agree.
func DialectByEnginePort(containerPort int) (string, bool) {
	for _, dialect := range []string{"postgres", "mysql", "redis"} {
		if p, ok := EnginePort(dialect); ok && p == containerPort {
			return dialect, true
		}
	}
	return "", false
}

// PublishedPorts asks THIS project's compose what each service publishes, keyed
// service -> container port -> host port. The second return says whether compose
// ANSWERED at all, which callers must tell apart from "answered, and this store
// is not in it".
//
// Asked of compose rather than of `docker ps`, so the answer is scoped to this
// project by construction: a host port is unique per MACHINE, so "who publishes
// 16209" is a question about the machine — the question whose answer reached
// another workspace's database (marb #75).
func PublishedPorts(root string) (map[string]map[int]int, bool) {
	raw, err := docker.CaptureComposeFn(root, append(docker.FileArgs(root), "ps", "--format", "json")...)
	if err != nil || len(raw) == 0 {
		return nil, false
	}
	pubs, ok := parsePsPublishers(raw)
	if !ok {
		// ⚠️ Non-empty output that does NOT parse is not an answer. Treating it
		// as one (it used to: `len(raw) > 0` was the whole test) made every
		// store take the "absent" branch, so a docker whose `ps --format json`
		// changed shape would skip EVERY local store — refusing to resolve
		// anything, which is worse than the remembered port this replaced. An
		// unreadable answer belongs in the same branch as no answer at all.
		return nil, false
	}
	out := map[string]map[int]int{}
	for _, pub := range pubs {
		if pub.service == "" {
			continue
		}
		if out[pub.service] == nil {
			out[pub.service] = map[int]int{}
		}
		out[pub.service][pub.target] = pub.published
	}
	return out, true
}

// ResolveLive is ResolveDSN with what compose publishes preferred over what the
// devconfig remembers.
//
// The remembered number was the remaining half of marb #75. Resolving the project
// by its lock stopped a command reading another project's allocation, and scoping
// the container lookup stopped a dump reaching another workspace — but the port
// itself was still a memory and nothing compared it with the machine. A remembered
// port for a store that is NOT running names whatever else took it: their dump came
// back 722 bytes because it found a different, empty Postgres there.
//
// Three branches, and each says what it did:
//
//	compose answered, store present  -> its port. `note` is set when that differs
//	                                    from the memory, because a silent
//	                                    correction is how nobody learns the config
//	                                    went stale.
//	compose answered, store absent   -> skipReason. The store is down and the
//	                                    remembered port now belongs to something
//	                                    else, so there is no honest DSN to return.
//	compose not askable              -> the memory. No docker, no compose file,
//	                                    nothing to check against: the pre-#75
//	                                    behaviour, and the only case where the
//	                                    remembered number is still the best answer.
func ResolveLive(connName string, p *devconfig.Project, published map[string]map[int]int, asked bool) (dsn, skipReason, note string) {
	remembered, rememberedSkip := ResolveDSN(connName, p)
	if !asked {
		return remembered, rememberedSkip, ""
	}
	live, liveSkip := livePort(connName, published)
	if liveSkip != "" {
		return "", liveSkip, ""
	}
	if remembered == "" {
		// The config never recorded this store and compose publishes it: the
		// live answer does not depend on the memory existing.
		if d := DSNFor(connName, live); d != "" {
			return d, "", ""
		}
		return "", rememberedSkip, ""
	}
	if was := PortOf(remembered); was != live {
		return DSNFor(connName, live), "", fmt.Sprintf(
			"store %s publishes :%d, not the :%d this machine's config remembers — using :%d",
			connName, live, was, live)
	}
	return remembered, "", ""
}

// livePort is the host port compose publishes for a store's ENGINE.
//
// The compose service is named after the connection (composegen), and the port
// asked for is the one the server listens on inside the container — never the
// first publisher on the service, or a store that also published an exporter
// would resolve to it.
func livePort(connName string, published map[string]map[int]int) (int, string) {
	dialect, ok := codegen.DialectFromConnectionName(connName)
	if !ok {
		return 0, fmt.Sprintf("%s: cannot infer a dialect from the connection name", connName)
	}
	engine, ok := EnginePort(dialect)
	if !ok {
		return 0, fmt.Sprintf("%s: dialect %q has no localhost DSN (schemaless / file store)", connName, dialect)
	}
	port := published[connName][engine]
	if port == 0 {
		return 0, fmt.Sprintf("%s: compose publishes no host port for it right now — run 'w17ctl stack up' first", connName)
	}
	return port, ""
}

// psPublisher is one container port mapping from `docker compose ps`: which
// SERVICE publishes it, the port the server listens on inside the container, and
// the host port that reaches it.
type psPublisher struct {
	service   string
	target    int
	published int
}

// parsePsPublishers extracts every publisher from `docker compose ps --format
// json`, and reports whether it UNDERSTOOD the blob.
//
// The second return exists because "no publishers" has two causes that must not
// share a branch: a stack with nothing running (a valid, empty answer) and output
// this cannot read at all. The NDJSON path tolerates a stray non-JSON line — a
// warning docker prints on stdout — so "zero lines parsed, and the blob was not
// empty" is the signal that nothing was understood.
func parsePsPublishers(raw []byte) ([]psPublisher, bool) {
	type publisher struct {
		TargetPort    int `json:"TargetPort"`
		PublishedPort int `json:"PublishedPort"`
	}
	type psEntry struct {
		Service    string      `json:"Service"`
		Publishers []publisher `json:"Publishers"`
	}
	collect := func(entries []psEntry) []psPublisher {
		var out []psPublisher
		for _, e := range entries {
			for _, pub := range e.Publishers {
				if pub.PublishedPort > 0 {
					out = append(out, psPublisher{service: e.Service, target: pub.TargetPort, published: pub.PublishedPort})
				}
			}
		}
		return out
	}
	// Whole-output JSON array form. An empty array is a SUCCESSFUL answer that
	// says nothing is running.
	var arr []psEntry
	if err := json.Unmarshal(raw, &arr); err == nil {
		return collect(arr), true
	}
	var entries []psEntry
	parsed := 0
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var e psEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue // tolerate a stray non-JSON line
		}
		parsed++
		entries = append(entries, e)
	}
	return collect(entries), parsed > 0
}

// PublishedPortsFrom is every host port in a `docker compose ps --format json`
// blob, for a caller that only needs the SET (the free-port preflight) rather
// than which service publishes what.
func PublishedPortsFrom(raw []byte) []int {
	pubs, _ := parsePsPublishers(raw)
	var out []int
	for _, pub := range pubs {
		out = append(out, pub.published)
	}
	return out
}
