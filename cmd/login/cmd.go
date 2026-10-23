// Package login implements `w17ctl login` — sign in to a console and store the
// session in the machine-local credential store (~/.w17/auth.yaml), with the
// caller's org memberships cached beside it.
//
// Two ways in, one result:
//
//   - the browser (default): w17ctl is a public OAuth 2.1 client of the
//     console's authorization server (core.SignInWithBrowser) — the user signs
//     in and approves on the console's own pages, and w17ctl never sees the
//     password;
//   - --password: email + password over the console's gRPC auth gateway
//     (core.SignIn), for a host without a browser. --email or W17_PASSWORD
//     select it too, so every script written before the browser login keeps
//     working unchanged.
//
// Either way the session is bound to this machine's key for that console
// (~/.w17/device-<hash>.key, made on the first login) when the console binds
// devices, and the store records which key.
package login

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sort"
	"time"

	"golang.org/x/term"

	"github.com/wandering-compiler/w17ctl/internal/authstore"
	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/prompter"
	"github.com/wandering-compiler/w17ctl/internal/ui"
)

// loginTimeout bounds the password sign-in round-trip, and each console call
// around the browser login (the user's part has its own bound).
const loginTimeout = 60 * time.Second

// newPrompter is indirected so tests can pump scripted answers.
var newPrompter = prompter.NewStdinPrompter

func init() { core.OpenBrowserFn = ui.OpenBrowser }

// Cmd is `w17ctl login [host]`.
type Cmd struct {
	Host string `arg:"" optional:"" help:"Console host to log in to (e.g. grpcs://api.w17.app:50051). Prompted if omitted; defaults to the current console."`
	// Method picks the sign-in; empty asks in a terminal and is the browser
	// on this machine elsewhere (a script), as before the choice existed.
	Method string `name:"method" help:"How to sign in: browser (a browser on this machine), code (a browser on any device; you paste the code it shows — for w17ctl over ssh or in a remote workspace), password (email + password here). Asked when omitted in a terminal; browser otherwise."`
	// NoBrowser selects the email + password sign-in in the terminal (a host
	// without a browser).
	NoBrowser bool   `name:"no-browser" help:"Same as --method password. Implied by --email, --password or W17_PASSWORD."`
	Email     string `name:"email" help:"Email to sign in with; implies --no-browser (prompted if omitted)."`
	// Password also reads W17_PASSWORD to keep it out of shell history / argv;
	// when neither is set it is prompted for, hidden (no echo). Either implies
	// the password sign-in, as before the browser one existed.
	Password string `name:"password" env:"W17_PASSWORD" help:"Password; implies --no-browser (prefer the W17_PASSWORD env var over the flag; prompted, hidden, if omitted)."`
	// Code answers the console's second factor for the password sign-in;
	// prompted when omitted and the console asks for one.
	Code   string `name:"code" env:"W17_MFA_CODE" help:"Second-factor code for the password sign-in: from your authenticator app, a recovery code, or the one the console e-mails you. Prompted when the console asks and this is unset."`
	Issuer string `name:"issuer" env:"W17_CONSOLE_OAUTH_ISSUER" help:"The console's OAuth issuer for the browser sign-in. Default: the console host over https without the gRPC port (grpcs://api.w17.app:50051 → https://api.w17.app); set it when the console's web API is elsewhere (a local dev stack)."`
}

// The sign-in methods, as --method names them and as the menu offers them.
const (
	methodBrowser  = "browser"
	methodCode     = "code"
	methodPassword = "password"
)

var methodChoices = []struct{ method, label string }{
	{methodBrowser, "Browser on this computer"},
	{methodCode, "Browser on another device — paste the code it shows (w17ctl over ssh, a remote workspace)"},
	{methodPassword, "Email and password here"},
}

// stdinIsTerminal is indirected so tests can stand in for a person.
var stdinIsTerminal = func() bool { return term.IsTerminal(int(os.Stdin.Fd())) }

