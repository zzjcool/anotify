#!/usr/bin/env bash
# B 路（WS 节点与多跳链路）性能评估 —— 公共库。
#
# 纪律（issue #217 约定，写死在常量里，各场景脚本不得绕开）：
#   - 只绑 4-7 核（taskset），A/C 两路跑 0-3 / 8-11，互不污染
#   - 端口只用 7920-7939
#   - 数据库用 mktemp -d，跑完删
#   - 不改产品代码：这里只调 cmd/* 的二进制与 HTTP API
#
# 用法：场景脚本里 `. "$(dirname "$0")/lib.sh"`，然后 pb_init → 各 pb_* 函数 → pb_cleanup。
set -uo pipefail

PB_CORES=${PB_CORES:-4-7}
# 仓库根：优先问 git（lib.sh 在 scripts/perf/b/，靠相对路径数 .. 很容易数错一层）
PB_ROOT=$(git -C "$(dirname "${BASH_SOURCE[0]}")" rev-parse --show-toplevel 2>/dev/null) \
	|| PB_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)
PB_BIN=${PB_BIN:-/tmp/perfB/bin}
PB_KEEP=${KEEP:-0}

# ---------- 通用小工具 ----------

pb_log() { printf '[%s] %s\n' "$(date +%H:%M:%S)" "$*" >&2; }
pb_say() { printf '\n\033[1;36m▶ %s\033[0m\n' "$*" >&2; }
pb_die() { printf '\n\033[1;31m✗ %s\033[0m\n' "$*" >&2; exit 1; }

# pb_secs：单调时钟秒（浮点，用于脚本内计时）
pb_secs() { date +%s.%N; }

# 浮点差：pb_sub <a> <b> → a-b
pb_sub() { awk -v a="$1" -v b="$2" 'BEGIN{printf "%.6f", a-b}'; }

# pb_check_port：端口必须在 7920-7939（越界直接退出，防止抢端口）
pb_check_port() {
	local p=$1
	if [ "$p" -lt 7920 ] || [ "$p" -gt 7939 ]; then
		pb_die "端口 $p 不在 B 路允许区间 7920-7939"
	fi
}

# ---------- 生命周期 ----------

pb_init() {
	# 注意：PORT 必须由场景脚本用**普通赋值**设好再调 pb_init。
	# 写成 `PORT=7920 pb_init` 会让 PORT 成为函数局部变量，返回后就没了（bash 语义）。
	PORT=${PORT:-7920}
	WORK=$(mktemp -d "${TMPDIR:-/tmp}/perfB-$(basename "$0" .sh)-XXXXXX") || pb_die "mktemp 失败"
	OUT=$WORK/out
	mkdir -p "$OUT" "$PB_BIN"
	PORT=${PORT:-7920}
	pb_check_port "$PORT"
	BASE="http://127.0.0.1:$PORT"
	DB=$WORK/db.sqlite
	PIDS=()
	PBPID=""
	trap pb_cleanup EXIT
	pb_log "work=$WORK port=$PORT cores=$PB_CORES"
}

pb_cleanup() {
	local p
	for p in "${PIDS[@]:-}"; do [ -n "$p" ] && kill -9 "$p" 2>/dev/null; done
	wait 2>/dev/null
	if [ "$PB_KEEP" = "1" ]; then
		printf '\n（临时目录已保留：%s）\n' "$WORK" >&2
	else
		rm -rf "$WORK"
	fi
}

# pb_build：构建四个二进制（有缓存，秒级）。构建也用 taskset，免得更新的 gc 抢别人的核。
pb_build() {
	# 注意：这里只按「二进制存在」判断，换 commit 复现时会静默用旧二进制。
	# 换版本复现请先 `rm -rf $PB_BIN` 或设 PB_REBUILD=1。
	if [ "${PB_REBUILD:-0}" != "1" ] && [ -x "$PB_BIN/anotify" ] && [ -x "$PB_BIN/agent" ] \
		&& [ -x "$PB_BIN/loadgen" ] && [ -x "$PB_BIN/devseed" ]; then return 0; fi
	pb_say "构建 server/agent/loadgen/devseed"
	(
		cd "$PB_ROOT" || exit 1
		export GOPROXY=${GOPROXY:-direct} GOSUMDB=${GOSUMDB:-sum.golang.org} GOTOOLCHAIN=${GOTOOLCHAIN:-auto}
		taskset -c "$PB_CORES" go build -o "$PB_BIN/anotify" ./cmd/server || exit 1
		taskset -c "$PB_CORES" go build -o "$PB_BIN/agent" ./cmd/agent || exit 1
		taskset -c "$PB_CORES" go build -o "$PB_BIN/loadgen" ./scripts/perf/loadgen || exit 1
		taskset -c "$PB_CORES" go build -o "$PB_BIN/devseed" ./cmd/devseed || exit 1
	) || pb_die "构建失败"
}

