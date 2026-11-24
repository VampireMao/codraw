#!/usr/bin/env bash
_SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
_REPO_ROOT="$(cd "$_SCRIPT_DIR/.." && pwd)"
. "$_SCRIPT_DIR/_env.sh"
set -uo pipefail
cd "$_REPO_ROOT/web" || exit 1

echo "=== unit tests ==="
npm run test 2>&1 | tail -30
echo "test exit=${PIPESTATUS[0]}"

echo
echo "=== build ==="
npm run build 2>&1 | tail -12
echo "build exit=${PIPESTATUS[0]}"
