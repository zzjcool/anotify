#!/usr/bin/env bash
# scripts/mock.sh — 纯前端 mock 预览 + trycloudflare 公网隧道
#
# 用途：把"只有前端、数据全是假的"这套页面直接发给别人看。
# 不需要数据库、不需要后端、不需要节点在线。
#
#   ./scripts/mock.sh                  # 本地 + 公网（trycloudflare 随机域名）
#   NO_TUNNEL=1 ./scripts/mock.sh      # 只本地
#   MOCK_PORT=7789 ./scripts/mock.sh   # 换端口
#   make mock                          # 默认带隧道
#
# 和 dev.sh 的区别：这里没有 server、登录页、示例数据、可达性之后的一堆步骤。
# mock 是纯静态站点，隧道只是把本地静态服务器反代出去，所以脚本短得多。
#
# 环境变量：
#   MOCK_PORT=7788        监听端口
#   MOCK_BIND=127.0.0.1   监听地址。默认只绑回环（不暴露到局域网）。
#                         要给手机/别人同 WiFi 看，用 MOCK_BIND=0.0.0.0，
#                         并按脚本打印的提示放行防火墙端口。
set -uo pipefail

cd "$(dirname "$0")/.." || exit 1

MOCK_PORT="${MOCK_PORT:-7788}"
TUNNEL_MODE="${ANOTIFY_TUNNEL:-quick}"
[ "${NO_TUNNEL:-0}" = "1" ] && TUNNEL_MODE="none"

# ---------- 端口占用检查 ----------
#
# 先查再起：go run 编译要几秒，等它报 "address already in use" 时
# 用户已经以为快好了，比较挫。
if command -v ss >/dev/null 2>&1 && ss -tln 2>/dev/null | grep -q ":${MOCK_PORT} "; then
	echo "⚠️  端口 ${MOCK_PORT} 已被占用。"
	holder=$(ss -tlnp 2>/dev/null | grep ":${MOCK_PORT} " | grep -oP 'pid=\K[0-9]+' | head -1)
	[ -n "$holder" ] && ps -o pid=,cmd= -p "$holder" 2>/dev/null | sed 's/^/     /'
	echo "     换端口：MOCK_PORT=7789 make mock"
	exit 1
fi

PREVIEW_PID=""
TUNNEL_PID=""
TUNNEL_URL=""
TUNNEL_LOG=/tmp/anotify-mock-tunnel.log

cleanup() {
	echo ""
	echo "▶ 停止…"
	[ -n "$TUNNEL_PID" ] && kill "$TUNNEL_PID" 2>/dev/null

	# 和 dev.sh 同样的问题：`go run ./cmd/fe-preview` 是个编译器进程，
	# 真正的静态服务器是它 fork 的子进程。只 kill go run 会留下孤儿占着端口，
	# 下次 make mock 就报端口被占用。所以要杀整个进程组。
	if [ -n "$PREVIEW_PID" ]; then
		kill -TERM -- "-$PREVIEW_PID" 2>/dev/null || kill "$PREVIEW_PID" 2>/dev/null
		sleep 1
		kill -KILL -- "-$PREVIEW_PID" 2>/dev/null
		pkill -P "$PREVIEW_PID" 2>/dev/null
		kill "$PREVIEW_PID" 2>/dev/null
	fi
	wait 2>/dev/null
	echo "✅ 已停止"
}
# 只 trap EXIT：trap "..." EXIT INT TERM 会在 TERM 后又被 EXIT 触发一次 → cleanup 跑两遍
trap cleanup EXIT

# ---------- 1. 起静态预览服务器 ----------
#
# 必须显式传 -addr：fe-preview 的 -addr 默认是 "localhost:PORT"（只绑回环），
# 而 -port 只替换 addr 的端口部分、保留 host。曾因此出现过「内网打不开、
# 看起来像防火墙没配」的误判——实际是内核直接回 RST，包根本没进 INPUT 链，
# ufw 怎么放行都没用。所以要暴露到局域网，必须改的是 addr 的 host。
MOCK_BIND="${MOCK_BIND:-127.0.0.1}"
echo "▶ 启动前端预览（mock 数据，不连后端）…"
# setsid 让它自成进程组，退出时能整组收掉（见 cleanup）
setsid go run ./cmd/fe-preview -dir web -addr "${MOCK_BIND}:${MOCK_PORT}" >/tmp/anotify-mock.log 2>&1 &
PREVIEW_PID=$!

