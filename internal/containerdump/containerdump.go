// Package containerdump takes a snapshot of a store whose client tools live
// where the store does — inside its container.
//
// A snapshot is made by the database's OWN tools (`pg_dump`, `mysqldump`), and
// w17 ran them on the machine w17ctl runs on. That machine often does not have
// them, and not by accident: a project driven by `w17ctl stack up` keeps
// everything in Docker on purpose — an adopter reported having no `go` either,
// and asked why w17 was telling them to install a Postgres client when one was
// already running two inches away, inside the container w17ctl had just
// started.
//
// So when the host has no client, the dump runs in the container that serves
// the store. Found by the PORT the caller is already dialling: whatever
// publishes it is the store, whether compose started it or a bare
// `docker run` did.
//
// The stream is the same either way — plain SQL from the same tool with the
// same flags — so a snapshot taken through the container restores through the
// host and back.
package containerdump

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"strings"

	"github.com/wandering-compiler/sdk/go/tooling/migrate"
)

// Snapshotter runs a store's own dump/restore client inside the container that
// serves it.
type Snapshotter struct {
	container string
	// dsn is the DSN as seen FROM INSIDE the container: the same database and
	// credentials, reached on the port the server listens on rather than the
	// one it publishes.
	dsn        string
	dumpBin    string
	restoreBin string
	dialect    string
}

var _ migrate.Snapshotter = (*Snapshotter)(nil)

// For returns a container-backed Snapshotter for a DSN, or nil when there is
// no container to use.
//
// Nil rather than an error: this is a FALLBACK. The caller's first choice is
// the host's own client, and "no container either" has to read as "this route
// is unavailable" so the caller can say something true about both.
func For(ctx context.Context, dsn string) *Snapshotter {
	s, _ := ForReason(ctx, dsn)
	return s
}

// ForReason is For plus the reason it declined, in a phrase that completes
// "the dump could not run inside a container: …".
//
// The reason exists because declining used to be SILENT. With two stores in a
// project the fallback engaged for one and not the other, and the only thing
// the person saw was the host error — `pg_dump: executable file not found in
// $PATH` — which names the host and says nothing about the route that was
// tried and refused. marb could not tell whether the fallback had failed or
// simply did not exist (#62/3). A fallback that declines without a word is
// indistinguishable from one that is not there.
func ForReason(ctx context.Context, dsn string) (*Snapshotter, string) {
	u, err := url.Parse(dsn)
	if err != nil {
		return nil, "its DSN could not be parsed"
	}
	if u.Port() == "" {
		return nil, "its DSN names no port, so there is nothing to match a container against"
	}
	var (
		dumpBin    string
		restoreBin string
		inner      int
	)
	switch u.Scheme {
	case "postgres", "postgresql":
		dumpBin, restoreBin, inner = "pg_dump", "psql", 5432
	case "mysql":
		dumpBin, restoreBin, inner = "mysqldump", "mysql", 3306
	default:
		return nil, fmt.Sprintf("%q has no in-container dump route", u.Scheme)
	}
	// ⛔ The DSN must address THIS machine, and nothing checked that until
	// 2026-09-24.
	//
	// This route matches a container by the PORT the caller is dialling, on the
	// premise that the port is one the host publishes. The host half of the DSN
	// was never consulted — so a DSN naming a docker-network service
	// (`finplatform-postgres:5432`) or any other address matched whatever
	// happened to publish those digits locally, and the dump then ran inside
	// THAT container against ITS OWN server (the DSN's host is rewritten to
	// 127.0.0.1 below). When both servers hold a database of the same name,
	// `pg_dump` connects, succeeds, and returns the wrong database.
	//
	// Measured on two throwaway stores: a DSN for `10.9.9.9:17004` — an address
	// that exists nowhere on the machine — produced a 724-byte dump with zero
	// CREATE TABLE and no error, while the correct DSN produced 1277 bytes with
	// the table in it. marb's silently-empty branch snapshots are 722 bytes
	// (#68), and this is how a snapshot of a populated store becomes a dump of
	// somebody else's empty one.
	//
	// So: refuse a non-local host rather than guess. The caller falls back to
	// the host client and fails LOUDLY if it has none, which is a person who
	// knows their snapshot did not happen — the outcome this route exists to
	// improve on, and still better than one that silently is not theirs.
	if !isLocalHost(u.Hostname()) {
		return nil, fmt.Sprintf(
			"its DSN names host %q, which is not this machine — the in-container route matches a container by the PUBLISHED port, so it can only be trusted for a local DSN (a remote host with the same port digits would dump a different server's database of the same name)",
			u.Hostname())
	}
	cid := containerPublishing(ctx, u.Port())
	if cid == "" {
		return nil, fmt.Sprintf("no running container publishes port %s", u.Port())
	}
	if !hasBinary(ctx, cid, dumpBin) {
		return nil, fmt.Sprintf("container %s publishes port %s but has no %s", cid, u.Port(), dumpBin)
	}
	return &Snapshotter{
		container:  cid,
		dsn:        rewriteHost(u, inner),
		dumpBin:    dumpBin,
		restoreBin: restoreBin,
		dialect:    u.Scheme,
	}, ""
}