// method is the sign-in to run: the flags', else asked in a terminal, else
// the browser — what a script got before the choice existed.
func (c *Cmd) method(p prompter.Prompter) (string, error) {
	switch {
	case c.passwordSignIn():
		return methodPassword, nil
	case c.Method == methodBrowser || c.Method == methodCode:
		return c.Method, nil
	case c.Method != "":
		return "", fmt.Errorf("login: --method %q: want browser, code or password", c.Method)
	case !stdinIsTerminal():
		return methodBrowser, nil
	}
	labels := make([]string, len(methodChoices))
	for i, m := range methodChoices {
		labels[i] = m.label
	}
	got, err := p.Select("How do you want to sign in?", labels, labels[0])
	if err != nil {
		return "", err
	}
	for _, m := range methodChoices {
		if m.label == got {
			return m.method, nil
		}
	}
	return "", fmt.Errorf("login: no sign-in method %q", got)
}

func (c *Cmd) Run() error {
	p := newPrompter()
	method, err := c.method(p)
	if err != nil {
		return err
	}
	if method != methodPassword {
		err := c.runBrowser(p, method == methodCode)
		if !errors.Is(err, core.ErrOAuthUnavailable) {
			return err
		}
		// A console from before its authorization server (oauth_server)
		// offers no browser sign-in: the password one is what it has.
		fmt.Fprintf(core.Stdout, "%s does not offer browser sign-in yet — signing in with email and password\n", c.Host)
	}
	host, email, password, err := c.gather(p)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), loginTimeout)
	defer cancel()

	signed, err := core.SignInWithSecondFactor(ctx, host, email, password, func() (string, error) {
		if c.Code != "" {
			return c.Code, nil
		}
		return p.Text("Code from your authenticator app, or the one the console e-mailed you", "")
	})
	if err != nil {
		return fmt.Errorf("login: %w", err)
	}
	return finish(ctx, host, signed, email)
}

// passwordSignIn reports whether this login is the email + password one:
// asked for, or implied by an email or a password already given — the shape
// of every unattended login written before the browser one existed.
func (c *Cmd) passwordSignIn() bool {
	return c.NoBrowser || c.Method == methodPassword || c.Email != "" || c.Password != ""
}

// runBrowser is the default login: the console's OAuth authorization server,
// in the browser.
func (c *Cmd) runBrowser(p prompter.Prompter, withCode bool) error {
	host, err := c.gatherHost(p)
	if err != nil {
		return err
	}
	c.Host = host // a fallback to the password sign-in does not ask for it again
	issuer := core.OAuthIssuer(c.Issuer, host)
	if issuer == "" {
		return fmt.Errorf("login: cannot derive the console's OAuth issuer from %q — pass --issuer", host)
	}
	st, err := authstore.LoadDefault()
	if err != nil {
		return err
	}
	fmt.Fprintf(core.Stdout, "Signing in to %s through %s\n", host, issuer)
	ctx, cancel := context.WithTimeout(context.Background(), core.OAuthCallbackWait+2*loginTimeout)
	defer cancel()
	signed, err := core.SignInWithBrowser(ctx, core.OAuthLogin{
		Addr:         host,
		Issuer:       issuer,
		ClientID:     st.OAuthClientID(issuer),
		SaveClientID: func(id string) error { return saveOAuthClientID(issuer, id) },
		ReadCode:     readCode(p, withCode),
	})
	if err != nil {
		return fmt.Errorf("login: %w", err)
	}
	orgCtx, orgCancel := context.WithTimeout(context.Background(), loginTimeout)
	defer orgCancel()
	return finish(orgCtx, host, signed, "")
}

// readCode asks for the code the console's code page shows, for the sign-in
// with a code; nil for the browser on this machine.
func readCode(p prompter.Prompter, withCode bool) func() (string, error) {
	if !withCode {
		return nil
	}
	return func() (string, error) { return p.Text("Code", "") }
}

// saveOAuthClientID keeps the client id the issuer registered w17ctl under.
func saveOAuthClientID(issuer, clientID string) error {
	st, err := authstore.LoadDefault()
	if err != nil {
		return err
	}
	st.SetOAuthClientID(issuer, clientID)
	return authstore.SaveDefault(st)
}

