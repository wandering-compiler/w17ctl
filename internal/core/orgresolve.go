package core

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wandering-compiler/w17ctl/internal/lockfile"
)

// lockOrg is the organization a lock names, and the lock that names it.
type lockOrg struct {
	ID   string // org_id — authoritative
	Slug string // org_slug — display, and the key that lets the console answer (listOrgsOnce)
	// URL is the lock's w17_url: the console whose organization org_id is.
	URL  string
	Path string
}

// forConsole reports whether the lock's organization belongs to the console
// being dialed. An org id is one console's: a lock made on the hosted console
// names an organization a dev console does not have, and the examples the
// e2e lane registers on its dev console are exactly that (`project register
// --org test` inside a hosted-console lock). Sent there, the lock's org was a
// refusal before the call went out.
//
// A lock with no w17_url cannot say, so it gets only the cache translation —
// the cache is per console, so an id from elsewhere matches nothing there.
func (lo lockOrg) forConsole(addr string) bool {
	return lo.URL != "" && normalizeConsoleAddr(lo.URL) == normalizeConsoleAddr(addr)
}

var (
	orgLockDirMu sync.Mutex
	orgLockDir   string
)

// ActInOrgOfLockAbove makes the lock above dir decide the organization this
// process acts in, ahead of the lock above the working directory.
//
// For the commands that take a directory: `plugin sign <dir>` and `plugin gen
// <dir>` act on a tree that need not be where they run. Without it the
// organization came from the working directory alone, so pointing the command
// at another checkout acted in whichever organization the CURRENT one names —
// the silent wrong-organization the lock's org_id exists to prevent.
//
// The working directory stays the fallback: the plugins repository's release
// signs a `git archive` of the plugin extracted to a temporary directory, which
// has no lock above it, while the release runs from the repository root, which
// does.
//
// "" clears it: the working directory alone decides again.
func ActInOrgOfLockAbove(dir string) {
	abs := ""
	if dir != "" {
		var err error
		if abs, err = filepath.Abs(dir); err != nil {
			return
		}
	}
	orgLockDirMu.Lock()
	orgLockDir = abs
	orgLockDirMu.Unlock()
}

var (
	orgOverrideMu  sync.Mutex
	orgOverrideSet bool
	orgOverride    string
)

// ActInOrg fixes the organization every later console call in this process
// acts in, ahead of any lock.
//
// For the commands that REPLACE the lock: `project register` adopts a lock
// made elsewhere (or before a console rebuild gave the organization a new
// id), and `init` writes the first one. The lock on disk names the
// organization the project is LEAVING, so reading it for the calls that
// rewrite it acted in the wrong one — or, when the caller is not a member of
// it, stopped the lock edit before it was sent, with the project already
// registered and its lock never rewritten.
//
// slug "" means "act in no lock's organization": the machine default
// (OrgSlugFor), else no header and the console's single-org inference — what
// the registration itself acts in when no organization is given.
func ActInOrg(slug string) {
	orgOverrideMu.Lock()
	orgOverrideSet, orgOverride = true, slug
	orgOverrideMu.Unlock()
}

// ClearActInOrg undoes ActInOrg: the lock decides again.
func ClearActInOrg() {
	orgOverrideMu.Lock()
	orgOverrideSet, orgOverride = false, ""
	orgOverrideMu.Unlock()
}

func actInOrg() (string, bool) {
	orgOverrideMu.Lock()
	defer orgOverrideMu.Unlock()
	return orgOverride, orgOverrideSet
}

func lockOrgBestEffort() lockOrg {
	orgLockDirMu.Lock()
	dir := orgLockDir
	orgLockDirMu.Unlock()
	if dir != "" {
		// ok = a lock was FOUND, whether or not it names an organization:
		// the nearest lock decides, and one the console has not stamped yet
		// does not hand the decision to the working directory's.
		if lo, ok := lockOrgAbove(dir); ok {
			return lo
		}
	}
	root, err := FindProjectRoot()
	if err != nil {
		return lockOrg{}
	}
	return readLockOrg(filepath.Join(root, lockfile.DefaultPath))
}

