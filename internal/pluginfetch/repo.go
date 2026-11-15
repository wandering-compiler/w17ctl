package pluginfetch

import (
	"fmt"
	"strings"
	"unicode"
)

// CheckRepo refuses a plugin repository that git would not read as a
// repository.
//
// The repository usually comes from the lock's
// plugins[].git.repo, and w17ctl reads the lock OFFLINE without verifying a
// signature (lockfile.Load — by design, D4), so a hand-edited lock in a cloned
// project reaches git through `update --check/--all`, the codegen
// pre-generate update check and `plugin update`. A value starting with `-`
// was parsed as an option: `--upload-pack=<cmd>` made `git ls-remote` run
// <cmd>. Two layers, either of which stops that:
//
//   - this check, at every entry point that hands a repository to git
//     (Source.validate, LatestVersion, ReleaseAtCommit, PublishedPlugins);
//   - `--` before the repository in every git argv, so even a value this
//     check did not anticipate is a positional, never an option.
//
// Refused: a leading `-`; a remote-helper address `<transport>::<address>`
// (git runs the helper program `git-remote-<transport>`); a control character;
// and a URL whose scheme is not a git transport for fetching a plugin. A local
// path (tests, an air-gapped mirror) and an scp-style `user@host:path` stay
// accepted. The console's lock validator holds the lock to the same rule
// (lock.CheckGitRepo), so a signed lock never vouches for such a value — but
// this side cannot rely on a signature it never checks.
func CheckRepo(repo string) error {
	if repo == "" {
		return fmt.Errorf("no repository given")
	}
	if strings.HasPrefix(repo, "-") {
		return fmt.Errorf("repository %q starts with `-`, which git reads as an option, not a repository", repo)
	}
	if strings.Contains(repo, "::") {
		return fmt.Errorf("repository %q is a remote-helper address (`<transport>::…`), which runs a helper program; give the repository URL", repo)
	}
	for _, r := range repo {
		if unicode.IsControl(r) {
			return fmt.Errorf("repository %q carries the control character %U", repo, r)
		}
	}
	if i := strings.Index(repo, "://"); i >= 0 {
		switch strings.ToLower(repo[:i]) {
		case "https", "http", "ssh", "git", "file":
		default:
			return fmt.Errorf("repository %q: scheme %q is not one git fetches plugins over (https, http, ssh, git, file)", repo, repo[:i])
		}
	}
	return nil
}
