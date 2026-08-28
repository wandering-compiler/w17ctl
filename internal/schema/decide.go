package schema

import (
	"fmt"
	"os"
	"strings"
)

// BuildDecidePayload prepares the `--decide` inputs for a PushSchemaRequest. The
// raw flag strings are forwarded VERBATIM — the dumb client never parses the
// decide grammar (closure 0; the console owns the compiler know-how). The only
// client-local work is file access the console can't do: for any flag whose
// value references a `custom:<path>` SQL file, read the local file and return
// its body keyed by that path so the server's decide loader resolves
// `custom:<path>` from the map instead of the (client-side) filesystem.
//
// Malformed flags are left for the server to report with the full decide-grammar
// diagnostic — the client only needs to recognise the `custom:` shape well
// enough to know which files to ship.
func BuildDecidePayload(flags []string) (decide []string, customSQL map[string]string, err error) {
	if len(flags) == 0 {
		return nil, nil, nil
	}
	custom := map[string]string{}
	for _, f := range flags {
		eq := strings.IndexByte(f, '=')
		if eq < 0 {
			continue // no `=` → malformed; the server reports it against the grammar
		}
		val := f[eq+1:]
		const pfx = "custom:"
		if !strings.HasPrefix(val, pfx) {
			continue
		}
		path := strings.TrimSpace(val[len(pfx):])
		if path == "" {
			continue
		}
		body, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil, nil, fmt.Errorf("--decide %q: read custom SQL file: %w", f, rerr)
		}
		custom[path] = string(body)
	}
	if len(custom) == 0 {
		custom = nil
	}
	return flags, custom, nil
}
