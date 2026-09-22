#!/usr/bin/env sh
# Install w17ctl.
#
#   curl -fsSL https://get.w17.dev/install.sh | sh -s -- --pre
#   curl -fsSL https://get.w17.dev/install.sh | sh -s -- --version v0.1.0-rc.1 --dir ~/bin
#
# The URL carries a path: get.w17.dev is GitHub Pages, which serves files
# rather than a root handler, so the bare domain 404s. `61569aa3a` fixed that
# in the README and not here — and this file is what a reader sees when the
# README's command has already failed them.
#
# `--pre` during the rc phase: `latest` means the newest STABLE release and
# excludes prereleases by design, so without it the resolve correctly finds
# nothing.
#
# POSIX sh, not bash: this runs on whatever the user has, including a minimal
# container where bash is not installed.
#
# It ALWAYS verifies the download against the release's SHA256SUMS. A pipe from
# the internet into a shell is already asking for trust; silently installing an
# archive nobody checked would be spending that trust badly.

set -eu

REPO="wandering-compiler/w17ctl"
VERSION="latest"
PRE=0
PRINT_VERSION=0
DIR="${W17CTL_INSTALL_DIR:-/usr/local/bin}"

while [ $# -gt 0 ]; do
	case "$1" in
		--version) VERSION="$2"; shift 2 ;;
		--pre)     PRE=1; shift ;;
		--dir)     DIR="$2"; shift 2 ;;
		# --print-version resolves and prints the tag, installing nothing.
		#
		# It exists so `w17ctl update` and `w17ctl version --check` can ask
		# "what is newest?" without a SECOND implementation of the answer.
		# The resolve below is not obvious code — it is the fix for an
		# installer that handed out an older rc than the one available, and
		# a Go copy of it would be a second place for that to come back,
		# drifting silently from this one.
		--print-version) PRINT_VERSION=1; shift ;;
		# Printed from a here-doc, not read back from "$0". The documented
		# invocation is `curl … | sh`, where "$0" is `sh` and the file is
		# stdin — so the version that re-read its own source printed either
		# nothing or a fragment of whatever `sh` happened to be, exactly
		# under the usage that is documented (hosted review 2026-08-30,
		# HOST-C-7).
		-h|--help)
			cat <<'USAGE'
Install w17ctl.

  curl -fsSL https://get.w17.dev/install.sh | sh -s -- --pre
  curl -fsSL https://get.w17.dev/install.sh | sh -s -- --version v0.1.0-rc.1 --dir ~/bin

  --version <tag>   install this release (default: newest stable)
  --pre             allow prereleases (needed while the project is in rc)
  --dir <path>      install into this directory (default: /usr/local/bin,
                    or $W17CTL_INSTALL_DIR)
  --print-version   resolve the release and print its tag on stdout, then
                    exit without installing anything

The download is always verified against the release's SHA256SUMS.
USAGE
			exit 0 ;;
		*) echo "unknown option: $1" >&2; exit 2 ;;
	esac
done

need() { command -v "$1" >/dev/null 2>&1 || { echo "install.sh: needs $1" >&2; exit 1; }; }
# --proto '=https' on every fetch below: without it a redirect can walk the
# transfer down to plain HTTP, and this script's whole trust story is that
# what it downloads came from the release it verified against
# (hosted review 2026-08-30, HOST-C-6).
CURL="curl -fsSL --proto =https --tlsv1.2"

need curl
need tar

os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m)
case "$arch" in
	x86_64|amd64) arch=amd64 ;;
	aarch64|arm64) arch=arm64 ;;
	*) echo "install.sh: unsupported architecture $arch" >&2; exit 1 ;;
esac
case "$os" in
	linux|darwin) ;;
	*) echo "install.sh: unsupported OS $os — build from source: go install github.com/$REPO@latest" >&2; exit 1 ;;
esac

if [ "$VERSION" = "latest" ] && [ "$PRE" = 0 ]; then
	# Resolved from the redirect rather than the API, so this needs no token
	# and does not count against an unauthenticated rate limit.
	#
	# GitHub's /releases/latest deliberately skips prereleases, so during an
	# rc phase this resolves to nothing — correctly, because "latest" means
	# the newest STABLE release and there is not one yet. Failing here beats
	# quietly handing someone an rc they did not ask for.
	VERSION=$($CURL -I -o /dev/null -w '%{url_effective}' "https://github.com/$REPO/releases/latest" \
		| sed 's#.*/tag/##')
	case "$VERSION" in
		*/releases|"")
			echo "install.sh: no stable release yet." >&2
			echo "  Use --pre for the newest prerelease, or --version vX.Y.Z-rc.N for a specific one." >&2
			exit 1 ;;
	esac