# ---------- server ----------

pb_start_server() {
	pb_say "起 server（端口 $PORT，pprof 开，库 $DB）"
	taskset -c "$PB_CORES" env \
		ANOTIFY_ADDR=":$PORT" \
		ANOTIFY_DB="$DB" \
		ANOTIFY_STATIC= \
		ANOTIFY_PPROF=1 \
		ANOTIFY_LOG_LEVEL=info \
		ANOTIFY_LOG_FORMAT=json \
		ANOTIFY_RP_ORIGIN="$BASE" \
		"$PB_BIN/anotify" >"$OUT/server.log" 2>&1 &
	PBPID=$!
	PIDS+=("$PBPID")
	pb_wait_until "server /health 200" 20 curl -sf -o /dev/null "$BASE/health"
	pb_log "server pid=$PBPID"
}

# pb_seed：造用户与会话（devseed 不幂等，每次新建 key+session）
pb_seed() {
	pb_say "devseed（用户 + 会话 + API Key）"
	local seed
	seed=$(cd "$PB_ROOT" && taskset -c "$PB_CORES" "$PB_BIN/devseed" -db "$DB" -username perf 2>/dev/null) || pb_die "devseed 失败"
	SESSION=$(printf '%s\n' "$seed" | sed -n 's/^SESSION=//p')
	SEND_KEY=$(printf '%s\n' "$seed" | sed -n 's/^SEND_KEY=//p')
	[ -n "$SESSION" ] || pb_die "devseed 没输出 SESSION：$seed"
	COOKIE="Cookie: anotify_session=$SESSION"
	pb_log "session=${SESSION:0:12}… key=${SEND_KEY:0:12}…"
}

# pb_api METHOD PATH [BODY] → 响应体到 stdout，状态码到 $OUT/.code
pb_api() {
	local m=$1 p=$2 body=${3-}
	if [ -n "$body" ]; then
		curl -sS -o "$OUT/.body" -w '%{http_code}' -X "$m" -H "$COOKIE" \
			-H 'content-type: application/json' -d "$body" "$BASE$p" >"$OUT/.code"
	else
		curl -sS -o "$OUT/.body" -w '%{http_code}' -X "$m" -H "$COOKIE" "$BASE$p" >"$OUT/.code"
	fi
	cat "$OUT/.body"
}
pb_code() { cat "$OUT/.code"; }

pb_ns() {
	NS=$(pb_api GET /v1/namespaces | jq -r '(.namespaces // [])[] | select(.slug=="default") | .id' | head -1)
	[ -n "$NS" ] && [ "$NS" != "null" ] || pb_die "拿不到默认命名空间"
	pb_log "ns=$NS"
}

# pb_cap <name> [outputs-json] → CAP_ID（paramsSchema 留空 = {"type":"object"}，接受任意参数）
pb_cap() {
	local name=$1 outputs=${2:-'["ok"]'} json
	json=$(pb_api POST "/v1/capabilities?namespaceId=$NS" \
		"$(jq -nc --arg n "$name" --argjson o "$outputs" '{name:$n,outputs:$o,paramsSchema:{type:"object"}}')")
	CAP=$(printf '%s' "$json" | jq -r '.capability.id // empty')
	[ -n "$CAP" ] || pb_die "建能力 $name 失败：$json（HTTP $(pb_code)）"
	pb_log "capability $name = $CAP"
}

# pb_source <name> <capId> → SRC_ID / HOOK（无 rate limit 用 fire，大 payload 用 HOOK）
pb_source() {
	local name=$1 cap=$2 json
	json=$(pb_api POST "/v1/sources?namespaceId=$NS" \
		"$(jq -nc --arg n "$name" --arg c "$cap" '{name:$n,kind:"webhook",targets:[$c]}')")
	SRC=$(printf '%s' "$json" | jq -r '.source.id // empty')
	HOOK=$(printf '%s' "$json" | jq -r '.source.hookUrl // empty')
	[ -n "$SRC" ] || pb_die "建源 $name 失败：$json（HTTP $(pb_code)）"
	pb_log "source $name = $SRC hook=$HOOK"
}

