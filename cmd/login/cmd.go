// Package login implements `w17ctl login` — email + password sign-in over the
// console's gRPC auth gateway (ONE endpoint; the same console address every
// other w17ctl command dials). The console mints the bearer; the client only
// stores it in the machine-local credential store (~/.w17/auth.yaml) and caches
// the caller's org memberships. Missing inputs are prompted interactively
// (email → password → host). See docs/specs/plugins/auth-cli-login-and-orgs.md.
package login

import (
	"context"
	"fmt"
	"net"
	"sort"
	"time"

	"github.com/wandering-compiler/w17ctl/internal/authstore"
	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/w17ctl/internal/prompter"
)

// loginTimeout bounds the whole sign-in round-trip.
const loginTimeout = 60 * time.Second

// newPrompter is indirected so tests can pump scripted answers.
var newPrompter = prompter.NewStdinPrompter

// Cmd is `w17ctl login [host]`.
type Cmd struct {
	Host  string `arg:"" optional:"" help:"Console host to log in to (e.g. grpcs://api.w17.app:50051). Prompted if omitted; defaults to the current console."`
	Email string `name:"email" help:"Email to sign in with (prompted if omitted)."`
	// Password also reads W17_PASSWORD to keep it out of shell history / argv;
	// when neither is set it is prompted for, hidden (no echo).
	Password string `name:"password" env:"W17_PASSWORD" help:"Password (prefer the W17_PASSWORD env var over the flag; prompted, hidden, if omitted)."`
}

func (c *Cmd) Run() error {
	p := newPrompter()
	host, email, password, err := c.gather(p)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), loginTimeout)
	defer cancel()

	token, userID, err := core.SignIn(ctx, host, email, password)
	if err != nil {
		return fmt.Errorf("login: %w", err)
	}

	// Orgs — best-effort with the freshly minted bearer (a token already
	// exists; the cache can refresh later via `w17ctl org list`).
	orgs, orgErr := core.ListMyOrgs(ctx, host, token)
	if orgErr != nil {
		fmt.Fprintf(core.Stdout, "warning: logged in, but couldn't list organizations: %v\n", orgErr)
	}

	return store(host, token, userID, email, orgs)
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
	host = c.Host
	if host == "" {
		if host, err = p.Text("Console host", defaultHost()); err != nil {
			return "", "", "", err
		}
		if host == "" {
			return "", "", "", fmt.Errorf("login: no console host — pass it as an argument or at the prompt")
		}
	}
	return host, email, password, nil
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
func store(host, token, userID, email string, orgs []core.AuthOrg) error {
	st, err := authstore.LoadDefault()
	if err != nil {
		return err
	}
	inst := &authstore.Instance{
		URL:   host,
		Token: token,
		User:  &authstore.User{ID: userID, Email: email},
		Orgs:  toStoreOrgs(orgs),
	}
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
	return nil
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
