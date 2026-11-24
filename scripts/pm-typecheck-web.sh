#!/usr/bin/env bash
_SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
_REPO_ROOT="$(cd "$_SCRIPT_DIR/.." && pwd)"
. "$_SCRIPT_DIR/_env.sh"
set -uo pipefail
cd "$_REPO_ROOT/web" || exit 1

echo "=== typecheck ==="
npm run typecheck 2>&1 | tail -25
echo "exit=${PIPESTATUS[0]}"
