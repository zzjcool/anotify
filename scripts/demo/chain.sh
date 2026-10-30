#!/usr/bin/env bash
# 多跳链路 demo（issue #186）
#
# 一条命令验证主流程：
#
#   POST /v1/hooks/<token> → 事件 → 派发 → demo.prep → demo.build → demo.report
#                                             (node-1)     (node-2)      (node-3)
#
# 为什么要有这个脚本：单测和 e2e 都在测"服务端内部"，
# 而这条链路把**外部契约**串起来——节点用令牌连 WS、自己声明能力与执行方式、
# 中枢只派发不含命令的 assign 帧、节点执行 bash 并回报出口。
#
# 用法：
#   ./scripts/demo/chain.sh              # 跑完自动清理
#   KEEP=1 ./scripts/demo/chain.sh       # 保留临时目录（排查用）
#   PORT=7891 ./scripts/demo/chain.sh    # 换端口
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
PORT=${PORT:-7890}
BASE="http://localhost:$PORT"
WORK=$(mktemp -d "${TMPDIR:-/tmp}/anotify-chain.XXXXXX")
PIDS=()
TOTAL=${TOTAL_STEPS:-8}
STEP=0

say()   { STEP=$((STEP + 1)); printf '\n\033[1;36m▶ %d/%d %s\033[0m\n' "$STEP" "$TOTAL" "$*"; }
info()  { printf '  %s\n' "$*"; }
fail() {
	printf '\n\033[1;31m❌ %s\033[0m\n' "$*" >&2
	if [ -f "$WORK/server.log" ]; then echo "--- server.log（末 15 行）"; tail -15 "$WORK/server.log"; fi
	# agent 日志是排查"连不上 / 握手被拒"的第一现场，失败时一并打出来
	for f in "$WORK"/node-*.log; do
		[ -f "$f" ] || continue
		echo "--- $(basename "$f")（末 8 行）"; tail -8 "$f"
	done
	exit 1
}

cleanup() {
	for p in "${PIDS[@]:-}"; do
		if [ -n "$p" ]; then kill "$p" 2>/dev/null || true; fi
	done
	wait 2>/dev/null || true
	if [ "${KEEP:-0}" = "1" ]; then printf '\n（临时目录已保留：%s）\n' "$WORK"; else rm -rf "$WORK"; fi
}
trap cleanup EXIT

# wait_until <描述> <超时秒> <命令...>：等条件成立，不 sleep 固定时间。
wait_until() {
	local desc=$1 secs=$2; shift 2
	local deadline=$((SECONDS + secs))
	while [ $SECONDS -lt $deadline ]; do
		if "$@" >/dev/null 2>&1; then return 0; fi
		sleep 0.1
	done
	fail "等不到：$desc（超过 ${secs}s）"
}

# ---------- 1. 构建 ----------
say "构建二进制（server + agent）"
(cd "$ROOT" && GOPROXY=${GOPROXY:-direct} GOSUMDB=${GOSUMDB:-sum.golang.org} GOTOOLCHAIN=${GOTOOLCHAIN:-auto} \
	go build -tags embeddist -o "$WORK/anotify" ./cmd/server) || fail "server 构建失败"
(cd "$ROOT" && GOPROXY=${GOPROXY:-direct} GOSUMDB=${GOSUMDB:-sum.golang.org} GOTOOLCHAIN=${GOTOOLCHAIN:-auto} \
	go build -o "$WORK/agent" ./cmd/agent) || fail "agent 构建失败"
info "ok：$WORK/anotify, $WORK/agent"

# ---------- 2. 建用户与会话 ----------
say "建用户与会话（devseed）"
SEED=$(cd "$ROOT" && GOPROXY=${GOPROXY:-direct} GOSUMDB=${GOSUMDB:-sum.golang.org} GOTOOLCHAIN=${GOTOOLCHAIN:-auto} \
	go run ./cmd/devseed -db "$WORK/db.sqlite" -username demo 2>/dev/null) || fail "devseed 失败"
SESSION=$(printf '%s\n' "$SEED" | sed -n 's/^SESSION=//p')
[ -n "$SESSION" ] || fail "devseed 没输出 SESSION"
info "会话已建（${SESSION:0:12}…）"

# ---------- 3. 起 server ----------
say "起 server（临时库，端口 $PORT）"
ANOTIFY_ADDR=":$PORT" ANOTIFY_DB="$WORK/db.sqlite" ANOTIFY_STATIC="" \
	"$WORK/anotify" >"$WORK/server.log" 2>&1 &
PIDS+=($!)
wait_until "server /health 200" 20 curl -sf "$BASE/health"
info "ok：$BASE"

