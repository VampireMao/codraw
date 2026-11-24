#!/usr/bin/env bash
# 公共定位helper：被其他脚本 source，导出仓库根目录。
#
# 为什么需要：脚本原本写死了开发机的绝对路径（/mnt/d/...），
# 在 CI 或其他人的机器上直接失败。改成基于脚本自身位置推断后，
# clone 到任何目录都能跑。
#
# 用法（在每个脚本开头）：
#   . "$(dirname "${BASH_SOURCE[0]}")/_root.sh"
#   cd "$REPO_ROOT"
#
# 注意：用 BASH_SOURCE 而不是 $0，否则被 source 时定位到的是调用者的路径。

# 解析出「包含本文件的目录」的上一级，即仓库根
_REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export REPO_ROOT="$_REPO_ROOT"
