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
#   6. server 就绪后造示例数据（能力图 + 真跑过的节点 + 事件链）
#   6. 捕获 SIGINT/SIGTERM，退出时一并停掉 tunnel 和 server
#
# 可选环境变量：
#   NO_TUNNEL=1      只起 server，不起 tunnel
#   NO_AUTOLOGIN=1   不播种开发账号、不生成自动登录页（只用 passkey）
#   NO_DEMO_DATA=1   不造示例数据（能力图/节点/事件链），起一个**完全空**的实例
#   ANOTIFY_DEV_USER 开发账号用户名（默认 demo）
#   ANOTIFY_DEMO_SEC 示例节点保持在线的秒数（默认 30；期间事件链真的跑完）
#
# 输出的访问地址有三种：本地(localhost) / 内网(局域网 IP，手机可用) / 公网(trycloudflare)。
# 内网地址取默认路由的出口 IP——不用 `hostname -I`，那个在有 docker 网桥的机器上
# 常返回 172.17.0.1，手机连不上。
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

# ---------- 3.5 探测内网地址（手机/别的机器访问用）----------
#
# 为什么要单独探：开发时最常见的需求是"用手机看看效果"。localhost 不行，
# trycloudflare 要等隧道且域名会变，内网直连是最省事的一条路。
#
# 取默认路由的出口 IP，而不是 `hostname -I | awk '{print $1}'`：
# 后者在有 docker0 / br-* 网桥的机器上经常先返回 172.17.0.1，
# 那个地址手机根本连不上。
LAN_IP=""
if command -v ip >/dev/null 2>&1; then
	LAN_IP=$(ip route get 1.1.1.1 2>/dev/null | sed -n 's/.* src \([0-9.]*\).*/\1/p' | head -1)
fi
# 兜底：路由查不到就直接问内核（排除回环与 docker 网桥）
if [ -z "$LAN_IP" ]; then
	LAN_IP=$(ip -4 -brief addr show 2>/dev/null \
		| awk '$2=="UP" && $3 !~ /^(127\.|172\.1[0-9]\.|172\.2[0-9]\.|172\.3[01]\.)/ {split($3,a,"/"); print a[1]; exit}')
fi
# 只保留私有网段，公网 IP 不该被当成"内网地址"展示
case "$LAN_IP" in
	10.*|192.168.*|172.1[6-9].*|172.2[0-9].*|172.3[01].*) ;;
	*) LAN_IP="" ;;
esac

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
#
# 注意：这一步**只造账号和登录页**。示例数据（-demo）放在第 8 步，
# 因为它要通过 HTTP 投事件、用 WS 把节点挂上线，server 必须先跑起来。
if [ "${NO_AUTOLOGIN:-0}" = "1" ] && [ "${NO_DEMO_DATA:-0}" = "1" ]; then
	echo "▶ NO_AUTOLOGIN=1 + NO_DEMO_DATA=1，跳过开发账号与示例数据"
else
	DEV_USER="${ANOTIFY_DEV_USER:-demo}"
	# 默认造示例数据；`NO_DEMO_DATA=1` 起一个**完全空**的实例。
	#
	# 示例数据不是"往库里塞假状态"：devseed -demo 会注册一个节点、用真实 WS
	# 握手上线、再通过 /v1/events 投递事件，让整条派发链路真的跑一遍。
	# 所以界面上看到的事件流、任务链、out 三态都是真跑出来的。
	echo "▶ 播种开发账号 ${DEV_USER}…"

	if go run ./cmd/devseed \
		-db "${ANOTIFY_DB:-./anotify.db}" \
		-username "$DEV_USER" \
		-admin \
		-autologin-file "$AUTOLOGIN_PAGE" >/tmp/anotify-devseed.log 2>&1; then
		AUTOLOGIN_URL="http://localhost:${PORT_NUM}/$(basename "$AUTOLOGIN_PAGE")"
		echo "  ✅ 自动登录页：$AUTOLOGIN_URL"
		# TUNNEL_URL 为空 = 没起 tunnel 或起了但连不通，别拼出一个 "//__dev_login.html"
		if [ -n "$TUNNEL_URL" ]; then
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
[ -n "$LAN_IP" ] && echo "  内网：http://${LAN_IP}:${PORT_NUM}  （手机/同网段机器）"
[ -n "$TUNNEL_URL" ] && echo "  公网：${TUNNEL_URL}  （启动后自动验证）"
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

