#!/usr/bin/env bash
# Anotify 集成测试（curl 契约矩阵 + 全链路）
# 用法：BASE=http://localhost:8080 ./scripts/integration.sh
set -uo pipefail
BASE="${BASE:-http://localhost:8080}"
PASS=0
FAIL=0
ok() {
	PASS=$((PASS + 1))
	echo "  ✅ $1"
}
bad() {
	FAIL=$((FAIL + 1))
	echo "  ❌ $1"
}
check() { # check <描述> <期望> <实际>
	if [ "$2" = "$3" ]; then ok "$1"; else bad "$1 (期望=$2 实际=$3)"; fi
}

echo "=== 1. 健康检查 ==="
code=$(curl -s -o /dev/null -w '%{http_code}' "$BASE/health")
check "GET /health → 200" "200" "$code"

echo "=== 2. 静态缓存分级（响应头）==="
# 先确认一个哈希文件与 index.html 的缓存头
idx=$(curl -s -D - -o /dev/null "$BASE/index.html" | grep -i '^cache-control' | tr -d '\r')
echo "  index.html: $idx"
echo "$idx" | grep -qi 'max-age=60' && ok "index.html 短缓存" || bad "index.html 缓存头 ($idx)"

echo "=== 3. /v1/* 必须 no-store ==="
api=$(curl -s -D - -o /dev/null "$BASE/v1/notifications" | grep -i '^cache-control' | tr -d '\r')
echo "  /v1/notifications: $api"
echo "$api" | grep -qi 'no-store' && ok "API no-store" || bad "API 缓存头 ($api)"

echo "=== 4. 鉴权矩阵 ==="
code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE/v1/notify" -H 'Content-Type: application/json' -d '{"status":"success","title":"t"}')
check "无 Key 上报 → 401/403" "401" "$code"
code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE/v1/notify" -H 'Authorization: Bearer ant_live_wrong' -H 'Content-Type: application/json' -d '{"status":"success","title":"t"}')
check "错误 Key 上报 → 401" "401" "$code"

echo "=== 5. VAPID 公钥 ==="
code=$(curl -s -o /dev/null -w '%{http_code}' "$BASE/v1/vapid-public-key")
check "GET /v1/vapid-public-key → 200" "200" "$code"

echo ""
echo "=========================================="
echo "结果：$PASS 通过 / $FAIL 失败"
[ "$FAIL" -eq 0 ] && exit 0 || exit 1
