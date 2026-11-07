#!/bin/sh
# 校验本仓 sessiongovernance 三文件副本与 .sessiongovernance.sha256 完全一致，防止跨仓漂移。
# 用法：sh scripts/check-session-governance-sha256.sh [--check]
set -eu

case "${1:---check}" in
--check) ;;
*)
	echo "用法: $0 [--check]" >&2
	exit 2
	;;
esac

cd "$(dirname "$0")/.."

if [ ! -f .sessiongovernance.sha256 ]; then
	echo "缺少 .sessiongovernance.sha256；先运行工作区根 scripts/check-session-governance-sync.sh --sync" >&2
	exit 1
fi

if command -v shasum >/dev/null 2>&1; then
	checker="shasum -a 256 -c"
elif command -v sha256sum >/dev/null 2>&1; then
	checker="sha256sum -c"
else
	echo "缺少 shasum 或 sha256sum，无法校验 sessiongovernance 副本" >&2
	exit 1
fi

if ! $checker .sessiongovernance.sha256; then
	echo "sessiongovernance 副本与 .sessiongovernance.sha256 不一致；先运行工作区根 scripts/check-session-governance-sync.sh --sync 后重新校验" >&2
	exit 1
fi

echo "sessiongovernance sha256 校验通过"
