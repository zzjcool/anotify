#!/usr/bin/env bash
# 多跳链路 demo（issue #186；v2 console-declares 形状见 N3）
#
# 一条命令验证主流程：
#
#   POST /v1/hooks/<token> → 事件 → 派发 → demo.prep → demo.build → demo.report
#                                             (node-1)     (node-2)      (node-3)
#
# 为什么要有这个脚本：单测和 e2e 都在测"服务端内部"，
# 而这条链路把**外部契约**串起来——节点用令牌连 WS、中枢只派发不含命令的 assign 帧、
# 节点执行产物里的脚本、脚本用 `emit` 把产出交给下一跳。
#
# v2（console-declares，契约 D4）之后，「跑什么」不再由节点自报：
#
#   能力定义（POST /v1/capabilities，带 run）→ 编译出一份**产物**（内容寻址）
#     → 部署到某台机器（POST /v1/deployments，deploy 帧）
#       → 节点落盘后回 apply 并**重发 hello** → 中枢重建 node_capabilities
#         → 派发才看得见这台机器
#
# 所以本脚本比 v1 多两步：建能力时**带上脚本**、起 agent 前**部署产物**。
# node.yaml 只剩 bootstrap 四件（name/server/token/instances）——v1 的
# `capabilities:` 段现在是未知字段，agent 启动即拒。
# 配方来源：scripts/e2e/suites/live_agent.mjs（真 agent + 真部署）。
#
# 另一个 v2 变化：路由只认 `emit` 帧（result 里的 `out` 已退役），所以脚本要用
# `"${ANOTIFY_AGENT_BIN:-anotify-agent}" emit --to <下游能力>` 把产出交出去。
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
	# agent 日志是排查"连不上 / 握手被拒 / 部署失败"的第一现场，失败时一并打出来
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
	[ "$got" = "$1" ] || fail "$2：期望 HTTP $1，实际 ${got:-空} —— $(head -c 400 "$WORK/.body" 2>/dev/null)"
}

# ---------- 4. 命名空间 ----------
say "取默认命名空间"
NS_JSON=$(api GET /v1/namespaces); expect 200 "GET /v1/namespaces"
NS=$(printf '%s' "$NS_JSON" | jq -r '(.namespaces // .)[] | select(.slug=="default") | .id' | head -1)
[ -n "$NS" ] && [ "$NS" != "null" ] || fail "拿不到默认命名空间：$NS_JSON"
info "namespaceId=$NS"

# ---------- 5. 能力（定义 + 产物）+ 预案安装 ----------
say "建 3 个能力（定义 + 产物）并安装预案"

# 三个能力的脚本。要点：
#   ① 产物入口必须 `#!` 开头（执行方式是直接 exec，不经 shell）；
#   ② 出口只有一个，下游用 `emit --to <能力名>` 显式钉住（v2 路由只看 emit）；
#   ③ 参数原样传下去：ANOTIFY_PARAMS 就是本次 params 的 JSON 对象，
#      直接当下一跳的 data（`emit` 的 data 直接就是下游的 params）。
# shellcheck disable=SC2016  # 这里的 $ANOTIFY_* 是给**节点侧**脚本展开的
SCRIPT_PREP='#!/bin/bash
set -euo pipefail
echo "prep title=${ANOTIFY_PARAM_TITLE:-}"
data=${ANOTIFY_PARAMS:-}
[ -n "$data" ] || data="{}"
"${ANOTIFY_AGENT_BIN:-anotify-agent}" emit --to demo.build --data "$data"'

# shellcheck disable=SC2016
SCRIPT_BUILD='#!/bin/bash
set -euo pipefail
echo "build got title=${ANOTIFY_PARAM_TITLE:-}"
data=${ANOTIFY_PARAMS:-}
[ -n "$data" ] || data="{}"
"${ANOTIFY_AGENT_BIN:-anotify-agent}" emit --to demo.report --data "$data"'

# shellcheck disable=SC2016
SCRIPT_REPORT='#!/bin/bash
set -euo pipefail
echo "report 收尾（title=${ANOTIFY_PARAM_TITLE:-}）"'

# mkcap <name> <script> → 打印能力 id（响应原文留在 $WORK/.body）。
# v2：有 run 才会编译出产物；没有产物的能力**部署不了**（POST /v1/deployments 404），
# 也就没有节点会声明它 —— 任务永远派不出去。
mkcap() {
	local name=$1 body=$2
	api POST "/v1/capabilities?namespaceId=$NS" "$(jq -nc \
		--arg n "$name" --arg s "$body" \
		'{name:$n,version:1,outputs:["ok"],paramsSchema:{type:"object"},
		  run:{entrypoint:"run.sh",files:{"run.sh":$s},timeoutSec:20}}')" >/dev/null
	expect 200 "建能力 $name"
	jq -r '.capability.id // empty' "$WORK/.body"
}

CAP_PREP=$(mkcap demo.prep "$SCRIPT_PREP")
info "demo.prep@1   = $CAP_PREP   digest=$(jq -r '.digest // "-"' "$WORK/.body")"
CAP_BUILD=$(mkcap demo.build "$SCRIPT_BUILD")
info "demo.build@1  = $CAP_BUILD  digest=$(jq -r '.digest // "-"' "$WORK/.body")"
CAP_REPORT=$(mkcap demo.report "$SCRIPT_REPORT")
info "demo.report@1 = $CAP_REPORT digest=$(jq -r '.digest // "-"' "$WORK/.body")"
for c in "$CAP_PREP" "$CAP_BUILD" "$CAP_REPORT"; do
	[ -n "$c" ] || fail "有能力的 id 是空的：$c"