// lockOrgAbove finds the NEAREST lock walking up from dir. Nearest, not the
// nearest one naming an organization: a lock the console has not stamped yet
// says nothing, and an outer one is somebody else's.
func lockOrgAbove(dir string) (lockOrg, bool) {
	for {
		path := filepath.Join(dir, lockfile.DefaultPath)
		if _, err := os.Stat(path); err == nil {
			return readLockOrg(path), true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return lockOrg{}, false
		}
		dir = parent
	}
}

// readLockOrg reads the organization a lock names; the zero value for a lock
// that names none or does not parse.
func readLockOrg(path string) lockOrg {
	lk, err := lockfile.Load(path)
	if err != nil {
		return lockOrg{}
	}
	return lockOrg{ID: lk.OrgID, Slug: lk.OrgSlug, URL: lk.W17URL, Path: path}
}

// ---- the console's answer -------------------------------------------------

// orgListTimeout bounds the one ListMyOrgs a process makes per console to
// translate the lock's organization.
const orgListTimeout = 30 * time.Second

type orgListing struct {
	orgs []AuthOrg
	err  error
	// keyRefused: listing with no organization was refused for permissions,
	// and the retry under the lock's slug was refused too — the slug names an
	// organization the account cannot act in.
	keyRefused bool
}

var (
	orgListingsMu sync.Mutex
	orgListings   = map[string]orgListing{}
)

func resetOrgListings() {
	orgListingsMu.Lock()
	orgListings = map[string]orgListing{}
	orgListingsMu.Unlock()
}

// listOrgsOnce asks the console which organizations the caller belongs to —
// once per process per console, because every call this process makes needs
// the answer and it does not change under it.
//
// Two attempts, because two kinds of credential reach this:
//
//   - a person's session token holds the realm-wide `account` role, so
//     ListMyOrgs answers with no organization named;
//   - an api-realm token's grants (ci-push, deploy-fetch) are all scoped to an
//     organization, so for an account in several the console narrows them to
//     nothing when the request names none, ListMyOrgs included. Named under
//     the lock's org_slug, the request carries the grants of THAT
//     organization, ListMyOrgs among them.
//
// The slug only opens the door. What the console lists is every membership
// the account has, and the caller matches the lock's org_id against it — so a
// stale or edited slug cannot select an organization the id does not name.
//
// A transport failure is not remembered: the console may be back for the next
// call, and the call that needed the answer fails with the reason anyway.
func listOrgsOnce(addr, token, keySlug string) orgListing {
	key := normalizeConsoleAddr(addr) + "|" + keySlug
	orgListingsMu.Lock()
	defer orgListingsMu.Unlock()
	if l, ok := orgListings[key]; ok {
		return l
	}
	ctx, cancel := context.WithTimeout(context.Background(), orgListTimeout)
	defer cancel()
	b := bindingFor(addr, token)
	var l orgListing
	l.orgs, l.err = listMyOrgsIn(ctx, addr, token, b, "")
	if status.Code(l.err) == codes.PermissionDenied && keySlug != "" {
		l.orgs, l.err = listMyOrgsIn(ctx, addr, token, b, keySlug)
		if c := status.Code(l.err); c == codes.PermissionDenied || c == codes.Unauthenticated {
			l.keyRefused = true
		}
	}
	switch status.Code(l.err) {
	case codes.Unavailable, codes.DeadlineExceeded, codes.Canceled, codes.Unknown:
		return l
	}
	orgListings[key] = l
	return l
}