# 等端口真的能应答再继续：tunnel 是反代到本端口的，
# 本地还没起来就起 tunnel，打开公网地址只会看到 502。
ready=0
for _ in $(seq 1 60); do
	if curl -fsS -o /dev/null --max-time 2 "http://127.0.0.1:${MOCK_PORT}/index.html" 2>/dev/null; then
		ready=1
		break
	fi
	sleep 0.5
done
if [ "$ready" != "1" ]; then
	echo "  ⚠️  预览服务器 30s 内没起来，日志："
	tail -10 /tmp/anotify-mock.log 2>/dev/null | sed 's/^/     /'
	exit 1
fi
echo "  ✅ 本地已就绪：http://127.0.0.1:${MOCK_PORT}"

# 绑回环时，内网地址压根不存在——不是防火墙的问题，多说一句会误导
case "$MOCK_BIND" in
	127.0.0.1|localhost) EXPOSED_LAN=0 ;;
	*)                  EXPOSED_LAN=1 ;;
esac

# ---------- 2. 内网地址（手机看）----------
#
# 用默认路由出口的 IP，不用 hostname -I：这台机器有 5 个 docker/br 网桥，
# hostname -I 第一个常是 172.17.0.1（手机连不上）。
LAN_IP=""
if command -v ip >/dev/null 2>&1; then
	LAN_IP=$(ip route get 1.1.1.1 2>/dev/null | grep -oP 'src \K[0-9.]+' | head -1)