// finish is what every sign-in ends with: the org cache, then the store.
func finish(ctx context.Context, host string, signed core.SignedIn, email string) error {
	// Orgs — best-effort with the freshly minted bearer (a token already
	// exists; the cache can refresh later via `w17ctl org list`).
	orgs, orgErr := core.ListMyOrgsAs(ctx, host, signed.Token, signed.Device)
	if orgErr != nil {
		fmt.Fprintf(core.Stdout, "warning: logged in, but couldn't list organizations: %v\n", orgErr)
	}
	return store(host, signed, email, orgs)
}

// gather resolves the three inputs, prompting interactively for any not
// supplied via flag / env. Fully-flagged invocations (--email + password +
// host) never touch the prompter, so scripts and CI stay non-interactive.
func (c *Cmd) gather(p prompter.Prompter) (host, email, password string, err error) {
	email = c.Email
	if email == "" {
		if email, err = p.Text("Email", ""); err != nil {
			return "", "", "", err
		}
		if email == "" {
			return "", "", "", fmt.Errorf("login: email is required")
		}
	}
	password = c.Password
	if password == "" {
		if password, err = p.Password("Password"); err != nil {
			return "", "", "", err
		}
		if password == "" {
			return "", "", "", fmt.Errorf("login: password is required")
		}
	}
	if host, err = c.gatherHost(p); err != nil {
		return "", "", "", err
	}
	return host, email, password, nil
}

// gatherHost is the console to log in to: the argument, else prompted with
// the current console as the default.
func (c *Cmd) gatherHost(p prompter.Prompter) (string, error) {
	if c.Host != "" {
		return c.Host, nil
	}
	host, err := p.Text("Console host", defaultHost())
	if err != nil {
		return "", err
	}
	if host == "" {
		return "", fmt.Errorf("login: no console host — pass it as an argument or at the prompt")
	}
	return host, nil
}

// defaultHost is the prompt default: the current console instance, else the
// compiled-in console address.
func defaultHost() string {
	if st, err := authstore.LoadDefault(); err == nil {
		if inst := st.ActiveInstance(); inst != nil && inst.URL != "" {
			// A dead LOCAL console is not offered. Its port is ephemeral and
			// docker hands freed ports on, so a stale entry names an address
			// that is either gone or now somebody else's console — and
			// offering it as the default is how a login lands on the wrong
			// one without anybody choosing it.
			//
			// The pruning this complements runs AFTER a successful login, so
			// on its own it never reaches the prompt that caused the problem.
			// One probe, of the ONE address about to be suggested — not a
			// sweep of every stored instance, which would put a dial on the
			// critical path of every login to save a keystroke.
			if !authstore.IsLoopbackURL(inst.URL) || tcpListening(authstore.StripScheme(inst.URL)) {
				return inst.URL
			}
		}
	}
	return core.DefaultConsoleAddr
}

// store writes the just-logged-in instance to the credential store as the
// active console.
func store(host string, signed core.SignedIn, email string, orgs []core.AuthOrg) error {
	st, err := authstore.LoadDefault()
	if err != nil {
		return err
	}
	inst := &authstore.Instance{
		URL:    host,
		Token:  signed.Token,
		Device: signed.Device,
		Orgs:   toStoreOrgs(orgs),
	}
	// The browser login learns no identity (the token response names no
	// user); an empty one is left out rather than stored as "".
	if signed.UserID != "" || email != "" {
		inst.User = &authstore.User{ID: signed.UserID, Email: email}
	}
	inst.RecordSessionExpiry(signed.ExpiresAt)
	// Read BEFORE the pointer moves, and used after the prune below — a login
	// to a second console takes the active one with it, and until `console use`
	// existed there was no way back but editing the YAML.
	wasActive := st.DefaultInstance
	st.SetInstance(inst)
	st.SetDefaultInstance(host) // the just-logged-in instance becomes active
	// Login is the one moment this store is already being rewritten, so it is
	// where dead LOCAL credentials go. They are not merely clutter: a dev
	// console's port is ephemeral, docker reuses freed ports, and a token left
	// behind for a port another project's console later occupies is presented
	// to that console as if it were its own. The refusal is `invalid
	// credentials`, which has two other causes and reads like all three.
	if n := st.PruneDeadLoopbackInstances(host, tcpListening); n > 0 {
		fmt.Fprintf(core.Stdout, "login: dropped %d stored credential(s) for local consoles that are gone\n", n)
	}
	if err := authstore.SaveDefault(st); err != nil {
		return err
	}
	printResult(host, inst)
	reportTheActiveConsoleMoved(st, wasActive, host)
	return nil
}

