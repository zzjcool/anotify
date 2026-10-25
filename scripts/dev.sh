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
#   4. 播种开发账号并生成自动登录页（web/__dev_login.html，退出时删除）
#   5. 前台起 go run ./cmd/server
#   6. 捕获 SIGINT/SIGTERM，退出时一并停掉 tunnel 和 server
#
# 可选环境变量：
#   NO_TUNNEL=1      只起 server，不起 tunnel
#   NO_AUTOLOGIN=1   不播种开发账号、不生成自动登录页（只用 passkey）
#   ANOTIFY_DEV_USER 开发账号用户名（默认 demo）
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
	make sitegen || {
		echo "❌ make sitegen 失败"
		exit 1
	}
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
TAIL_PID=""

# 自动登录页（开发专用，见下方 ---------- 5 ----------）
# 放在 web/ 下是因为 ANOTIFY_STATIC=./web 时静态服务直接读这个目录；
# .gitignore 的 `web/*.html` 已覆盖它，不会入库。
AUTOLOGIN_PAGE="web/__dev_login.html"
AUTOLOGIN_URL=""
TUNNEL_URL=""  # 由 tunnel 段填（quick 模式下来自 trycloudflare 日志）

cleanup() {
	echo ""
	echo "▶ 停止服务…"
	[ -n "$TUNNEL_PID" ] && kill "$TUNNEL_PID" 2>/dev/null
	[ -n "$TAIL_PID" ] && kill "$TAIL_PID" 2>/dev/null

	# 为什么不能只 kill $SERVER_PID：
	# `go run ./cmd/server` 自己是个编译器进程，真正的 server 是它 fork 出来的
	# 子进程。杀掉 go run 后子进程会变孤儿，**继续活着占着端口**，
	# 下次 make dev 就报「端口已被占用」。
	#
	# 所以要杀整个进程组：起 server 时用 setsid 让它自成一组，
	# 退出时对组发 TERM（负 PID），组内所有进程一并收掉。
	if [ -n "$SERVER_PID" ]; then
		kill -TERM -- "-$SERVER_PID" 2>/dev/null || kill "$SERVER_PID" 2>/dev/null
		sleep 1
		kill -KILL -- "-$SERVER_PID" 2>/dev/null
		pkill -P "$SERVER_PID" 2>/dev/null
		kill "$SERVER_PID" 2>/dev/null
	fi

	# 页面里内嵌着有效会话，退出时必须删掉，否则它一直能被本地静态服务读到
	rm -f "$AUTOLOGIN_PAGE"
	wait 2>/dev/null
	echo "✅ 已停止"
}
# 只 trap EXIT 一个：trap "..." EXIT INT TERM 会在 TERM 后又被 EXIT 触发一次，
# 导致 cleanup 跑两遍（日志里两行「停止服务」）。EXIT 在正常退出和被信号杀死时都会跑。
trap cleanup EXIT

# tunnel 模式（默认 quick = trycloudflare 随机域名）
#
#   quick（默认）  cloudflared tunnel --url …  → https://<随机>.trycloudflare.com
#                  零配置、不碰任何已登录的命名 tunnel。域名每次启动都会变，
#                  所以靠日志解析（没有别的 API 能拿到它）。
#   named          走你自己的命名 tunnel（固定域名，来自 .env.local 的
#                  ANOTIFY_RP_ID / ANOTIFY_RP_ORIGIN）。需要事先 cloudflared login
#                  + tunnel create + 配好 DNS。默认**不用**，因为随机域名已经够用，
#                  动用户的命名 tunnel 属于越界。
#   none           NO_TUNNEL=1，只起本地
TUNNEL_MODE="${ANOTIFY_TUNNEL:-quick}"
[ "${NO_TUNNEL:-0}" = "1" ] && TUNNEL_MODE="none"

if [ "$TUNNEL_MODE" = "none" ]; then
	echo "▶ 跳过 cloudflared tunnel（TUNNEL_MODE=none）"
elif ! command -v cloudflared >/dev/null 2>&1; then
	echo "⚠️  cloudflared 未安装，跳过 tunnel（brew install cloudflared）"
