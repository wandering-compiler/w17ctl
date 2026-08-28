package core

import "runtime/debug"

// Build-injected identity. A released binary carries all three; a locally
// built one carries none and says so.
//
// This exists because the client had NO version at all, and the cost of that
// showed up the first time a project and a console disagreed: the only way to
// say which client was running was the binary's FILE DATE. A mismatch between
// console, client and SDK is a normal condition for a hosted product — the
// three move on their own schedules — so the client has to be able to state
// which one it is before any check on that mismatch can exist.
//
//	go build -ldflags "-X .../w17ctl/internal/core.Version=v0.1.0 -X .../core.Commit=abc1234 -X .../core.BuildDate=2026-08-28T08:00:00Z"
var (
	Version   string
	Commit    string
	BuildDate string
)

// VersionString renders the build identity for `w17ctl version` and for any
// diagnostic that needs to name this binary.
//
// An unset Version means a local build rather than a release, and it says
// "dev" rather than inventing a number — a made-up version in a bug report is
// worse than an honest absence, because it reads as a release nobody can find.
func VersionString() string {
	v := Version
	if v == "" {
		v = "dev"
	}
	c := Commit
	if c == "" {
		// A `go install`ed binary has VCS stamps even with no ldflags.
		if bi, ok := debug.ReadBuildInfo(); ok {
			for _, s := range bi.Settings {
				if s.Key == "vcs.revision" && len(s.Value) >= 7 {
					c = s.Value[:7]
				}
			}
		}
	}
	if c != "" {
		v += " (" + c + ")"
	}
	return v
}
