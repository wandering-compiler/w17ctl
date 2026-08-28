#!/usr/bin/env sh
# Install w17ctl.
#
#   curl -fsSL https://get.w17.dev | sh
#   curl -fsSL https://get.w17.dev | sh -s -- --version v0.1.0-rc.1 --dir ~/bin
#   curl -fsSL https://get.w17.dev | sh -s -- --pre        # newest, prereleases included
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
DIR="${W17CTL_INSTALL_DIR:-/usr/local/bin}"

while [ $# -gt 0 ]; do
	case "$1" in
		--version) VERSION="$2"; shift 2 ;;
		--pre)     PRE=1; shift ;;
		--dir)     DIR="$2"; shift 2 ;;
		-h|--help) sed -n '2,14p' "$0"; exit 0 ;;
		*) echo "unknown option: $1" >&2; exit 2 ;;
	esac
done

need() { command -v "$1" >/dev/null 2>&1 || { echo "install.sh: needs $1" >&2; exit 1; }; }
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
	VERSION=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/$REPO/releases/latest" \
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
	VERSION=$(curl -fsSL "https://api.github.com/repos/$REPO/releases?per_page=1" \
		| sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -1)
	[ -n "$VERSION" ] || { echo "install.sh: no releases found at all" >&2; exit 1; }
	echo "install.sh: --pre selected $VERSION" >&2
fi

asset="w17ctl_${VERSION}_${os}_${arch}.tar.gz"
base="https://github.com/$REPO/releases/download/$VERSION"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

echo "install.sh: fetching $asset ($VERSION)" >&2
curl -fsSL "$base/$asset"     -o "$tmp/$asset"
curl -fsSL "$base/SHA256SUMS" -o "$tmp/SHA256SUMS"

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
