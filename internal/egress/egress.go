// Package egress checks the project's egress clients
// (`<proto root>/clients/<name>/`, docs/todos/egress-rest-client.md) against
// the console — the one check `w17ctl verify` (the release gate) and
// `w17ctl codegen` (before it compiles them) share.
package egress

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/wandering-compiler/w17ctl/internal/core"
	codegenpb "github.com/wandering-compiler/sdk/go/pb/w17compiler"
)

// DriftError is a client under <proto root>/clients/ that does not verify:
// edited, copied from another project or under another name, or signed by
// another console. Not lock drift — `codegen` does not touch a client — so
// it gets its own headline (see verify.Run).
type DriftError struct{ Name, Msg string }

func (e *DriftError) Error() string { return e.Msg }

// Verify asks the console to check each committed client's
// signature (docs/todos/egress-rest-client.md). The client holds no
// verifier; it ships the proto and client.yaml, and the document's digest
// rather than the document — with CRLF read as LF, as the console's own
// digest is (a Windows checkout rewrites line endings).
func Verify(out io.Writer, cl core.CodegenConsole, root, protoDir string, lock []byte) (int, []error) {
	dir := filepath.Join(root, protoDir, "clients")
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	names, errs := layout(dir)
	for _, name := range names {
		fmt.Fprintf(out, "verifying client %s…\n", name)
		req, err := egressRequest(filepath.Join(dir, name), name, lock)
		if err != nil {
			errs = append(errs, &DriftError{Name: name, Msg: fmt.Sprintf("clients/%s: %v", name, err)})
			continue
		}
		ctx, cancel := core.ClientCtx()
		res, verr := cl.VerifyEgressClient(ctx, req)
		cancel()
		switch {
		case verr != nil:
			errs = append(errs, fmt.Errorf("clients/%s: %w", name, verr))
		case !res.GetOk():
			errs = append(errs, &DriftError{Name: name, Msg: res.GetMessage()})
		}
	}
	return len(names) + len(errs), errs
}

// layout walks clients/ and returns the clients — the directories holding a
// client.yaml — and a DriftError for every .proto in it that is not a
// client's own. Codegen compiles every .proto under the proto root, so one
// here that is not part of a signed client would reach generated code
// unchecked: beside the clients, inside one, in a directory that is no
// client. Other files (.DS_Store, an editor's swap file) are not compiled
// and are not this gate's business.
func layout(dir string) ([]string, []error) {
	var names []string
	var errs []error
	stray := func(rel string) {
		errs = append(errs, &DriftError{Name: rel, Msg: fmt.Sprintf("clients/%s is a .proto that is not part of a client — only `w17ctl client` writes protos here; remove it", rel)})
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, []error{fmt.Errorf("clients: %w", err)}
	}
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() {
			if isProto(name) {
				stray(name)
			}
			continue
		}
		client := true
		if _, err := os.Stat(filepath.Join(dir, name, "client.yaml")); err != nil {
			client = false
		}
		err := filepath.WalkDir(filepath.Join(dir, name), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !isProto(d.Name()) {
				return err
			}
			rel, _ := filepath.Rel(dir, path)
			if !client || filepath.ToSlash(rel) != name+"/"+name+".proto" {
				stray(filepath.ToSlash(rel))
			}
			return nil
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("clients/%s: %w", name, err))
		}
		if client {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names, errs
}

func isProto(name string) bool { return strings.HasSuffix(name, ".proto") }

func egressRequest(dir, name string, lock []byte) (*codegenpb.VerifyEgressClientRequest, error) {
	proto, err := os.ReadFile(filepath.Join(dir, name+".proto"))
	if err != nil {
		return nil, err
	}
	cfgBytes, err := os.ReadFile(filepath.Join(dir, "client.yaml"))
	if err != nil {
		return nil, err
	}
	var cfg struct {
		Document string `yaml:"document"`
	}
	if err := yaml.Unmarshal(cfgBytes, &cfg); err != nil {
		return nil, fmt.Errorf("client.yaml: %w", err)
	}
	if cfg.Document != "openapi.json" && cfg.Document != "openapi.yaml" {
		return nil, fmt.Errorf("client.yaml names the document %q", cfg.Document)
	}
	doc, err := os.ReadFile(filepath.Join(dir, cfg.Document))
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(bytes.ReplaceAll(doc, []byte("\r\n"), []byte("\n")))
	return &codegenpb.VerifyEgressClientRequest{
		Lock: lock, Name: name, Proto: proto, ClientYaml: cfgBytes, DocumentSha256: hex.EncodeToString(sum[:]),
	}, nil
}

// DriftOnly reports whether every error is a client that does not verify,
// and names them.
func DriftOnly(errs []error) ([]string, bool) {
	var names []string
	for _, e := range errs {
		var d *DriftError
		if !errors.As(e, &d) {
			return nil, false
		}
		names = append(names, d.Name)
	}
	return names, len(names) > 0
}

// Advice is what to do about clients that do not verify.
const Advice = "a client is changed only by regenerating it: `w17ctl client update <name>` (or restore it from git); anything else under clients/ does not belong there"
