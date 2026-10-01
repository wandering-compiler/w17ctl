// Package console implements `w17ctl console` — list the consoles you are
// logged into and choose which one subsequent commands talk to.
//
// # Why this exists
//
// ~/.w17/auth.yaml holds one entry per console (`instances`) plus a pointer at
// the active one (`default_instance`), and the store has always assumed the
// pointer is something a person moves: `RemoveInstance` clears it on logout and
// says in as many words that "a remaining instance is NOT auto-promoted — the
// user chooses explicitly". There was nothing to choose WITH. The only writer of
// that pointer was `login`, which sets it to whatever it just authenticated
// against.
//
// That made a two-console setup a trap rather than a configuration. Signing a
// plugin release needs the console holding the signing key — production — while
// everyday work belongs on a local console: codegen, dev locks, `plugin dev`,
// e2e. Logging into production to publish silently moved the default there, and
// the next ordinary `w17ctl codegen` would ask the PRODUCTION console to sign a
// development lock and register a throwaway project in the production registry.
// Nothing fails; it just quietly happens on the wrong console.
//
// Switching back meant hand-editing YAML, which is not a thing to put in a
// workflow people repeat.
//
// # Deliberately offline
//
// Choosing the active console does not call it. The pointer is local state, and
// requiring a round-trip would make the command unusable in the case it is most
// needed — the console you are leaving is down, or unreachable from here. A
// stale token surfaces at the next command that actually talks, with that
// command's own error.
package console

import (
	"fmt"
	"sort"
	"strings"

	"github.com/wandering-compiler/w17ctl/internal/authstore"
	"github.com/wandering-compiler/w17ctl/internal/core"
)

// Cmd is the `w17ctl console` group.
type Cmd struct {
	List ListCmd `cmd:"" help:"List the consoles you are logged into, marking the active one."`
	Use  UseCmd  `cmd:"" help:"Point subsequent commands at one of them — the console a command talks to when no --console flag or W17_CONSOLE_ADDR says otherwise."`
}

// ListCmd is `w17ctl console list`.
type ListCmd struct{}

func (c *ListCmd) Run() error {
	st, err := authstore.LoadDefault()
	if err != nil {
		return err
	}
	urls := storedURLs(st)
	if len(urls) == 0 {
		fmt.Fprintln(core.Stdout, "Not logged into any console. Run `w17ctl login <console-url>`.")
		return nil
	}
	fmt.Fprintln(core.Stdout, "Logged in to:")
	for _, u := range urls {
		marker := " "
		if u == st.DefaultInstance {
			marker = "*"
		}
		identity := ""
		if inst := st.Instance(u); inst != nil && inst.User != nil && inst.User.Email != "" {
			identity = "  " + inst.User.Email
		}
		fmt.Fprintf(core.Stdout, "  %s %s%s\n", marker, u, identity)
	}
	if st.DefaultInstance == "" {
		fmt.Fprintln(core.Stdout, "(no active console — pick one with `w17ctl console use <url>`)")
	}
	return nil
}

// UseCmd is `w17ctl console use <url>`.
type UseCmd struct {
	URL string `arg:"" help:"Console to make active. The scheme is optional — 'api.w17.app:50051' finds 'grpcs://api.w17.app:50051'."`
}

func (c *UseCmd) Run() error {
	st, err := authstore.LoadDefault()
	if err != nil {
		return err
	}
	key, err := resolveStored(st, c.URL)
	if err != nil {
		return err
	}
	if st.DefaultInstance == key {
		fmt.Fprintf(core.Stdout, "%s is already the active console.\n", key)
		return nil
	}
	// SetDefaultInstance refuses an instance that is not stored, which is the
	// invariant worth keeping: you cannot default to a console you never logged
	// into. resolveStored has already found the key in the map, so a false here
	// would mean the two disagree.
	if !st.SetDefaultInstance(key) {
		return fmt.Errorf("console use: %s is not a stored login (this is a bug in w17ctl)", key)
	}
	if err := authstore.SaveDefault(st); err != nil {
		return err
	}
	fmt.Fprintf(core.Stdout, "Active console: %s\n", key)
	fmt.Fprintln(core.Stdout, "  (a --console flag or W17_CONSOLE_ADDR still wins over this for one command)")
	return nil
}

// resolveStored maps what the user typed onto a stored instance key.
//
// Exact first. Then scheme-insensitively and case-insensitively, because the
// stored key is whatever `login` was given — `grpcs://api.w17.app:50051` from a
// copied URL, `localhost:13444` from a typed host — and nobody should have to
// remember which form a particular login used.
//
// An AMBIGUOUS match is refused rather than resolved. Two logins differing only
// by scheme are two different consoles as far as this store is concerned, and
// picking one of them for the user is how the wrong console becomes active
// without anybody choosing it — the failure this command exists to prevent.
func resolveStored(st *authstore.Store, want string) (string, error) {
	want = strings.TrimSpace(want)
	urls := storedURLs(st)
	if len(urls) == 0 {
		return "", fmt.Errorf("console use: not logged into any console — run `w17ctl login %s` first", want)
	}
	if st.Instance(want) != nil {
		return want, nil
	}

	needle := strings.ToLower(authstore.StripScheme(want))
	var hits []string
	for _, u := range urls {
		if strings.ToLower(authstore.StripScheme(u)) == needle {
			hits = append(hits, u)
		}
	}
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		return "", fmt.Errorf("console use: not logged into %q. You are logged into:\n%s\n"+
			"  fix: `w17ctl login %s` to add it, or name one of the above",
			want, indentList(urls), want)
	default:
		return "", fmt.Errorf("console use: %q matches %d stored logins (%s) — name one exactly, "+
			"because they are different consoles to this store",
			want, len(hits), strings.Join(hits, ", "))
	}
}

func storedURLs(st *authstore.Store) []string {
	out := make([]string, 0, len(st.Instances))
	for u := range st.Instances {
		out = append(out, u)
	}
	sort.Strings(out)
	return out
}

func indentList(urls []string) string {
	var b strings.Builder
	for i, u := range urls {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString("    " + u)
	}
	return b.String()
}
