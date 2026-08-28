package storageclient

import (
	"fmt"
)

// CurrentInitiative is what a push learned about the change request it
// belongs to. Exactly one of ID / Why is set.
//
// Why exists because "no change request" must never be a silent
// outcome. A push whose initiative did not resolve is a push that no
// freeze will ever collapse (docs/decisions/squash-supersede-and-adopt.md), and
// the operator finds that out at freeze time — long after the branch is
// gone — unless the push says so at the time it happens.
type CurrentInitiative struct {
	// ID is the console initiative id (UUID), or empty.
	ID string
	// Name is the initiative's name (= the git branch, trunk for
	// main/master), when one resolved.
	Name string
	// Why explains an empty ID in the operator's terms. Empty when ID
	// is set.
	Why string
}

// Describe renders the one line a push prints about its change request.
func (c CurrentInitiative) Describe() string {
	if c.ID == "" {
		return "no change request — " + c.Why
	}
	return fmt.Sprintf("change request %q (%s)", c.Name, c.ID)
}

// ResolveCurrentInitiative answers "which change request is this push
// part of", reading exactly where `w17ctl initiative current` reads:
// the current git branch (main/master → trunk), looked up against the
// console.
//
// # It never creates anything
//
// `initiative push` materializes lazily on first write, and a schema
// push is a write, so materializing here would be defensible. It is
// deliberately NOT done: stamping a CR onto a push must not have the
// side effect of CREATING that CR on a console the operator may only be
// pushing through. An unmaterialized branch reports the reason and the
// push proceeds unstamped — `w17ctl initiative materialize` is one
// command away, and the push says so.
//
// # It never fails the push
//
// Every failure path returns a REASON, not an error. A schema push that
// refused because the initiative tier was unreachable would make an
// optional record a hard dependency of the core workflow, and there are
// consoles that serve the registry without the storage tier at all (the
// e2e in-process console is one). What must not happen is a silent
// empty, and that is what Why prevents.
//
// explicit wins outright: a pipeline has no git branch to derive from,
// and an operator naming the id has already made the decision.
func ResolveCurrentInitiative(console, project, explicit string) CurrentInitiative {
	if explicit != "" {
		return CurrentInitiative{ID: explicit, Name: explicit}
	}
	name, _, err := ResolveInitiativeTarget("")
	if err != nil {
		return CurrentInitiative{Why: fmt.Sprintf("%v (pass --initiative ID to stamp one)", err)}
	}
	sc, err := DialStorageFn(console)
	if err != nil {
		return CurrentInitiative{Why: fmt.Sprintf("console unreachable for initiative lookup: %v", err)}
	}
	defer sc.Close()

	found, err := sc.FindInitiative(project, name)
	if err != nil {
		return CurrentInitiative{Why: fmt.Sprintf("initiative lookup failed: %v", err)}
	}
	if found == nil {
		return CurrentInitiative{Why: fmt.Sprintf(
			"branch %q has no initiative yet — run `w17ctl initiative materialize` first if this push should belong to one", name)}
	}
	return CurrentInitiative{ID: found.GetId(), Name: found.GetName()}
}
