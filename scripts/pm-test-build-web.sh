#!/usr/bin/env bash
set -uo pipefail
cd /mnt/d/Work/AI-TMP/codraw/web || exit 1
export NVM_DIR="$HOME/.nvm"
# shellcheck disable=SC1091
. "$NVM_DIR/nvm.sh" >/dev/null 2>&1
nvm use 24.18.0 >/dev/null 2>&1

echo "=== unit tests ==="
npm run test 2>&1 | tail -30
echo "test exit=${PIPESTATUS[0]}"

echo
echo "=== build ==="
npm run build 2>&1 | tail -12
echo "build exit=${PIPESTATUS[0]}"