fi
if [ -z "$LAN_IP" ]; then
	# 兜底：遍历网卡，取第一个私有网段（排除回环与 docker 网桥）
	# 用 glob 而不是 $(ls)：文件名带空格时 for 会拆错
	for netdir in /sys/class/net/*; do
		[ -e "$netdir" ] || continue
		iface=$(basename "$netdir")
		[ "$iface" = "lo" ] && continue
		case "$iface" in docker*|br-*|veth*) continue ;; esac
		addr=$(ip -4 addr show "$iface" 2>/dev/null | grep -oP 'inet \K[0-9.]+' | head -1)
		[ -z "$addr" ] && continue
		LAN_IP="$addr"
		break
	done
fi
case "$LAN_IP" in
	10.*|192.168.*|172.1[6-9].*|172.2[0-9].*|172.3[01].*) ;;
	*) LAN_IP="" ;;
esac

# 防火墙会拦外部访问，而**本机 curl 永远能通**（回环不进 INPUT 链）。
# 所以单独查 ufw 有没有放行本端口，别等用户拿手机试半天。
# 只在真绑了非回环地址时才判断：绑 127.0.0.1 时问题不在防火墙，
# 否则会打印「被 ufw 拦着 / 放行：sudo ufw allow ...」，让人白白去改防火墙。
LAN_BLOCKED_BY=""
if [ "$EXPOSED_LAN" = "1" ] && [ -n "$LAN_IP" ] && [ -r /etc/ufw/ufw.conf ] && grep -qi '^ENABLED=yes' /etc/ufw/ufw.conf 2>/dev/null; then
	if ! grep -q -- "--dport ${MOCK_PORT}" /etc/ufw/user.rules 2>/dev/null; then
		if grep -qi '^DEFAULT_INPUT_POLICY="DROP"' /etc/default/ufw 2>/dev/null; then
			LAN_BLOCKED_BY="ufw"
		fi
	fi
fi

# ---------- 3. 起 cloudflared tunnel ----------
if [ "$TUNNEL_MODE" = "none" ]; then
	echo "▶ 跳过 tunnel（NO_TUNNEL=1）"
elif ! command -v cloudflared >/dev/null 2>&1; then
	echo "⚠️  cloudflared 未安装，跳过 tunnel"
	echo "    安装：https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/downloads/"
else
	: >"$TUNNEL_LOG"
	echo "▶ 启动 quick tunnel（trycloudflare 随机域名 → localhost:${MOCK_PORT}）…"
	cloudflared tunnel --url "http://localhost:${MOCK_PORT}" --no-autoupdate >"$TUNNEL_LOG" 2>&1 &
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
		# 域名出来了不等于边缘已经能访问：quick tunnel 要几秒到几十秒传播。
		# 真探测一次再宣布可用，否则用户点开看到 502 会以为是页面坏了。
		echo "▶ 检查公网可达性…"
		#
		# 域名出现在日志里 ≠ 边缘已经能访问：quick tunnel 新域名要下发到
		# 边缘节点，实测 60~90s 才稳定（dev.sh 的 48s 窗口对它是偏紧的，
		# 曾出现过"探测说不可达、其实再等十几秒就 200"）。
		#
		# 所以：总窗口给到 ~120s，并且前 20s 先安静等待——刚注册就猛探
		# 只会拿到一串 502，白白消耗重试次数。
		reachable=0
		sleep 15
		for i in $(seq 1 18); do
			code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 12 "$TUNNEL_URL/" 2>/dev/null)
			if [ "$code" = "200" ]; then
				reachable=1
				break
			fi
			if [ $((i % 3)) -eq 0 ]; then
				echo "  等待边缘传播…(${i}/18, 最近 HTTP ${code:-超时})"
			fi
			sleep 6
		done
		if [ "$reachable" = "1" ]; then
			echo "  ✅ 公网可达"
		else
			# 不把 TUNNEL_URL 清空：它多半是好的，只是本机到该域名的
			# 路由/代理有问题。保留地址让用户自己用手机或换网络试，
			# 比直接说"不可达"然后藏起地址有用。
			echo "  ⚠️  本机 120s 内没探通（隧道本身已注册成功）。"
			echo "     地址仍然给你，用手机或换网络试：$TUNNEL_URL"
			echo "     常见原因：本机代理把 *.trycloudflare.com 拦了。"
		fi
	else
		echo "  ⚠️  30s 内没拿到 trycloudflare 域名，日志：$TUNNEL_LOG"
		tail -5 "$TUNNEL_LOG" 2>/dev/null | sed 's/^/     /'
	fi
	echo "  tunnel 日志：tail -f $TUNNEL_LOG"
fi

# ---------- 4. 地址汇总 ----------
echo ""
echo "=========================================="
echo "  🎭 前端 mock 预览（数据全是假的）"
echo "=========================================="
echo "  本机：  http://127.0.0.1:${MOCK_PORT}/sources.html"
if [ -n "$LAN_IP" ]; then
	if [ "$EXPOSED_LAN" != "1" ]; then
		# 别说「手机同一 WiFi 可用」：绑回环时内网访问会被内核 RST，
		# 那句提示是假的，还会把排查方向带偏到防火墙上。
		echo "  内网：  http://${LAN_IP}:${MOCK_PORT}  ✖ 不可用（当前只绑 ${MOCK_BIND}）"
		echo "          要共享：MOCK_BIND=0.0.0.0 make mock"
	elif [ "$LAN_BLOCKED_BY" = "ufw" ]; then
		echo "  内网：  http://${LAN_IP}:${MOCK_PORT}  ⚠️ 被 ufw 拦着（连上去要等超时）"
		echo "          放行：sudo ufw allow ${MOCK_PORT}/tcp"
	else
		echo "  内网：  http://${LAN_IP}:${MOCK_PORT}  （手机连同一 WiFi 可用）"
	fi
fi
if [ -n "$TUNNEL_URL" ]; then
	echo "  公网：  ${TUNNEL_URL}/sources.html"
	echo "          （随进程退出失效，下次启动会变）"
fi
echo ""
echo "  页面：/sources.html  /capabilities.html  /events.html  /nodes.html"
echo "  多语言：加 ?lang=en / ?lang=ja / ?lang=es"
echo "  看错误态：任意接口加 ?mockFail=1"
echo ""
echo "  提示：数据是内存里的假数据，刷新即回到初始状态。"
echo "       改 web/mock.js 后刷新浏览器即可，不用重启本脚本。"
echo "  Ctrl-C 停止"
echo "=========================================="
echo ""

wait
