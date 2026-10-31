#!/usr/bin/env bash
# C1 · 持续 10 分钟写入：RSS / goroutine / fd / DB / WAL 五条资源曲线。
#
# 量什么：在**同一批核**（taskset -c 8-11）上，用 Bearer 机器入口
#   POST /v1/events（无限速）持续压 10 分钟，同时每 30s 采一行资源快照。
#   另开一条 ≤30/min 的 webhook 支路（POST /v1/hooks/{token}），保证
#   「webhook 投递 → 派发 → 任务落库 → 分配 → 节点回报」这一环始终在链路上，
#   而不是只往 events 表里灌行。
#
# 为什么不用 hook 压满：`POST /v1/hooks/{token}` 有 60/min/IP 硬限速
#   （internal/server/ratelimit.go: rlHookPerMin）。压它只能量到 429。
#
# 为什么必须有常驻节点（scripts/perf/c/agent.mjs）：光造事件只会走到
#   pruned/no_node，tasks 表一行不涨，量出来的不是同一条写路径。
#
# 用法：
#   bash scripts/perf/c/s1-soak.sh              # 默认 600s 主压
#   S1_DURATION=60 bash scripts/perf/c/s1-soak.sh  # 冒烟（数字不能用于结论）
#
# 产物（全部落 $S1_DIR/raw/，10 分钟跑崩也保留已采到的部分）：
#   samples.csv          每 30s 一行：label,elapsed_s,rss_kb,goroutines,fds,db_bytes,wal_bytes
#   extra.csv            同一时刻的旁证：events/tasks 行数、/health time_total、压测进程存活、hook 200/429
#   ingest.log           主压分段汇总（loadgen 原始输出逐段追加）
#   ingest_<a>-<b>.log   0-2min / 2-8min / 8-10min 三段各自的分位数字
#   hook.log             支路每一投的 (elapsed,http_code)
#   agent.log            常驻节点的 ready / tick（acked,resulted）
#   goroutine_<t>.txt    pprof goroutine?debug=1 原文（total 行 + 栈）
#   *.pprof              heap / goroutine / mutex / cpu profile（从 $PC_RUN 拷回）
#   server.log           服务端日志（pc_start 写的，拷一份）
#
# 纪律（契约 §1）：所有压测/采样进程都在 PC_CPUS（默认 8-11）上；端口 7940
#   首选、被占则顺延到保留重试位 7943；库只用 pc_start 建的 $PC_DB。
# 本脚本只读产品代码，不改任何 internal/** / web-src/**。
set -uo pipefail   # 故意不加 -e：采样/压测里个别命令失败不该让整场数据丢掉

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
S1_DIR="${S1_DIR:-/tmp/perf-c/s1}"
RAW="$S1_DIR/raw"
PROF="$RAW/profiles"

# 独立可跑：PC_ROOT 指到本场景目录，和其他 C 路成员（s2/s3…）的 bin/runs 互不干扰。
export PC_ROOT="$S1_DIR"
# shellcheck source=./common.sh
source "$HERE/common.sh"

DURATION="${S1_DURATION:-600}"      # 主压总时长
SEG_A="${S1_SEG_A:-120}"            # 0-2min 段
SEG_C="${S1_SEG_C:-120}"            # 最后 2min 段（即 8-10min）
SEG_B=$((DURATION - SEG_A - SEG_C)) # 中间段
HOOK_EVERY="${S1_HOOK_EVERY:-2}"    # 支路间隔 → 600s/2s = 300 投 = 30/min（< 60/min 限速）
SAMPLE_EVERY="${S1_SAMPLE_EVERY:-30}"
EVENT_TYPE="${S1_EVENT_TYPE:-go}"   # 事件 type = origin 能力的出口名（fromPort）
CPU_PROFILE_AT="${S1_CPU_PROFILE_AT:-300}"  # CPU profile 抓取起点（相对开压）

