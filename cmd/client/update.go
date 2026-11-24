package client

import (
	"fmt"
	"os"

	codegenpb "github.com/wandering-compiler/sdk/go/pb/w17compiler"
)

// UpdateCmd is `w17ctl client update`.
type UpdateCmd struct {
	Name       string   `arg:"" help:"The client to regenerate."`
	Source     string   `name:"source" help:"Fetch the document from here instead of the recorded source (and record this one)."`
	Endpoint   []string `name:"endpoint" help:"Replace the recorded selection: an operation as its request line. Repeatable."`
	Operation  []string `name:"operation" help:"Replace the recorded selection: an operationId (a WSDL: an operation name). Repeatable."`
	Idempotent []string `name:"idempotent" help:"Replace the recorded idempotent SOAP operations. Repeatable."`
	Console    string   `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console."`
	// The recorded per-call settings are kept; these set one, the --no-
	// form clears it.
	PerCallCredential  *bool `name:"per-call-credential" negatable:"" help:"Set (or, as --no-per-call-credential, clear) that the credential comes with each call. Default: as recorded."`
	PerCallDestination *bool `name:"per-call-destination" negatable:"" help:"Set (or clear) that the destination comes with each call. Default: as recorded."`
}

func (c *UpdateCmd) Run() error {
	if !nameRE.MatchString(c.Name) {
		return fmt.Errorf("client update: %q is not a client name", c.Name)
	}
	p, err := loadProject()
	if err != nil {
		return fmt.Errorf("client update: %w", err)
	}
	cfg, err := p.readConfig(c.Name)
	if err != nil {
		return fmt.Errorf("client update: %w", err)
	}
	if cfg.Mail {
		return c.updateMail(p)
	}
	source := cfg.Source
	if c.Source != "" {
		source = c.Source
	}
	endpoints, operations := cfg.Endpoints, cfg.Operations
	if len(c.Endpoint)+len(c.Operation) > 0 {
		endpoints, operations = c.Endpoint, c.Operation
	}
	idempotent := cfg.Idempotent
	if len(c.Idempotent) > 0 {
		idempotent = c.Idempotent
	}
	perCallCred, perCallDest := cfg.PerCallCredential, cfg.PerCallDestination
	if c.PerCallCredential != nil {
		perCallCred = *c.PerCallCredential
	}
	if c.PerCallDestination != nil {
		perCallDest = *c.PerCallDestination
	}
	// A recorded source is relative to the project root; one passed with
	// --source to the directory it was typed in.
	base := p.root
	if c.Source != "" {
		if base, err = os.Getwd(); err != nil {
			return fmt.Errorf("client update: %w", err)
		}
	}
	doc, recorded, err := fetch(source, p.root, base)
	if err != nil {
		return fmt.Errorf("client update: %w", err)
	}
	cl, conn, err := dial(c.Console)
	if err != nil {
		return fmt.Errorf("client update: %w", err)
	}
	defer func() { _ = conn.Close() }()
	before := snapshot(p.clientDir(c.Name))
	resp, err := generate(cl, p, &codegenpb.GenerateEgressClientRequest{
		Lock: p.lock, Source: recorded, Name: c.Name, Endpoints: endpoints, Operations: operations, Idempotent: idempotent,
		PerCallCredential: perCallCred, PerCallDestination: perCallDest,
	}, doc, true)
	if err != nil {
		return fmt.Errorf("client update: %w", err)
	}
	report(resp, diffSnapshots(before, snapshot(p.clientDir(c.Name))))
	return nil
}

// updateMail regenerates a mail client: nothing to fetch, nothing to
// select — it re-signs against this project and this platform.
func (c *UpdateCmd) updateMail(p *project) error {
	if c.Source != "" || len(c.Endpoint)+len(c.Operation)+len(c.Idempotent) > 0 || c.PerCallCredential != nil || c.PerCallDestination != nil {
		return fmt.Errorf("client update: %s is a mail client: it has no document to fetch and nothing to select", c.Name)
	}
	cl, conn, err := dial(c.Console)
	if err != nil {
		return fmt.Errorf("client update: %w", err)
	}
	defer func() { _ = conn.Close() }()
	before := snapshot(p.clientDir(c.Name))
	resp, err := generate(cl, p, &codegenpb.GenerateEgressClientRequest{Lock: p.lock, Name: c.Name, Mail: true}, nil, true)
	if err != nil {
		return fmt.Errorf("client update: %w", err)
	}
	report(resp, diffSnapshots(before, snapshot(p.clientDir(c.Name))))
	return nil
}
