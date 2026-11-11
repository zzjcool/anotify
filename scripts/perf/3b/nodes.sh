#!/usr/bin/env bash
# #223 测量：#100 个节点的 GET /v1/nodes（2N+1 条 N+1 查询 → 常数条）。
#
#   PORT=8041 bash scripts/perf/3b/nodes.sh /tmp/p3b/bin/anotify-base base
#   PORT=8041 bash scripts/perf/3b/nodes.sh /tmp/p3b/bin/anotify-new  new
#
# 节点用**真实 agent 握手**写入 node_capabilities / node_instances 行（这才是线上
# 那两张表的样子：每节点 1 个能力、2 个槽位），握手完成后立刻 kill 掉 agent ——
# 测量只针对读路径，避免 100 个进程的心跳噪声盖住要量的东西。
# v2（console-declares）：node_capabilities 只从**部署**派生，所以每个节点先部署一次
# 产物（POST /v1/deployments），agent hello 之后中枢补推 → apply → 节点重发 hello。
# 前/后两个二进制都跑同一个脚本、同一组参数，交替执行，数字才有可比性。
set -uo pipefail
. "$(dirname "$0")/lib.sh"

BIN=${1:?用法: nodes.sh <server 二进制> <tag>}
TAG=${2:?缺少 tag}
PORT=${PORT:-8041}
N=${N:-100}
REPS=${REPS:-2}
BASE="http://127.0.0.1:$PORT"
REPORT=$P3B_OUT/nodes-$TAG.txt

p3b_port "$PORT"
p3b_ensure_bins
mkdir -p "$P3B_OUT"
: >"$REPORT"

WORK=$(mktemp -d "${TMPDIR:-/tmp}/p3b-nodes-XXXXXX")
DB=$WORK/a.db

cleanup() {
	[ -n "${SRVPID:-}" ] && kill -9 "$SRVPID" 2>/dev/null
	[ -n "${AGENT_PIDS:-}" ] && kill -9 ${AGENT_PIDS} 2>/dev/null
	wait 2>/dev/null
	rm -rf "$WORK"
}
trap cleanup EXIT

json() { curl -sS -H "Cookie: anotify_session=$SESSION" -H 'content-type: application/json' "$@"; }
capcount() { sqlite3 "$DB" "select count(distinct node_id) from node_capabilities;" 2>/dev/null; }

p3b_say "起 server（$TAG）port=$PORT bin=$BIN"
SRVPID=$(p3b_start_server "$BIN" "$PORT" "$DB" "$WORK/server.log")
p3b_log "server pid=$SRVPID loadavg=$(awk '{print $1" "$2" "$3}' /proc/loadavg)"

p3b_seed "$DB"

p3b_say "建 1 个能力（带 run → 编译成产物，供部署）"
# v2（console-declares）：节点不再在 node.yaml 里声明能力，能力归属**只**由部署派生。
# 而“部署”需要一份产物（`POST /v1/deployments` 没产物就是 404），所以这里必须带 run。
CAP=$(json -X POST "$BASE/v1/capabilities?namespaceId=$NS" \
	-d '{"name":"perf.noop","outputs":["ok"],"paramsSchema":{"type":"object"},"run":{"entrypoint":"run.sh","files":{"run.sh":"#!/bin/bash\necho ok\n"},"timeoutSec":60}}' \
	| jq -r '.capability.id // empty')
[ -n "$CAP" ] || p3b_die "建能力失败"
p3b_log "cap=$CAP"

p3b_say "建 $N 个节点（HTTP，instances=2）+ 逐个部署产物"
mkdir -p "$WORK/yaml"
for i in $(seq 1 "$N"); do
	r=$(json -X POST "$BASE/v1/nodes?namespaceId=$NS" -d "{\"name\":\"n$i\",\"instances\":2}")
	id=$(printf '%s' "$r" | jq -r '.node.id // empty')
	tok=$(printf '%s' "$r" | jq -r '.token // empty')
	[ -n "$id" ] && [ -n "$tok" ] || p3b_die "建节点 n$i 失败：$r"
	# v2 的 node.yaml 只有 bootstrap 四件（name/server/token/instances）：
	# 带 capabilities: 会被 agent 的 KnownFields(true) 当未知字段拒启动（N3）。
	# 配方：scripts/e2e/lib/nodes.mjs:180-260。
	cat >"$WORK/yaml/n$i.yaml" <<EOF
name: n$i
server: $BASE
token: $tok
instances: 2
EOF
	# 能力靠部署落到本机（此刻节点 offline → 行是 pending，hello 之后中枢补推）。
	dep=$(json -X POST "$BASE/v1/deployments?namespaceId=$NS" \
		-d "{\"namespaceId\":\"$NS\",\"capabilityId\":\"$CAP\",\"nodeIds\":[\"$id\"]}")
	[ "$(printf '%s' "$dep" | jq -r '.deployments | length')" = "1" ] || p3b_die "部署能力到 n$i 失败：$dep"
done

