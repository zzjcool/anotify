#!/usr/bin/env bash
# Anotify 端到端测试总编排：构建 → 逐套件运行 → 汇总报告
# 用法：
#   ./scripts/e2e/run_all.sh            # 全量
#   ./scripts/e2e/run_all.sh auth_flow  # 只跑指定套件
set -uo pipefail
cd "$(dirname "$0")/../.." # 仓库根
ROOT=$(pwd)
export GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto

# VAPID（推送相关套件需要）；优先环境变量，否则读原型 vapid.json
if [ -z "${ANOTIFY_VAPID_PUBLIC_KEY:-}" ] && [ -f vapid.json ]; then
	ANOTIFY_VAPID_PUBLIC_KEY=$(python3 -c "import json;print(json.load(open('vapid.json'))['publicKey'])" 2>/dev/null || true)
	ANOTIFY_VAPID_PRIVATE_KEY=$(python3 -c "import json;print(json.load(open('vapid.json'))['privateKey'])" 2>/dev/null || true)
fi
export ANOTIFY_VAPID_PUBLIC_KEY ANOTIFY_VAPID_PRIVATE_KEY

echo "=========================================="
echo " Anotify E2E 全量测试"
echo "=========================================="

# 1. 构建测试二进制
echo "▶ 构建二进制…"
mkdir -p .e2e-bin
go build -o .e2e-bin/anotify ./cmd/server || {
	echo "❌ server 构建失败"
	exit 1
}
go build -o .e2e-bin/devseed ./cmd/devseed || {
	echo "❌ devseed 构建失败"
	exit 1
}
export ANOTIFY_BIN="$ROOT/.e2e-bin/anotify" DEVSEED_BIN="$ROOT/.e2e-bin/devseed"
echo "  ✅ 二进制就绪"

# 2. 单元测试（Go）
echo ""
echo "▶ Go 单元测试…"
if go test ./... -count=1 >/tmp/e2e-unit.log 2>&1; then
	echo "  ✅ go test ./... 全绿"
else
	echo "  ❌ go test 失败（见 /tmp/e2e-unit.log）"
	tail -15 /tmp/e2e-unit.log
	UNIT_FAIL=1
fi

# 3. 逐套件运行
SUITES=(api_contract auth_flow ws_protocol routing persistence security edge_cases frontend deeplink push_e2e i18n lang_hint)
ONLY="${1:-}"
TOTAL_PASS=0
TOTAL_FAIL=0
FAILED_SUITES=()
echo ""
for s in "${SUITES[@]}"; do
	f="scripts/e2e/suites/$s.mjs"
	[ -f "$f" ] || continue
	if [ -n "$ONLY" ] && [ "$ONLY" != "$s" ]; then continue; fi
	echo "▶ 套件: $s"
	if node "$f" 2>&1 | sed 's/^/  /'; then
		TOTAL_PASS=$((TOTAL_PASS + 1))
	else
		TOTAL_FAIL=$((TOTAL_FAIL + 1))
		FAILED_SUITES+=("$s")
		echo "  ❌ 套件 $s 失败"
	fi
	# 套件间隔：等上一套件的服务进程完全释放端口/资源，避免干扰
	sleep 1.5
	echo ""
done

# 4. 汇总
echo "=========================================="
echo " 汇总"
echo "=========================================="
[ "${UNIT_FAIL:-0}" = "1" ] && echo "  ❌ Go 单元测试失败"
echo "  套件通过: $TOTAL_PASS"
echo "  套件失败: $TOTAL_FAIL"
if [ ${#FAILED_SUITES[@]} -gt 0 ]; then
	echo "  失败套件: ${FAILED_SUITES[*]}"
fi
echo "=========================================="
if [ "${UNIT_FAIL:-0}" = "1" ] || [ "$TOTAL_FAIL" -gt 0 ]; then
	echo "❌ E2E 未全绿"
	exit 1
fi
echo "✅ E2E 全绿"
exit 0