// orgSlugForRequest picks the organization THIS command acts in: the lock's
// if the directory has one, the machine default otherwise.
//
// The lock wins because it is the specific answer. A default in
// ~/.w17/auth.yaml is one value for every checkout on the machine, so with two
// projects in two organizations it is right for at most one of them — and
// being wrong is silent, since the console checks the header against
// membership rather than against the project.
//
// The lock stores an ID and the wire wants a SLUG (the console resolves the
// header through GetUserOrgBySlug, and an id there is "not a member"). The
// login-time membership cache translates it when it can. When it cannot — an
// environment token has no cache at all, and a cache from before the account
// joined the organization does not hold it — the console is asked
// (listOrgsOnce). That was the defect: an env-token run sent NO organization
// even when the lock named one, and a console facing an account in several
// organizations refuses to guess, so every org-scoped call was refused with a
// `missing permission` that named nothing about the organization.
//
// A lock naming an organization the account is not in is an error here, not a
// request with no organization: the refusal that request would earn reads as
// a missing grant, and the cause is the lock.
func orgSlugForRequest(addr string) (string, error) {
	if slug, ok := actInOrg(); ok {
		if slug != "" {
			return slug, nil
		}
		return OrgSlugFor(addr), nil
	}
	lo := lockOrgBestEffort()
	if lo.ID == "" {
		return OrgSlugFor(addr), nil
	}
	if inst := instanceFor(addr); inst != nil {
		for _, o := range inst.Orgs {
			if o != nil && o.ID == lo.ID {
				return o.Slug, nil
			}
		}
	}
	if !lo.forConsole(addr) {
		return OrgSlugFor(addr), nil
	}
	token := AuthTokenFn(addr)
	if token == "" {
		// Nothing to ask with, and the call carries no credential either —
		// its own refusal is the one to read.
		return "", nil
	}
	l := listOrgsOnce(addr, token, lo.Slug)
	if l.err != nil && !l.keyRefused {
		return "", lo.listingFailed(addr, l.err)
	}
	for _, o := range l.orgs {
		if o.ID == lo.ID {
			return o.Slug, nil
		}
	}
	return "", status.Error(codes.PermissionDenied, notMemberMessage(addr, lo, l))
}

// listingFailed explains a failed ListMyOrgs WITHOUT losing what the console
// said: the code and the details stay the console's, and only the message
// gains the sentence about the lock. The details are what the refusal
// explainers read — SESSION_EXPIRED, DEVICE_REVOKED (ExplainAuthFailure) — so
// a rewrap that kept only the message turned an expired login into an
// unexplained refusal.
func (lo lockOrg) listingFailed(addr string, err error) error {
	st, ok := status.FromError(err)
	if !ok {
		st = status.New(codes.Unavailable, err.Error())
	}
	p := st.Proto()
	p.Message = fmt.Sprintf("%s names organization %s; asking %s which organizations %s belongs to, to send it, failed: %s%s",
		lo.Path, lo.describe(), displayAddr(addr), accountFor(addr), p.GetMessage(), lo.noSlugHint(st.Code()))
	return status.ErrorProto(p)
}

// noSlugHint answers the one listing refusal a lock can fix: grants that are
// all org-scoped, and a lock with no org_slug to name the organization by.
func (lo lockOrg) noSlugHint(code codes.Code) string {
	if code != codes.PermissionDenied || lo.Slug != "" {
		return ""
	}
	return "\n  the credential's grants are scoped to organizations, so the console lists them only for a request\n" +
		"  that names one, and this lock carries no org_slug to name it by.\n" +
		"  fix: re-sign the lock (any signed write stamps org_slug), or add `org_slug: <slug>` beside org_id"
}

func (lo lockOrg) describe() string {
	if lo.Slug != "" {
		return fmt.Sprintf("%s (%s)", lo.Slug, lo.ID)
	}
	return lo.ID
}

func notMemberMessage(addr string, lo lockOrg, l orgListing) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s names organization %s, and %s is not a member of it on %s",
		lo.Path, lo.describe(), accountFor(addr), displayAddr(addr))
	if slugs := orgSlugs(l.orgs); len(slugs) > 0 {
		fmt.Fprintf(&b, "\n  member of: %s", strings.Join(slugs, ", "))
	}
	b.WriteString("\n  nothing was sent: acting in another organization than the lock names is the mistake the lock exists to stop.")
	b.WriteString("\n  fix: use an account that belongs to it, or — when the lock is wrong — correct it")
	b.WriteString("\n       (a project's lock is stamped by the console; a pinned one is rewritten by `w17ctl org pin <slug>`)")
	return b.String()
}

// accountFor names the caller as far as this side can: the console's own
// answer (WhoAmI) is org-scoped for an api-realm token and would be refused
// for the same reason the request is.
func accountFor(addr string) string {
	if envTokenFor(addr) != "" {
		return "the account " + EnvTokenVar + " belongs to"
	}
	if inst := instanceFor(addr); inst != nil && inst.User != nil && inst.User.Email != "" {
		return inst.User.Email
	}
	return "the account logged in to " + displayAddr(addr)
}

func orgSlugs(orgs []AuthOrg) []string {
	var out []string
	for _, o := range orgs {
		if o.Slug != "" {
			out = append(out, o.Slug)
		}
	}
	sort.Strings(out)
	return out
}
