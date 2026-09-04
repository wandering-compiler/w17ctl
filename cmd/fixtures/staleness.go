package fixtures

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
)

// The registry is what `fixtures apply` renders, and the working tree is
// what the author edits. When they disagree, rendering the registry's
// copy silently is the failure that matters.
//
// deinvo, 2026-09-04: a 356-row local fixture, a 1-row copy in the
// registry, and `--out` wrote the 1-row version under a "DO NOT EDIT"
// header claiming "the fixture is the source of truth" — a sentence
// about a file that was not the one rendered. The output goes into
// `db/init/`, so the stale seed inserted a second `admin` role with a
// different id, `ON CONFLICT (id)` did not catch the name collision,
// initdb failed, and the stack restart-looped. Three failed runs and one
// retracted finding before anyone suspected the seed.
//
// Nothing was wrong with the rendering. The gap is that `apply` never
// looks at the tree it was invoked in, so "the registry is behind" and
// "the registry is current" produce identical, confident output. The
// remedy is the one deinvo proposed: compare, and refuse.

// localFixturePath is where the working tree keeps a fixture, mirroring
// the layout `push` derives its registry key from.
func localFixturePath(dir, domain, group, name string) string {
	if group == "" {
		return filepath.Join(dir, domain, name+".json")
	}
	return filepath.Join(dir, domain, group, name+".json")
}

// staleFixtureError reports a working-tree fixture that differs from the
// registry copy `apply` just rendered.
type staleFixtureError struct {
	Path   string
	Domain string
	Name   string
}

func (e *staleFixtureError) Error() string {
	return fmt.Sprintf(
		"fixtures apply: %s differs from the copy in the registry, and the registry is what was rendered.\n"+
			"  The output would describe %s/%s as the console last saw it, not as %s defines it — "+
			"and for --out that file gets committed and seeds a database.\n"+
			"  fix: `w17ctl push` first, then re-run\n"+
			"  override: --allow-stale renders the registry copy anyway",
		e.Path, e.Domain, e.Name, e.Path)
}

// checkFixtureFresh compares the working-tree fixture against the stored
// one and returns a staleFixtureError when they differ.
//
// No local file → nothing to compare, no error: `apply` is legitimately
// run in trees that hold no fixtures (a deploy box seeding from the
// registry alone). Silence there is the right answer; silence when the
// file EXISTS and disagrees is not.
func checkFixtureFresh(ctx context.Context, cl applyfetchpb.FixtureFetchClient, projectID, dir, domain, group, name string) error {
	path := localFixturePath(dir, domain, group, name)
	localRaw, err := os.ReadFile(path)
	if err != nil {
		return nil // absent or unreadable — see above
	}
	resp, err := cl.FetchFixtureFiles(ctx, &applyfetchpb.FetchFixtureFilesRequest{
		ProjectId: projectID,
		Domain:    domain,
	})
	if err != nil {
		return nil // the freshness check must not be the thing that fails the run
	}
	want := registryFixtureName(group, name)
	for _, f := range resp.GetFiles() {
		if f.GetName() != want {
			continue
		}
		if sameFixtureJSON(localRaw, f.GetJson()) {
			return nil
		}
		return &staleFixtureError{Path: path, Domain: domain, Name: want}
	}
	// In the tree, absent from the registry — never pushed. The seed
	// fetch that follows reports its own NotFound, which says the same
	// thing more precisely than a diff would.
	return nil
}

// sameFixtureJSON compares two fixture bodies by VALUE, not bytes: the
// stored copy is server-encoded and the local one is hand-authored, so
// key order and whitespace differ routinely between two fixtures that
// are the same fixture. A byte compare would refuse every run.
func sameFixtureJSON(a, b []byte) bool {
	var av, bv any
	if json.Unmarshal(a, &av) != nil || json.Unmarshal(b, &bv) != nil {
		// Unparseable on either side: not a mismatch this check can
		// claim. Fall back to trimmed bytes so a genuinely different
		// body is still caught.
		return strings.TrimSpace(string(a)) == strings.TrimSpace(string(b))
	}
	return reflect.DeepEqual(av, bv)
}