// Container is the container the snapshot would run in, for a message that
// says where the work happened.
func (s *Snapshotter) Container() string { return s.container }

// Dump streams the store's contents, produced by the client inside the
// container. The flags mirror the host-side snapshotter's exactly — a stream
// that only one of the two can read would make the route a person took part
// of whether their snapshot is restorable.
func (s *Snapshotter) Dump(ctx context.Context, w io.Writer) error {
	var args []string
	switch s.dialect {
	case "mysql":
		args = []string{s.dumpBin, "--single-transaction", "--routines", "--events", "--no-tablespaces", s.dsn}
	default:
		args = []string{s.dumpBin, "--clean", "--if-exists", "--no-owner", "--no-privileges", "--format=plain", "--dbname=" + s.dsn}
	}
	// Said once, when a dump actually runs. Building the snapshotter says
	// nothing: it is built on every branch-switch reconcile too, and a line
	// about client tools printed where no dump happens is the kind of notice
	// people learn to skip.
	fmt.Fprintf(os.Stderr, "snapshot: no %s on this machine — dumping inside the store's container (%s)\n",
		s.dumpBin, s.container)
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "docker", append([]string{"exec", "-i", s.container}, args...)...)
	cmd.Stdout = w
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s Dump (in container %s): %w: %s", s.dialect, s.container, err, stderr.String())
	}
	return nil
}

// Restore replays a stream through the same container.
func (s *Snapshotter) Restore(ctx context.Context, r io.Reader) error {
	var args []string
	switch s.dialect {
	case "mysql":
		args = []string{s.restoreBin, s.dsn}
	default:
		args = []string{s.restoreBin, "--quiet", "--no-psqlrc", "-v", "ON_ERROR_STOP=1", "--dbname=" + s.dsn}
	}
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "docker", append([]string{"exec", "-i", s.container}, args...)...)
	cmd.Stdin = r
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s Restore (in container %s): %w: %s", s.dialect, s.container, err, stderr.String())
	}
	return nil
}

// isLocalHost reports whether a DSN's host names THIS machine.
//
// Only these spellings: the check is what makes "the port is a published host
// port" safe to assume, so it has to be the set a person can publish onto, not
// every name that might resolve here. A hostname that resolves to a local
// address is deliberately NOT accepted — resolution can change under the same
// DSN, and a snapshot route that is sometimes right is the shape this guard
// exists to remove.
//
// An empty host (a DSN like `postgres:///db?host=/var/run`) is a unix socket,
// which publishes no port and therefore never reaches here.
func isLocalHost(host string) bool {
	switch strings.ToLower(host) {
	case "localhost", "127.0.0.1", "::1", "0.0.0.0":
		return true
	}
	return false
}

// containerPublishing is the id of the running container that publishes a host
// port, or "".
//
// The port is the one thing the caller already knows about the store — it is
// dialling it — so nothing here needs to know how the container was started,
// what it is called, or whether compose owns it.
func containerPublishing(ctx context.Context, port string) string {
	out, err := exec.CommandContext(ctx, "docker", "ps", "--format", "{{.ID}}\t{{.Ports}}").Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		id, ports, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		// `0.0.0.0:37116->5432/tcp, [::]:37116->5432/tcp`
		for _, mapping := range strings.Split(ports, ",") {
			host, _, ok := strings.Cut(strings.TrimSpace(mapping), "->")
			if !ok {
				continue
			}
			if p := host[strings.LastIndex(host, ":")+1:]; p == port {
				return id
			}
		}
	}
	return ""
}

// ComposeServiceFor names the compose SERVICE of the container that publishes
// the DSN's port, or "" when there is none.
//
// It exists because a store has two names that are not required to agree: the
// connection name an author picks in their targets, and the compose service
// name in their stack file. `stack build` quiesced by excluding CONNECTION
// names from a list of SERVICE names, so where the two differed the store was
// stopped like anything else — and the snapshot arranged to run inside that
// container then failed with "pg_dump: executable file not found", which reads
// like a missing tool rather than a container that is no longer running (marb
// #57). Crossing the namespaces through the container is the one comparison
// that does not depend on the two names matching.
func ComposeServiceFor(ctx context.Context, dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil || u.Port() == "" {
		return ""
	}
	cid := containerPublishing(ctx, u.Port())
	if cid == "" {
		return ""
	}
	out, err := exec.CommandContext(ctx, "docker", "inspect", "--format",
		"{{index .Config.Labels \"com.docker.compose.service\"}}", cid).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// hasBinary asks the container whether it carries the client. A container that
// publishes the port but is something else entirely (a proxy, a tunnel) fails
// here rather than at dump time with an exec error.
func hasBinary(ctx context.Context, container, bin string) bool {
	return exec.CommandContext(ctx, "docker", "exec", container, "sh", "-c", "command -v "+bin).Run() == nil
}

// rewriteHost points a DSN at the server as seen from inside its own
// container: loopback, and the port it LISTENS on rather than the one it
// publishes.
func rewriteHost(u *url.URL, inner int) string {
	v := *u
	v.Host = fmt.Sprintf("127.0.0.1:%d", inner)
	return v.String()
}
