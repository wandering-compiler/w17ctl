package verify

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"

	"github.com/wandering-compiler/w17ctl/internal/adminruntime"
)

// adminRuntimeError is a vendored admin runtime that is not the one this
// client ships. Its own type so the headline can give the advice that repairs
// it: the copy is gitignored output, so "commit the locks" does not apply.
type adminRuntimeError struct{ msg string }

func (e *adminRuntimeError) Error() string { return e.msg }

// verifyAdminRuntime compares the project's vendored @w17/admin-runtime with
// the one this client ships. Purely local. It reports whether it applied —
// a project with no admin bundle on disk (no admin surface, or a fresh clone
// that has not run codegen: both trees are gitignored) has nothing to check.
//
// The SPA builds against this copy, and nothing else says how old it is: a
// stale one used to survive any codegen run that did not rewrite the admin
// bundle, and an adopter read it as a shipped fix not having landed.
func verifyAdminRuntime(out io.Writer, root, servicesDir string) (bool, error) {
	if !adminruntime.ProjectHasAdmin(root, servicesDir) {
		return false, nil
	}
	rel := adminruntime.ProjectDir(servicesDir)
	st, err := adminruntime.Inspect(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		return true, &adminRuntimeError{fmt.Sprintf("admin runtime: inspect %s: %v", rel, err)}
	}
	if st.State == adminruntime.Fresh {
		fmt.Fprintf(out, "verify: vendored admin runtime %s\n", st.Describe())
		return true, nil
	}
	if st.State == adminruntime.Missing {
		return true, &adminRuntimeError{fmt.Sprintf(
			"admin runtime: %s is missing, and the admin bundle's SPA builds against it\n"+
				"  fix: `w17ctl codegen` vendors it", rel)}
	}
	return true, &adminRuntimeError{fmt.Sprintf(
		"admin runtime: %s %s\n"+
			"  fix: `w17ctl codegen` re-vendors it (gitignored output — nothing to commit)", rel, st.Describe())}
}

// adminRuntimeOnly reports whether every error is an admin-runtime one.
func adminRuntimeOnly(errs []error) bool {
	if len(errs) == 0 {
		return false
	}
	for _, e := range errs {
		var ae *adminRuntimeError
		if !errors.As(e, &ae) {
			return false
		}
	}
	return true
}