LOGF="$S1_DIR/run.log"
mkdir -p "$RAW" "$PROF"
: >"$LOGF"

log() { printf '[%s +%ss] %s\n' "$(date +%H:%M:%S)" "$(elapsed_s)" "$*" | tee -a "$LOGF"; }
elapsed_s() { if [ -n "${T0:-}" ]; then echo $(( $(date +%s) - T0 )); else echo 0; fi; }
die() { log "FATAL: $*"; exit 1; }

# s1_sql <sql>：只读查库（服务在跑，WAL 模式下只读连接是安全的）。
s1_sql() {
	sqlite3 -readonly -noheader "$PC_DB" "$1" 2>/dev/null || sqlite3 -noheader "$PC_DB" "$1" 2>/dev/null
}
s1_count() { s1_sql "select count(*) from $1" | tr -d ' '; }
# s1_health：/health 的状态码与 time_total（服务是否还活着 + 延迟是否劣化）。
s1_health() { curl -s -o /dev/null --max-time 5 -w '%{http_code}/%{time_total}' "$PC_BASE/health" 2>/dev/null; }

# ---------------------------------------------------------------------------
# 收尾：任何退出路径都要收掉压测/采样/节点/服务，避免残留进程抢核抢端口。
# ---------------------------------------------------------------------------
CLEANED=0
cleanup() {
	[ "$CLEANED" = 1 ] && return 0
	CLEANED=1
	rm -f "$S1_DIR/.running"
	local p
	for p in ${LG_PID:-} ${SAMPLER_PID:-} ${HOOK_PID:-} ${PROF_PID:-} ${AGENT_PID:-}; do
		kill "$p" 2>/dev/null
	done
	sleep 0.3
	for p in ${LG_PID:-} ${SAMPLER_PID:-} ${HOOK_PID:-} ${PROF_PID:-} ${AGENT_PID:-}; do
		kill -9 "$p" 2>/dev/null
	done
	[ -f "${PC_RUN:-}/server.log" ] && cp "$PC_RUN/server.log" "$RAW/server.log" 2>/dev/null
	pc_stop 2>/dev/null
}
trap cleanup EXIT INT TERM

# ---------------------------------------------------------------------------
# 0. 环境自检 + 构建（带 embeddist，静态资源场景同款二进制）
# ---------------------------------------------------------------------------
pc_require_env || die "pc_require_env 失败（taskset/curl/jq/分核/端口段）"
pc_build || die "pc_build 失败（见 $PC_ROOT/logs/fe.log）"

# ---------------------------------------------------------------------------
# 1. 起服务：7940 首选；被别人的服务占着就顺延到契约保留的 7943
# ---------------------------------------------------------------------------
PORT_OK=""
for p in 7940 7943; do
	if pc_start soak "$p" >/dev/null 2>&1; then PORT_OK="$p"; break; fi
	log "端口 $p 起不来（多半被别的实例占着），按契约顺延"
done
[ -n "$PORT_OK" ] || die "7940/7943 都起不来；最后 20 行日志见上面 stderr 或 $PC_ROOT/runs/soak/server.log"
log "服务已就绪 base=$PC_BASE db=$PC_DB pid=$PC_PID"

T0=$(date +%s)
touch "$S1_DIR/.running"