p3b_say "分批拉起 agent（hello → 部署补推 → apply → 重发 hello）写真实 node_capabilities / node_instances"
# 节点侧未迁移（#338）：v3 的 cmd/agent 已被 anode 取代，而下面这条命令用的是 v3 专属的
# `-f` / `-grace`（v4 的 anode 是 `anode serve --config …`，配置形状也不同）。
# 显式失败，而不是拉起一只形态不对的进程然后在“节点一直不上线”里挖半天。
p3b_die "nodes.sh 的节点侧仍是 v3 形状（agent -f yaml -grace），节点产品已换成 anode（v4）：见 docs/v4/anode-cli.md / make dev-node"
AGENT_PIDS=""
BATCH=20
for start in $(seq 1 $BATCH "$N"); do
	end=$((start + BATCH - 1)); [ $end -gt $N ] && end=$N
	batch_pids=()
	for i in $(seq $start $end); do
		taskset -c "$P3B_CORES" "$P3B_BIN/agent" -f "$WORK/yaml/n$i.yaml" -grace 1 >"$WORK/yaml/n$i.log" 2>&1 &
		batch_pids+=($!)
	done
	AGENT_PIDS="$AGENT_PIDS ${batch_pids[*]}"
	cur=0
	for _ in $(seq 1 300); do
		cur=$(capcount)
		[ "${cur:-0}" -ge "$end" ] && break
		sleep 0.2
	done
	for p in "${batch_pids[@]}"; do kill -9 "$p" 2>/dev/null; done
	sleep 0.3
	p3b_log "批次 $start-$end：已申报能力的节点数=$cur"
done

rows=$(sqlite3 "$DB" "select (select count(*) from nodes)||'/'||(select count(*) from node_capabilities)||'/'||(select count(*) from node_instances);")
printf '库内行数 nodes/node_capabilities/node_instances = %s\n' "$rows" | tee -a "$REPORT"
sleep 1

p3b_say "参照 /health（判断这台机当前被污染的严重程度）"
taskset -c "$P3B_CORES" "$P3B_BIN/loadgen" -url "$BASE/health" -c 32 -d 3s 2>&1 | tee "$P3B_OUT/nodes-$TAG-health.log" | sed -n '4,5p' | tee -a "$REPORT"

URL="$BASE/v1/nodes?namespaceId=$NS"
COOKIE="Cookie: anotify_session=$SESSION"
for r in $(seq 1 "$REPS"); do
	echo "--- rep $r（loadavg $(awk '{print $1}' /proc/loadavg)）---" >>"$REPORT"
	p3b_say "GET /v1/nodes c=8 d=10s rep$r"
	p3b_load "$URL" "$COOKIE" identity 8 10s "$P3B_OUT/nodes-$TAG-id-r$r.log" | tee -a "$REPORT"
	p3b_load "$URL" "$COOKIE" gzip 8 10s "$P3B_OUT/nodes-$TAG-gz-r$r.log" | tee -a "$REPORT"
done

p3b_say "响应字节数"
plain=$(curl -s -o /dev/null -w '%{size_download}' -H "$COOKIE" -H 'Accept-Encoding: identity' "$URL")
wire=$(curl -s -o /dev/null -w '%{size_download}' -H "$COOKIE" -H 'Accept-Encoding: gzip' "$URL")
printf 'identity 响应 %s B；gzip 线上字节 %s B\n' "$plain" "$wire" | tee -a "$REPORT"

p3b_say "profile：identity 视角 8 并发 20s + 15s CPU profile"
taskset -c "$P3B_CORES" "$P3B_BIN/loadgen" -url "$URL" -header "$COOKIE" \
	-header "Accept-Encoding: identity" -c 8 -d 20s >"$WORK/prof-load.log" 2>&1 &
LG=$!
sleep 2
curl -s -o "$P3B_OUT/nodes-$TAG.pprof" "$BASE/debug/pprof/profile?seconds=15"
kill -9 "$LG" 2>/dev/null
taskset -c "$P3B_CORES" go tool pprof -top -nodecount=40 "$BIN" "$P3B_OUT/nodes-$TAG.pprof" >"$P3B_OUT/nodes-$TAG.pprof-top.txt" 2>&1
taskset -c "$P3B_CORES" go tool pprof -top -cum -nodecount=45 "$BIN" "$P3B_OUT/nodes-$TAG.pprof" >"$P3B_OUT/nodes-$TAG.pprof-cum.txt" 2>&1
{
	echo '--- cum top（每请求路径；占比取同一 15s 窗口的 sample 分布）'
	grep -E "consoleHandler\).nodes$|viewFor|viewsFor|ListNodeCapabilities|ListInstances|ListCapabilitiesByNode|InstanceStatsByNode|_sqlite3Parser|_yy_reduce|prepareV2" \
		"$P3B_OUT/nodes-$TAG.pprof-cum.txt" || true
	echo "（完整 top / cum 见 $P3B_OUT/nodes-$TAG.pprof{,-top,-cum}.txt）"
} | tee -a "$REPORT"

p3b_say "TAG=$TAG 完成"
cat "$REPORT"