C="Cookie: anotify_session=$SESSION"
# api 的状态码必须走**文件**：调用点几乎都是 $(api ...)，
# 命令替换是子 shell，在那里赋给变量传不回父 shell（第一次就踩了这个坑：
# 服务器日志明明是 200，脚本却判定"实际为空"）。
api() { # api METHOD PATH [BODY] → 响应体写 stdout，状态码写 $WORK/.code
	local m=$1 p=$2 body=${3-}
	if [ -n "$body" ]; then
		curl -sS -o "$WORK/.body" -w '%{http_code}' -X "$m" \
			-H "$C" -H 'content-type: application/json' -d "$body" "$BASE$p" >"$WORK/.code"
	else
		curl -sS -o "$WORK/.body" -w '%{http_code}' -X "$m" -H "$C" "$BASE$p" >"$WORK/.code"
	fi
	cat "$WORK/.body"
}
code() { cat "$WORK/.code"; }
expect() { # expect <期望状态码> <说明>
	local got; got=$(code)
	[ "$got" = "$1" ] || fail "$2：期望 HTTP $1，实际 ${got:-空}"
}

# ---------- 4. 命名空间 ----------
say "取默认命名空间"
NS_JSON=$(api GET /v1/namespaces); expect 200 "GET /v1/namespaces"
NS=$(printf '%s' "$NS_JSON" | jq -r '(.namespaces // .)[] | select(.slug=="default") | .id' | head -1)
[ -n "$NS" ] && [ "$NS" != "null" ] || fail "拿不到默认命名空间：$NS_JSON"
info "namespaceId=$NS"

# ---------- 5. 预案：创建 + 安装 ----------
say "建预案并安装（3 能力 + 2 边 + 1 webhook 触发）"
# manifest 的形状与 mock / docs/design/playbooks.md 一致：
# capabilities[] 每条带 runner（节点侧怎么写执行方式，中枢只存不解释）
PB_MANIFEST='{
  "capabilities": [
    {"name":"demo.prep","version":1,"outputs":["ok"],"runner":{"runner":"bash","command":"echo prep","timeoutSec":20}},
    {"name":"demo.build","version":1,"outputs":["ok"],"runner":{"runner":"bash","command":"echo build","timeoutSec":20}},
    {"name":"demo.report","version":1,"outputs":["ok"],"runner":{"runner":"bash","command":"echo report","timeoutSec":20}}
  ],
  "edges": [
    {"from":"demo.prep","fromPort":"ok","to":"demo.build"},
    {"from":"demo.build","fromPort":"ok","to":"demo.report"}
  ],
  "triggers": [{"kind":"webhook","types":["chain.hook"]}]
}'
PB_BODY=$(jq -nc --arg ns "$NS" --argjson m "$PB_MANIFEST" \
	'{namespaceId:$ns,slug:"chain-demo",name:"链路 demo",description:"prep → build → report",manifest:$m}')
PB_JSON=$(api POST "/v1/playbooks?namespaceId=$NS" "$PB_BODY"); expect 201 "建预案"
PB=$(printf '%s' "$PB_JSON" | jq -r '.playbook.id // empty')
[ -n "$PB" ] || fail "预案没有 id：$PB_JSON"
info "预案 $PB，装之前 status=$(printf '%s' "$PB_JSON" | jq -r '.playbook.status') ready=$(printf '%s' "$PB_JSON" | jq -r '.playbook.ready')/$(printf '%s' "$PB_JSON" | jq -r '.playbook.total')"

INSTALL1=$(api POST "/v1/playbooks/$PB/install?namespaceId=$NS"); expect 200 "安装预案"
GOT=$(printf '%s' "$INSTALL1" | jq -r '"\(.created.capabilities|length)/\(.created.edges|length)/\(.created.sources|length)"')
info "第一次安装 created = $GOT（能力/边/源）"
[ "$GOT" = "3/2/1" ] || fail "安装结果不是 3/2/1：$GOT —— $INSTALL1"

INSTALL2=$(api POST "/v1/playbooks/$PB/install?namespaceId=$NS"); expect 200 "重复安装"
AGAIN=$(printf '%s' "$INSTALL2" | jq -r '[(.created.capabilities // []),(.created.edges // []),(.created.sources // [])] | add | length')
[ "$AGAIN" = "0" ] || fail "安装不幂等：第二次仍创建了 $AGAIN 项 —— $INSTALL2"
info "第二次安装 created 全空（幂等 ✓）"

# ---------- 6. webhook 入口 ----------
say "取安装出来的 webhook 入口"
SRC_JSON=$(api GET "/v1/sources?namespaceId=$NS"); expect 200 "列出源"
HOOK=$(printf '%s' "$SRC_JSON" | jq -r '[.sources[]? | select(.kind=="webhook") | .hookUrl] | first // empty')
[ -n "$HOOK" ] || fail "装完没有 webhook 源：$SRC_JSON"
info "hookUrl=$HOOK"

