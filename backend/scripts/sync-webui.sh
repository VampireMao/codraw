#!/usr/bin/env bash
#
# Copies the built SPA into the embed directory.
#
#   scripts/sync-webui.sh            # copy web/dist -> backend/internal/webui/dist
#   scripts/sync-webui.sh --clean    # remove the copy first (returns to placeholder)
#
# Idempotent: running it twice produces the same tree. If web/dist does not exist
# it prints a clear hint and exits 0, because "frontend not built yet" is a normal
# state during parallel development and must not fail a backend build or CI job.
set -euo pipefail

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
SRC="$REPO_ROOT/web/dist"
DST="$REPO_ROOT/backend/internal/webui/dist"

CLEAN=0
for arg in "$@"; do
  case "$arg" in
    --clean) CLEAN=1 ;;
    -h|--help) sed -n '2,12p' "${BASH_SOURCE[0]}"; exit 0 ;;
    *) echo "unknown argument: $arg" >&2; exit 2 ;;
  esac
done

if [ "$CLEAN" -eq 1 ]; then
  # Keep .gitkeep so the embed directive always has a directory to point at.
  find "$DST" -mindepth 1 ! -name '.gitkeep' -delete
  echo "cleaned $DST (placeholder page will be served)"
  exit 0
fi

if [ ! -d "$SRC" ]; then
  echo "skip: $SRC does not exist yet (build the frontend first: cd web && npm ci && npm run build)"
  echo "      the backend will serve its built-in placeholder page at /"
  exit 0
fi

if [ ! -f "$SRC/index.html" ]; then
  echo "skip: $SRC exists but has no index.html; is the Vite build configured?" >&2
  exit 0
fi

mkdir -p "$DST"
# Remove only generated content, preserving .gitkeep (see .gitignore).
find "$DST" -mindepth 1 ! -name '.gitkeep' -delete
# `cp -a` keeps the assets/ subdirectory and its content-hashed names intact.
cp -a "$SRC/." "$DST/"

count=$(find "$DST" -type f ! -name '.gitkeep' | wc -l)
echo "copied $count files from $SRC to $DST"