else
	TUNNEL_LOG=/tmp/anotify-tunnel.log
	: >"$TUNNEL_LOG"
	if [ "$TUNNEL_MODE" = "named" ]; then
		TUNNEL_NAME="${ANOTIFY_TUNNEL_NAME:-anotify}"
		echo "▶ 启动命名 tunnel（$TUNNEL_NAME → localhost:${PORT_NUM}）…"
		cloudflared tunnel run "$TUNNEL_NAME" >"$TUNNEL_LOG" 2>&1 &
		TUNNEL_PID=$!
		for _ in $(seq 1 20); do
			if grep -q "Registered tunnel connection\|connection registered" "$TUNNEL_LOG" 2>/dev/null; then
				TUNNEL_URL="${ANOTIFY_RP_ORIGIN:-https://dev.openaaas.org}"
				echo "  ✅ tunnel 就绪（$TUNNEL_URL）"
				break
			fi
			sleep 1
		done
	else
		echo "▶ 启动 quick tunnel（trycloudflare 随机域名 → localhost:${PORT_NUM}）…"
		cloudflared tunnel --url "http://localhost:${PORT_NUM}" --no-autoupdate >"$TUNNEL_LOG" 2>&1 &
		TUNNEL_PID=$!
		# 域名只能从日志里解析：cloudflared 会打一行
		#   INF |  https://<随机>.trycloudflare.com   |
		for _ in $(seq 1 30); do
			TUNNEL_URL=$(grep -oE "https://[a-z0-9-]+\.trycloudflare\.com" "$TUNNEL_LOG" 2>/dev/null | head -1)
			[ -n "$TUNNEL_URL" ] && break
			sleep 1
		done
		if [ -n "$TUNNEL_URL" ]; then
			echo "  ✅ tunnel 就绪（$TUNNEL_URL）"
		else
			echo "  ⚠️  30s 内没拿到 trycloudflare 域名，日志：$TUNNEL_LOG"
			tail -5 "$TUNNEL_LOG" 2>/dev/null | sed 's/^/     /'
		fi
	fi
	echo "  tunnel 日志：tail -f $TUNNEL_LOG"
fi

# ---------- 5. 开发账号 + 自动登录页 ----------
#
# 为什么需要这个：WebAuthn 的 RP_ID 必须与访问域名**严格一致**。换了域名
# （trycloudflare 随机域名、局域网 IP、手机访问）就注册不了 passkey，
# 于是每次都要手工造会话、手工往浏览器里粘 cookie。
#
# 这里用 devseed 直接落一个会话，再生成一个「写 cookie 并跳转」的页面，
# 打开就能进工作台 —— passkey 那条路依然完整可用，这只是多一条便道。
if [ "${NO_AUTOLOGIN:-0}" = "1" ]; then
	echo "▶ NO_AUTOLOGIN=1，跳过开发账号播种"
else
	DEV_USER="${ANOTIFY_DEV_USER:-demo}"
	echo "▶ 播种开发账号 ${DEV_USER}…"
	if go run ./cmd/devseed \
		-db "${ANOTIFY_DB:-./anotify.db}" \
		-username "$DEV_USER" \
		-admin \
		-autologin-file "$AUTOLOGIN_PAGE" >/tmp/anotify-devseed.log 2>&1; then
		AUTOLOGIN_URL="http://localhost:${PORT_NUM}/$(basename "$AUTOLOGIN_PAGE")"
		echo "  ✅ 自动登录页：$AUTOLOGIN_URL"
		if [ "${NO_TUNNEL:-0}" != "1" ] && command -v cloudflared >/dev/null 2>&1; then
			echo "     公网版本：${TUNNEL_URL}/$(basename "$AUTOLOGIN_PAGE")"
		fi
	else
		echo "  ⚠️  devseed 失败（passkey 登录仍可正常用），日志：/tmp/anotify-devseed.log"
		tail -3 /tmp/anotify-devseed.log 2>/dev/null | sed 's/^/     /'
	fi
fi

# ---------- 6. 起 server（前台）----------
echo ""
echo "▶ 启动 Anotify server（ANOTIFY_ADDR=$ANOTIFY_ADDR, RP_ID=${ANOTIFY_RP_ID:-localhost}）…"
echo "  本地：http://localhost:${PORT_NUM}"
echo "  公网：${TUNNEL_URL}  （需 tunnel）"
[ -n "$AUTOLOGIN_URL" ] && echo "  免 passkey 登录：$AUTOLOGIN_URL"
echo "  日志：/tmp/anotify-dev.log"
echo "  Ctrl-C 停止 server + tunnel"
echo ""
# 用 setsid 让 server 自成进程组，退出时能整组收掉（见 cleanup 的说明）。
# 不用 `| tee`：管道下 $! 拿到的是 tee 的 PID，杀它不会停 server。
setsid go run ./cmd/server >/tmp/anotify-dev.log 2>&1 &
SERVER_PID=$!

# 顺手把日志回显到终端，保留原来 `| tee` 的观感
tail -n +1 -f /tmp/anotify-dev.log 2>/dev/null &
TAIL_PID=$!

wait "$SERVER_PID"