# pb_source_edge <sourceName> <capId>：让 POST /v1/events（API Key，无限流）能派到能力
pb_source_edge() {
	local sname=$1 cap=$2 json
	json=$(pb_api POST "/v1/edges?namespaceId=$NS" \
		"$(jq -nc --arg s "$sname" --arg c "$cap" '{fromIsSource:true,fromCapability:$s,fromPort:"perf.event",toCapability:$c}')")
	[ "$(pb_code)" = "201" ] || [ "$(pb_code)" = "200" ] || pb_die "建入口边失败：$(pb_code) $json"
}

# ---------- node + agent ----------

# pb_node <name> <instances> <concurrency> <command> [timeoutSec]
#   → NODE_ID / NODE_TOKEN，写 $OUT/<name>.yaml 并起 agent（日志 $OUT/<name>.log）
pb_node() {
	local name=$1 instances=$2 conc=$3 cmd=$4 tmo=${5:-60} json token yaml
	json=$(pb_api POST "/v1/nodes?namespaceId=$NS" \
		"$(jq -nc --arg n "$name" --argjson i "$instances" '{name:$n,instances:$i}')")
	NODE_ID=$(printf '%s' "$json" | jq -r '.node.id // empty')
	NODE_TOKEN=$(printf '%s' "$json" | jq -r '.token // empty')
	[ -n "$NODE_ID" ] && [ -n "$NODE_TOKEN" ] || pb_die "建节点 $name 失败：$json"

	yaml=$OUT/$name.yaml
	{
		printf 'name: %s\n' "$name"
		printf 'server: %s\n' "$BASE"
		printf 'token: %s\n' "$NODE_TOKEN"
		printf 'instances: %s\n' "$instances"
		printf 'capabilities:\n'
		printf '  - capability: %s\n' "$PB_CAP_NAME"
		printf '    version: 1\n'
		printf '    run:\n'
		printf '      runner: bash\n'
		printf '      command: |\n'
		printf '%s\n' "$cmd" | sed 's/^/        /'
		printf '      out: ok\n'
		printf '      timeoutSec: %s\n' "$tmo"
		printf '      concurrency: %s\n' "$conc"
	} >"$yaml"
	printf '%s' "$yaml" >"$OUT/.lastyaml"

	taskset -c "$PB_CORES" "$PB_BIN/agent" -f "$yaml" -grace 1 >"$OUT/$name.log" 2>&1 &
	local apid=$!
	PIDS+=("$apid")
	eval "AGENT_PID_${name//-/_}=$apid"
	pb_log "node $name（$NODE_ID）instances=$instances concurrency=$conc timeout=${tmo}s agent=$apid"
}

# pb_node_create <name> <instances> <conc> <cmd> [timeoutSec]：只建节点 + 写 yaml，不起 agent
pb_node_create() {
	local name=$1 instances=$2 conc=$3 cmd=$4 tmo=${5:-60} json
	json=$(pb_api POST "/v1/nodes?namespaceId=$NS" \
		"$(jq -nc --arg n "$name" --argjson i "$instances" '{name:$n,instances:$i}')")
	NODE_ID=$(printf '%s' "$json" | jq -r '.node.id // empty')
	NODE_TOKEN=$(printf '%s' "$json" | jq -r '.token // empty')
	[ -n "$NODE_ID" ] && [ -n "$NODE_TOKEN" ] || pb_die "建节点 $name 失败：$json"
	local yaml=$OUT/$name.yaml
	{
		printf 'name: %s\nserver: %s\ntoken: %s\ninstances: %s\n' "$name" "$BASE" "$NODE_TOKEN" "$instances"
		printf 'capabilities:\n  - capability: %s\n    version: 1\n    run:\n' "$PB_CAP_NAME"
		printf '      runner: bash\n      command: |\n'
		printf '%s\n' "$cmd" | sed 's/^/        /'
		printf '      out: ok\n      timeoutSec: %s\n      concurrency: %s\n' "$tmo" "$conc"
	} >"$yaml"
	printf '%s' "$yaml" >"$OUT/.lastyaml"
	pb_log "node $name（$NODE_ID）instances=$instances concurrency=$conc timeout=${tmo}s yaml=$yaml"
}