# ---------- 7. 3 个节点 + 3 个 agent ----------
say "注册 3 个节点并启动 3 个 agent"
mkdir -p "$WORK/out"
# 注意：下面三个 command 用**单引号**传入，$ANOTIFY_PARAM_TITLE 是给 agent 侧
# 的 shell 求值的（脚本里不能先展开，否则参数就失去意义了）。
# shellcheck disable=SC2016
mknodewith() { # mknodewith <name> <capability> <command> <out>
	local name=$1 cap=$2 cmd=$3 out=$4
	local json token
	json=$(api POST "/v1/nodes?namespaceId=$NS" "$(printf '{"name":"%s","instances":1}' "$name")"); expect 201 "注册节点 $name"
	token=$(printf '%s' "$json" | jq -r '.token // empty')
	[ -n "$token" ] || fail "节点 $name 没有 token：$json"
	cat >"$WORK/$name.yaml" <<YAML
name: $name
server: $BASE
token: $token
instances: 1
capabilities:
  - capability: $cap
    version: 1
    run:
      runner: bash
      command: |
        $cmd
      out: $out
      timeoutSec: 20
      concurrency: 1
YAML
	"$WORK/agent" -f "$WORK/$name.yaml" >"$WORK/$name.log" 2>&1 &
	PIDS+=($!)
	info "节点 $name 已启动（能力 $cap，出口 $out）"
}

mknodewith node-1 demo.prep   'echo "prep title=$ANOTIFY_PARAM_TITLE"; echo prep-ok'   ok
mknodewith node-2 demo.build  'echo "build got title=$ANOTIFY_PARAM_TITLE"; echo build-ok' ok
mknodewith node-3 demo.report 'echo "report 收尾"; echo report-ok'                    ok

# 三个 agent 都 online（等数据，不等时间）
online() {
	local n
	n=$(api GET "/v1/nodes?namespaceId=$NS" | jq -r '[.nodes[] | select(.status=="online")] | length')
	[ "${n:-0}" -ge 3 ]
}
wait_until "3 个节点 online" 30 online
info "3 个节点均 online"

# 预案的就绪状态必须随真实数据变化：节点没起来时是 incomplete，全上线后才是 ready。
pb_ready() {
	api GET "/v1/playbooks/$PB?namespaceId=$NS" | jq -e '.playbook.status=="ready"' >/dev/null 2>&1
}
wait_until "预案体检变 ready" 20 pb_ready
PB_VIEW=$(api GET "/v1/playbooks/$PB?namespaceId=$NS")
info "预案 $PB status=$(printf '%s' "$PB_VIEW" | jq -r '.playbook.status') ready=$(printf '%s' "$PB_VIEW" | jq -r '.playbook.ready')/$(printf '%s' "$PB_VIEW" | jq -r '.playbook.total')"

# ---------- 8. 触发 + 断言 ----------
say "触发 webhook，断言 3 跳全成功"
TRIG=$(curl -sS -w $'\n%{http_code}' -X POST -H 'content-type: application/json' \
	-d '{"type":"chain.hook","payload":{"title":"hello"}}' "$BASE$HOOK")
TRIG_CODE=${TRIG##*$'\n'}
BODY=${TRIG%$'\n'*}
[ "$TRIG_CODE" = 200 ] || fail "触发失败：HTTP $TRIG_CODE $BODY"
TRACE=$(printf '%s' "$BODY" | jq -r '.event.traceId // .traceId // empty')
[ -n "$TRACE" ] || fail "触发没返回 traceId：$BODY"
info "traceId=$TRACE"

chain_done() {
	local json
	json=$(api GET "/v1/traces/$TRACE?namespaceId=$NS")
	[ "$(code)" = 200 ] || return 1
	printf '%s' "$json" | jq -e '([.tasks[]? | select(.status=="succeeded")] | length) >= 3' >/dev/null 2>&1
}
wait_until "trace 里 3 个任务都 succeeded" 40 chain_done

SUMMARY=$(api GET "/v1/traces/$TRACE?namespaceId=$NS")
printf '%s' "$SUMMARY" | jq -r '.tasks[] | "  ✓ \(.capability)@\(.version)  status=\(.status)  out=\(.out // "-")  summary=\(.summary // "-")"'

COUNT=$(printf '%s' "$SUMMARY" | jq -r '[.tasks[] | select(.status=="succeeded")] | length')
CAPS=$(printf '%s' "$SUMMARY" | jq -r '[.tasks[].capability] | sort | join(",")')
[ "$COUNT" -ge 3 ] || fail "成功任务数 = $COUNT，期望 3"
[ "$CAPS" = "demo.build,demo.prep,demo.report" ] || fail "走的不是整条链：$CAPS"

printf '\n\033[1;32m✅ 多跳链路跑通：demo.prep → demo.build → demo.report（3 跳全 succeeded）\033[0m\n'
