#!/usr/bin/env bash
# Installs the kb CLI from a GitHub release (default) or from source.
#
#   scripts/install.sh [--version vX.Y.Z] [--prefix DIR] [--from-source]
#
# Release install downloads kb-<tag>-<os>-<arch>.tar.gz from github.com/nickstrad/kb, verifies
# its sha256, and writes DIR/kb (default ~/.local/bin). No sudo.
set -euo pipefail

REPO=nickstrad/kb
PREFIX="$HOME/.local/bin"
VERSION=""
FROM_SOURCE=0

usage() {
  cat <<'USAGE'
Usage: install.sh [--version vX.Y.Z] [--prefix DIR] [--from-source]

  --version TAG    install this release tag (default: latest)
  --prefix DIR     install directory for the kb binary (default: ~/.local/bin)
  --from-source    run: go install -tags fts5 github.com/nickstrad/kb/cmd/kb@latest
                   (needs Go and a C compiler; ignores --version and --prefix)
  -h, --help       show this help
USAGE
}

die() { echo "install.sh: $*" >&2; exit 1; }

while [ $# -gt 0 ]; do
  case "$1" in
    --version) [ $# -ge 2 ] || die "--version needs a value"; VERSION=$2; shift 2 ;;
    --prefix) [ $# -ge 2 ] || die "--prefix needs a value"; PREFIX=$2; shift 2 ;;
    --from-source) FROM_SOURCE=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; die "unknown argument: $1" ;;
  esac
done

if [ "$FROM_SOURCE" = 1 ]; then
  command -v go >/dev/null 2>&1 || die "go is required for --from-source"
  go install -tags fts5 "github.com/$REPO/cmd/kb@latest"
  echo "install.sh: installed kb into ${GOBIN:-$(go env GOPATH)/bin}"
  exit 0
fi

case "$(uname -s)" in
  Darwin) OS=darwin ;;
  Linux) OS=linux ;;
  *) die "unsupported OS: $(uname -s)" ;;
esac
case "$(uname -m)" in
  arm64|aarch64) ARCH=arm64 ;;
  x86_64|amd64) ARCH=amd64 ;;
  *) die "unsupported architecture: $(uname -m)" ;;
esac

command -v curl >/dev/null 2>&1 || die "curl is required"
if command -v sha256sum >/dev/null 2>&1; then
  sha_check() { sha256sum -c "$1" >/dev/null; }
elif command -v shasum >/dev/null 2>&1; then
  sha_check() { shasum -a 256 -c "$1" >/dev/null; }
else
  die "sha256sum or shasum is required"
fi

if [ -z "$VERSION" ]; then
  # The /releases/latest redirect ends in the tag name; no API token needed.
  VERSION=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/$REPO/releases/latest") \
    || die "could not resolve the latest release"
  VERSION=${VERSION##*/}
  [ -n "$VERSION" ] && [ "$VERSION" != latest ] || die "no release found for $REPO"
fi

NAME="kb-$VERSION-$OS-$ARCH.tar.gz"
BASE="https://github.com/$REPO/releases/download/$VERSION"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

echo "install.sh: downloading $NAME"
curl -fsSL -o "$TMP/$NAME" "$BASE/$NAME" || die "download failed: $BASE/$NAME"
curl -fsSL -o "$TMP/$NAME.sha256" "$BASE/$NAME.sha256" || die "checksum download failed"
(cd "$TMP" && sha_check "$NAME.sha256") || die "sha256 mismatch for $NAME"

tar -xzf "$TMP/$NAME" -C "$TMP" kb
mkdir -p "$PREFIX"
install -m 0755 "$TMP/kb" "$PREFIX/kb"
echo "install.sh: installed $PREFIX/kb"

if [ "$OS" = darwin ]; then
  echo "install.sh: macOS binaries are unsigned; if Gatekeeper blocks kb, run:"
  echo "  xattr -d com.apple.quarantine $PREFIX/kb"
fi
case ":$PATH:" in
  *":$PREFIX:"*) ;;
  *) echo "install.sh: warning: $PREFIX is not on your PATH; add it to your shell profile" >&2 ;;
esac

"$PREFIX/kb" version
