#!/bin/sh
# anotify · CLI 设备授权登录脚本
#
# 用途：在本机为 Agent/插件申请一个 Anotify 上报凭证（API Key），全程
#       Key 明文不打印到终端、不进入对话上下文。
#
# 用法：
#   ANOTIFY_BASE_URL=https://push.example.com sh web/agent-login.sh
#   sh web/agent-login.sh https://push.example.com
#
# 环境变量：
#   ANOTIFY_BASE_URL      服务器地址（必填，或第一个位置参数）
#   ANOTIFY_DEVICE_NAME   覆盖设备名（默认 hostname）
#
# 退出码：
#   0  成功（凭证已写入）
#   1  通用错误（参数缺失/建会话失败/落盘失败等）
#   2  用户在确认页选择了「拒绝」
#   3  授权会话已过期
#   4  超时或网络错误
#
# 依赖：POSIX sh + curl（不依赖 jq/python/node）。JSON 字段用 sed 提取。
set -eu

# ----------------------------------------------------------------------------
# 工具函数
# ----------------------------------------------------------------------------

# json_str 从 JSON 文本里提取 "key":"value" 的 value（首处匹配）。
# 仅用于本脚本已知的简单扁平响应体（apiKey/server/deviceName 等不含双引号的值）。
json_str() {
	_key="$1"
	printf '%s' "$2" \
		| sed -n 's/.*"'"$_key"'"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' \
		| head -n1
}

# json_num 提取数值字段（无引号）：  "key": 123
json_num() {
	_key="$1"
	printf '%s' "$2" \
		| sed -n 's/.*"'"$_key"'"[[:space:]]*:[[:space:]]*\([0-9][0-9]*\).*/\1/p' \
		| head -n1
}

die() {
	echo "✗ $*" >&2
	exit 1
}

# now_epoch 输出 Unix 秒。优先 date +%s（POSIX）。
now_epoch() {
	date +%s
}

# credentials 目录与文件路径
config_dir() {
	if [ -n "${XDG_CONFIG_HOME:-}" ]; then
		printf '%s/anotify' "$XDG_CONFIG_HOME"
	else
		printf '%s/.config/anotify' "$HOME"
	fi
}
cred_file() {
	printf '%s/credentials.json' "$(config_dir)"
}

# ----------------------------------------------------------------------------
# 参数与自检
# ----------------------------------------------------------------------------

BASE="${ANOTIFY_BASE_URL:-${1:-}}"
if [ -z "$BASE" ]; then
	echo "用法: ANOTIFY_BASE_URL=https://push.example.com sh $0 [URL]" >&2
	echo "      sh $0 https://push.example.com" >&2
	exit 1
fi
# 去掉尾部斜杠
BASE="${BASE%/}"

DEVICE_NAME="${ANOTIFY_DEVICE_NAME:-$(hostname 2>/dev/null || echo unknown)}"

CRED="$(cred_file)"

# 幂等自检：已有凭证则验证是否仍有效
if [ -f "$CRED" ]; then
	EXISTING_KEY="$(json_str apiKey "$(cat "$CRED" 2>/dev/null || echo "")")"
	EXISTING_SRV="$(json_str server "$(cat "$CRED" 2>/dev/null || echo "")")"
	if [ -n "$EXISTING_KEY" ] && [ -n "$EXISTING_SRV" ]; then
		# 用已存 server 自检（保持与原配置一致）
		self_code=$(curl -fsS -o /dev/null -w '%{http_code}' \
			-H "Authorization: Bearer $EXISTING_KEY" \
			"$EXISTING_SRV/v1/keys/self" 2>/dev/null || true)
		case "$self_code" in
			200)
				echo "✓ 已登录（凭证有效）：$CRED"
				exit 0
				;;
			401)
				echo "• 旧凭证已失效，重新发起授权…" >&2
				;;
			"")
				echo "• 无法连接服务器 $EXISTING_SRV 验证旧凭证，继续发起新授权…" >&2
				;;
			*)
				echo "• 旧凭证自检异常（HTTP $self_code），继续发起新授权…" >&2
				;;
		esac
	fi