# ---------------------------------------------------------------------------
# 2. 播种 → 建能力 → 建边
#
# 计划里给的边体是 {namespaceId, fromCapability:"perf.echo", fromPort:"go",
# toCapability:"perf.sink"}，直接发会被 store 拒两次（踩过的坑，这里显式处理）：
#   a) toCapability 必须是**能力 id**：内部按 id 查表（store.GetCapabilityByID），
#      填名字就是曾出现的 `edge_rejected: to capability "perf.sink": store: record not found`。
#   b) fromPort 必须 ∈ 上游能力的 outputs（不变量 I2）。pc_cap 的签名是
#      (name, schema)，不给 outputs，所以 perf.echo 默认 outputs=[]，端口 "go" 同样被拒。
# 还有第三条是**语义**层面的：POST /v1/events 的首轮派发固定用「入口边」
#   （route.DispatchInput{FromCap: <source 名>, FromIsSource: true, Out: <type>}，
#   见 internal/trigger/trigger.go），所以真正让 tasks 增长的是
#   fromIsSource=true、fromCapability="perf.echo"（来源名）、fromPort=<事件 type> 的那条边。
# 因此这里建两条边：E1 入口边（驱动主压链路）+ E2 计划原文那条能力边（fromPort "go"，
# 需要 perf.echo 有 outputs=["go"]，用同 name 新版本补上）。两条都断言 HTTP 200。
# ---------------------------------------------------------------------------
pc_seed || die "pc_seed 失败（devseed 或 /v1/namespaces）"
log "seed 完成 ns=$PC_NS uid=$PC_UID"

# s1_cap <name> <outputs-json> [schema]：pc_cap 的超集（多一个 outputs，不动 common.sh）。
s1_cap() {
	local name="$1" outputs="${2:-[]}" schema="${3:-{\"type\":\"object\"}}" body
	body=$(curl -s -X POST "$PC_BASE/v1/capabilities" \
		-H "Cookie: anotify_session=$PC_SESSION" -H 'content-type: application/json' \
		-d "{\"namespaceId\":\"$PC_NS\",\"name\":\"$name\",\"outputs\":$outputs,\"paramsSchema\":$schema}")
	printf '%s' "$body"
}

ECHO_ID=$(pc_cap perf.echo)
SINK_ID=$(pc_cap perf.sink)
[ -n "$ECHO_ID" ] || die "建能力 perf.echo 失败（pc_cap 返回空 id）"
[ -n "$SINK_ID" ] || die "建能力 perf.sink 失败（pc_cap 返回空 id）"
log "能力就绪 perf.echo=$ECHO_ID perf.sink=$SINK_ID"

# pc_cap 不给 outputs → 若 "go" 不在 perf.echo 的 outputs 里，补一个新版本（append-only，同 name 自动开 v2）。
ECHO_OUT=$(curl -s -H "Cookie: anotify_session=$PC_SESSION" "$PC_BASE/v1/capabilities?namespaceId=$PC_NS" \
	| jq -c --arg id "$ECHO_ID" '(.capabilities // [])[] | select(.id==$id) | .outputs' 2>/dev/null)
log "perf.echo 当前 outputs=$ECHO_OUT"
if ! printf '%s' "$ECHO_OUT" | grep -q "\"$EVENT_TYPE\""; then
	NEW=$(s1_cap perf.echo "[\"$EVENT_TYPE\"]")
	LED=$(printf '%s' "$NEW" | jq -r '.capability.id // empty')
	[ -n "$LED" ] || { log "补 outputs 失败，响应体："; printf '%s' "$NEW" | jq . | tee -a "$LOGF"; die "perf.echo 新版本创建失败"; }
	log "perf.echo 补 outputs=[\"$EVENT_TYPE\"] → 新版本 id=$LED"
	ECHO_ID_V2="$LED"
else
	ECHO_ID_V2="$ECHO_ID"
fi

# s1_post_edge <body>：建边；非 200 一律把响应体打出来（jq 美化）并失败。
s1_post_edge() {
	local req="$1" resp code payload_out
	resp=$(curl -s -w '\n%{http_code}' -X POST "$PC_BASE/v1/edges" \
		-H "Cookie: anotify_session=$PC_SESSION" -H 'content-type: application/json' -d "$req")
	code=$(printf '%s' "$resp" | tail -1)
	payload_out=$(printf '%s' "$resp" | sed '$d')
	if [ "$code" != "200" ]; then
		log "边建失败 HTTP $code"
		printf '请求体: %s\n响应体:\n' "$req" | tee -a "$LOGF"
		{ printf '%s' "$payload_out" | jq . 2>/dev/null || printf '%s\n' "$payload_out"; } | tee -a "$LOGF"
		return 1
	fi
	log "边已建 $(printf '%s' "$payload_out" | jq -c '.edge' 2>/dev/null)"
	printf '%s' "$payload_out"
}