// reportTheActiveConsoleMoved says out loud that this login took the active
// console with it.
//
// The behaviour is unchanged — the just-logged-in console becomes active, which
// is right for the first login and for the common case of one console. What was
// missing is that it is SILENT, and the shape it is silent about is the normal
// one: production holds the plugin signing key, a local console carries codegen,
// dev locks, `plugin dev` and e2e. Logging in to publish moved the pointer, and
// the next ordinary `codegen` asked production to sign a development lock and
// registered a throwaway project in the production registry. Nothing failed.
//
// ⚠️ It runs AFTER the dead-loopback prune, and checks the old console is still
// stored. Logging into production while the local console happens to be down
// drops that credential (an ephemeral port another project's console may later
// occupy), and advising `console use` on a credential this command just deleted
// would be worse than saying nothing.
func reportTheActiveConsoleMoved(st *authstore.Store, wasActive, host string) {
	if wasActive == "" || wasActive == host {
		return
	}
	if st.Instance(wasActive) == nil {
		fmt.Fprintf(core.Stdout, "login: the active console is now %s\n", host)
		return
	}
	fmt.Fprintf(core.Stdout,
		"login: the active console is now %s (was %s)\n"+
			"  every command without --console or W17_CONSOLE_ADDR now talks to it, including codegen\n"+
			"  switch back: w17ctl console use %s\n",
		host, wasActive, wasActive)
}

func toStoreOrgs(orgs []core.AuthOrg) []*authstore.Org {
	if len(orgs) == 0 {
		return nil
	}
	out := make([]*authstore.Org, 0, len(orgs))
	for _, o := range orgs {
		out = append(out, &authstore.Org{ID: o.ID, Slug: o.Slug, Name: o.Name, Kind: o.Kind, Role: o.Role})
	}
	return out
}

// tcpListening reports whether anything answers at hostPort right now. A short
// deadline on purpose: this runs inside `login`, and a slow probe of a dead
// address would make every login feel broken to save a diagnosis later.
func tcpListening(hostPort string) bool {
	conn, err := net.DialTimeout("tcp", hostPort, 300*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func printResult(host string, inst *authstore.Instance) {
	fmt.Fprintf(core.Stdout, "✓ Logged in to %s\n", host)
	if inst.User != nil && inst.User.Email != "" {
		fmt.Fprintf(core.Stdout, "  identity: %s\n", inst.User.Email)
	} else if inst.User != nil && inst.User.ID != "" {
		fmt.Fprintf(core.Stdout, "  identity: %s\n", inst.User.ID)
	}
	switch len(inst.Orgs) {
	case 0:
		fmt.Fprintln(core.Stdout, "  organizations: (none yet)")
	default:
		fmt.Fprintf(core.Stdout, "  organizations (%d):\n", len(inst.Orgs))
		orgs := append([]*authstore.Org(nil), inst.Orgs...)
		sort.Slice(orgs, func(i, j int) bool { return orgs[i].Slug < orgs[j].Slug })
		for _, o := range orgs {
			fmt.Fprintf(core.Stdout, "    - %s (%s) — %s\n", o.Slug, kindOrUnknown(o.Kind), o.Role)
		}
	}
}

func kindOrUnknown(kind string) string {
	if kind == "" {
		return "org"
	}
	return kind
}
