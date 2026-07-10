#!/usr/bin/env bash
# package.sh — bundle the user-facing files into a single zip for
# release. Bash equivalent of package.ps1; same output, same contents.
#
# Usage:
#   ./package.sh                # auto-detect version from git tag
#   ./package.sh 1.2.3          # explicit version
#
# Output:
#   dist/indexer-agent-<version>.zip
#   dist/indexer-agent-latest.zip
#
# Requires: bash, zip (or python3 as a fallback). git is optional.

set -euo pipefail

cd "$(dirname "$0")"

VERSION="${1:-}"
if [[ -z "$VERSION" ]]; then
    VERSION="$(git describe --tags --always 2>/dev/null || echo dev)"
fi

DIST_DIR="dist"
ZIP_NAME="indexer-agent-${VERSION}.zip"
ZIP_PATH="${DIST_DIR}/${ZIP_NAME}"
LATEST_PATH="${DIST_DIR}/indexer-agent-latest.zip"

# Required source files. README lives in dist/ because it's only
# bundled — keeps it out of the way of any future top-level README
# for the agent source itself.
for f in docker-compose.yml .env.example dist/README.md; do
    if [[ ! -f "$f" ]]; then
        echo "missing required file: $f" >&2
        exit 1
    fi
done

mkdir -p "$DIST_DIR"

# Stage in a temp folder so zip entries are clean (no parent dirs)
# and the bundled README doesn't get a "dist/" prefix.
STAGE="$(mktemp -d)"
trap 'rm -rf "$STAGE"' EXIT

cp docker-compose.yml "$STAGE/docker-compose.yml"
cp .env.example       "$STAGE/.env.example"
cp dist/README.md     "$STAGE/README.md"
printf '%s\n' "$VERSION" > "$STAGE/VERSION"

rm -f "$ZIP_PATH" "$LATEST_PATH"

# Prefer the system `zip` tool. Fall back to python3 if it's not
# installed (some minimal containers and Git Bash setups don't have it).
if command -v zip >/dev/null 2>&1; then
    (cd "$STAGE" && zip -q -r "$OLDPWD/$ZIP_PATH" .)
elif command -v python3 >/dev/null 2>&1; then
    python3 -c "
import os, zipfile, sys
stage, out = sys.argv[1], sys.argv[2]
with zipfile.ZipFile(out, 'w', zipfile.ZIP_DEFLATED) as z:
    for root, _, files in os.walk(stage):
        for f in files:
            full = os.path.join(root, f)
            z.write(full, os.path.relpath(full, stage))
" "$STAGE" "$ZIP_PATH"
else
    echo "neither 'zip' nor 'python3' is installed; can't build the archive" >&2
    exit 1
fi

cp "$ZIP_PATH" "$LATEST_PATH"

SIZE_KB=$(( $(wc -c < "$ZIP_PATH") / 1024 ))
echo
echo "Packaged indexer-agent $VERSION"
echo "  $ZIP_PATH"
echo "  $LATEST_PATH"
echo "  size: ${SIZE_KB} KB"
echo
echo "Contents:"
unzip -l "$ZIP_PATH" 2>/dev/null | awk 'NR>3 && $4 != "" {print "  " $4}' || true