# E1：入口边（/v1/events 首轮派发真正查的就是这条）
s1_post_edge "$(jq -nc --arg ns "$PC_NS" --arg port "$EVENT_TYPE" --arg to "$SINK_ID" \
	'{namespaceId:$ns, fromIsSource:true, fromCapability:"perf.echo", fromPort:$port, toCapability:$to}')" \
	|| die "入口边（fromIsSource=true perf.echo:$EVENT_TYPE → perf.sink）建立失败——链路不通，不压"

# E2：计划原文那条能力边（fromCapability=上游能力 id、fromPort ∈ outputs）
s1_post_edge "$(jq -nc --arg ns "$PC_NS" --arg from "$ECHO_ID_V2" --arg port "$EVENT_TYPE" --arg to "$SINK_ID" \
	'{namespaceId:$ns, fromCapability:$from, fromPort:$port, toCapability:$to}')" \
	|| die "能力边（perf.echo@$ECHO_ID_V2:$EVENT_TYPE → perf.sink）建立失败"

# 支路：webhook 源 → 直接指向 sink（绕开边，保证 hook 这条链一定通）
HOOK_TOKEN=$(pc_source perf-soak-hook "$SINK_ID")
[ -n "$HOOK_TOKEN" ] || die "建 webhook 源失败（pc_source 返回空 token）"
log "webhook 源就绪 token=${HOOK_TOKEN:0:8}…"

# 主压请求体：一次写好复用（loadgen 每次读同一个文件）
BODY_JSON="$RAW/body.json"
jq -nc --arg ns "$PC_NS" --arg src "perf.echo" --arg t "$EVENT_TYPE" \
	'{namespaceId:$ns, source:$src, type:$t, payload:{n:1, msg:"soak", at:"perf-c-s1"}}' >"$BODY_JSON"
log "请求体 $(wc -c <"$BODY_JSON") 字节：$(cat "$BODY_JSON")"

# ---------------------------------------------------------------------------
# 3. 常驻节点 + 链路自检（tasks 表必须先真的涨，否则不压）
# ---------------------------------------------------------------------------
node "$PC_REPO/scripts/perf/c/agent.mjs" --base "$PC_BASE" --db "$PC_DB" --ns "$PC_NS" \
	--cap-id "$SINK_ID" --cap-name perf.sink --slots 8 --tick-ms 30000 >"$RAW/agent.log" 2>&1 &
AGENT_PID=$!
for _ in $(seq 1 50); do grep -q '"ev":"ready"' "$RAW/agent.log" 2>/dev/null && break; sleep 0.2; done
grep -q '"ev":"ready"' "$RAW/agent.log" || { tail -20 "$RAW/agent.log" | tee -a "$LOGF"; die "常驻节点没 ready"; }
log "节点就绪 $(head -1 "$RAW/agent.log")"

TASKS_BEFORE=$(s1_count tasks)
ONE=$(curl -s -X POST "$PC_BASE/v1/events" -H "Authorization: Bearer $PC_SEND_KEY" \
	-H 'content-type: application/json' -d @"$BODY_JSON")
log "自检事件响应 $(printf '%s' "$ONE" | jq -c '{inserted, tasks:(.tasks|length), pruned:(.pruned//[]|map(.reason))}' 2>/dev/null || printf '%s' "$ONE")"
sleep 1
TASKS_AFTER=$(s1_count tasks)
if [ "${TASKS_AFTER:-0}" -le "${TASKS_BEFORE:-0}" ]; then
	log "tasks before=$TASKS_BEFORE after=$TASKS_AFTER —— 链路没通，先修通再压"
	printf '%s' "$ONE" | jq . 2>/dev/null | tee -a "$LOGF"
	tail -20 "$PC_RUN/server.log" | tee -a "$LOGF"
	tail -20 "$RAW/agent.log" | tee -a "$LOGF"
	die "派发链路未打通（tasks 表没增长）"