# pb_node_start <name>：按已写好的 yaml 起 agent（用于「一次性拉起 N 个」的握手测试）
pb_node_start() {
	local name=$1
	taskset -c "$PB_CORES" "$PB_BIN/agent" -f "$OUT/$name.yaml" -grace 1 >"$OUT/$name.log" 2>&1 &
	local apid=$!
	PIDS+=("$apid")
	eval "AGENT_PID_${name//-/_}=$apid"
	echo "$apid"
}

# pb_event <payload-json>：POST /v1/events（API Key，无限流）→ 原样返回响应
pb_event() {
	local payload=${1:-'{"i":1}'}
	curl -sS -H "Authorization: Bearer $SEND_KEY" -H 'content-type: application/json' \
		-d "$(jq -nc --arg ns "$NS" --arg s "$PB_SRC_NAME" --argjson p "$payload" \
			'{namespaceId:$ns,source:$s,type:"perf.event",payload:$p}')" \
		"$BASE/v1/events"
}

# pb_event_timed <payload>：记客户端 t0（浮点秒）→ 追加 "taskid t0" 到 $CLIENT_FILE
pb_event_timed() {
	local payload=${1:-'{"i":1}'} t0 resp task
	t0=$(pb_secs)
	resp=$(pb_event "$payload")
	task=$(printf '%s' "$resp" | jq -r '.tasks[0].id // .dispatch.tasks[0].id // empty')
	[ -n "$task" ] && printf '%s %s\n' "$task" "$t0" >>"$CLIENT_FILE"
	printf '%s' "$task"
}

# pb_finished_count：server.log 里 task finished 的条数
pb_finished_count() { grep -c '"node.ws.result"' "$OUT/server.log" 2>/dev/null || true; }

# pb_fire_many <n> <parallel>：批量触发 n 个任务（P 并发，不记时间）
#
# 路径选择（实测结论，别改回去）：
#   - /v1/hooks/{token}：60/min/IP 限流（internal/server/ratelimit.go:53）→ 会把限流当瓶颈
#   - /v1/events（API Key）：**每次校验 argon2id(m=64MB)**，16 并发把 RSS 抬到 1.4GB、
#     单请求延迟 790ms（本目录实测，见 REPORT）→ 测出来的不是派发成本
#   - /v1/sources/{id}/fire（session cookie）：无限流、无 argon2 → 用它
pb_fire_many() {
	local n=$1 par=${2:-8}
	seq "$n" | xargs -P "$par" -I{} curl -sS -o /dev/null -H "$COOKIE" \
		-H 'content-type: application/json' \
		-d '{"type":"perf.event","payload":{"i":{}}}' \
		"$BASE/v1/sources/$SRC/fire?namespaceId=$NS"
}

# pb_fire_timed_many <n> <parallel>：P 并发触发 n 个任务，每个都记客户端 t0 → $CLIENT_FILE
pb_fire_timed_many() {
	local n=$1 par=${2:-8}
	export PB_BASE="$BASE" PB_NS="$NS" PB_COOKIE="$COOKIE" PB_SRC_ID="$SRC" CLIENT_FILE
	seq "$n" | xargs -P "$par" -I{} bash -c '
		t0=$EPOCHREALTIME
		resp=$(curl -sS -H "$PB_COOKIE" -H "content-type: application/json" \
			-d "{\"type\":\"perf.event\",\"payload\":{\"i\":{}}}" \
			"$PB_BASE/v1/sources/$PB_SRC_ID/fire?namespaceId=$PB_NS")
		task=$(printf "%s" "$resp" | jq -r ".dispatch.tasks[0].id // empty")
		[ -n "$task" ] && printf "%s %s\n" "$task" "$t0" >>"$CLIENT_FILE"
	'
}

# pb_loadavg：当前 1/5/15 分钟负载（三路并行的污染证据，要写进报告）
pb_loadavg() { awk '{print $1" "$2" "$3}' /proc/loadavg; }


pb_node_pid() { local v="AGENT_PID_${1//-/_}"; echo "${!v:-}"; }

pb_online() {
	local n
	n=$(pb_api GET "/v1/nodes?namespaceId=$NS" | jq -r '[.nodes[]|select(.status=="online")]|length')
	echo "${n:-0}"
}

pb_wait_online() {
	local want=$1 secs=${2:-40}
	pb_wait_until "$want 个节点 online" "$secs" pb_online_at_least "$want"
}

pb_online_at_least() {
	local want=$1 n
	n=$(pb_online) || return 1
	[ "$n" -ge "$want" ]
}

# ---------- 触发 ----------

