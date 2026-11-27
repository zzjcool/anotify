#!/bin/sh
# CLI 设备授权 · 端到端冒烟脚本（手动可复跑，不注册进 run_all.sh）
#
# 用途：验证 CLI 设备授权全链路 + web/agent-login.sh 的幂等自检与输出安全。
# 前置：make build + go build -o .e2e-bin/devseed ./cmd/devseed。
#
# 说明：login.sh 是阻塞式交互脚本，无法在非交互冒烟里驱动 approve 时序，
#       故改用 curl 走全链路（建会话→批准→领证→Key 可用），再跑幂等自检并扫描输出无明文。
set -eu

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

BIN="$ROOT/anotify"
DEVSEED="$ROOT/.e2e-bin/devseed"
PORT="${PORT:-5999}"
BASE="http://localhost:${PORT}"
TMP="$(mktemp -d /tmp/anotify-smoke.XXXXXX)"
DB="$TMP/test.db"
FAKE_HOME="$TMP/home"
CREDS_DIR="$FAKE_HOME/.config/anotify"
CREDS="$CREDS_DIR/credentials.json"

SRV_PID=""
cleanup() {
	[ -n "$SRV_PID" ] && kill "$SRV_PID" 2>/dev/null || true
	rm -rf "$TMP"
}
trap cleanup EXIT

echo "=== 启动服务（端口 ${PORT}）==="
ANOTIFY_ADDR=":${PORT}" \
ANOTIFY_DB="$DB" \
ANOTIFY_STATIC="" \
ANOTIFY_RP_ID=localhost \
ANOTIFY_RP_ORIGIN="$BASE" \
"$BIN" >"$TMP/srv.log" 2>&1 &
SRV_PID=$!

i=0
while [ $i -lt 50 ]; do
	if curl -fsS "$BASE/health" >/dev/null 2>&1; then
		echo "✓ 服务就绪"
		break
	fi
	i=$((i + 1))
	sleep 0.2
done
[ $i -lt 50 ] || { echo "✗ 服务启动失败"; cat "$TMP/srv.log" >&2; exit 1; }

echo "=== 播种用户 ==="
SEED_OUT="$("$DEVSEED" -db "$DB" -username smoker 2>&1)"
SESSION="$(printf '%s\n' "$SEED_OUT" | sed -n 's/^SESSION=//p')"
echo "✓ session=${SESSION:+<set>}"

echo "=== 1. 建会话（curl）==="
CREATE=$(curl -fsS -H 'Content-Type: application/json' \
	-d '{"deviceName":"smoke-host","scopes":["notify:send"]}' \
	"$BASE/v1/cli-auth/sessions")
SID=$(printf '%s' "$CREATE" | sed -n 's/.*"sessionId":"\([^"]*\)".*/\1/p')
SECRET=$(printf '%s' "$CREATE" | sed -n 's/.*"secret":"\([^"]*\)".*/\1/p')
echo "✓ 会话已建：sid=${SID}（secret 已记录不打印）"
[ -n "$SID" ] && [ -n "$SECRET" ] || { echo "✗ 建会话响应不完整"; exit 1; }

echo "=== 2. 批准（session cookie）==="
APPROVE_HTTP=$(curl -sS -o /dev/null -w '%{http_code}' \
	-H 'Content-Type: application/json' \
	-H "Cookie: anotify_session=$SESSION" \
	-d '{"scopes":["notify:send"]}' \
	"$BASE/v1/cli-auth/sessions/$SID/approve")
echo "✓ 批准 HTTP=$APPROVE_HTTP"
[ "$APPROVE_HTTP" = "200" ] || { echo "✗ 批准失败"; exit 1; }

echo "=== 3. 轮询领证（一次性）==="
POLL=$(curl -fsS "$BASE/v1/cli-auth/sessions/$SID/poll?secret=$SECRET")
POLL_STATUS=$(printf '%s' "$POLL" | sed -n 's/.*"status":"\([^"]*\)".*/\1/p')
APIKEY=$(printf '%s' "$POLL" | sed -n 's/.*"apiKey":"\([^"]*\)".*/\1/p')
echo "✓ 轮询 status=$POLL_STATUS apiKey=${APIKEY:+<got>}"
[ "$POLL_STATUS" = "approved" ] && [ -n "$APIKEY" ] || { echo "✗ 领证失败"; exit 1; }

echo "=== 4. 二次轮询应为 consumed（一次性不变量）==="
# 等待 pollInterval 避免 429；pollInterval 默认 2 秒
sleep 3
POLL2_HTTP=$(curl -sS -o "$TMP/poll2.json" -w '%{http_code}' "$BASE/v1/cli-auth/sessions/$SID/poll?secret=$SECRET")
if [ "$POLL2_HTTP" = "429" ]; then
	sleep 3
	POLL2_HTTP=$(curl -sS -o "$TMP/poll2.json" -w '%{http_code}' "$BASE/v1/cli-auth/sessions/$SID/poll?secret=$SECRET")
fi
POLL2_STATUS=$(sed -n 's/.*"status":"\([^"]*\)".*/\1/p' < "$TMP/poll2.json")
echo "✓ 二次轮询 status=$POLL2_STATUS (HTTP $POLL2_HTTP)"
[ "$POLL2_STATUS" = "consumed" ] || { echo "✗ 应为 consumed"; cat "$TMP/poll2.json" >&2; exit 1; }

