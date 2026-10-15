// Package key implements `w17ctl key` — the device keys w17ctl signs DPoP
// proofs (RFC 9449) with.
//
// A console whose auth binds devices binds every token to a key and accepts it
// only with a proof signed by that key. w17ctl keeps one key per console
// (~/.w17/device-<hash>.key), made by the first `login` to it; nothing else
// needs a command. The exception is a machine account's token for a CI job or
// a deployment: the console issues it for the PUBLIC half of the key the job
// will sign with, and `key public` prints it.
//
//	W17_DPOP_KEY=$PWD/ci-device.key w17ctl key public   # makes the key, prints its public JWK
//
// The job then sets W17_DPOP_KEY to that file beside W17_TOKEN.
package key

import (
	"encoding/json"
	"fmt"
	"os"

	jose "github.com/go-jose/go-jose/v4"

	"github.com/wandering-compiler/w17ctl/internal/core"
)

// Cmd is `w17ctl key`.
type Cmd struct {
	Public PublicCmd `cmd:"" help:"Print a device key's public half as a JWK (made first if there is none): the key W17_DPOP_KEY names, else this machine's key for the console (the argument; default the active console). Give this JWK to the console when issuing a machine account's token, and set W17_DPOP_KEY to the same file where that token is used."`
}

// PublicCmd is `w17ctl key public [console]`.
type PublicCmd struct {
	Console string `arg:"" optional:"" help:"Console whose key to print (e.g. grpcs://api.w17.app:50051). Default: the active console. Ignored when W17_DPOP_KEY is set."`
}

// Run prints the public JWK on stdout — and nothing else there, so it can be
// piped — and where the key lives and its thumbprint (the device id the
// console shows) on stderr.
func (c PublicCmd) Run() error {
	addr := c.Console
	if os.Getenv(core.EnvDPoPKeyVar) == "" {
		var err error
		if addr, err = core.ResolveConsoleAddr(c.Console); err != nil {
			return fmt.Errorf("key public: %w", err)
		}
	}
	k, err := core.EnsureDeviceKey(addr)
	if err != nil {
		return fmt.Errorf("key public: %w", err)
	}
	out, err := json.Marshal(jose.JSONWebKey{Key: k.Public()})
	if err != nil {
		return fmt.Errorf("key public: %w", err)
	}
	fmt.Fprintln(core.Stdout, string(out))
	path, _, _ := core.DeviceKeyPath(addr)
	fmt.Fprintf(core.Stderr, "device key: %s\ndevice id (thumbprint): %s\n", path, k.Thumbprint())
	return nil
}