elif [ "$VERSION" = "latest" ]; then
	# --pre: newest release of any kind. Needs the API (the redirect only ever
	# points at a stable one), which is unauthenticated and rate-limited — fine
	# for an installer, and the failure is legible when it is not.
	need sed
	need awk
	# ⚠️ Do NOT take the API's first row. It is not ordered the way a reader
	# assumes: on 2026-09-08 `?per_page=1` returned v0.1.0-rc.9 while
	# v0.1.0-rc.10 existed and was newer, so `--pre` installed the OLDER
	# release — and that one shipped a security defect the newer one fixed.
	# An installer that quietly hands out a stale build is worse than one
	# that fails.
	#
	# So: pull a page and pick the highest by SEMVER ourselves. `sort -V`
	# would do it on GNU, but macOS sort has no -V and this installer is
	# published for darwin too — hence awk building a zero-padded key that
	# plain lexical sort orders correctly.
	#
	# The key is major.minor.patch, then 1 for a stable release and 0 for a
	# prerelease (so v0.1.0 outranks v0.1.0-rc.9), then the prerelease's
	# digits as a number — which is what puts rc.10 above rc.9 where a
	# string comparison puts it below. A prerelease carrying several numbers
	# (rc.1.2) collapses to 12; our tags do not, and the alternative is a
	# full semver parser in POSIX sh.
	VERSION=$($CURL "https://api.github.com/repos/$REPO/releases?per_page=100" \
		| sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' \
		| awk '
			{
				tag = $0
				v = tag; sub(/^v/, "", v)
				pre = ""
				if (index(v, "-") > 0) {
					pre = substr(v, index(v, "-") + 1)
					v = substr(v, 1, index(v, "-") - 1)
				}
				split(v, p, ".")
				rank = (pre == "") ? 1 : 0
				num = pre; gsub(/[^0-9]/, "", num)
				printf "%010d.%010d.%010d.%d.%010d\t%s\n", p[1]+0, p[2]+0, p[3]+0, rank, num+0, tag
			}' \
		| sort -r | head -1 | cut -f2)
	[ -n "$VERSION" ] || { echo "install.sh: no releases found at all" >&2; exit 1; }
	echo "install.sh: --pre selected $VERSION" >&2
fi

# Nothing below this point runs for --print-version: the caller wanted the
# answer to "which release is newest", and that answer is now resolved.
# Printed on STDOUT (the progress notes above all go to stderr) so a caller
# can read it with a plain command substitution.
if [ "$PRINT_VERSION" = 1 ]; then
	echo "$VERSION"
	exit 0
fi

asset="w17ctl_${VERSION}_${os}_${arch}.tar.gz"
base="https://github.com/$REPO/releases/download/$VERSION"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

echo "install.sh: fetching $asset ($VERSION)" >&2
$CURL "$base/$asset"     -o "$tmp/$asset"
$CURL "$base/SHA256SUMS" -o "$tmp/SHA256SUMS"

# Verify. Not optional and not skippable by a flag: an installer with a
# --no-verify switch is an installer that gets run with it.
echo "install.sh: verifying checksum" >&2
if command -v sha256sum >/dev/null 2>&1; then
	( cd "$tmp" && grep " $asset\$" SHA256SUMS | sha256sum -c - >/dev/null )
elif command -v shasum >/dev/null 2>&1; then
	( cd "$tmp" && grep " $asset\$" SHA256SUMS | shasum -a 256 -c - >/dev/null )
else
	echo "install.sh: no sha256sum or shasum available — refusing to install unverified" >&2
	exit 1
fi

tar -xzf "$tmp/$asset" -C "$tmp" w17ctl

if [ -w "$DIR" ] || mkdir -p "$DIR" 2>/dev/null && [ -w "$DIR" ]; then
	install -m 0755 "$tmp/w17ctl" "$DIR/w17ctl"
else
	echo "install.sh: $DIR is not writable — retrying with sudo" >&2
	sudo install -m 0755 "$tmp/w17ctl" "$DIR/w17ctl"
fi

echo "install.sh: installed $DIR/w17ctl" >&2
"$DIR/w17ctl" version || true

case ":${PATH}:" in
	*":$DIR:"*) ;;
	*) echo "install.sh: NOTE — $DIR is not on your PATH" >&2 ;;
esac
