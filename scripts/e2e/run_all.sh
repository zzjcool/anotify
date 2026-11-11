#!/usr/bin/env bash
# Anotify 端到端测试总编排：环境准备 → 构建二进制 → 委托 Node 并行执行器
# 用法：
#   ./scripts/e2e/run_all.sh                 # 全量（并行模式）
#   ./scripts/e2e/run_all.sh --serial        # 全量（串行模式）
#   ./scripts/e2e/run_all.sh auth_flow        # 只跑指定套件（串行）
#   ./scripts/e2e/run_all.sh --serial auth_flow  # 同上（显式串行）
#   ./scripts/e2e/run_all.sh --serial node_v4    # v4 真实节点核心链（真 anode 二进制）
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
#
# 三个真实产物，没有 mock：
#   anotify  中枢（cmd/server，带 embeddist 的前端产物）
#   devseed  播种用户/API Key/会话（+ -demo 的 v4 真实资源）
#   anode    v4 节点（cmd/anode：serve / install / emit 三个子命令）
#
# v4 已退役 cmd/agent 与旧协议（#331/#332/#333）：节点侧**只有** cmd/anode，
# 帧契约唯一来源是 internal/protocol。e2e 因此不再构建 agent，也不再有
# 第二套节点 CLI（旧 AGENT_BIN 环境变量随之改名为 ANODE_BIN）。
echo "▶ 构建二进制…"
mkdir -p .e2e-bin
go build -tags embeddist -o .e2e-bin/anotify ./cmd/server || {
	echo "❌ server 构建失败"
	exit 1
}
go build -o .e2e-bin/devseed ./cmd/devseed || {
	echo "❌ devseed 构建失败"
	exit 1
}
# node_v4 套件要真起 anode：真 hello/配置对齐/真部署拉取/真执行/真结果上报。
go build -o .e2e-bin/anode ./cmd/anode || {
	echo "❌ anode 构建失败"
	exit 1
}
export ANOTIFY_BIN="$ROOT/.e2e-bin/anotify" DEVSEED_BIN="$ROOT/.e2e-bin/devseed" ANODE_BIN="$ROOT/.e2e-bin/anode"
echo "  ✅ 二进制就绪"

# 3. 委托 Node 并行执行器
#    注：go test ./... 已拆到 `make test`，不再阻塞 e2e
echo ""
#
#    外面套一层临时目录守卫（#326）：/tmp 是 tmpfs，泄漏吃的是内存。守卫给
#    被包命令一个私有 TMPDIR，跑完断言其中没有 anotify-* 残留 —— 归因隔离而
#    不是直接比对 /tmp，本机同时有别的 e2e / 测试在跑也不会误报（见 E2E_TESTING.md）。
node scripts/check-tmp-leaks.mjs --lint || exit $?
node scripts/check-tmp-leaks.mjs -- node scripts/e2e/parallel_runner.mjs "$@"
exit $?
