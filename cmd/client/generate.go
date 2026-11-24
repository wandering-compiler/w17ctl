package client

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"google.golang.org/grpc"

	"github.com/wandering-compiler/w17ctl/internal/core"
	codegenpb "github.com/wandering-compiler/sdk/go/pb/w17compiler"
)

// GenerateCmd is `w17ctl client generate`.
type GenerateCmd struct {
	Source             string   `arg:"" optional:"" help:"The OpenAPI document (JSON or YAML) or the WSDL of a SOAP service: an http(s) URL, or a path to a file. None with --mail."`
	Name               string   `name:"name" help:"The client's name (snake_case). Default: derived from the source — the organisation in the host name, or the directory of the file."`
	Endpoint           []string `name:"endpoint" help:"An operation to include, as its request line: \"GET /users/{id}\" (path as the document writes it; * for any method; a trailing /* for a subtree). Repeatable."`
	Operation          []string `name:"operation" help:"An operation to include, by its operationId (a WSDL: by its operation name). Repeatable."`
	Idempotent         []string `name:"idempotent" help:"A SOAP operation that only reads and may be repeated: it is retried on a 429 or 5xx, as a GET is. A SOAP call is a POST, so none is retried unless named. Repeatable."`
	Console            string   `name:"console" placeholder:"HOST:PORT" env:"W17_CONSOLE_ADDR" help:"gRPC endpoint of the console. Optional — falls back to the console you are logged in to, then the binary's compile-time default."`
	PerCallCredential  bool     `name:"per-call-credential" help:"The credential comes with each call, not with the deployment (a customer's own token at the upstream): the project's getter takes it as a parameter, and the DSN writes it per_call (?auth=bearer&token=per_call)."`
	PerCallDestination bool     `name:"per-call-destination" help:"The destination comes with each call (a customer's webhook URL): the getter takes it (egress.To(url)), and the DSN is a policy, dynamic?…, that refuses private, loopback and link-local addresses unless allow= names them."`
	Mail               bool     `name:"mail" help:"A mail client instead of an API's: no document, one method, Send(w17.MailMessage) — the getter egress.Mail() sends through the SMTP server or the transactional-mail API (Postmark, SendGrid, Mailgun, Resend) its DSN names. --name defaults to mail."`
	Force              bool     `name:"force" help:"Replace an existing client of the same name. Without it generate refuses one — 'client update' is how an existing client is regenerated."`
}

func (c *GenerateCmd) Run() error {
	p, err := loadProject()
	if err != nil {
		return fmt.Errorf("client generate: %w", err)
	}
	if c.Name != "" && !nameRE.MatchString(c.Name) {
		return fmt.Errorf("client generate: --name %q: a client name is snake_case", c.Name)
	}
	if err := c.checkMail(); err != nil {
		return fmt.Errorf("client generate: %w", err)
	}
	if c.Mail && c.Name == "" {
		c.Name = "mail"
	}
	if c.Name != "" && !c.Force {
		// Before fetching and generating: refusing after would waste both.
		if _, err := os.Stat(filepath.Join(p.clientDir(c.Name), configFile)); err == nil {
			return fmt.Errorf("client generate: client %q already exists — `w17ctl client update %s` regenerates it, or pass --force to replace it", c.Name, c.Name)
		}
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("client generate: %w", err)
	}
	var doc []byte
	var source string
	if !c.Mail {
		if doc, source, err = fetch(c.Source, p.root, cwd); err != nil {
			return fmt.Errorf("client generate: %w", err)
		}
	}
	if c.Mail || c.PerCallCredential || c.PerCallDestination {
		// The console writes these clients' proto whatever the project's
		// engine, but only an engine from 0.2 compiles it (w17/mail.proto,
		// the per-call options) and only SDK 0.2 runs it.
		view, err := core.DescribeLock(c.Console, p.lock)
		if err != nil {
			return fmt.Errorf("client generate: %w", err)
		}
		if core.EngineBefore(view, "v0.2") {
			return fmt.Errorf("client generate: --mail and --per-call-* need codegen 0.2 or newer, and this project's engine is %s — its codegen could not compile the client; `w17ctl codegen-version` moves the project forward", view.GetSdkFloor())
		}
	}
	cl, conn, err := dial(c.Console)
	if err != nil {
		return fmt.Errorf("client generate: %w", err)
	}
	defer func() { _ = conn.Close() }()
	resp, err := generate(cl, p, &codegenpb.GenerateEgressClientRequest{
		Lock: p.lock, Source: source, Name: c.Name, Endpoints: c.Endpoint, Operations: c.Operation, Idempotent: c.Idempotent,
		PerCallCredential: c.PerCallCredential, PerCallDestination: c.PerCallDestination, Mail: c.Mail,
	}, doc, c.Force)
	if err != nil {
		return fmt.Errorf("client generate: %w", err)
	}
	report(resp, nil)
	return nil
}