fi
log "链路自检通过：tasks $TASKS_BEFORE → $TASKS_AFTER（+$((TASKS_AFTER - TASKS_BEFORE))）"

# ---------------------------------------------------------------------------
# 4. t≈0 的基线 profile + argon2 单请求复现
# ---------------------------------------------------------------------------
GPROF_T0=$(pc_pprof soak heap)
curl -s "$PC_BASE/debug/pprof/goroutine?debug=1" >"$RAW/goroutine_t0.txt"
pc_pprof soak goroutine >/dev/null
pc_pprof soak mutex >/dev/null
log "t=0 profile 就绪 $(basename "$GPROF_T0") / goroutine_t0.txt"

# argon2id 硬事实复现（#217）：每个带 Bearer 的请求都跑一次 argon2id，单次 64MB。
# 三点量测：heap0 → 1 次 Bearer 请求 → heap1 → 再 3 次 → heap4，用 alloc_space
# （累计分配量）看 argon2.initBlocks 的增量。
# 必须用 POST /v1/events：GET /v1/capabilities 是**会话 Cookie** 端点，带 Bearer
# 直接 401（见 raw/server.log 的 "session cookie missing"），根本不会跑 argon2 ——
# 第一版脚本就是踩了这个，差分是空的。
A2BODY=$(jq -nc --arg ns "$PC_NS" '{namespaceId:$ns, source:"perf.echo", type:"go", payload:{n:1}}')
s1_a2_post() {
	curl -s -o /dev/null -X POST "$PC_BASE/v1/events" -H "Authorization: Bearer $PC_SEND_KEY" \
		-H 'content-type: application/json' -d "$A2BODY"
}
HEAP0=$(pc_pprof heap-0req heap)
s1_a2_post
HEAP1=$(pc_pprof heap-1req heap)
s1_a2_post; s1_a2_post; s1_a2_post
HEAP4=$(pc_pprof heap-4req heap)
{
	echo '== alloc_space / argon2.initBlocks（累计分配，每行是当时整个进程的累计值）=='
	for f in "$HEAP0" "$HEAP1" "$HEAP4"; do
		printf '%s: ' "$(basename "$f")"
		go tool pprof -top -sample_index=alloc_space -nodecount=40 "$f" 2>/dev/null \
			| grep argon2.initBlocks || echo '（无 argon2 分配）'
	done
} >"$RAW/argon2.txt" 2>&1
log "argon2 三点量测完成（期望 3 行，末行应为 4 请求 × 64MB），明细见 raw/argon2.txt"

# ---------------------------------------------------------------------------
# 5. 三个后台采样/触发循环（每行立刻落盘 → 跑崩也有前半段数据）
# ---------------------------------------------------------------------------
pc_sample_header >"$RAW/samples.csv"
echo 'elapsed_s,events,tasks,health_codes,loadgen_seg,loadgen_alive,hook_200,hook_429,agent_tick_acked' >"$RAW/extra.csv"
: >"$RAW/ingest.log"
: >"$RAW/hook.log"   # 必须在支路循环**之前**清空，否则会截掉已经开始写的那几行

# 注意 grep -c 在 0 匹配时仍会打印 "0" 但退出码非 0，所以不能写 `|| echo 0`（会拼出两行）。
s1_count_code() { local n; n=$(grep -c "$1" "$2" 2>/dev/null); printf '%s' "${n:-0}"; }