done

# manifest 只声明**图**（能力 / 边 / 触发），不再带 runner：
# v2 里执行方式属于产物（上面 mkcap 建的），manifest 的 runner 中枢只透传、永不执行。
PB_MANIFEST='{
  "capabilities": [
    {"name":"demo.prep","version":1,"outputs":["ok"]},
    {"name":"demo.build","version":1,"outputs":["ok"]},
    {"name":"demo.report","version":1,"outputs":["ok"]}
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
# 能力上一步已经建好（带产物）→ 安装只补边和源，能力全部 skipped。
# 这是 v2 的正常形状：安装不会凭空造出「怎么跑」，那只能来自产物。
GOT=$(printf '%s' "$INSTALL1" | jq -r '"\(.created.capabilities|length)/\(.created.edges|length)/\(.created.sources|length)"')
SKIPPED_CAPS=$(printf '%s' "$INSTALL1" | jq -r '(.skipped.capabilities // []) | length')
info "第一次安装 created = $GOT（能力/边/源），skipped.capabilities = $SKIPPED_CAPS"
[ "$GOT" = "0/2/1" ] || fail "安装结果不是 0/2/1：$GOT —— $INSTALL1"
[ "$SKIPPED_CAPS" = "3" ] || fail "应跳过 3 个已建能力（mkcap 建的），实际 $SKIPPED_CAPS —— $INSTALL1"

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
say "注册 3 个节点、各部署一份产物并启动 3 个 agent"
mkdir -p "$WORK/out"
# v2：node.yaml 只有 bootstrap 四件；能力靠**部署**下发（v1 的 capabilities: 段是
# 未知字段，agent 的 KnownFields(true) 会让它启动即退）。
mknodewith() { # mknodewith <name> <capabilityId>
	local name=$1 cap=$2 json token node
	json=$(api POST "/v1/nodes?namespaceId=$NS" "$(printf '{"name":"%s","instances":1}' "$name")"); expect 201 "注册节点 $name"
	node=$(printf '%s' "$json" | jq -r '.node.id // empty')
	token=$(printf '%s' "$json" | jq -r '.token // empty')
	[ -n "$node" ] && [ -n "$token" ] || fail "节点 $name 没有 id/token：$json"
	cat >"$WORK/$name.yaml" <<YAML
name: $name
server: $BASE
token: $token
instances: 1
YAML
	# 此刻节点还离线 → 部署行是 pending，agent hello 之后中枢立刻补推
	# （internal/ws/node_handler.go: resendDeployments）。pushed=false 不是失败。
	api POST "/v1/deployments?namespaceId=$NS" \
		"$(jq -nc --arg c "$cap" --arg n "$node" '{capabilityId:$c,nodeIds:[$n]}')" >/dev/null
	expect 200 "部署产物到节点 $name"
	info "节点 $name（$node）已部署能力 $cap，pushed=$(jq -r '.deployments[0].pushed' "$WORK/.body")"
	# -data-dir 每台独立：默认 ~/.anotify 会留下上一轮跑剩的产物，干扰对账。
	"$WORK/agent" -f "$WORK/$name.yaml" -data-dir "$WORK/data-$name" -grace 1 \
		>"$WORK/$name.log" 2>&1 &
	PIDS+=($!)
}

mknodewith node-1 "$CAP_PREP"
mknodewith node-2 "$CAP_BUILD"
mknodewith node-3 "$CAP_REPORT"

# 三个 agent 都 online（等数据，不等时间）
online() {
	local n
	n=$(api GET "/v1/nodes?namespaceId=$NS" | jq -r '[.nodes[] | select(.status=="online")] | length')
	[ "${n:-0}" -ge 3 ]
}
wait_until "3 个节点 online" 30 online
info "3 个节点均 online"

# online 只说明 WS 握手成功；节点能接单靠的是 node_capabilities，而那张表只由部署派生
# （节点 apply 完产物会**重发 hello**，中枢据此重建）。少等这一步，派发会被剪成 no_node。
caps_declared() {
	local json
	json=$(api GET "/v1/capabilities?namespaceId=$NS")
	printf '%s' "$json" | jq -e --arg a "$CAP_PREP" --arg b "$CAP_BUILD" --arg c "$CAP_REPORT" '
		([.capabilities[]? | select(.id==$a or .id==$b or .id==$c)
		  | .declaredBy[]? | select(.status=="online")] | length) >= 3' >/dev/null 2>&1
}
wait_until "3 个能力都被在线节点声明（部署生效）" 40 caps_declared
info "3 个能力均已由在线节点声明（部署 applied ✓）"

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
# out 已退役（v2 路由只看 emit），所以只打 status / summary。
printf '%s' "$SUMMARY" | jq -r '.tasks[] | "  ✓ \(.capability)@\(.version)  status=\(.status)  summary=\(.summary // "-")"'

COUNT=$(printf '%s' "$SUMMARY" | jq -r '[.tasks[] | select(.status=="succeeded")] | length')
CAPS=$(printf '%s' "$SUMMARY" | jq -r '[.tasks[].capability] | sort | join(",")')
[ "$COUNT" -ge 3 ] || fail "成功任务数 = $COUNT，期望 3"
[ "$CAPS" = "demo.build,demo.prep,demo.report" ] || fail "走的不是整条链：$CAPS"

printf '\n\033[1;32m✅ 多跳链路跑通：demo.prep → demo.build → demo.report（3 跳全 succeeded）\033[0m\n'
