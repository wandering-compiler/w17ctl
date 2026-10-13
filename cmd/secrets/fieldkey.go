package secrets

import (
	"fmt"

	"github.com/wandering-compiler/w17ctl/internal/core"
	"github.com/wandering-compiler/sdk/go/lib/fieldcrypt"
)

// FieldKeyCmd is `w17ctl secrets field-key` — a keyring for W17_FIELD_KEYS,
// the key of a project's CRYPTED_SECRET columns. Random bytes from this
// machine, printed once, never written anywhere: it goes into the service's
// sealed .env like any other secret (one of the few pieces of crypto the client
// does itself — the key never travels to the console).
type FieldKeyCmd struct {
	Rotate string `name:"rotate" placeholder:"KEYRING" help:"The current keyring: the result keeps every version of it and adds the next, which new values are encrypted with."`
}

func (c *FieldKeyCmd) Run() error {
	spec, err := fieldcrypt.NewKeySpec(c.Rotate)
	if err != nil {
		return fmt.Errorf("secrets field-key: %w", err)
	}
	fmt.Fprintln(core.Stdout, spec)
	return nil
}
