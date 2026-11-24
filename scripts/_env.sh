#!/usr/bin/env bash
# 工具链探测：让脚本在开发机（gvm + nvm）和 CI（setup-go + setup-node）上都能跑。
#
# 被 source，不单独执行。设计原则：
#   - 已有 go / node 在 PATH 上就直接用（CI 的 setup-* 已经准备好在 PATH 上）
#   - 否则尝试从 gvm / nvm 里找（本地开发机的情况）
#   - 两者都没有才报错，并给出清晰的提示

# ---------- Go ----------
if command -v go >/dev/null 2>&1; then
  :
elif [ -x "$HOME/.gvm/gos/go1.27.3/bin/go" ]; then
  export PATH="$HOME/.gvm/gos/go1.27.3/bin:$PATH"
elif [ -x "$HOME/.gvm/bin/go" ]; then
  export PATH="$HOME/.gvm/bin:$PATH"
else
  echo "error: 找不到 go。请安装 Go 1.27+ 或把它放进 PATH。" >&2
  return 1 2>/dev/null || exit 1
fi

# ---------- Node ----------
if command -v npm >/dev/null 2>&1; then
  :
elif [ -s "$HOME/.nvm/nvm.sh" ]; then
  # shellcheck disable=SC1091
  . "$HOME/.nvm/nvm.sh" >/dev/null 2>&1
  nvm use 24.18.0 >/dev/null 2>&1 || nvm use --lts >/dev/null 2>&1 || true
fi

if ! command -v npm >/dev/null 2>&1; then
  # node 常见安装位置兜底
  for d in "$HOME/.nvm/versions/node"/*/bin; do
    [ -x "$d/npm" ] && export PATH="$d:$PATH" && break
  done
fi

if ! command -v npm >/dev/null 2>&1; then
  echo "error: 找不到 npm。请安装 Node 20+ 或把它放进 PATH。" >&2
  return 1 2>/dev/null || exit 1
fi

# 构建缓存放 /tmp，避免污染仓库（Windows 挂载盘上 /tmp 也快得多）
export GOCACHE="${GOCACHE:-/tmp/gocache-build}"
export GOFLAGS="${GOFLAGS:--mod=mod}"
