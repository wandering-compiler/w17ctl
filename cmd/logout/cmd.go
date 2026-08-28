// Package logout implements `w17ctl logout` — drop a stored console
// credential. Local-only: it removes the bearer from ~/.w17/auth.yaml. (A
// server-side session revoke can be layered on later; the token also
// expires on its own TTL.) See docs/specs/plugins/auth-cli-login-and-orgs.md.
package logout

import (
	"fmt"

	"github.com/wandering-compiler/w17ctl/internal/authstore"
	"github.com/wandering-compiler/w17ctl/internal/core"
)

// Cmd is `w17ctl logout [url]`.
type Cmd struct {
	URL string `arg:"" optional:"" help:"Console URL to log out of. Default: the current default instance."`
}

func (c *Cmd) Run() error {
	st, err := authstore.LoadDefault()
	if err != nil {
		return err
	}

	target := c.URL
	if target == "" {
		if inst := st.ActiveInstance(); inst != nil {
			target = inst.URL
		}
	}
	if target == "" {
		fmt.Fprintln(core.Stdout, "Not logged in to any console.")
		return nil
	}
	if st.Instance(target) == nil {
		fmt.Fprintf(core.Stdout, "Not logged in to %s.\n", target)
		return nil
	}

	st.RemoveInstance(target)
	if err := authstore.SaveDefault(st); err != nil {
		return err
	}
	fmt.Fprintf(core.Stdout, "Logged out of %s.\n", target)
	return nil
}