// checkMail refuses what a mail client does not take, and a missing
// source for any other.
func (c *GenerateCmd) checkMail() error {
	if !c.Mail {
		if c.Source == "" {
			return errors.New("name the document: `client generate <url|path>` (or --mail for a mail client)")
		}
		return nil
	}
	switch {
	case c.Source != "":
		return errors.New("--mail takes no document: a mail client has one method, Send")
	case len(c.Endpoint)+len(c.Operation)+len(c.Idempotent) > 0:
		return errors.New("--endpoint, --operation and --idempotent select an API's operations; a mail client has one, Send")
	case c.PerCallCredential || c.PerCallDestination:
		return errors.New("a mail client's server and credential come from its DSN: --per-call-credential and --per-call-destination do not apply")
	}
	return nil
}

var nameRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

// documentNames are the names a client's document is kept under, after its
// format.
var documentNames = []string{"openapi.json", "openapi.yaml", "service.wsdl"}

// egressConsole is the part of the console these commands call.
type egressConsole interface {
	GenerateEgressClient(ctx context.Context, opts ...grpc.CallOption) (grpc.BidiStreamingClient[codegenpb.GenerateEgressClientRequest, codegenpb.GenerateEgressClientResponse], error)
}

// dial is swapped in tests.
var dial = func(console string) (egressConsole, io.Closer, error) {
	addr, err := core.ResolveConsoleAddr(console)
	if err != nil {
		return nil, nil, err
	}
	cl, conn, err := core.DialCodegen(addr)
	if err != nil {
		return nil, nil, err
	}
	return cl, conn, nil
}

const (
	// maxDocument matches the console's cap: a stream past it is refused
	// there, so it is refused here before it is sent.
	maxDocument = 64 << 20
	// chunk is one message's share of the document — well under the 4 MiB
	// the console's gateway→backend hop carries.
	chunk = 1 << 20
	// generateTimeout bounds one generation: a large document, converted
	// and compiled on the console.
	generateTimeout = 5 * time.Minute
)

// fetch reads the document: an http(s) URL with a GET, anything else as a
// file, a relative one resolved against base (the working directory for a
// path the developer typed, the project root for one client.yaml recorded).
// It returns the bytes and the source as client.yaml records it — a URL as
// given, a file relative to the project root when it is inside it, so the
// recorded source works from any checkout and any directory.
func fetch(source, root, base string) ([]byte, string, error) {
	if u, err := url.Parse(source); err == nil && (u.Scheme == "http" || u.Scheme == "https") {
		if err := refuseCredentials(u); err != nil {
			return nil, "", err
		}
		doc, err := fetchURL(u)
		return doc, source, err
	}
	path := source
	if !filepath.IsAbs(path) {
		path = filepath.Join(base, path)
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, "", fmt.Errorf("the document: %w", err)
	}
	if info.Size() > maxDocument {
		return nil, "", fmt.Errorf("the document is %d MiB; the console takes at most %d", info.Size()>>20, maxDocument>>20)
	}
	doc, err := os.ReadFile(path)
	if err != nil {
		return nil, "", fmt.Errorf("the document: %w", err)
	}
	recorded := source
	if rel, err := filepath.Rel(root, path); err == nil && !strings.HasPrefix(rel, "..") {
		recorded = filepath.ToSlash(rel)
	}
	return doc, recorded, nil
}

// credentialWords are words of a query parameter's name that make it a
// credential; credentialNames are whole names that do. Matched by word, not
// substring: `author`, `keyword` and `sortKey` are not credentials.
var (
	credentialWords = map[string]bool{"token": true, "secret": true, "password": true, "passwd": true, "pwd": true,
		"signature": true, "credential": true, "credentials": true, "jwt": true, "bearer": true, "apikey": true}
	credentialNames = map[string]bool{"key": true, "apikey": true, "hapikey": true, "accesskey": true, "privatekey": true, "secretkey": true,
		"auth": true, "authorization": true, "sig": true, "session": true, "sessionid": true, "sid": true, "pass": true}
	// keyOwners are the words that make a trailing `key` (or `key id`) a
	// credential — `api_key`, `X-Api-Key`, `subscription-key`,
	// `aws_access_key_id` — while `sortKey` and `partitionKey` stay names.
	keyOwners = map[string]bool{"api": true, "access": true, "auth": true, "app": true, "subscription": true,
		"secret": true, "private": true, "client": true, "service": true, "master": true, "license": true}
	nameWord = regexp.MustCompile(`[A-Z]?[a-z0-9]+|[A-Z]+`)
)