# pb_fire <srcId> <payload-json> → 打印 task ids（一行一个）
# 走 /v1/sources/{id}/fire（session，无限流，body ≤1MiB）
pb_fire() {
	local src=$1 payload=${2:-'{}'}
	pb_api POST "/v1/sources/$src/fire?namespaceId=$NS" \
		"$(jq -nc --argjson p "$payload" '{type:"perf.event",payload:$p}')" |
		jq -r '.dispatch.tasks[]?.id'
}

# pb_fire_timed <srcId> <payload>：边发边记 t0（浮点秒）→ 追加到 $CLIENT_FILE
# 输出 "taskid t0" 每行一条；用于端到端延迟分布（与 server.log 的 task finished 对齐）
pb_fire_timed() {
	local src=$1 payload=${2:-'{}'} t0 resp task
	t0=$(pb_secs)
	resp=$(pb_api POST "/v1/sources/$src/fire?namespaceId=$NS" \
		"$(jq -nc --argjson p "$payload" '{type:"perf.event",payload:$p}')")
	task=$(printf '%s' "$resp" | jq -r '.dispatch.tasks[0].id // empty')
	[ -n "$task" ] && printf '%s %s\n' "$task" "$t0" >>"$CLIENT_FILE"
	printf '%s' "$task"
}

# ---------- 采样 ----------

# pb_gsnap <tag>：goroutine 总数（/debug/pprof/goroutine?debug=1 首行）
pb_gsnap() {
	local tag=$1 n
	n=$(curl -s "$BASE/debug/pprof/goroutine?debug=1" | sed -n '1s/.*total \([0-9]*\).*/\1/p')
	printf '%s %s\n' "$tag" "${n:-0}" >>"$OUT/goroutines.txt"
	echo "${n:-0}"
}

# pb_rss <tag>：server 常驻内存 KB
pb_rss() {
	local tag=$1 kb
	kb=$(awk '/^VmRSS:/{print $2}' "/proc/$PBPID/status" 2>/dev/null)
	printf '%s %s\n' "$tag" "${kb:-0}" >>"$OUT/rss.txt"
	echo "${kb:-0}"
}

# pb_cputime：server 进程累计 CPU 秒（utime+stime，clock ticks → 秒）
pb_cputime() {
	local t
	t=$(awk '{print $14+$15}' "/proc/$PBPID/stat" 2>/dev/null || echo 0)
	awk -v t="$t" 'BEGIN{printf "%.2f", t/100}'
}

# pb_cpu_over <秒> <tag>：在窗口内测 server 的 CPU 占用（%），用于「空闲期成本」
pb_cpu_over() {
	local secs=$1 tag=$2 a b
	a=$(pb_cputime)
	sleep "$secs"
	b=$(pb_cputime)
	printf '%s window=%ss cpu=%.3fs pct=%.2f%%\n' "$tag" "$secs" "$(pb_sub "$b" "$a")" \
		"$(awk -v a="$a" -v b="$b" -v s="$secs" 'BEGIN{printf "%.2f", (b-a)/s*100}')" | tee -a "$OUT/cpu.txt"
}

# pb_pprof <seconds> <tag>：采集 CPU profile 并打印 top
pb_pprof() {
	local secs=$1 tag=$2
	pb_log "采 CPU profile ${secs}s → $tag"
	curl -s -o "$OUT/$tag.pprof" "$BASE/debug/pprof/profile?seconds=$secs"
	{
		echo "### CPU profile -top -nodecount=25（$tag，${secs}s）"
		taskset -c "$PB_CORES" go tool pprof -top -nodecount=25 "$OUT/$tag.pprof" 2>&1
	} >"$OUT/$tag.top.txt"
	cat "$OUT/$tag.top.txt"
}

# pb_goroutine_dump <tag>：完整 goroutine 栈 + 按函数聚合（查泄漏）
pb_goroutine_dump() {
	local tag=$1
	curl -s "$BASE/debug/pprof/goroutine?debug=1" >"$OUT/$tag.goroutines.txt"
	{
		echo "### goroutine aggregate（$tag）"
		grep -E '^[0-9]+ @' "$OUT/$tag.goroutines.txt" | sort -k3 | head -25
	} | tee "$OUT/$tag.goroutines.agg.txt"
}

# ---------- 断言/等待 ----------