s1_sampler() {
	local e ev tk hc n200 n429 ack seg lgp
	while [ -f "$S1_DIR/.running" ]; do
		e=$(elapsed_s)
		PC_ELAPSED=$e
		pc_sample "t$e" >>"$RAW/samples.csv"
		ev=$(s1_count events); tk=$(s1_count tasks); hc=$(s1_health)
		n200=$(s1_count_code ',200$' "$RAW/hook.log")
		n429=$(s1_count_code ',429$' "$RAW/hook.log")
		ack=$(grep -o '"acked":[0-9]*' "$RAW/agent.log" 2>/dev/null | tail -1 | cut -d: -f2)
		seg=$(cat "$S1_DIR/seg" 2>/dev/null); seg=${seg:-none}
		lgp=$(cat "$S1_DIR/lg.pid" 2>/dev/null)
		echo "$e,${ev:-0},${tk:-0},$hc,$seg,$([ -n "$lgp" ] && kill -0 "$lgp" 2>/dev/null && echo 1 || echo 0),$n200,$n429,${ack:-0}" \
			>>"$RAW/extra.csv"
		sleep "$SAMPLE_EVERY"
	done
}
s1_sampler &
SAMPLER_PID=$!

# 支路：每 2s 投一条（30/min < 60/min 限速）；记 (elapsed,code) 供统计 200/429。
(
	while [ -f "$S1_DIR/.running" ]; do
		c=$(pc_hook "$HOOK_TOKEN" "{\"type\":\"perf.hook\",\"payload\":{\"n\":1}}")
		printf '%s,%s\n' "$(elapsed_s)" "$c" >>"$RAW/hook.log"
		sleep "$HOOK_EVERY"
	done
) &
HOOK_PID=$!

# CPU profile：压到一半抓 30s（pc_pprof soak cpu:30）
(
	while [ "$(elapsed_s)" -lt "$CPU_PROFILE_AT" ] && [ -f "$S1_DIR/.running" ]; do sleep 1; done
	[ -f "$S1_DIR/.running" ] || exit 0
	log "t=$(elapsed_s) 开始抓 30s CPU profile"
	pc_pprof soak cpu:30 >/dev/null
	log "CPU profile 完成（$PC_RUN/soak.cpu.pprof）"
	# 同一时刻的堆/协程快照：用来判断「曲线是平台化还是单调涨」
	pc_pprof heap-mid heap >/dev/null
	curl -s "$PC_BASE/debug/pprof/goroutine?debug=1" >"$RAW/goroutine_t$(elapsed_s).txt"
) &
PROF_PID=$!

# ---------------------------------------------------------------------------
# 6. 主压：同一份代码预编译成二进制再跑（`go run` 每段都要重新编译，会在
#    分段落盘时插进几秒空档并抢核；预编译后三段之间只隔几十毫秒，10 分钟连续）。
#    等价命令：taskset -c 8-11 go run ./scripts/perf/loadgen -url $PC_BASE/v1/events ...
# ---------------------------------------------------------------------------
mkdir -p "$S1_DIR/bin"
LG_BIN="$S1_DIR/bin/loadgen"
( cd "$PC_REPO" && go build -o "$LG_BIN" ./scripts/perf/loadgen ) || die "构建 loadgen 失败"

s1_seg() {  # s1_seg <a> <b>  跑 [a,b) 这一段
	local a="$1" b="$2" secs=$(( $2 - $1 )) tag
	tag=$(printf '%03d-%03d' "$a" "$b")
	echo "$tag" >"$S1_DIR/seg"   # 采样子进程是独立进程，共享变量传不过去，用文件传

	log "主压段 $tag（${secs}s，-c 8）开始 @ t=$(elapsed_s)"
	taskset -c "$PC_CPUS" "$LG_BIN" -url "$PC_BASE/v1/events" -method POST \
		-body-file "$BODY_JSON" -header 'content-type: application/json' \
		-header "Authorization: Bearer $PC_SEND_KEY" -c 8 -d "${secs}s" \
		2>&1 | tee -a "$RAW/ingest.log" >>"$RAW/ingest_$tag.log" &
	LG_PID=$!
	echo "$LG_PID" >"$S1_DIR/lg.pid"
	wait "$LG_PID"
	LG_PID=""
	: >"$S1_DIR/lg.pid"
	log "主压段 $tag 结束 @ t=$(elapsed_s)"
}