func isCredentialParam(name string) bool {
	lower := strings.ToLower(name)
	if strings.HasPrefix(lower, "x-amz-") || strings.HasPrefix(lower, "x-goog-") {
		return true
	}
	words := nameWord.FindAllString(name, -1)
	joined := ""
	for i, w := range words {
		w = strings.ToLower(w)
		words[i] = w
		if credentialWords[w] {
			return true
		}
		joined += w
	}
	if n := len(words); n >= 2 && words[n-1] == "id" {
		words = words[:n-1] // aws_access_key_id: the key, by id
	}
	for i := 1; i < len(words); i++ {
		if words[i] == "key" && i == len(words)-1 && keyOwners[words[i-1]] {
			return true
		}
	}
	return credentialNames[joined]
}

// refuseCredentials refuses a source URL that carries a credential: the
// source is recorded in client.yaml, which is committed (and signed, so it
// cannot be scrubbed afterwards without regenerating). A document behind
// authentication is downloaded first and generated from the file.
func refuseCredentials(u *url.URL) error {
	const how = "it would be committed in client.yaml — download the document and run `client generate <file>`"
	if u.User != nil {
		return fmt.Errorf("the source URL carries credentials (%s@); %s", u.User.Username(), how)
	}
	for k := range u.Query() {
		if isCredentialParam(k) {
			return fmt.Errorf("the source URL's query parameter %q looks like a credential; %s", k, how)
		}
	}
	return nil
}

var httpClient = &http.Client{Timeout: 2 * time.Minute}

