#!/usr/bin/env bash
# scripts/dev.sh — 本地开发启动：读 .env.local → 起 server + cloudflared tunnel
#
# 用法：
#   ./scripts/dev.sh           # server(前台) + tunnel(后台)，Ctrl-C 一起停
#   make dev                   # 等价（含 sitegen 预构建 web/*.html）
#   NO_TUNNEL=1 ./scripts/dev.sh   # 只起 server，不起 tunnel
#
# 环境文件（.gitignore，不入库）：
#   .env.local  —— 本地开发配置（VAPID 密钥、RP_ID=dev.openaaas.org 等）
#                 缺失时自动用 .env.example 作模板提示用户复制
#
# 行为：
#   1. 加载 .env.local（不覆盖已 export 的环境变量，便于临时覆盖）
#   2. 确保 web/*.html 存在（否则 make sitegen）
#   3. 后台起 cloudflared tunnel run anotify（固定 dev.openaaas.org → :8080）
#   4. 前台起 go run ./cmd/server
#   5. 捕获 SIGINT/SIGTERM，退出时一并停掉 tunnel 和 server
set -uo pipefail
cd "$(dirname "$0")/.."
ROOT=$(pwd)

ENV_FILE="$ROOT/.env.local"
ENV_EXAMPLE="$ROOT/.env.example"

# ---------- 1. 加载 .env.local ----------
if [ ! -f "$ENV_FILE" ]; then
	echo "⚠️  .env.local 不存在。"
	if [ -f "$ENV_EXAMPLE" ]; then
		echo "   复制模板：cp .env.example .env.local  然后填入 VAPID 密钥"
	else
		echo "   请创建 .env.local（至少含 ANOTIFY_VAPID_PUBLIC_KEY / ANOTIFY_VAPID_PRIVATE_KEY）"
	fi
	echo ""
fi
# source .env.local：只设未 export 的变量（set -a 会 export 所有，但 .env.local 里就是要 export 的）
if [ -f "$ENV_FILE" ]; then
	set -a
	# shellcheck disable=SC1090
	source "$ENV_FILE"
	set +a
fi

# 开发模式默认值（.env.local 没设就用这些，可被已有 env 覆盖）
: "${ANOTIFY_ADDR:=:8080}"
: "${ANOTIFY_STATIC:=./web}"
export ANOTIFY_ADDR ANOTIFY_STATIC

# ---------- 2. 确保 web/*.html 存在 ----------
if [ ! -f "web/index.html" ] || [ ! -f "web/login.html" ]; then
	echo "▶ web/*.html 不存在，生成中…"
	make sitegen || { echo "❌ make sitegen 失败"; exit 1; }
fi

# ---------- 3. 端口检查 ----------
# 从 ANOTIFY_ADDR（如 :8080 或 127.0.0.1:8080）提取端口号
PORT_NUM="${ANOTIFY_ADDR##*:}"
if [ -z "${PORT_NUM}" ]; then PORT_NUM=8080; fi
if lsof -nP -iTCP:"${PORT_NUM}" -sTCP:LISTEN >/dev/null 2>&1; then
	echo "❌ 端口 ${PORT_NUM} 已被占用，请先停掉占用进程："
	lsof -nP -iTCP:"${PORT_NUM}" -sTCP:LISTEN
	exit 1
fi

# ---------- 4. 起 cloudflared tunnel ----------
TUNNEL_PID=""
SERVER_PID=""
cleanup() {
	echo ""
	echo "▶ 停止服务…"
	[ -n "$TUNNEL_PID" ] && kill "$TUNNEL_PID" 2>/dev/null
	[ -n "$SERVER_PID" ] && kill "$SERVER_PID" 2>/dev/null
	wait 2>/dev/null
	echo "✅ 已停止"
}
trap cleanup EXIT INT TERM

if [ "${NO_TUNNEL:-0}" = "1" ]; then
	echo "▶ NO_TUNNEL=1，跳过 cloudflared tunnel"
else
	if ! command -v cloudflared >/dev/null 2>&1; then
		echo "⚠️  cloudflared 未安装，跳过 tunnel（brew install cloudflared）"
	else
		echo "▶ 启动 cloudflared tunnel（dev.openaaas.org → localhost:${PORT_NUM}）…"
		cloudflared tunnel run anotify > /tmp/anotify-tunnel.log 2>&1 &
		TUNNEL_PID=$!
		# 等 tunnel 就绪（日志出现 "Registered tunnel connection"）
		for _ in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15; do
			if grep -q "Registered tunnel connection\|connection registered" /tmp/anotify-tunnel.log 2>/dev/null; then
				echo "  ✅ tunnel 就绪（https://dev.openaaas.org）"
				break
			fi
			sleep 1
		done
		echo "  tunnel 日志：tail -f /tmp/anotify-tunnel.log"
	fi
fi

# ---------- 5. 起 server（前台）----------
echo ""
echo "▶ 启动 Anotify server（ANOTIFY_ADDR=$ANOTIFY_ADDR, RP_ID=${ANOTIFY_RP_ID:-localhost}）…"
echo "  本地：http://localhost:${PORT_NUM}"
echo "  公网：https://dev.openaaas.org  （需 tunnel）"
echo "  日志：/tmp/anotify-dev.log"
echo "  Ctrl-C 停止 server + tunnel"
echo ""
go run ./cmd/server 2>&1 | tee /tmp/anotify-dev.log &
SERVER_PID=$!
wait "$SERVER_PID"
