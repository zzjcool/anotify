#!/usr/bin/env bash
# Anotify 端到端测试总编排：环境准备 → 构建二进制 → 委托 Node 并行执行器
# 用法：
#   ./scripts/e2e/run_all.sh                 # 全量（并行模式）
#   ./scripts/e2e/run_all.sh --serial        # 全量（串行模式）
#   ./scripts/e2e/run_all.sh auth_flow        # 只跑指定套件（串行）
#   ./scripts/e2e/run_all.sh --serial auth_flow  # 同上（显式串行）
# 环境变量：
#   E2E_CONCURRENCY  Chrome 类套件并发上限（默认 4）
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

# 1. 前置：检测 dist 是否存在（embed 依赖，gitignore 产物）
#    同时恢复 git 追踪的 web/ 源文件（partials.js/sw.js/tokens.css 等），
#    它们是运行时依赖但 sitegen 不生成（sitegen 只产 HTML + i18n JS）。
#    全新 clone 会带这些文件，但手动删 web/ 后需恢复。
NEED_FE=0
if [ ! -d "internal/server/dist" ] || [ -z "$(ls -A internal/server/dist 2>/dev/null)" ]; then
	NEED_FE=1
fi
if [ ! -f "web/partials.js" ]; then
	NEED_FE=1
	# 恢复 git 追踪的 web/ 源文件（sitegen 不生成它们）
	git checkout HEAD -- web 2>/dev/null || true
fi
if [ "$NEED_FE" = "1" ]; then
	echo "▶ 生成前端产物（dist/web 不完整）…"
	make fe || { echo "❌ make fe 失败"; exit 1; }
	echo "  ✅ 前端产物就绪"
fi

# 2. 构建测试二进制
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

# 3. 委托 Node 并行执行器
#    注：go test ./... 已拆到 `make test`，不再阻塞 e2e
echo ""
node scripts/e2e/parallel_runner.mjs "$@"
exit $?
