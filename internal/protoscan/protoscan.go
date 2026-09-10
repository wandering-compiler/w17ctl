// Package protoscan finds the proto files an IR build needs, by reading what
// they declare rather than by convention.
//
// It lives outside cmd/stack because two commands need the same answer: the
// dev diff-apply, and rendering fixtures. A copy in each is a copy that agrees
// until one of the markers changes.
package protoscan

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// modelTableMarker is the option a model proto carries on its
// table-backed messages. Its presence distinguishes model protos
// (which ir.BuildMany turns into tables) from query/mutation/service
// protos (which carry `(w17.db.method)` + `service` blocks instead), so
// `stack build` can auto-discover exactly the schema files.
var modelTableMarker = []byte("(w17.db.table)")

// moduleMarker is the FILE-level option that declares a module's
// connection. It has to join the discovery set even though such a file
// often declares no table at all, because ir.BuildManyWithOptions builds
// its domain-wide connection registry from the files it is HANDED: a
// connection declared in a module that this walk skipped is, to that
// build, undeclared. A KV/LOCAL_FS module is the shape that makes the
// gap reachable — it holds uploads, so it never declares a table, so the
// table marker alone can never pick it up, while a sibling module's
// `(w17.field).upload.connection` points straight at it.
//
// deinvo hit this on 2026-08-30: `codegen`, `verify` and `test` compile
// the whole tree and accepted their project; `stack build` compiled the
// table-declaring subset and refused it with `upload.connection
// "core-uploads" is not declared in this domain (declared:
// core-postgres)`. The refusal was right about what it saw — the file
// set was wrong. Note the registry's permissive gate does NOT cover
// this: it opens only when NO connection is declared at all, and here a
// sibling module declared one, so the check ran against a half-built
// registry.
var moduleMarker = []byte("(w17.module)")

// projectMarker is the FILE-level option that declares PROJECT-WIDE compiler
// vocabulary — today the `(w17.pg.project).custom_types` catalogue, where a
// dialect escape-hatch type is given its alias.
//
// It joins the discovery set for the same reason modules do, and it was missed
// for the same reason: the file declares no table, so a walk keyed on tables
// does not see it. A model column then names an alias nothing registered, and
// the IR build refuses with `custom_type alias "tag_map" not registered` —
// pointing at the model, which is correct and completely unhelpful, because
// the file that was left out is somewhere else entirely.
var projectMarker = []byte("(w17.pg.project)")

// discoverModelProtos walks a proto root and returns the `.proto` files
// the IR build needs, split by what they contribute:
//
//   - models — those declaring a `(w17.db.table)`, which the build turns
//     into tables. Emptiness of THIS list is what makes a diff-apply a
//     no-op, and it has to stay that way: a build handed zero tables
//     would diff an empty schema against the checkpoint and propose
//     dropping every table in it.
//   - modules — those declaring a `(w17.module)` or a `(w17.pg.project)`,
//     which contribute only the connection registry and the project-wide
//     type vocabulary. They ride ALONG with models; they never make an
//     empty project look non-empty.
//
// Returns sorted paths for determinism; an empty result (no models /
// missing dir) is not an error.
func DiscoverModelProtos(protoRoot string) (models, modules []string, err error) {
	walkErrOut := filepath.WalkDir(protoRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if os.IsNotExist(walkErr) {
				return nil
			}
			return walkErr
		}
		if d.IsDir() || !strings.HasSuffix(path, ".proto") {
			return nil
		}
		body, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		switch {
		case bytes.Contains(body, modelTableMarker):
			models = append(models, path)
		case bytes.Contains(body, moduleMarker), bytes.Contains(body, projectMarker):
			// Only when it declares no table of its own — a module file
			// that also carries tables is already a model and must not be
			// listed twice.
			modules = append(modules, path)
		}
		return nil
	})
	if walkErrOut != nil {
		return nil, nil, walkErrOut
	}
	sort.Strings(models)
	sort.Strings(modules)
	return models, modules, nil
}
