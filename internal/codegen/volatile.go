package codegen

import (
	"bytes"
	"os"
	"strings"
)

// volatileFields are the generated fields that carry the MOMENT a file was
// produced rather than anything about its content.
//
// Two exist: gettext's `POT-Creation-Date`, which the .po marshaller stamps
// from time.Now() on every call, and `generated_at` in a web client's
// manifest. Both advance on every regeneration whether or not a single
// translated string or endpoint changed.
//
// They are listed here rather than detected, because "looks like a timestamp"
// would also match a field whose VALUE is the point — a migration id, a
// fixture's created_at. A field earns a line here by being the file's own
// birth certificate.
var volatileFields = []string{
	"POT-Creation-Date",
	"generated_at",
}

// unchangedApartFromTimestamp reports whether the file already on disk says
// the same thing as the freshly generated body, once both are read without
// their volatile fields.
//
// It exists because a rewrite that changes only the timestamp is a change that
// did not happen: it shows as a `git status` entry, a diff in review, and a
// conflict in every parallel PR — a consumer put it exactly that way, and this
// repo had been working around its own symptom with a post-regen revert step
// in `scripts/regenerate-e2e.sh`, which only ever helped US.
//
// It also costs nothing to be wrong in the safe direction: a file that differs
// anywhere else is written normally, WITH its new timestamp, because then the
// timestamp is telling the truth about a real regeneration.
func unchangedApartFromTimestamp(target string, fresh []byte) bool {
	existing, err := os.ReadFile(target)
	if err != nil {
		return false // no file, or unreadable: write it
	}
	if bytes.Equal(existing, fresh) {
		return true // identical, volatile fields included
	}
	return stripVolatile(existing) == stripVolatile(fresh)
}

// stripVolatile drops every line mentioning a volatile field.
//
// Line-wise, which is what both carriers happen to be: a .po header entry is
// one line, and the client manifest is pretty-printed JSON with one field per
// line. A minified carrier would compare as "changed" and be rewritten —
// noisy, never wrong, and the day one appears this is where it is taught.
func stripVolatile(body []byte) string {
	lines := strings.Split(string(body), "\n")
	kept := lines[:0]
	for _, line := range lines {
		if containsVolatile(line) {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

func containsVolatile(line string) bool {
	for _, f := range volatileFields {
		if strings.Contains(line, f) {
			return true
		}
	}
	return false
}