fi

# ----------------------------------------------------------------------------
# 建会话
# ----------------------------------------------------------------------------

echo "→ 正在向 $BASE 发起设备授权…"

# 构造 JSON 请求体（不用 jq）
REQ_BODY="{\"deviceName\":\"$DEVICE_NAME\",\"scopes\":[\"notify:send\"]}"

# -s 静默进度 -S 出错提示；--fail-with-body 在 4xx/5xx 仍输出 body 便于诊断（curl>=7.76）
# 为兼容旧 curl，用 -sS 并手动捕获 http_code 与 body。
create_resp_file=$(mktemp 2>/dev/null || echo "/tmp/anotify.$$")
trap 'rm -f "$create_resp_file"' EXIT

create_http=$(curl -sS -o "$create_resp_file" -w '%{http_code}' \
	-H 'Content-Type: application/json' \
	-d "$REQ_BODY" \
	"$BASE/v1/cli-auth/sessions" 2>/dev/null || true)

if [ "$create_http" != "200" ]; then
	body="$(cat "$create_resp_file" 2>/dev/null || true)"
	die "建会话失败（HTTP ${create_http:-网络错误}）${body:+：$body}"
fi

CREATE_BODY="$(cat "$create_resp_file")"
SESSION_ID="$(json_str sessionId "$CREATE_BODY")"
SECRET="$(json_str secret "$CREATE_BODY")"
USER_CODE="$(json_str userCode "$CREATE_BODY")"
AUTH_URL="$(json_str authUrl "$CREATE_BODY")"
POLL_INTERVAL="$(json_num pollInterval "$CREATE_BODY")"
EXPIRES_AT="$(json_num expiresAt "$CREATE_BODY")"

# 任一关键字段缺失即报错（不打印 secret 缺失信息以免暗示其值）
if [ -z "$SESSION_ID" ] || [ -z "$SECRET" ] || [ -z "$USER_CODE" ] \
	|| [ -z "$AUTH_URL" ] || [ -z "$POLL_INTERVAL" ] || [ -z "$EXPIRES_AT" ]; then
	die "建会话响应字段不完整"
fi

# pollInterval 最小 1 秒，防止过快轮询
[ "$POLL_INTERVAL" -lt 1 ] 2>/dev/null && POLL_INTERVAL=1

# ----------------------------------------------------------------------------
# 三入口同时输出
# ----------------------------------------------------------------------------

echo ""
echo "════════════════════════════════════════════════════════════"
echo "  请在任意一台已登录 $BASE 的设备上完成授权"
echo "════════════════════════════════════════════════════════════"
echo ""

# 入口①：本地有浏览器则自动打开
open_browser() {
	_url="$1"
	if [ -n "${SSH_TTY:-}${SSH_CONNECTION:-}" ]; then
		return 1
	fi
	case "$(uname 2>/dev/null)" in
		Darwin)
			if command -v open >/dev/null 2>&1; then
				open "$_url" >/dev/null 2>&1 || return 1
				return 0
			fi
			;;
		*)
			if [ -n "${DISPLAY:-}${WAYLAND_DISPLAY:-}" ] && command -v xdg-open >/dev/null 2>&1; then
				xdg-open "$_url" >/dev/null 2>&1 || return 1
				return 0
			fi
			;;
	esac
	return 1
}

if open_browser "$AUTH_URL"; then
	echo "  ✓ 已尝试用浏览器打开授权页面"
else
	echo "  · 未检测到可用浏览器（或处于远程会话）"
fi
echo ""

# 入口②：ASCII 二维码（服务端渲染）
qr_tmp=$(mktemp 2>/dev/null || echo "/tmp/anotify-qr.$$")
if curl -fsS -o "$qr_tmp" "$BASE/v1/cli-auth/sessions/$SESSION_ID/qr.txt" 2>/dev/null; then
	echo "  ▼ 扫描下方二维码（手机相机即可）"
	echo ""
	cat "$qr_tmp"
	echo ""
