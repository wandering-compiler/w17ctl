// Package version implements `w17ctl version`.
package version

import (
	"fmt"
	"runtime"
	"strings"

	updatecmd "github.com/wandering-compiler/w17ctl/cmd/update"
	"github.com/wandering-compiler/w17ctl/internal/core"
)

// Cmd prints this binary's identity.
//
// It also prints the COMPILED-IN console address, which is not decoration: a
// client reaching the wrong console is one of the two ways a w17 setup goes
// wrong silently, and the resolution order (flag > logged-in console > this
// default) means the compiled value is the one nobody can see any other way.
type Cmd struct {
	// Check asks which release is newest. It is a FLAG rather than part of
	// the default output because it costs a network round trip, and
	// `version` is printed by scripts, by the installer's own last line,
	// and by anyone diagnosing a mismatch offline — none of which should
	// start depending on GitHub being reachable.
	Check bool `help:"Also ask which release is newest, and say whether this binary is behind. Costs one network call."`
}

func (c *Cmd) Run() error {
	fmt.Fprintf(core.Stdout, "w17ctl %s\n", core.VersionString())
	if core.BuildDate != "" {
		fmt.Fprintf(core.Stdout, "  built:   %s\n", core.BuildDate)
	}
	fmt.Fprintf(core.Stdout, "  go:      %s %s/%s\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)
	def := core.DefaultConsoleAddr
	if def == "" {
		def = "(none compiled in — pass --console or log in)"
	}
	fmt.Fprintf(core.Stdout, "  console: %s (compiled-in default; a logged-in console wins over it)\n", def)
	if c.Check {
		return c.check()
	}
	return nil
}

// check reports the newest release, via the SAME resolver `update` uses —
// the installer's `--print-version`.
//
// Not a second implementation, deliberately: the installer's awk key exists
// because a naive "take the API's first row" once installed an older rc than
// the one available, and that build carried a security defect the newer one
// fixed. A `version --check` that answered from its own reading of the API
// could disagree with what `update` would actually install, which is a worse
// failure than not offering the check at all.
//
// A failure here is REPORTED, not fatal: `version`'s job is to say what this
// binary is, and it has already done that by the time this runs. An
// unreachable GitHub must not turn an offline diagnostic into an error.
func (c *Cmd) check() error {
	out, err := updatecmd.RunInstallerFn(installerArgs())
	if err != nil {
		fmt.Fprintf(core.Stdout, "  latest:  (could not ask: %v)\n", err)
		return nil
	}
	latest := strings.TrimSpace(out)
	switch {
	case latest == "":
		fmt.Fprintf(core.Stdout, "  latest:  (the installer resolved no release)\n")
	case latest == core.Version:
		fmt.Fprintf(core.Stdout, "  latest:  %s — up to date\n", latest)
	case core.Version == "":
		fmt.Fprintf(core.Stdout, "  latest:  %s (this is a local build, so there is nothing to compare)\n", latest)
	default:
		fmt.Fprintf(core.Stdout, "  latest:  %s — this binary is %s; upgrade with `w17ctl update`\n", latest, core.Version)
	}
	return nil
}

// installerArgs asks the installer to RESOLVE only, on the same track
// `update` would use.
//
// The track comes from [updatecmd.OnPrereleaseTrack] rather than being
// decided again here: a `--check` that answered about a different track than
// `update` installs from would be a report nobody could act on.
func installerArgs() []string {
	args := []string{"--print-version"}
	if updatecmd.OnPrereleaseTrack() {
		args = append(args, "--pre")
	}
	return args
}
