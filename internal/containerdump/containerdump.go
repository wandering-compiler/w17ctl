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
	u, err := url.Parse(dsn)
	if err != nil || u.Port() == "" {
		return nil
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
		return nil
	}
	cid := containerPublishing(ctx, u.Port())
	if cid == "" || !hasBinary(ctx, cid, dumpBin) {
		return nil
	}
	return &Snapshotter{
		container:  cid,
		dsn:        rewriteHost(u, inner),
		dumpBin:    dumpBin,
		restoreBin: restoreBin,
		dialect:    u.Scheme,
	}
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