pb_wait_until() {
	local desc=$1 secs=$2; shift 2
	local deadline=$((SECONDS + secs))
	while [ $SECONDS -lt $deadline ]; do
		if "$@" >/dev/null 2>&1; then return 0; fi
		sleep 0.1
	done
	pb_die "等不到：$desc（超过 ${secs}s）"
}

# pb_tasks_count <status> → 条数（带 "≥" 表示打满了服务端上限）
#
# 服务端上限有两层：handler 的 listLimitCap(500)，以及 store.ListTasks 里
# `limit > 500 → limit = 50` 的兜底（internal/store/task.go:301-303）。
# 所以**只能要 500**；要 1000 会掉回 50，看起来像「任务凭空少了」。
pb_tasks_count() {
	local st=$1 n
	n=$(pb_api GET "/v1/tasks?namespaceId=$NS&status=$st&limit=500" | jq -r '(.tasks // []) | length')
	if [ "${n:-0}" -ge 500 ]; then echo ">=500"; else echo "${n:-0}"; fi
}

# pb_tasks_dist → "assigned=1 pending=99 ..."（一次拉 1000 条按状态分组）
pb_tasks_dist() {
	pb_api GET "/v1/tasks?namespaceId=$NS&limit=500" \
		| jq -r '[.tasks[].status] | group_by(.) | map("\(.[0])=\(length)") | join(" ")'
}

# ---------- 延迟汇总（python3，读 client 文件 + server 日志） ----------

# pb_latency <client-file> <marker-file> <label>
#   client-file: "taskid t0"（客户端 fire 时刻，浮点秒）
#   marker-file: "taskid start EPOCHREALTIME"（agent 侧命令实际开始执行的时刻）
#   server.log  : JSON 日志，取 event=node.ws.result 的 time 与 task_id
pb_latency() {
	local client=$1 marker=$2 label=$3
	python3 - "$client" "$marker" "$OUT/server.log" "$label" <<'PY'
import json, sys, datetime, statistics
client_f, marker_f, log_f, label = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4]
def load_pairs(path):
    d = {}
    try:
        for line in open(path, encoding='utf-8', errors='replace'):
            p = line.split()
            if len(p) >= 2:
                try: d.setdefault(p[0], []).append(float(p[1]))
                except ValueError: pass
    except FileNotFoundError: pass
    return d
fires = load_pairs(client_f)          # taskid -> [t0]
starts = load_pairs(marker_f)         # taskid -> [epochrealtime]（可能多条 = 重复执行）
fin = {}
for line in open(log_f, encoding='utf-8', errors='replace'):
    if '"node.ws.result"' not in line: continue
    try: o = json.loads(line)
    except Exception: continue
    if o.get('event') != 'node.ws.result': continue
    ts = o.get('time','')
    try:
        t = datetime.datetime.fromisoformat(ts.replace('Z','+00:00')).timestamp()
    except Exception: continue
    fin.setdefault(o.get('task_id'), []).append(t)
def pct(v, q):
    if not v: return float('nan')
    v = sorted(v); i = min(len(v)-1, int(round(q*(len(v)-1))))
    return v[i]
def report(name, vals):
    if not vals:
        print(f'{name}: 无样本'); return
    vals_ms = [x*1000 for x in vals]
    print(f'{name}: n={len(vals_ms)} p50={pct(vals_ms,.5):.1f}ms p90={pct(vals_ms,.9):.1f}ms '
          f'p99={pct(vals_ms,.99):.1f}ms max={max(vals_ms):.1f}ms mean={statistics.mean(vals_ms):.1f}ms')
print(f'== {label} ==')
print(f'fire 记录 {len(fires)} 个任务，agent 侧执行 {sum(len(v) for v in starts.values())} 次，'
      f'服务端 finished {sum(len(v) for v in fin.values())} 次')
dup = {k: len(v) for k, v in starts.items() if len(v) > 1}
if dup: print(f'!! 重复执行的任务 {len(dup)} 个: {dict(list(dup.items())[:5])}')
e2e = [fin[t][0]-t0 for t, t0list in fires.items() for t0 in t0list[:1] if t in fin]
assign = [starts[t][0]-t0 for t, t0list in fires.items() for t0 in t0list[:1] if t in starts]
exec_ = [(fin[t][0]-starts[t][0]) for t in fires if t in fin and t in starts]
report('端到端 fire→finished', e2e)
report('派发 fire→开始执行', assign)
report('执行+回报 开始→finished', exec_)
PY
}

# pb_pypercent <label> <秒列表...> 之类的小工具直接用 awk，不再单开
