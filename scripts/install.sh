#!/usr/bin/env bash
# Builds the kb CLI with cgo (fts5 + sqlite-vec) and links it to /usr/local/bin/kb.
#
#   KB_DEFAULT_ROOT=/path/to/knowledge scripts/install.sh
#
# KB_DEFAULT_ROOT is the knowledge repository kb uses when KB_ROOT is unset. It is remembered in
# .cache/default-root, so later installs on the same machine need no arguments; without either,
# the binary defaults to ~/knowledge.
set -euo pipefail

# Resolve the module dir from the script's own location, not from cwd, so this works no matter
# where it is invoked from, even through a symlink.
SELF=$(readlink -f "$0")
MODULE_DIR=$(cd "$(dirname "$SELF")/.." && pwd)
cd "$MODULE_DIR"

# --- Prerequisites -----------------------------------------------------------------------------

GO=""
if command -v go >/dev/null 2>&1; then
  GO=$(command -v go)
elif [ -x /usr/local/go/bin/go ]; then
  GO=/usr/local/go/bin/go
else
  echo "install.sh: go is required (checked PATH and /usr/local/go/bin/go)" >&2
  exit 1
fi

if ! command -v gcc >/dev/null 2>&1; then
  echo "install.sh: gcc is required for the cgo build (fts5 + sqlite-vec)" >&2
  exit 1
fi

echo "install.sh: using go at $GO ($("$GO" version))"
echo "install.sh: using gcc at $(command -v gcc) ($(gcc -dumpversion))"

# --- Build ---------------------------------------------------------------------------------------

mkdir -p .cache

VERSION=$(git rev-parse --short HEAD 2>/dev/null || echo dev)

DEFAULT_ROOT=${KB_DEFAULT_ROOT:-}
if [ -z "$DEFAULT_ROOT" ] && [ -f .cache/default-root ]; then
  DEFAULT_ROOT=$(cat .cache/default-root)
fi
if [ -n "$DEFAULT_ROOT" ]; then
  printf '%s\n' "$DEFAULT_ROOT" > .cache/default-root
  echo "install.sh: default knowledge repository: $DEFAULT_ROOT"
else
  echo "install.sh: no KB_DEFAULT_ROOT; kb will default to ~/knowledge when KB_ROOT is unset"
fi
LDFLAGS="-X main.version=$VERSION -X github.com/nickstrad/kb/internal/cli.defaultRoot=$DEFAULT_ROOT"

echo "install.sh: building kb (cgo: fts5 + sqlite-vec) from $MODULE_DIR"
echo "install.sh: the first cgo build of mattn/go-sqlite3 + sqlite-vec can take ~2 minutes;" \
     "later builds are fast because the build cache is warm."

START=$SECONDS
"$GO" build -tags fts5 -trimpath -ldflags "$LDFLAGS" -o "$PWD/.cache/kb" ./cmd/kb
ELAPSED=$((SECONDS - START))
echo "install.sh: build finished in ${ELAPSED}s -> $PWD/.cache/kb"

# --- Link ------------------------------------------------------------------------------------

# Symlink (not copy): the binary in .cache/ is what gets updated on the next install, and
# /usr/local/bin/kb always points at it.
ln -sfn "$PWD/.cache/kb" /usr/local/bin/kb
echo "install.sh: linked /usr/local/bin/kb -> $PWD/.cache/kb"

# --- Verify ------------------------------------------------------------------------------------

/usr/local/bin/kb version

# The final health check exits non-zero if the database, embedder, or generated index needs attention.
echo "install ok; running kb doctor:"
/usr/local/bin/kb doctor
