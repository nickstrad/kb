#!/usr/bin/env bash
# Builds the kb CLI with cgo (fts5 + sqlite-vec) and links it to /usr/local/bin/kb.
set -euo pipefail

# Resolve the module dir from the script's own location, not from cwd, so this works no matter
# where it is invoked from (and even through a symlink, like /root/Software/skills-tools/bin/tutor
# handles its own path).
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

echo "install.sh: building kb (cgo: fts5 + sqlite-vec) from $MODULE_DIR"
echo "install.sh: the first cgo build of mattn/go-sqlite3 + sqlite-vec can take ~2 minutes;" \
     "later builds are fast because the build cache is warm."

START=$SECONDS
"$GO" build -tags fts5 -trimpath -ldflags "-X main.version=$VERSION" -o "$PWD/.cache/kb" ./cmd/kb
ELAPSED=$((SECONDS - START))
echo "install.sh: build finished in ${ELAPSED}s -> $PWD/.cache/kb"

# --- Link ------------------------------------------------------------------------------------

# Symlink (not copy), same pattern as /root/Software/skills-tools/bin/tutor: the binary in .cache/
# is what gets updated on the next install, and /usr/local/bin/kb always points at it.
ln -sfn "$PWD/.cache/kb" /usr/local/bin/kb
echo "install.sh: linked /usr/local/bin/kb -> $PWD/.cache/kb"

# --- Verify ------------------------------------------------------------------------------------

/usr/local/bin/kb version

# The final health check exits non-zero if the database, embedder, or generated index needs attention.
echo "install ok; running kb doctor:"
/usr/local/bin/kb doctor