# ---------- 7. 公网可达性探测 ----------
#
# 必须放在 server 起来**之后**：tunnel 是反代到 localhost:PORT 的，
# 本地还没服务时探测必然失败，那只会误报「公网不通」。
#
# 两个坑都踩过：
#   - quick tunnel 的域名在边缘侧要几秒到几十秒才传播开 → 必须重试
#   - 本机 DNS 可能把外网域名全解析到 198.18.0.0/15（clash fake-ip 的正常
#     表现，不是劫持），域名照样能通 → **不能靠解析结果判死**，只认真实请求
if [ -n "$TUNNEL_URL" ]; then
	# 先等本地 server 应答，再探公网
	for _ in $(seq 1 20); do
		curl -fsS -o /dev/null --max-time 3 "http://localhost:${PORT_NUM}/" 2>/dev/null && break
		sleep 1
	done
	echo "▶ 检查公网可达性…"
	reachable=0
	for i in $(seq 1 12); do
		if curl -fsS -o /dev/null --max-time 10 "$TUNNEL_URL/" 2>/dev/null; then
			reachable=1
			break
		fi
		[ "$i" -lt 12 ] && echo "  等待边缘传播…($i/12)"
		sleep 4
	done
	if [ "$reachable" = "1" ]; then
		echo "  ✅ 公网可达：$TUNNEL_URL"
		if [ -f "$AUTOLOGIN_PAGE" ]; then
			echo "     免 passkey 登录：$TUNNEL_URL/$(basename "$AUTOLOGIN_PAGE")"
		fi
		echo "     （此地址随进程退出失效，下次启动会变）"
	else
		echo "  ⚠️  公网域名 48s 内仍不可达（tunnel 进程本身注册成功）。"
		echo "     常见原因：本地代理/防火墙拦了 *.trycloudflare.com。"
		echo "     本地地址照常可用：http://localhost:${PORT_NUM}"
		TUNNEL_URL=""
	fi
fi

# ---------- 8. 示例数据（默认造；NO_DEMO_DATA=1 起空实例）----------
#
# 顺序上必须在这里：devseed -demo 要通过 HTTP /v1/events 投事件、还要用 WS
# 把节点挂上线，server 不先起来只会 connection refused。
#
# 这也是示例数据「是真的」的原因——它不走后门：等同于一个外部系统在发
# webhook，外加一台真节点在接单、ack、回报，于是事件流与任务链自然长出来。
if [ "${NO_DEMO_DATA:-0}" != "1" ] && [ "${NO_AUTOLOGIN:-0}" != "1" ]; then
	DEV_USER="${ANOTIFY_DEV_USER:-demo}"
	echo "▶ 造示例数据（能力图 + 节点 + 事件链）…"
	# 等 server 真能应答，别抢跑
	for _ in $(seq 1 25); do
		curl -fsS -o /dev/null --max-time 2 "http://localhost:${PORT_NUM}/v1/vapid-public-key" 2>/dev/null && break
		sleep 1
	done
	if go run ./cmd/devseed \
		-db "${ANOTIFY_DB:-./anotify.db}" \
		-username "$DEV_USER" \
		-demo \
		-demo-url "http://localhost:${PORT_NUM}" \
		-demo-follow-sec "${ANOTIFY_DEMO_SEC:-30}" >/tmp/anotify-demo.log 2>&1; then
		grep -E "节点已上线|已投递|链路已全部跑完" /tmp/anotify-demo.log 2>/dev/null \
			| sed 's/^[0-9/: ]*//' | sed 's/^/     /'
	else
		echo "  ⚠️  示例数据失败（不影响使用），日志：/tmp/anotify-demo.log"
		tail -3 /tmp/anotify-demo.log 2>/dev/null | sed 's/^/     /'
	fi
fi

# ---------- 9. 访问地址汇总 ----------
#
# 放在最后是因为此时 server 已确认能应答，内网地址也才值得打印
# （早打印等于承诺一个还没验证的地址）。
if [ -n "$LAN_IP" ]; then
	lan_login="http://${LAN_IP}:${PORT_NUM}/$(basename "$AUTOLOGIN_PAGE")"
	if curl -fsS -o /dev/null --max-time 3 "http://${LAN_IP}:${PORT_NUM}/" 2>/dev/null; then
		echo ""
		echo "▶ 内网访问（手机/同网段机器，同一 WiFi 下直接打开）："
		echo "   http://${LAN_IP}:${PORT_NUM}"
		if [ -f "$AUTOLOGIN_PAGE" ]; then
			echo "   免 passkey 登录：${lan_login}"
		fi
	else
		echo ""
		echo "▶ 内网地址 http://${LAN_IP}:${PORT_NUM} 本机连不通"
		echo "   （常见原因：防火墙拦了 ${PORT_NUM} 端口，或 server 只绑了 127.0.0.1）"
	fi
fi

wait "$SERVER_PID"