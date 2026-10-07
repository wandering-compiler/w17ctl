// Package client is `w17ctl client`: egress clients — a third-party REST API,
// from its OpenAPI document, as a signed generated gRPC client under
// `<proto root>/clients/<name>/` (docs/todos/egress-rest-client.md).
//
// Not to be confused with `w17ctl target client`, which declares the
// project's OWN generated frontend clients. These are clients of somebody
// else's API.
//
// The client is dumb here as everywhere (public-split §4): it fetches or
// reads the document (transport), streams it to the console, and writes back
// what the console returns, verbatim. Which operations exist, how they map to
// proto and the signature over the result are the console's.
package client

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"gopkg.in/yaml.v3"

	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/lockfile"
)

// Cmd is `w17ctl client`.
type Cmd struct {
	Generate GenerateCmd `cmd:"" help:"Generate a client of a third-party REST API from its OpenAPI document (a URL or a file): 'client generate <url|path> --endpoint \"GET /users/{id}\" --operation createUser'. Writes proto/clients/<name>/ — the proto, client.yaml and the document — signed by the console. No selector = every operation."`
	Update   UpdateCmd   `cmd:"" help:"Regenerate a client from its source: re-fetch the document, regenerate with the recorded selection (or new --endpoint/--operation), and report what changed."`
	List     ListCmd     `cmd:"" help:"List the project's clients: name, source, selection."`
	Remove   RemoveCmd   `cmd:"" help:"Delete a client's directory (proto/clients/<name>/)."`
}

// config is the part of client.yaml the client reads: what to fetch again,
// and what to ask for. The file is the console's (it is signed); the client
// only reads the request it records.
type config struct {
	Name       string   `yaml:"name"`
	Source     string   `yaml:"source"`
	Document   string   `yaml:"document"`
	Endpoints  []string `yaml:"endpoints"`
	Operations []string `yaml:"operations"`
}

const configFile = "client.yaml"

// project is the project a command runs in.
type project struct {
	root     string
	protoDir string
	lock     []byte
}

func loadProject() (*project, error) {
	root, err := core.FindProjectRoot()
	if err != nil {
		return nil, err
	}
	lockPath := filepath.Join(root, "w17", "lock.yaml")
	lock, err := os.ReadFile(lockPath)
	if err != nil {
		return nil, fmt.Errorf("read lock: %w", err)
	}
	protoDir := "proto"
	if lk, err := lockfile.Load(lockPath); err == nil && lk.GeneratedCode.ProtoDir != "" {
		protoDir = lk.GeneratedCode.ProtoDir
	}
	return &project{root: root, protoDir: protoDir, lock: lock}, nil
}

func (p *project) clientsDir() string { return filepath.Join(p.root, p.protoDir, "clients") }

func (p *project) clientDir(name string) string { return filepath.Join(p.clientsDir(), name) }

func (p *project) readConfig(name string) (*config, error) {
	data, err := os.ReadFile(filepath.Join(p.clientDir(name), configFile))
	if err != nil {
		return nil, fmt.Errorf("client %q: %w", name, err)
	}
	var c config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("client %q: %s: %w", name, configFile, err)
	}
	return &c, nil
}

// names lists the clients: the directories under clients/ holding a
// client.yaml.
func (p *project) names() ([]string, error) {
	entries, err := os.ReadDir(p.clientsDir())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(p.clientsDir(), e.Name(), configFile)); err == nil {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

// ListCmd is `w17ctl client list`.
type ListCmd struct{}

func (c *ListCmd) Run() error {
	p, err := loadProject()
	if err != nil {
		return err
	}
	names, err := p.names()
	if err != nil {
		return err
	}
	if len(names) == 0 {
		fmt.Fprintln(core.Stdout, "no clients — `w17ctl client generate <url|path>` makes one")
		return nil
	}
	for _, n := range names {
		cfg, err := p.readConfig(n)
		if err != nil {
			return err
		}
		sel := "every operation"
		if k := len(cfg.Endpoints) + len(cfg.Operations); k > 0 {
			sel = fmt.Sprintf("%d selector(s)", k)
		}
		fmt.Fprintf(core.Stdout, "%-20s %s (%s)\n", n, cfg.Source, sel)
	}
	return nil
}

// RemoveCmd is `w17ctl client remove`.
type RemoveCmd struct {
	Name string `arg:"" help:"The client to delete."`
}

func (c *RemoveCmd) Run() error {
	if !nameRE.MatchString(c.Name) {
		return fmt.Errorf("client remove: %q is not a client name", c.Name)
	}
	p, err := loadProject()
	if err != nil {
		return err
	}
	if _, err := p.readConfig(c.Name); err != nil {
		return fmt.Errorf("client remove: %w", err)
	}
	if err := os.RemoveAll(p.clientDir(c.Name)); err != nil {
		return fmt.Errorf("client remove: %w", err)
	}
	fmt.Fprintf(core.Stdout, "removed %s\n", filepath.Join(p.protoDir, "clients", c.Name))
	return nil
}
