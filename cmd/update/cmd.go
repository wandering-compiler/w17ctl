// Package update implements `w17ctl update`.
package update

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/wandering-compiler/w17ctl/internal/core"
)

// InstallerURL is the canonical installer. It is the ONE place that knows how
// to resolve "which release is newest", and this command does not learn a
// second way — see [Cmd].
const InstallerURL = "https://get.w17.dev/install.sh"

// RunInstallerFn shells out to the installer. Indirected so tests can drive
// the command without a network or a write to a real bin directory.
var RunInstallerFn = runInstaller

// Cmd upgrades this binary in place by invoking the canonical installer.
//
// It does NOT resolve the release itself, and that restraint is the whole
// design. `install.sh` picks the newest release with an awk key that exists
// because of a real incident: on 2026-09-08 taking the GitHub API's first row
// returned `v0.1.0-rc.9` while `rc.10` existed — the list is ordered by
// creation, not by version — so the older build was installed, and it carried
// a security defect the newer one fixed. A Go reimplementation would be a
// second place for that to come back, and the two would drift without ever
// disagreeing loudly. So: one resolver, invoked.
//
// The installer also verifies the download against the release's SHA256SUMS
// and cannot be told not to. Inheriting that is the other half of the reason
// this command is a caller rather than a downloader.
type Cmd struct {
	Version string `help:"Install this exact release instead of the newest (e.g. v0.1.0-rc.46)." placeholder:"TAG"`
	Dir     string `help:"Install into this directory instead of the one this binary is running from." placeholder:"PATH"`
	Stable  bool   `help:"Refuse prereleases. Off by default while this binary is itself a prerelease — see the note in --help."`
	DryRun  bool   `name:"dry-run" help:"Resolve the release that WOULD be installed and print it, changing nothing."`
}

func (c *Cmd) Run() error {
	dir := c.Dir
	if dir == "" {
		exe, err := os.Executable()
		if err != nil {
			return fmt.Errorf("update: cannot find this binary's own path (%w) — pass --dir to say where to install", err)
		}
		// EvalSymlinks: a binary reached through a symlink (a version manager,
		// a ~/bin shim) would otherwise install beside the LINK and leave the
		// real file untouched, so `version` would keep reporting the old build
		// and the update would look like it silently did nothing.
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
		dir = filepath.Dir(exe)
	}

	args := []string{"--dir", dir}
	if c.Version != "" {
		args = append(args, "--version", c.Version)
	} else if c.allowPrerelease() {
		args = append(args, "--pre")
	}
	if c.DryRun {
		args = append(args, "--print-version")
	}

	out, err := RunInstallerFn(args)
	if err != nil {
		return fmt.Errorf("update: %w", err)
	}
	if c.DryRun {
		tag := strings.TrimSpace(out)
		fmt.Printf("update: would install %s into %s\n", tag, dir)
		if tag == core.Version {
			fmt.Printf("update: that is what is already running\n")
		}
		return nil
	}
	return nil
}

// OnPrereleaseTrack reports whether this binary should be offered
// prereleases — i.e. whether the installer gets `--pre`.
//
// TRUE while this binary is itself a prerelease, which is the state every
// consumer is in today: `latest` means the newest STABLE release, so a client
// that only ever offered stable upgrades would tell every current user there
// is nothing to install. It flips off by itself the moment a stable release
// is what is running — no flag day, no second thing to remember.
//
// An unset Version is a local build (`core.VersionString` reports "dev") and
// counts as a prerelease: a developer running a self-built binary who asks
// what is newest wants the newest thing that exists, and the stable track
// would resolve to nothing at all during rc.
//
// Exported because `version --check` asks the same question, and it has to
// get the SAME answer. Two copies of this rule would mean `--check` could
// report a release `update` would not install — a disagreement nobody would
// notice until it mattered.
func OnPrereleaseTrack() bool {
	return core.Version == "" || strings.Contains(core.Version, "-")
}

// allowPrerelease is OnPrereleaseTrack with this command's opt-out applied.
// `--stable` is for someone leaving the rc track deliberately.
func (c *Cmd) allowPrerelease() bool {
	return !c.Stable && OnPrereleaseTrack()
}

// runInstaller fetches the installer and runs it, streaming its progress to
// this process's stderr. Stdout is captured, because --print-version answers
// there.
//
// `curl … | sh -s -- args` is the documented invocation and the one the
// installer is written for (its own --help says so, and it prints usage from
// a here-doc rather than re-reading "$0" precisely because "$0" is `sh` under
// this form).
func runInstaller(args []string) (string, error) {
	if _, err := exec.LookPath("curl"); err != nil {
		return "", fmt.Errorf("needs curl on PATH to fetch %s", InstallerURL)
	}
	if _, err := exec.LookPath("sh"); err != nil {
		return "", fmt.Errorf("needs a POSIX sh on PATH")
	}
	script := exec.Command("curl", "-fsSL", "--proto", "=https", "--tlsv1.2", InstallerURL)
	body, err := script.Output()
	if err != nil {
		return "", fmt.Errorf("fetch %s: %w", InstallerURL, err)
	}
	sh := exec.Command("sh", append([]string{"-s", "--"}, args...)...)
	sh.Stdin = strings.NewReader(string(body))
	sh.Stderr = os.Stderr
	out, err := sh.Output()
	if err != nil {
		return "", fmt.Errorf("installer: %w", err)
	}
	return string(out), nil
}