func fetchURL(u *url.URL) ([]byte, error) {
	source := u.Redacted()
	req, err := http.NewRequest(http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json, application/yaml;q=0.9, */*;q=0.5")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", source, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: %s", source, resp.Status)
	}
	doc, err := io.ReadAll(io.LimitReader(resp.Body, maxDocument+1))
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", source, err)
	}
	if len(doc) > maxDocument {
		return nil, fmt.Errorf("fetch %s: the document exceeds %d MiB", source, maxDocument>>20)
	}
	return doc, nil
}

// maxImportRounds bounds the rounds of a WSDL's imports: each round fetches
// what the previous one's documents import.
const maxImportRounds = 8

// generate streams the request to the console and writes what comes back:
// the files the console returned, verbatim, and the document, byte for byte
// as it was sent (the signature covers its digest). A WSDL may import
// schemas: the console names the ones it lacks, they are fetched — the
// console never fetches — and the request goes again with them.
func generate(cl egressConsole, p *project, head *codegenpb.GenerateEgressClientRequest, doc []byte, replace bool) (*codegenpb.GenerateEgressClientResponse, error) {
	size := len(doc)
	for round := 0; ; round++ {
		resp, err := send(cl, head, doc)
		if err != nil {
			return nil, err
		}
		if len(resp.GetMissingImports()) == 0 {
			if err := write(p, resp, doc, replace); err != nil {
				return nil, err
			}
			return resp, nil
		}
		if round == maxImportRounds {
			return nil, fmt.Errorf("the document still imports more after %d rounds: %s", maxImportRounds, strings.Join(resp.GetMissingImports(), ", "))
		}
		if head.Imports == nil {
			head.Imports = map[string][]byte{}
		}
		for _, loc := range resp.GetMissingImports() {
			if _, done := head.GetImports()[loc]; done {
				return nil, fmt.Errorf("the console asked again for %s", loc)
			}
			b, _, err := fetch(loc, p.root, p.root)
			if err != nil {
				return nil, fmt.Errorf("import %s: %w", loc, err)
			}
			if size += len(b); size > maxDocument {
				return nil, fmt.Errorf("the document and its imports exceed %d MiB", maxDocument>>20)
			}
			head.Imports[loc] = b
		}
	}
}

// send streams one request — the header and the first chunk in one message,
// the rest of the document after it — and returns the answer.
func send(cl egressConsole, head *codegenpb.GenerateEgressClientRequest, doc []byte) (*codegenpb.GenerateEgressClientResponse, error) {
	ctx, cancel := context.WithTimeout(context.Background(), generateTimeout)
	defer cancel()
	stream, err := cl.GenerateEgressClient(ctx)
	if err != nil {
		return nil, err
	}
	for i := 0; i == 0 || i < len(doc); i += chunk {
		msg := &codegenpb.GenerateEgressClientRequest{Document: doc[i:min(i+chunk, len(doc))]}
		if i == 0 {
			head.Document = msg.GetDocument()
			msg = head
		}
		if err := stream.Send(msg); err != nil {
			if errors.Is(err, io.EOF) {
				break // the console answered early; Recv carries why
			}
			return nil, err
		}
	}
	if err := stream.CloseSend(); err != nil {
		return nil, err
	}
	return stream.Recv()
}

// write lays the client down. The console names the files, and the client
// accepts exactly the set a client is — `<name>.proto`, client.yaml, and
// openapi.json, openapi.yaml or service.wsdl — in exactly its directory; anything else is
// refused before a byte is written (a path with `..` or a `\` that is a
// separator on Windows included). An existing client is replaced only when
// replace is set. A previous document in the other format is removed: it
// would be a stale copy nothing verifies.
func write(p *project, resp *codegenpb.GenerateEgressClientResponse, doc []byte, replace bool) error {
	name := resp.GetName()
	if !nameRE.MatchString(name) {
		return fmt.Errorf("the console named the client %q", name)
	}
	dir := filepath.ToSlash(filepath.Join(p.protoDir, "clients", name)) + "/"
	allowed := map[string]bool{name + ".proto": true, configFile: true}
	for _, d := range documentNames {
		allowed[d] = true
	}
	files := map[string][]byte{}
	// A mail client has no document (and the request sent none).
	if resp.GetDocumentPath() != "" || len(doc) > 0 {
		docBase := strings.TrimPrefix(resp.GetDocumentPath(), dir)
		if !slices.Contains(documentNames, docBase) {
			return fmt.Errorf("the console put the document at %q, not %s{%s}", resp.GetDocumentPath(), dir, strings.Join(documentNames, ","))
		}
		files[resp.GetDocumentPath()] = doc
	}
	for _, f := range resp.GetFiles() {
		files[f.GetRelativePath()] = f.GetContents()
	}
	for rel := range files {
		if !strings.HasPrefix(rel, dir) || !allowed[rel[len(dir):]] {
			return fmt.Errorf("the console returned %q — a client is %s{%s.proto,%s,%s} and nothing else", rel, dir, name, configFile, strings.Join(documentNames, "|"))
		}
	}
	target := filepath.Join(p.root, filepath.FromSlash(dir))
	if _, err := os.Stat(filepath.Join(target, configFile)); err == nil && !replace {
		return fmt.Errorf("client %q already exists — `w17ctl client update %s` regenerates it, or pass --force to replace it", name, name)
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		return err
	}
	for _, stale := range documentNames {
		if _, keep := files[dir+stale]; !keep {
			_ = os.Remove(filepath.Join(target, stale))
		}
	}
	for rel, contents := range files {
		if err := os.WriteFile(filepath.Join(target, rel[len(dir):]), contents, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// report prints what was generated, and — on an update — which files
// changed.
func report(resp *codegenpb.GenerateEgressClientResponse, changed []string) {
	out := core.Stdout
	fmt.Fprintf(out, "client %s: %d operation(s)\n", resp.GetName(), len(resp.GetOperations()))
	for _, op := range resp.GetOperations() {
		fmt.Fprintf(out, "  %s\n", op)
	}
	if changed != nil {
		if len(changed) == 0 {
			fmt.Fprintln(out, "unchanged")
		} else {
			fmt.Fprintf(out, "changed: %s\n", strings.Join(changed, ", "))
		}
	}
	fmt.Fprintf(out, "\nconfigure it with (a secret — never commit it):\n  %s\n", resp.GetDsnTemplate())
	for _, n := range resp.GetAuthNotes() {
		fmt.Fprintf(out, "  note: %s\n", n)
	}
	for _, w := range resp.GetWarnings() {
		fmt.Fprintf(out, "warning: %s\n", w)
	}
}

// snapshot reads a client's files, to tell an update what changed.
func snapshot(dir string) map[string][]byte {
	out := map[string][]byte{}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if b, err := os.ReadFile(filepath.Join(dir, e.Name())); err == nil {
			out[e.Name()] = b
		}
	}
	return out
}

func diffSnapshots(before, after map[string][]byte) []string {
	changed := []string{}
	for name, b := range after {
		if !bytes.Equal(before[name], b) {
			changed = append(changed, name)
		}
	}
	for name := range before {
		if _, ok := after[name]; !ok {
			changed = append(changed, name+" (removed)")
		}
	}
	sort.Strings(changed)
	return changed
}
