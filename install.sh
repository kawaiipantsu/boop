#!/usr/bin/env sh
# Boop installer.
#
#   curl -fsSL https://raw.githubusercontent.com/kawaiipantsu/boop/main/install.sh | sh
#
# Downloads the release archive for your platform, verifies it against the
# release checksums, and installs the binary. It does not use sudo on its own:
# if the target directory is not writable it says what to run instead.
#
# Environment:
#   BOOP_VERSION   version to install, e.g. v0.1.0-rc.1 (default: latest release)
#   BOOP_INSTALL   install directory (default: ~/.local/bin, or /usr/local/bin if writable)
#   BOOP_NO_VERIFY set to 1 to skip checksum verification (not recommended)
set -eu

REPO="kawaiipantsu/boop"
BINARY="boop"

say()  { printf '%s\n' "$*"; }
warn() { printf '%s\n' "$*" >&2; }
die()  { printf 'error: %s\n' "$*" >&2; exit 1; }

need() { command -v "$1" >/dev/null 2>&1 || die "$1 is required but not installed"; }

need uname
need mktemp
if command -v curl >/dev/null 2>&1; then
    fetch() { curl -fsSL "$1" -o "$2"; }
    fetch_stdout() { curl -fsSL "$1"; }
elif command -v wget >/dev/null 2>&1; then
    fetch() { wget -qO "$2" "$1"; }
    fetch_stdout() { wget -qO- "$1"; }
else
    die "curl or wget is required"
fi

# ---------------------------------------------------------------- platform
os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m)
case "$os" in
    linux)   os=linux ;;
    darwin)  os=darwin ;;
    mingw*|msys*|cygwin*) die "Windows is not supported by this script; download the .zip from the releases page" ;;
    *) die "unsupported operating system: $os" ;;
esac
case "$arch" in
    x86_64|amd64)  arch=amd64 ;;
    aarch64|arm64) arch=arm64 ;;
    *) die "unsupported architecture: $arch (Boop ships amd64 and arm64)" ;;
esac

# ---------------------------------------------------------------- version
version="${BOOP_VERSION:-}"
if [ -z "$version" ]; then
    say "resolving the latest release..."
    version=$(fetch_stdout "https://api.github.com/repos/$REPO/releases/latest" \
        | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -1)
    [ -n "$version" ] || die "could not determine the latest release; set BOOP_VERSION explicitly"
fi
plain=${version#v}

# ---------------------------------------------------------------- target dir
target="${BOOP_INSTALL:-}"
if [ -z "$target" ]; then
    if [ -w /usr/local/bin ] 2>/dev/null; then
        target=/usr/local/bin
    else
        target="$HOME/.local/bin"
    fi
fi

# ---------------------------------------------------------------- download
archive="${BINARY}_${plain}_${os}_${arch}.tar.gz"
base="https://github.com/$REPO/releases/download/$version"

tmp=$(mktemp -d)
# shellcheck disable=SC2064
trap "rm -rf '$tmp'" EXIT INT TERM

say "installing $BINARY $version ($os/$arch)"
fetch "$base/$archive" "$tmp/$archive" \
    || die "could not download $archive — check that $version has a build for $os/$arch"

# ---------------------------------------------------------------- verify
if [ "${BOOP_NO_VERIFY:-0}" != "1" ]; then
    if fetch "$base/checksums.txt" "$tmp/checksums.txt" 2>/dev/null; then
        expected=$(grep " $archive\$" "$tmp/checksums.txt" | awk '{print $1}' | head -1)
        if [ -z "$expected" ]; then
            warn "warning: $archive is not listed in checksums.txt; continuing unverified"
        else
            if command -v sha256sum >/dev/null 2>&1; then
                actual=$(sha256sum "$tmp/$archive" | awk '{print $1}')
            elif command -v shasum >/dev/null 2>&1; then
                actual=$(shasum -a 256 "$tmp/$archive" | awk '{print $1}')
            else
                actual=""
                warn "warning: no sha256 tool found; continuing unverified"
            fi
            if [ -n "$actual" ]; then
                [ "$actual" = "$expected" ] || die "checksum mismatch for $archive
  expected $expected
  actual   $actual
Refusing to install. This is worth investigating rather than retrying."
                say "checksum verified"
            fi
        fi
    else
        warn "warning: could not fetch checksums.txt; continuing unverified"
    fi
fi

# ---------------------------------------------------------------- install
tar -xzf "$tmp/$archive" -C "$tmp" || die "could not extract $archive"
found=$(find "$tmp" -type f -name "$BINARY" -perm -u+x 2>/dev/null | head -1)
[ -n "$found" ] || found=$(find "$tmp" -type f -name "$BINARY" 2>/dev/null | head -1)
[ -n "$found" ] || die "the archive did not contain a '$BINARY' binary"

mkdir -p "$target" 2>/dev/null || true
if [ ! -w "$target" ]; then
    die "$target is not writable.
Either choose a writable directory:
    BOOP_INSTALL=\$HOME/.local/bin curl -fsSL https://raw.githubusercontent.com/$REPO/main/install.sh | sh
or install this download yourself:
    sudo install -m 0755 '$found' '$target/$BINARY'"
fi

install -m 0755 "$found" "$target/$BINARY" 2>/dev/null \
    || { cp "$found" "$target/$BINARY" && chmod 0755 "$target/$BINARY"; } \
    || die "could not write $target/$BINARY"

say "installed $target/$BINARY"

# ---------------------------------------------------------------- verify + PATH
if "$target/$BINARY" version >/dev/null 2>&1; then
    "$target/$BINARY" version | head -2
else
    warn "warning: $target/$BINARY did not run; the download may be for the wrong platform"
fi

case ":$PATH:" in
    *":$target:"*) ;;
    *)
        say ""
        say "$target is not on your PATH. Add it:"
        say "    echo 'export PATH=\"$target:\$PATH\"' >> ~/.profile && . ~/.profile"
        ;;
esac

say ""
say "next: run '$BINARY prep' in a project, then '$BINARY' to start."