# 注意：`go run` 的等价性——这里分段是为了拿到 0-2min / 8-10min 的**分位数**，
# loadgen 只在退出时打一次汇总，单段 600s 拿不到分段对比。
s1_seg 0 "$SEG_A"
s1_seg "$SEG_A" $(( SEG_A + SEG_B ))
s1_seg $(( SEG_A + SEG_B )) "$DURATION"

# ---------------------------------------------------------------------------
# 7. 收尾：最后一次采样（仍在 running 标记内）→ 停采 → 停服务 → 停节点
# ---------------------------------------------------------------------------
# 等 CPU profile 那一路落完；若本次主压根本没跑到抓取点（比如冒烟跑），直接收掉再不会白等。
if kill -0 "${PROF_PID:-0}" 2>/dev/null; then
	if [ "$(elapsed_s)" -lt "$CPU_PROFILE_AT" ]; then kill "$PROF_PID" 2>/dev/null; fi
	wait "$PROF_PID" 2>/dev/null
fi
sleep 1
FRESH_HOOK_200=$(s1_count_code ',200$' "$RAW/hook.log")
FRESH_HOOK_429=$(s1_count_code ',429$' "$RAW/hook.log")
PC_ELAPSED=$(elapsed_s)
{ pc_sample "t${PC_ELAPSED}_final" >>"$RAW/samples.csv"; } 2>/dev/null
echo "$(elapsed_s),$(s1_count events),$(s1_count tasks),$(s1_health),end,0,$FRESH_HOOK_200,$FRESH_HOOK_429,$(grep -o '"acked":[0-9]*' "$RAW/agent.log" | tail -1 | cut -d: -f2)" \
	>>"$RAW/extra.csv"
rm -f "$S1_DIR/.running"

# t≈600 的 heap/goroutine/mutex（压测刚停、分配还没被 GC 清掉的时刻）
pc_pprof heap-end heap >/dev/null
pc_pprof goroutine-end goroutine >/dev/null
pc_pprof mutex-end mutex >/dev/null
curl -s "$PC_BASE/debug/pprof/goroutine?debug=1" >"$RAW/goroutine_t$(elapsed_s)_end.txt"

# 写路径的账：每类事件/任务各多少行（写放大 + 剪枝原因）在这一处能一眼看完。
{
	echo '== events by source =='
	s1_sql "select source, count(*) from events group by source order by 2 desc"
	echo '== tasks by status =='
	s1_sql "select status, count(*) from tasks group by status order by 2 desc"
	echo '== node_instances =='
	s1_sql "select count(*) from node_instances"
	echo '== 行数 =='
	s1_sql "select 'events', count(*) from events union all select 'tasks', count(*) from tasks union all select 'edges', count(*) from edges"
} >"$RAW/db_stats.txt" 2>&1

# profile 与日志收进 raw/profiles（pc_pprof 写在 $PC_RUN 里）
cp "$PC_RUN"/*.pprof "$PROF/" 2>/dev/null
cp "$PC_RUN/server.log" "$RAW/server.log" 2>/dev/null
sqlite3 -readonly "$PC_DB" .schema >"$RAW/schema.sql" 2>/dev/null
echo "server.log tail:" >"$RAW/server_log_tail.txt"
tail -40 "$PC_RUN/server.log" >>"$RAW/server_log_tail.txt" 2>/dev/null

log "压测结束：events=$(s1_count events) tasks=$(s1_count tasks) rss=$(ps -o rss= -p "$PC_PID" | tr -d ' ')KB"

# 停服务/节点（cleanup 幂等，EXIT 时再跑一次也无害）
cleanup

log "产物：$RAW（samples.csv / extra.csv / ingest.log / hook.log / agent.log / profiles/）"
log "DONE c1-soak 耗时 $(($(date +%s) - T0))s"
