#!/usr/bin/env bash
# 模拟 CI：把仓库复制到临时目录，用系统 go/npm（不用 gvm/nvm）跑 build-all.sh
set -uo pipefail

SRC=/mnt/d/Work/AI-TMP/codraw
TMP=/tmp/codraw-ci-sim

echo "=== 准备干净副本（模拟 git clone）==="
rm -rf "$TMP"
mkdir -p "$TMP"
cd "$SRC"
# 只复制会被提交的文件（用 git ls-files 精确模拟）
git ls-files -z | tar --null -T - -cf - 2>/dev/null | (cd "$TMP" && tar xf -)
echo "  复制了 $(cd "$TMP" && find . -type f | wc -l) 个文件"

echo
echo "=== 确认没有硬编码路径残留 ==="
LEFT=$(grep -rn '/mnt/d/Work' "$TMP" --include='*.sh' 2>/dev/null | wc -l)
echo "  残留 $LEFT 处"

echo
echo "=== 模拟 CI 环境：清空 PATH 里的 gvm/nvm ==="
# 只保留系统路径，去掉 $HOME/.gvm 和 $HOME/.nvm
export PATH="/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
export HOME_ORIG="$HOME"

echo "  go  : $(command -v go || echo '未找到')"
echo "  npm : $(command -v npm || echo '未找到')"

if ! command -v go >/dev/null 2>&1; then
  echo
  echo "  系统没有 go，无法完全模拟 CI。改用仓库内的 go："
  export PATH="$HOME_ORIG/.gvm/gos/go1.27.3/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
  echo "  go  : $(command -v go)"
fi

echo
echo "=== 在干净副本里执行 build-all.sh（这是 CI 的那个 job）==="
cd "$TMP"
bash scripts/build-all.sh 2>&1 | tail -24
RC=${PIPESTATUS[0]}

echo
echo "=== 退出码: $RC ==="
if [ "$RC" = "0" ] && [ -f "$TMP/dist/codraw-linux-amd64" ]; then
  echo "✅ CI 模拟通过"
  ls -lh "$TMP/dist/" | tail -3
else
  echo "❌ CI 模拟失败"
fi