else
	echo "  · 二维码获取失败，可改用下方链接或验证码"
	echo ""
fi
rm -f "$qr_tmp"

# 入口③：短码 + 手动入口
echo "  验证码：$USER_CODE"
echo "  手动打开：$BASE/cli-auth.html"
echo ""
echo "  等待批准中（可在此页面选择权限后点击「批准」）…"
echo ""

# ----------------------------------------------------------------------------
# 轮询
# ----------------------------------------------------------------------------

net_err=0
while :; do
	# 超时检查
	if [ "$(now_epoch)" -gt "$EXPIRES_AT" ]; then
		echo "✗ 授权超时（超过 10 分钟未完成）" >&2
		exit 4
	fi

	sleep "$POLL_INTERVAL"

	poll_resp_file=$(mktemp 2>/dev/null || echo "/tmp/anotify-poll.$$")
	poll_http=$(curl -sS -o "$poll_resp_file" -w '%{http_code}' \
		"$BASE/v1/cli-auth/sessions/$SESSION_ID/poll?secret=$SECRET" 2>/dev/null || true)

	case "$poll_http" in
		200)
			net_err=0
			POLL_BODY="$(cat "$poll_resp_file")"
			rm -f "$poll_resp_file"
			STATUS="$(json_str status "$POLL_BODY")"
			case "$STATUS" in
				pending)
					printf '.'
					;;
				approved)
					echo ""
					API_KEY="$(json_str apiKey "$POLL_BODY")"
					if [ -z "$API_KEY" ]; then
						die "领证响应缺少 apiKey 字段"
					fi
					# 落盘
					mkdir -p -m 0700 "$(config_dir)" 2>/dev/null || \
						die "无法创建配置目录 $(config_dir)"
					# 备份旧凭证
					if [ -f "$CRED" ]; then
						mv "$CRED" "$CRED.bak" 2>/dev/null || true
					fi
					now="$(now_epoch)"
					# umask 077 保证临时文件 0600；写完原子 mv
					tmp_out="${CRED}.tmp.$$"
					( umask 077
					  printf '%s\n' "{\"server\":\"$BASE\",\"apiKey\":\"$API_KEY\",\"deviceName\":\"$DEVICE_NAME\",\"createdAt\":$now}" > "$tmp_out" )
					mv "$tmp_out" "$CRED" || die "写入凭证文件失败"
					chmod 0600 "$CRED" 2>/dev/null || true
					echo "✓ 授权成功，凭证已写入：$CRED"
					exit 0
					;;
				denied)
					echo ""
					echo "✗ 授权已被拒绝" >&2
					exit 2
					;;
				expired|consumed)
					# consumed 理论上不该发生在首次成功领证前；按过期处理
					echo ""
					echo "✗ 授权会话已失效（$STATUS）" >&2
					exit 3
					;;
				*)
					echo ""
					echo "✗ 未知的轮询状态：$STATUS" >&2
					exit 1
					;;
			esac
			;;
		429)
			rm -f "$poll_resp_file"
			# 退避一个间隔后继续，不计网络错误
			sleep "$POLL_INTERVAL"
			;;
		401)
			rm -f "$poll_resp_file"
			echo "✗ secret 无效，无法领取凭证" >&2
			exit 1
			;;
		404)
			rm -f "$poll_resp_file"
			echo "✗ 授权会话不存在或已过期" >&2
			exit 3
			;;
		"")
			rm -f "$poll_resp_file"
			net_err=$((net_err + 1))
			if [ "$net_err" -ge 5 ]; then
				echo ""
				echo "✗ 网络错误次数过多，放弃" >&2
				exit 4
			fi
			printf '!' >&2
			;;
		*)
			rm -f "$poll_resp_file"
			net_err=$((net_err + 1))
			if [ "$net_err" -ge 5 ]; then
				echo ""
				echo "✗ 轮询异常（HTTP $poll_http），放弃" >&2
				exit 4
			fi
			;;
	esac
done
