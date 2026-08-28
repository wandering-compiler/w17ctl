// Package remotesnap implements TRUE remote-side dev-DB snapshots for
// remote-stack mode (docs/experiments/remote-stack.md §6, resolved
// "remote-side"): pg_dump / pg_restore run INSIDE the remote postgres
// container via `docker compose exec`, and the dump files live on the
// server under `<remote_path>/.snapshots/<project>`. The dump bytes never
// transit the laptop — the whole point of the remote-side choice.
//
// It is postgres-focused: the container supplies its own credentials
// ($POSTGRES_USER/$POSTGRES_DB), so w17ctl needs no DB secrets. Non-pg
// stores are not handled here (the caller filters to pg + notes the rest).
//
// Every command is a pure function of the store + inputs (unit-tested);
// the ssh exec is behind the remote.ExecFn seam.
package remotesnap

import (
	"fmt"
	"net/url"
	"path"
	"strings"

	"github.com/wandering-compiler/w17ctl/internal/remote"
)

// Store addresses one project's remote snapshot area.
type Store struct {
	// Dest is the SSH destination of the remote host.
	Dest remote.Dest
	// RemoteDir is the project's compose dir on the server (the `docker
	// compose exec` working dir).
	RemoteDir string
	// Root is the snapshot root on the server —
	// `<remote_path>/.snapshots/<project>`, a SIBLING of RemoteDir so
	// rsync --delete (which targets RemoteDir) never touches it.
	Root string
}

// safeComponent sanitises an untrusted name (initiative / savepoint) into
// one path component — url-escaped, and rejected if it would traverse.
func safeComponent(name string) (string, error) {
	key := url.PathEscape(name)
	if key == "" || key == "." || key == ".." || strings.ContainsRune(key, '/') {
		return "", fmt.Errorf("remotesnap: invalid name %q", name)
	}
	return key, nil
}

// dir returns the server-side savepoint directory for (initiative, name).
func (s Store) dir(initiative, name string) (string, error) {
	ik, err := safeComponent(initiative)
	if err != nil {
		return "", err
	}
	nk, err := safeComponent(name)
	if err != nil {
		return "", err
	}
	return path.Join(s.Root, ik, nk), nil
}

// initiativeDir returns the server-side directory holding an initiative's
// savepoints.
func (s Store) initiativeDir(initiative string) (string, error) {
	ik, err := safeComponent(initiative)
	if err != nil {
		return "", err
	}
	return path.Join(s.Root, ik), nil
}

// saveCommand builds the remote shell command that dumps every pg service
// into dir (atomically per file) and records the schema hash. One command
// so a partial failure leaves the prior savepoint intact (set -e aborts).
func saveCommand(remoteDir, dir, schemaHash string, pgServices []string) string {
	var b strings.Builder
	b.WriteString("set -e; cd ")
	b.WriteString(remote.ShellQuote(remoteDir))
	b.WriteString("; mkdir -p ")
	b.WriteString(remote.ShellQuote(dir))
	for _, svc := range pgServices {
		final := path.Join(dir, svc+".dump")
		tmp := final + ".tmp"
		// pg_dump inside the container using ITS OWN credentials, custom
		// format (-Fc) for pg_restore --clean; temp then mv = atomic.
		fmt.Fprintf(&b, "; docker compose exec -T %s sh -c 'pg_dump -U \"$POSTGRES_USER\" -Fc \"$POSTGRES_DB\"' > %s; mv %s %s",
			remote.ShellQuote(svc), remote.ShellQuote(tmp), remote.ShellQuote(tmp), remote.ShellQuote(final))
	}
	// Record the schema-hash pin (empty is fine).
	fmt.Fprintf(&b, "; printf %%s %s > %s", remote.ShellQuote(schemaHash), remote.ShellQuote(path.Join(dir, "meta")))
	return b.String()
}

// loadCommand builds the remote shell command that restores every pg
// service from its dump (clean + if-exists so a re-restore is idempotent).
func loadCommand(remoteDir, dir string, pgServices []string) string {
	var b strings.Builder
	b.WriteString("set -e; cd ")
	b.WriteString(remote.ShellQuote(remoteDir))
	for _, svc := range pgServices {
		final := path.Join(dir, svc+".dump")
		fmt.Fprintf(&b, "; docker compose exec -T %s sh -c 'pg_restore --clean --if-exists --no-owner -U \"$POSTGRES_USER\" -d \"$POSTGRES_DB\"' < %s",
			remote.ShellQuote(svc), remote.ShellQuote(final))
	}
	return b.String()
}

// SaveNamed dumps the pg services into a server-side savepoint.
func (s Store) SaveNamed(initiative, name, schemaHash string, pgServices []string) error {
	dir, err := s.dir(initiative, name)
	if err != nil {
		return err
	}
	if _, err := remote.Exec(s.Dest, saveCommand(s.RemoteDir, dir, schemaHash, pgServices), nil); err != nil {
		return fmt.Errorf("remotesnap save %q: %w", name, err)
	}
	return nil
}

// LoadNamed restores the pg services from a server-side savepoint.
func (s Store) LoadNamed(initiative, name string, pgServices []string) error {
	dir, err := s.dir(initiative, name)
	if err != nil {
		return err
	}
	if _, err := remote.Exec(s.Dest, loadCommand(s.RemoteDir, dir, pgServices), nil); err != nil {
		return fmt.Errorf("remotesnap load %q: %w", name, err)
	}
	return nil
}

// HasNamed reports whether a savepoint dir exists on the server.
func (s Store) HasNamed(initiative, name string) (bool, error) {
	dir, err := s.dir(initiative, name)
	if err != nil {
		return false, err
	}
	out, err := remote.Exec(s.Dest, "test -d "+remote.ShellQuote(dir)+" && echo yes || true", nil)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(out)) == "yes", nil
}

// ListNamed lists an initiative's savepoint names (url-decoded), sorted by
// the remote `ls`. A missing initiative dir yields an empty list.
func (s Store) ListNamed(initiative string) ([]string, error) {
	dir, err := s.initiativeDir(initiative)
	if err != nil {
		return nil, err
	}
	out, err := remote.Exec(s.Dest, "ls -1 "+remote.ShellQuote(dir)+" 2>/dev/null || true", nil)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if dec, derr := url.PathUnescape(line); derr == nil {
			names = append(names, dec)
		}
	}
	return names, nil
}

// NamedSchemaHash reads a savepoint's recorded schema hash ("" when none).
func (s Store) NamedSchemaHash(initiative, name string) (string, error) {
	dir, err := s.dir(initiative, name)
	if err != nil {
		return "", err
	}
	out, err := remote.Exec(s.Dest, "cat "+remote.ShellQuote(path.Join(dir, "meta"))+" 2>/dev/null || true", nil)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// RemoveNamed deletes a savepoint dir from the server.
func (s Store) RemoveNamed(initiative, name string) error {
	dir, err := s.dir(initiative, name)
	if err != nil {
		return err
	}
	if _, err := remote.Exec(s.Dest, "rm -rf "+remote.ShellQuote(dir), nil); err != nil {
		return fmt.Errorf("remotesnap remove %q: %w", name, err)
	}
	return nil
}
