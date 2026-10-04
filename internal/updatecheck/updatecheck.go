// Package updatecheck answers "is anything this project runs on behind?" —
// the client (w17ctl), the SDK the project pins, and every installed plugin —
// and sorts each answer into REQUIRED or AVAILABLE.
//
// It resolves nothing itself. Each question already has a command that owns
// its answer: the installer knows the newest w17ctl (`w17ctl update
// --dry-run`), the module proxy the newest sdk/go (`w17ctl sdk update`), the
// registry the newest release of a plugin (`w17ctl plugin list`). This package
// asks them through [Sources] and only compares, so `update --check`, `update
// --all` and the check codegen runs first can never disagree with the
// commands they point at.
//
// REQUIRED is narrow on purpose: the project pins an SDK older than the floor
// the code is generated against, so the generated code cannot build. Codegen
// refuses that case anyway (after generating); asking first only spares the
// wasted run. Everything else is AVAILABLE — the project still builds — and
// whether to push it is a policy, see [Alpha].
package updatecheck

import (
	"fmt"
	"io"
	"strings"

	"golang.org/x/mod/semver"

	"github.com/wandering-compiler/w17ctl/internal/pluginfetch"
)

// Kind names what an [Item] is about.
type Kind string

const (
	KindClient Kind = "w17ctl"
	KindSDK    Kind = "sdk/go"
	KindPlugin Kind = "plugin"
)

// Item is one thing that is behind.
type Item struct {
	Kind     Kind
	Name     string // the plugin's name; empty for the client and the SDK
	Current  string
	Latest   string
	Required bool   // the generated code cannot build without it
	Why      string // for a required item: what it is required by
	Fix      string // the command that moves just this one
}

// Label is how the item is named to a person.
func (i Item) Label() string {
	if i.Kind == KindPlugin {
		return "plugin " + i.Name
	}
	return string(i.Kind)
}

// Report is the answer, plus the questions that could not be answered.
type Report struct {
	Items []Item
	// Unknown names the sources that failed (no network, no proxy). A check
	// that could not look is not a check that found nothing, and is said so.
	Unknown []string
}

// Required returns the items the generated code cannot build without.
func (r Report) Required() []Item { return r.filter(true) }

// Available returns the items that are newer but not required.
func (r Report) Available() []Item { return r.filter(false) }

func (r Report) filter(required bool) []Item {
	var out []Item
	for _, it := range r.Items {
		if it.Required == required {
			out = append(out, it)
		}
	}
	return out
}

// Plugin is one installed plugin and the newest release its registry serves.
type Plugin struct {
	Name, Installed, Served string
}

// Sources is what the check asks. A nil function is a question not asked.
type Sources struct {
	ClientCurrent string
	ClientLatest  func() (string, error)

	SdkCurrent string // what the project builds against; "" = unknown (co-dev, no pin)
	SdkLatest  func() (string, error)
	// SdkFloor is the highest SDK floor known before generating: this
	// client's and the console's. A plugin's own requires_sdk is known only
	// once the code is generated, and codegen still checks that after.
	SdkFloor       string
	SdkFloorSource string

	Plugins func() ([]Plugin, error)
}

// Check compares and sorts.
func Check(s Sources) Report {
	var r Report

	if s.ClientLatest != nil && semver.IsValid(s.ClientCurrent) {
		if latest, err := s.ClientLatest(); err != nil {
			r.Unknown = append(r.Unknown, fmt.Sprintf("w17ctl (%v)", err))
		} else if semver.IsValid(latest) && semver.Compare(latest, s.ClientCurrent) > 0 {
			r.Items = append(r.Items, Item{Kind: KindClient, Current: s.ClientCurrent, Latest: latest,
				Fix: "w17ctl update"})
		}
	}

	if semver.IsValid(s.SdkCurrent) {
		latest := ""
		if s.SdkLatest != nil {
			v, err := s.SdkLatest()
			if err != nil {
				r.Unknown = append(r.Unknown, fmt.Sprintf("sdk/go (%v)", err))
			} else if semver.IsValid(v) {
				latest = v
			}
		}
		required := semver.IsValid(s.SdkFloor) && semver.Compare(s.SdkCurrent, s.SdkFloor) < 0
		target := latest
		if required && (target == "" || semver.Compare(target, s.SdkFloor) < 0) {
			target = s.SdkFloor
		}
		if required || (target != "" && semver.Compare(target, s.SdkCurrent) > 0) {
			it := Item{Kind: KindSDK, Current: s.SdkCurrent, Latest: target, Required: required,
				Fix: fmt.Sprintf("w17ctl sdk update --version %s && w17ctl sdk pin %s", target, target)}
			if required {
				it.Why = fmt.Sprintf("%s (sdk floor %s)", s.SdkFloorSource, s.SdkFloor)
			}
			r.Items = append(r.Items, it)
		}
	}

	if s.Plugins != nil {
		ps, err := s.Plugins()
		if err != nil {
			r.Unknown = append(r.Unknown, fmt.Sprintf("plugins (%v)", err))
		}
		for _, p := range ps {
			if p.Installed == "" || p.Served == "" || pluginfetch.CompareVersions(p.Installed, p.Served) >= 0 {
				continue
			}
			r.Items = append(r.Items, Item{Kind: KindPlugin, Name: p.Name, Current: p.Installed, Latest: p.Served,
				Fix: "w17ctl plugin update " + p.Name})
		}
	}
	return r
}

// Alpha reports whether this client is in the alpha phase: a v0 release, or a
// local build. In alpha every release may carry a breaking fix, so an
// available update is PUSHED (codegen offers it and asks before going on);
// from v1, where a major keeps its API, it is only mentioned. Derived from the
// version so the policy ends by itself with the first major, with no flag day.
func Alpha(clientVersion string) bool {
	return !semver.IsValid(clientVersion) || semver.Major(clientVersion) == "v0"
}

// Render writes the report for a person.
func Render(w io.Writer, r Report) {
	if len(r.Items) == 0 && len(r.Unknown) == 0 {
		fmt.Fprintln(w, "update check: everything is current")
	}
	for _, it := range r.Items {
		tag := "available"
		if it.Required {
			tag = "REQUIRED"
		}
		fmt.Fprintf(w, "  %-9s %-20s %s → %s\n", tag, it.Label(), it.Current, it.Latest)
		if it.Why != "" {
			fmt.Fprintf(w, "            needed by %s — without it the generated code does not build\n", it.Why)
		}
		fmt.Fprintf(w, "            %s\n", it.Fix)
	}
	if len(r.Unknown) > 0 {
		fmt.Fprintf(w, "update check: could not ask %s\n", strings.Join(r.Unknown, ", "))
	}
}