echo "=== 5. Key 可用（keys/self 200）==="
SELF_HTTP=$(curl -fsS -o /dev/null -w '%{http_code}' \
	-H "Authorization: Bearer $APIKEY" \
	"$BASE/v1/keys/self")
echo "✓ keys/self HTTP=$SELF_HTTP"
[ "$SELF_HTTP" = "200" ] || { echo "✗ keys/self 失败"; exit 1; }

echo "=== 6. 模拟脚本落盘（0600/0700 原子写）==="
mkdir -p -m 0700 "$CREDS_DIR"
printf '%s\n' "{\"server\":\"$BASE\",\"apiKey\":\"$APIKEY\",\"deviceName\":\"smoke-host\",\"createdAt\":$(date +%s)}" > "$CREDS"
chmod 0600 "$CREDS"
if [ "$(uname)" = "Darwin" ]; then
	FP=$(stat -f '%Lp' "$CREDS"); DP=$(stat -f '%Lp' "$CREDS_DIR")
else
	FP=$(stat -c '%a' "$CREDS"); DP=$(stat -c '%a' "$CREDS_DIR")
fi
echo "  文件权限=$FP 目录权限=$DP"
[ "$FP" = "600" ] || { echo "✗ 文件权限非 0600"; exit 1; }
[ "$DP" = "700" ] || { echo "✗ 目录权限非 0700"; exit 1; }
echo "✓ 权限正确"

echo "=== 7. 跑 login.sh 验幂等（已有有效凭证→退出 0）==="
SSH_TTY=1 ANOTIFY_BASE_URL="$BASE" HOME="$FAKE_HOME" \
	sh "$ROOT/web/agent-login.sh" >"$TMP/login.log" 2>&1
RC=$?
echo "  退出码=$RC"
cat "$TMP/login.log"
[ "$RC" = "0" ] || { echo "✗ 幂等应退出 0"; exit 1; }
grep -q "已登录" "$TMP/login.log" || { echo "✗ 未输出「已登录」"; exit 1; }

echo "=== 8. 扫描 login.sh 输出无明文 Key/secret ==="
if grep -qF "$APIKEY" "$TMP/login.log"; then
	echo "✗ 输出泄露了 apiKey"; exit 1
fi
if grep -qF "$SECRET" "$TMP/login.log"; then
	echo "✗ 输出泄露了 secret"; exit 1
fi
echo "✓ 输出无明文 Key/secret"

echo "=== 9. 脚本无参数应退出 1 并提示用法 ==="
# 用干净 HOME（无已有凭证）避免幂等短路
NOBASE_HOME="$TMP/nobase-home"
mkdir -p "$NOBASE_HOME"
# 临时关 -e 以捕获非零退出码
set +e
HOME="$NOBASE_HOME" sh "$ROOT/web/agent-login.sh" >"$TMP/usage.log" 2>&1
RC2=$?
set -e
[ "$RC2" = "1" ] || { echo "✗ 无参数应退出 1（实际 ${RC2}）"; exit 1; }
grep -q "用法" "$TMP/usage.log" || { echo "✗ 未提示用法"; exit 1; }
echo "✓ 用法提示正确"

echo "=== 10. 首页推荐的命令照抄可跑（#577 review r2 回归）==="
RAW_CMD="$(sed -n 's/^    cmd_install: //p' web-src/locales/zh-CN.yaml | head -n1)"
CMD="$(printf '%s' "$RAW_CMD" | sed "s|^'||; s|'$||; s|{base}|$BASE|g")"
echo "  命令=$CMD"
[ -n "$CMD" ] || { echo "✗ locale 里取不到 index.quickstart.cmd_install"; exit 1; }
case "$CMD" in
	*web/agent-login.sh*) echo "✗ 命令指向仓内路径（部署版没有这个文件）"; exit 1 ;;
esac
case "$CMD" in
	*ANOTIFY_BASE_URL=*) : ;;
	*) echo "✗ 命令没带 ANOTIFY_BASE_URL，脚本会退出 1"; exit 1 ;;
esac
CMD_HOME="$TMP/cmd-home"
mkdir -p "$CMD_HOME"
if command -v timeout >/dev/null 2>&1; then
	set +e
	( cd "$TMP" && HOME="$CMD_HOME" SSH_TTY=1 timeout 8 sh -c "$CMD" ) >"$TMP/cmd.log" 2>&1
	RC3=$?
	set -e
else
	echo "  · 无 timeout 命令，跳过可执行性断言（形状断言已过）"
	RC3=0
fi
if grep -q "用法" "$TMP/cmd.log" 2>/dev/null; then
	echo "✗ 照抄后仍是用法错误（命令缺 base）"; cat "$TMP/cmd.log" >&2; exit 1
fi
if command -v timeout >/dev/null 2>&1; then
	grep -q "正在向 $BASE 发起设备授权" "$TMP/cmd.log" \
		|| { echo "✗ 命令没进到授权流程（rc=${RC3}）"; cat "$TMP/cmd.log" >&2; exit 1; }
	echo "✓ 首页命令照抄可进到授权流程（rc=${RC3}：限时截断属预期）"
fi

echo ""
echo "════════════════════════════════════════"
echo "  ✅ 冒烟全通过"
echo "════════════════════════════════════════"
