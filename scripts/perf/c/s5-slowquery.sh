#!/usr/bin/env bash
# C5 · 慢查询评估：10 万事件 / 2 万任务之后的读路径延迟与索引覆盖
#
# 只评估、不改产品代码。做的事：
#   1. pc_start（空库）→ pc_seed → pc_cap，先量「0 条」基线
#   2. 停服务 → 用 sqlite3 + recursive CTE **批量直插**（不走接口：
#      接口实测 ~35 ev/s，10 万条要 48 分钟），每插一档就
#      PRAGMA wal_checkpoint(TRUNCATE) + ANALYZE，再重启服务量同一批端点
#   3. 三档数据量 0 / 1万 / 10万（任务 0 / 2千 / 2万），每档 8 条读路径（6 业务 + 2 底噪对照）
#      × 20 次 curl -w %{time_total}，取 p50/p95/max + 响应字节
#   4. EXPLAIN QUERY PLAN 逐条核对：走哪个索引 / 是否 USE TEMP B-TREE /
#      是否 SCAN（全表）。ANALYZE 有/无两态各拍一次，用来判断「统计信息是
#      不是瓶颈的主因」（公平起见两态都测）
#   5. idx_edges_lookup 在 schema.sql 里声明了两次（列不同）——查实际库里的
#      sqlite_master，并对派发用的 from_capability 查边 SQL 做 EXPLAIN
#
# 用法：bash scripts/perf/c/s5-slowquery.sh
#   端口 7945（撞了顺延 7949）；分核 taskset -c 10-11；产物 ${PC_ROOT:-/tmp/perf-c}/s5/
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# 契约 §3：C5 分核是 10-11（比 C1-C4 的 8-11 更窄），必须在 source 之前 export，
# 否则 common.sh 里的 ${PC_CPUS:-8-11} 会先把默认值钉死。
export PC_CPUS="${PC_CPUS:-10-11}"
# shellcheck source=/dev/null
source "$SCRIPT_DIR/common.sh"

OUT="$PC_ROOT/s5"
RAW="$OUT/raw"
mkdir -p "$RAW"
LOG="$RAW/run.log"
LAT="$RAW/latency.tsv"
SUM="$RAW/summary.tsv"
: >"$LOG"
: >"$LAT"
: >"$SUM"
rm -f "$RAW"/qtime-*.txt # 上一轮的残留会混进本次的 qtime 汇总

N_REQ="${PC_REQ:-20}" # 每项请求次数（契约要求 ≥20）
CURL=(taskset -c "$PC_CPUS" curl)
SQL3=(sqlite3)

# PC_TEE=1 表示「外层已经 tee 到 LOG 了」，此时 log() 只写 stdout，避免每行落两遍。
log() {
	if [ "${PC_TEE:-0}" = 1 ]; then
		printf '%s\n' "$*"
	else
		printf '%s\n' "$*" | tee -a "$LOG"
	fi
}
hr() {
	log ""
	log "=================================================================================="
	log "## $*"
	log "=================================================================================="
}

# ---------------------------------------------------------------------------
# 0. 环境
# ---------------------------------------------------------------------------
pc_require_env || exit 1
pc_build || exit 1

pick_port() {
	local p
	for p in 7945 7949; do # 7949 = 契约 §3 的重试位；两个都被占就放弃，不抢别人的端口
		if ! ss -lnt 2>/dev/null | grep -qE "[:.]${p}[[:space:]]"; then
			printf '%s' "$p"
			return 0
		fi
	done
	return 1
}
PORT="$(pick_port)" || {
	printf 's5: 7945/7949 都被占用，放弃（不抢别人的端口）\n' >&2
	exit 1
}

hr "环境"
log "repo   : $PC_REPO"
log "bin    : $PC_BIN"
log "cpus   : $PC_CPUS"
log "port   : $PORT"
log "date   : $(date -Is)"
log "sqlite : $(${SQL3[@]} --version)"
log "curl   : $(${CURL[@]} --version | head -1)"

# svc_up <name> <port>：**在已存在的库上**重启服务。
#
# 为什么不能复用 pc_start：它开头 `rm -rf $dbdir`（每场景一次性初始化），
# 批量插进来的 10 万行会被删掉。这里逐字照抄 pc_start 的环境（同 boot id、
# 同分核、同 pprof 开关），只是不删目录、日志改成追加。
svc_up() {
	local name="$1" port="$2"
	local dbdir="$PC_ROOT/runs/$name"
	mkdir -p "$dbdir"
	export PC_DB="$dbdir/perf.db"
	export PC_RUN="$dbdir"
	taskset -c "$PC_CPUS" env \
		ANOTIFY_ADDR=":$port" \
		ANOTIFY_DB="$PC_DB" \
		ANOTIFY_PPROF=1 \
		ANOTIFY_LOG_LEVEL="${PC_LOG_LEVEL:-warn}" \
		ANOTIFY_RP_ID=localhost \
		ANOTIFY_RP_ORIGIN="http://localhost:$port" \
		ANOTIFY_VAPID_PUBLIC_KEY=dummy \
		ANOTIFY_VAPID_PRIVATE_KEY=dummy \
		ANOTIFY_BOOT_ID="perf-c-$name" \
		"$PC_BIN" >>"$dbdir/server.log" 2>&1 &
	local pid=$!
	echo "$pid" >"$dbdir/server.pid"
	export PC_PID="$pid"
	export PC_PORT="$port"
	export PC_BASE="http://localhost:$port"
	local deadline=$((SECONDS + 20))
	while [ $SECONDS -lt $deadline ]; do
		local boot
		boot=$(curl -s --max-time 1 "$PC_BASE/health" 2>/dev/null | jq -r '.boot // empty' 2>/dev/null)
		if [ "$boot" = "perf-c-$name" ]; then
			return 0
		fi
		if ! kill -0 "$pid" 2>/dev/null; then
			log "svc_up: 服务启动即退出，日志："
			tail -20 "$dbdir/server.log" | tee -a "$LOG" >&2
			return 1
		fi
		sleep 0.2
	done
	log "svc_up: 20s 内 /health 未就绪"
	tail -20 "$dbdir/server.log" | tee -a "$LOG" >&2
	return 1
}

# ---------------------------------------------------------------------------
# 1. 空库基线（0 条）
# ---------------------------------------------------------------------------
hr "1. 空库基线"
pc_start slow "$PORT" >/dev/null || exit 1
trap 'pc_stop' EXIT
log "base : $PC_BASE   (pid $(cat "$PC_RUN/server.pid"))"
pc_seed || exit 1
log "ns   : $PC_NS"
log "uid  : $PC_UID"
CAP_ID="$(pc_cap perf.cap '{"type":"object"}')"
log "cap  : $CAP_ID"
[ -n "$CAP_ID" ] || {
	log "s5: 建能力失败，退出"
	exit 1
}
# 真实 schema 里没有 org_id：migration 14（#204）把组织层删了，命名空间是唯一隔离边界。
# 这里显式记一笔实际列，免得再造数时又按 schema.sql 的老写法插一遍（第一次就是这么挂的）。
log "events 实际列 : $(${SQL3[@]} "$PC_DB" "SELECT group_concat(name, ' ') FROM pragma_table_info('events');")"
log "tasks  实际列 : $(${SQL3[@]} "$PC_DB" "SELECT group_concat(name, ' ') FROM pragma_table_info('tasks');")"

# 各端点 URL（Cookie 取 console 会话；Bearer 那两条见下面的 scope 说明）
U_MEV500="$PC_BASE/v1/events?namespaceId=$PC_NS&limit=500"
U_MEV50="$PC_BASE/v1/events?namespaceId=$PC_NS&limit=50"
U_MEV1="$PC_BASE/v1/events?namespaceId=$PC_NS&limit=1"
U_CEV100="$PC_BASE/v1/console/events?namespaceId=$PC_NS&limit=100"
U_CEV1000="$PC_BASE/v1/console/events?namespaceId=$PC_NS&limit=1000"
U_TASKS200="$PC_BASE/v1/tasks?namespaceId=$PC_NS&limit=200"
U_TRACE="$PC_BASE/v1/traces/trace_bulk_0001?namespaceId=$PC_NS"
# GET /v1/events（机器入口）要求的是 **ScopeNotifySend**（internal/api/events.go:77），
# 所以这里必须用 SEND_KEY；用 RECV_KEY 会拿到 403 forbidden: insufficient scope（证据见 §附）。
AUTH_BEARER="Authorization: Bearer $PC_SEND_KEY"
AUTH_COOKIE="Cookie: anotify_session=$PC_SESSION"

# bench <phase> <label> <url> [curl 额外参数...]
# 每项：3 次预热（不进统计）+ N_REQ 次取样，落 latency.tsv：
#   phase<TAB>label<TAB>time_total(s)<TAB>size_download<TAB>http_code
bench() {
	local phase="$1" label="$2" url="$3"
	shift 3
	local out code
	for _ in 1 2 3; do
		"${CURL[@]}" -s -o /dev/null --max-time 30 "$@" "$url" >/dev/null 2>&1
	done
	for _ in $(seq 1 "$N_REQ"); do
		out=$("${CURL[@]}" -s -o /dev/null --max-time 30 \
			-w '%{time_total}\t%{size_download}\t%{http_code}' "$@" "$url" 2>&1)
		printf '%s\t%s\t%s\n' "$phase" "$label" "$out" >>"$LAT"
	done
	code="$(awk -F'\t' -v p="$phase" -v l="$label" '$1==p && $2==l{print $5}' "$LAT" | sort -u | paste -sd, -)"
	printf '  %-9s %-10s http=%s\n' "$phase" "$label" "$code" | tee -a "$LOG"
}

# bench_all <phase>：量全部 8 条读路径（6 条业务端点 + 2 条底噪对照：health / mev1）。
bench_all() {
	local ph="$1"
	bench "$ph" health "$PC_BASE/health"
	bench "$ph" mev1 "$U_MEV1" -H "$AUTH_BEARER"
	bench "$ph" mev500 "$U_MEV500" -H "$AUTH_BEARER"
	bench "$ph" mev50 "$U_MEV50" -H "$AUTH_BEARER"
	bench "$ph" cev100 "$U_CEV100" -H "$AUTH_COOKIE"
	bench "$ph" cev1000 "$U_CEV1000" -H "$AUTH_COOKIE"
	bench "$ph" trace "$U_TRACE" -H "$AUTH_COOKIE"
	bench "$ph" tasks200 "$U_TASKS200" -H "$AUTH_COOKIE"
}

# 附：机器读端点的 scope 事实（这条本身是个 bug：读事件流要 notify:send）
PC_TEE=1
{
	log ""
	log "--- 附：GET /v1/events 的鉴权口径（internal/api/events.go:77 要求 ScopeNotifySend）---"
	log "  $(${CURL[@]} -s -o /dev/null -w 'RECV_KEY(notify:receive) → code=%{http_code} bytes=%{size_download}' \
		-H "Authorization: Bearer $PC_RECV_KEY" "$U_MEV500")"
	log "  RECV_KEY 响应体: $(${CURL[@]} -s -H "Authorization: Bearer $PC_RECV_KEY" "$U_MEV500" | tr -d '\n' | tr -s ' ')"
	log "  $(${CURL[@]} -s -o /dev/null -w 'SEND_KEY(notify:send)    → code=%{http_code} bytes=%{size_download}' \
		-H "$AUTH_BEARER" "$U_MEV500")"
} 2>&1 | tee -a "$LOG"
PC_TEE=0

bench_all p0_empty

# ---------------------------------------------------------------------------
# 2. 批量造数（停服务 + sqlite3 直插；不走接口）
# ---------------------------------------------------------------------------
# evbulk <from> <to> <total> [start_epoch]：插 from..to 号事件。
#
# 形状刻意做成"真实样子"：
#   * 1 trace = 10 事件（trace_bulk_%04d），trace 视图能一次拿满 10 行；
#   * source='perfgen'（**不是** route）—— ListEvents 的空 source 分支会带
#     `AND source != 'route'`，所以每 50 条掺 1 条 source='route'（模拟 #142 的
#     剪枝事件），让这个过滤条件真的有事可做，否则测出来的索引扫描是假的；
#   * payload 约 200 字节（json），created_at 覆盖一天（86400/100000 ≈ 0.86s 一条，
#     同一秒内会有 ~1.2 条 id 碰撞 → ORDER BY created_at DESC, id DESC 的
#     第二排序键不是摆设）；
#   * id 用 evt_bulk_%06d，可复现。
evbulk() {
	local from="$1" to="$2" total="$3" t0="$4"
	local t_start t_end
	t_start=$(date +%s%N)
	${SQL3[@]} "$PC_DB" <<-SQL
		PRAGMA journal_mode=WAL;
		PRAGMA synchronous=OFF;
		BEGIN;
		WITH RECURSIVE seq(i) AS (SELECT $from UNION ALL SELECT i+1 FROM seq WHERE i < $to)
		INSERT INTO events (id, namespace_id, trace_id, source, type, payload, idem_key, created_at)
		SELECT
			printf('evt_bulk_%06d', i),
			'$PC_NS',
			printf('trace_bulk_%04d', (i-1)/10),
			CASE WHEN i % 50 = 0 THEN 'route' ELSE 'perfgen' END,
			CASE i % 4 WHEN 0 THEN 'perf.tick' WHEN 1 THEN 'pull_request.opened'
			             WHEN 2 THEN 'push' ELSE 'schedule.fired' END,
			printf('{"kind":"perfgen","seq":%d,"pad":"$PAD170"}', i),
			NULL,
			$t0 + (i * 86400 / $total)
		FROM seq;
		COMMIT;
	SQL
	if [ $? -ne 0 ]; then
		echo "evbulk: 直插失败（详见上方 sqlite3 报错），拒绝继续" | tee -a "$LOG" >&2
		return 1
	fi
	t_end=$(date +%s%N)
	echo "  events $from..$to → $(( (t_end - t_start) / 1000000 )) ms" | tee -a "$LOG"
}

# taskbulk <from> <to> <total> [start_epoch]：插 from..to 号任务。
#
# status 三态分布（succeeded / failed / pending 各占一份），但**所有 pending 都带
# node_id**：ListRedispatchable 的条件是 `status='pending' AND node_id IS NULL`，
# 若留一堆 node_id 为 NULL 的 pending，后台巡检会不停重派它们（等于自己给
# 测量窗口加背景负载）。带 node_id 的 pending = "已派给某个节点、还没回报"，
# 同样是真实状态，且不进重派池。
taskbulk() {
	local from="$1" to="$2" total="$3" t0="$4"
	local t_start t_end
	t_start=$(date +%s%N)
	${SQL3[@]} "$PC_DB" <<-SQL
		PRAGMA journal_mode=WAL;
		PRAGMA synchronous=OFF;
		BEGIN;
		WITH RECURSIVE seq(i) AS (SELECT $from UNION ALL SELECT i+1 FROM seq WHERE i < $to)
		INSERT INTO tasks (id, namespace_id, trace_id, parent_task_id, path,
			capability_id, capability, version, params, status, node_id, instance_id,
			lease_until, attempts, out, reported, summary, artifacts, error,
			cap_hash, graph_id, created_at, started_at, finished_at, not_before)
		SELECT
			printf('tsk_bulk_%06d', i),
			'$PC_NS',
			printf('trace_bulk_%04d', ((i-1)/2) % 10000),
			NULL, '["perf.cap"]',
			'$CAP_ID', 'perf.cap', 1, '{}',
			CASE i % 3 WHEN 0 THEN 'succeeded' WHEN 1 THEN 'failed' ELSE 'pending' END,
			CASE i % 3 WHEN 2 THEN 'node_perfgen_a' ELSE NULL END,
			CASE i % 3 WHEN 2 THEN 'inst_perfgen_a' ELSE NULL END,
			CASE i % 3 WHEN 2 THEN ($t0 + 86400 + 600) ELSE NULL END,
			CASE i % 3 WHEN 2 THEN 0 ELSE 1 END,
			CASE i % 3 WHEN 0 THEN '{"ok":true}' ELSE NULL END,
			1,
			CASE i % 3 WHEN 0 THEN 'done' WHEN 1 THEN 'node unreachable' ELSE '' END,
			'[]',
			CASE i % 3 WHEN 1 THEN 'node unreachable' ELSE '' END,
			'caphash_perfgen', 'graph_perfgen',
			$t0 + (i * 86400 / $total),
			$t0 + (i * 86400 / $total),
			CASE i % 3 WHEN 2 THEN NULL ELSE ($t0 + (i * 86400 / $total) + 3) END,
			NULL
		FROM seq;
		COMMIT;
	SQL
	if [ $? -ne 0 ]; then
		echo "taskbulk: 直插失败（详见上方 sqlite3 报错），拒绝继续" | tee -a "$LOG" >&2
		return 1
	fi
	t_end=$(date +%s%N)
	echo "  tasks  $from..$to → $(( (t_end - t_start) / 1000000 )) ms" | tee -a "$LOG"
}

# settle <label>：插完之后的收尾 —— checkpoint + （可选）ANALYZE + 采体积。
settle() {
	local label="$1" do_analyze="$2"
	local t0 t1
	t0=$(date +%s%N)
	if [ "$do_analyze" = analyze ]; then
		${SQL3[@]} "$PC_DB" "PRAGMA wal_checkpoint(TRUNCATE); ANALYZE;" >>"$LOG" 2>&1
	else
		${SQL3[@]} "$PC_DB" "PRAGMA wal_checkpoint(TRUNCATE);" >>"$LOG" 2>&1
	fi
	t1=$(date +%s%N)
	local db wal ev tk
	db=$(stat -c %s "$PC_DB")
	wal=$(stat -c %s "$PC_DB-wal" 2>/dev/null || echo 0)
	ev=$(${SQL3[@]} "$PC_DB" "SELECT count(*) FROM events;")
	tk=$(${SQL3[@]} "$PC_DB" "SELECT count(*) FROM tasks;")
	log "  [$label] checkpoint${do_analyze:+/$do_analyze} = $(( (t1 - t0) / 1000000 )) ms；db=$db B wal=$wal B events=$ev tasks=$tk"
	if [ "${ev:-0}" -lt 1 ] || [ "${tk:-0}" -lt 1 ]; then
		echo "settle: 行数为 0 —— 造数没落库，测量无意义，直接停手" | tee -a "$LOG" >&2
		return 1
	fi
}

PC_stop_wait() { pc_stop; sleep 0.3; }

# 170 个 x，撑 payload 到 ~200 字节
PAD170="$(printf 'x%.0s' $(seq 1 170))"
T0="$(date +%s)" # 造数的起点时间戳（覆盖一天 → T0 .. T0+86400）

hr "2. 第一档：1 万事件 / 2 千任务"
PC_stop_wait
evbulk 1 10000 10000 "$T0" || exit 1
taskbulk 1 2000 2000 "$T0" || exit 1
settle p1_10k analyze || exit 1
svc_up slow "$PORT" || exit 1
log "  服务已重启：$PC_BASE"
bench_all p1_10k

hr "3. 第二档：10 万事件 / 2 万任务"
PC_stop_wait
evbulk 10001 100000 100000 "$T0" || exit 1
taskbulk 2001 20000 20000 "$T0" || exit 1
settle p2_100k analyze || exit 1
svc_up slow "$PORT" || exit 1
log "  服务已重启：$PC_BASE"
bench_all p2_100k

# 附：limit 上限在三处口径不一致（api 允许 1000 → store 硬上限 500 → 超了打回 50）
PC_TEE=1
{
	log ""
	log "--- 附：limit 口径（api/events.go 允许 1000 ；store/event.go:230 的硬上限是 500，超了打回 50）---"
	for L in 100 500 501 1000; do
		n=$(${CURL[@]} -s -H "$AUTH_BEARER" "$PC_BASE/v1/events?namespaceId=$PC_NS&limit=$L" | jq '.events | length')
		nc=$(${CURL[@]} -s -H "$AUTH_COOKIE" "$PC_BASE/v1/console/events?namespaceId=$PC_NS&limit=$L" | jq '.events | length')
		log "  limit=$L → GET /v1/events 返回 $n 行；GET /v1/console/events 返回 $nc 行"
	done
} 2>&1 | tee -a "$LOG"
PC_TEE=0

# ---------------------------------------------------------------------------
# 4. EXPLAIN QUERY PLAN（ANALYZE 有 / 无两态）
# ---------------------------------------------------------------------------
# 每次 EXPLAIN 前都 dump 一份真实索引清单，避免"以为有索引"。
dump_indexes() {
	local tag="$1"
	PC_TEE=1
	{
		log ""
		log "--- [$tag] sqlite_master 索引清单（events / tasks / edges）---"
		${SQL3[@]} -separator ' | ' "$PC_DB" \
			"SELECT name, tbl_name, COALESCE(sql,'(自动索引)') FROM sqlite_master
			  WHERE type='index' AND tbl_name IN ('events','tasks','edges') ORDER BY tbl_name, name;"
		log ""
		log "--- [$tag] sqlite_stat1（ANALYZE 有没有真跑）---"
		${SQL3[@]} -separator ' | ' "$PC_DB" \
			"SELECT tbl, idx, stat FROM sqlite_stat1 ORDER BY tbl, idx;" 2>&1 || log "  （没有 sqlite_stat1）"
	} 2>&1 | tee -a "$LOG"
	PC_TEE=0
}

# eqp <tag> <说明> <sql>
# 真实 SQL 逐条 EXPLAIN QUERY PLAN；判定关键词：SCAN（大表扫描）/ TEMP B-TREE / idx_*。
eqp() {
	local tag="$1" desc="$2" sql="$3"
	PC_TEE=1
	{
		log ""
		log "### [$tag] $desc"
		log "SQL: $(printf '%s' "$sql" | tr '\n' ' ' | sed -E 's/  +/ /g')"
		log "PLAN:"
		${SQL3[@]} "$PC_DB" "EXPLAIN QUERY PLAN $sql" 2>&1 | sed -E 's/^[0-9]+\|[0-9]+\|[0-9]+\|/    /' | sed 's/^/    /'
	} 2>&1 | tee -a "$LOG"
	PC_TEE=0
}

# qtime <tag> <sql>：在 sqlite3 里直接跑 5 次，取真实执行时间（去掉 HTTP/argon2 的底噪）。
qtime() {
	local tag="$1" sql="$2"
	PC_TEE=1
	{
		log ""
		log "### [qtime] $tag  （sqlite3 直跑 5 次，.timer 实测）"
		for _ in 1 2 3 4 5; do
			# .timer 的输出走 stdout（本机 sqlite 3.53 实测），所以整段接住再筛
			${SQL3[@]} "$PC_DB" ".timer on" "$sql" 2>&1 | grep 'Run Time:' >>"$RAW/qtime-$tag.txt"
		done
		sed 's/^/    /' "$RAW/qtime-$tag.txt"
	} 2>&1 | tee -a "$LOG"
	PC_TEE=0
}

EV_COLS="id, namespace_id, trace_id, source, type, payload, idem_key, created_at"
TK_COLS="id, namespace_id, trace_id, parent_task_id, path, capability_id, capability,
	version, params, status, node_id, instance_id, lease_until, attempts,
	out, reported, summary, artifacts, error, cap_hash, graph_id,
	created_at, started_at, finished_at, not_before"

# 六条真实 SQL —— 逐字对齐 store/handler 里的拼装结果
SQL_MEV_BASE="SELECT $EV_COLS FROM events WHERE namespace_id = '$PC_NS' AND source != 'route' ORDER BY created_at DESC, id DESC LIMIT 500"
SQL_MEV_50="SELECT $EV_COLS FROM events WHERE namespace_id = '$PC_NS' AND source != 'route' ORDER BY created_at DESC, id DESC LIMIT 50"
SQL_CEV_100="SELECT $EV_COLS FROM events WHERE namespace_id = '$PC_NS' AND source != 'route' ORDER BY created_at DESC, id DESC LIMIT 100"
SQL_TRACE_EV="SELECT $EV_COLS FROM events WHERE namespace_id = '$PC_NS' AND trace_id = 'trace_bulk_0001' ORDER BY created_at DESC, id DESC LIMIT 500"
SQL_TRACE_TK="SELECT $TK_COLS FROM tasks WHERE namespace_id = '$PC_NS' AND trace_id = 'trace_bulk_0001' ORDER BY created_at DESC, id DESC LIMIT 500"
SQL_GETEV_BY_TRACE="SELECT $EV_COLS FROM events WHERE namespace_id = '$PC_NS' AND trace_id = 'trace_bulk_0001' ORDER BY created_at, id LIMIT 1"
SQL_TASKS="SELECT $TK_COLS FROM tasks WHERE namespace_id = '$PC_NS' ORDER BY created_at DESC, id DESC LIMIT 200"
SQL_REDISPATCH="SELECT $TK_COLS FROM tasks WHERE status = 'pending' AND node_id IS NULL AND (not_before IS NULL OR not_before <= $T0) ORDER BY COALESCE(not_before, 0), created_at, id LIMIT 100"
SQL_EDGES_FROMKEY="SELECT to_capability FROM edges WHERE namespace_id = '$PC_NS' AND from_is_source = 0 AND from_key = 'perf.cap' AND from_port = 'out' ORDER BY to_capability"

hr "4. EXPLAIN QUERY PLAN —— ANALYZE 之后（库与 p2_100k 测量时同一份）"
PC_stop_wait
dump_indexes "ANALYZE-on"
# 描述一律以 Q<n> 开头：后面 finding.md 要按 ID 精确截取计划原文（不能让
# "limit=50" 这种片段匹配到 "limit=500" 那一行）。
eqp "ANALYZE-on" "Q1 事件流 GET /v1/events?limit=500（store ListEvents，无 traceId/无 source）" "$SQL_MEV_BASE"
eqp "ANALYZE-on" "Q2 事件流 limit=50（同一条 SQL，只改 LIMIT）" "$SQL_MEV_50"
eqp "ANALYZE-on" "Q3 控制台事件流 limit=100（GET /v1/console/events）" "$SQL_CEV_100"
eqp "ANALYZE-on" "Q4 trace 视图：events 按 trace_id（GET /v1/traces/{id}）" "$SQL_TRACE_EV"
eqp "ANALYZE-on" "Q5 trace 视图：tasks 按 trace_id" "$SQL_TRACE_TK"
eqp "ANALYZE-on" "Q6 GetEventByTrace（event.go:268，ORDER BY created_at, id LIMIT 1）" "$SQL_GETEV_BY_TRACE"
eqp "ANALYZE-on" "Q7 任务列表 GET /v1/tasks?limit=200（store ListTasks）" "$SQL_TASKS"
eqp "ANALYZE-on" "Q8 后台重派 ListRedispatchable（task.go:1072，全库扫 pending）" "$SQL_REDISPATCH"
eqp "ANALYZE-on" "Q9 派发查边 EdgesFrom（graph.go:211）" "$SQL_EDGES_FROMKEY"
eqp "ANALYZE-on" "Q10 规范里那条已经不存在的列：edges 按 from_capability 查（schema.sql:290 的第二个 idx_edges_lookup）" \
	"SELECT to_capability FROM edges WHERE namespace_id = '$PC_NS' AND from_capability = 'x' AND from_port = 'out'"

hr "5. 索引缺口核对：idx_edges_lookup 被声明两次，实际库里是哪一条？"
PC_TEE=1
{
	log "--- internal/store/schema.sql:288 与 :290 的两条声明 ---"
	grep -n "idx_edges_lookup" "$PC_REPO/internal/store/schema.sql" | sed 's/^/    /'
	log "--- migration 12 重建 edges 时实际建的那条（migrate.go）---"
	grep -n -B1 -A2 "idx_edges_lookup" "$PC_REPO/internal/store/migrate.go" | sed 's/^/    /'
	log "--- schema.sql:290 引用的 from_capability 列现在还在不在？ ---"
	log "    edges 实际列：$(${SQL3[@]} "$PC_DB" "SELECT group_concat(name, ', ') FROM pragma_table_info('edges');")"
	log "--- 直接执行 schema.sql:290 那条 CREATE INDEX 会怎样 ---"
	${SQL3[@]} "$PC_DB" "CREATE INDEX IF NOT EXISTS idx_edges_lookup_probe ON edges(namespace_id, from_capability, from_port);" 2>&1 | sed 's/^/    ERROR: /'
	log "--- 库里 edges 的索引与行数 ---"
	${SQL3[@]} -separator ' | ' "$PC_DB" "SELECT name, sql FROM sqlite_master WHERE type='index' AND tbl_name='edges';" | sed 's/^/    /'
	log "    edges 行数 = $(${SQL3[@]} "$PC_DB" "SELECT count(*) FROM edges;")"
} 2>&1 | tee -a "$LOG"
PC_TEE=0

hr "6. 去掉统计信息再 EXPLAIN（分析口径对比：ANALYZE 是不是瓶颈主因）"
${SQL3[@]} "$PC_DB" "DELETE FROM sqlite_stat1; ANALYZE sqlite_schema; PRAGMA wal_checkpoint(TRUNCATE);" >>"$LOG" 2>&1
dump_indexes "ANALYZE-off"
eqp "ANALYZE-off" "Q1 事件流 limit=500" "$SQL_MEV_BASE"
eqp "ANALYZE-off" "Q7 任务列表 limit=200" "$SQL_TASKS"
eqp "ANALYZE-off" "Q8 后台重派 ListRedispatchable" "$SQL_REDISPATCH"
eqp "ANALYZE-off" "Q6 GetEventByTrace" "$SQL_GETEV_BY_TRACE"

hr "7. sqlite3 直跑的真实执行时间（剥离 HTTP/argon2 底噪）"
qtime mev500 "$SQL_MEV_BASE"
qtime tasks200 "$SQL_TASKS"
qtime trace_ev "$SQL_TRACE_EV"
qtime redispatch "$SQL_REDISPATCH"

# 行数 → 纯 SQL 耗时的缩放曲线。
# 为什么单做：HTTP 侧被 argon2id（每请求 ~55ms）和 JSON 序列化（500 行 187KB）盖住了，
# 看不出 SQL 自己随行数怎么变。这里在**库的副本**上删行 + VACUUM，直接量 store 的那条 SQL。
hr "7.5 缩放曲线：行数 → 纯 SQL 耗时（sqlite3 在库副本上直跑，删行 + VACUUM 造档）"
sqltime() { # sqltime <db> <sql> [次数]
	local db="$1" sql="$2" n="${3:-5}" best=""
	for _ in $(seq 1 "$n"); do
		local t
		t=$(${SQL3[@]} "$db" ".timer on" "$sql" 2>&1 | sed -n 's/^Run Time: real //p' | awk '{print $1}')
		best="$best $t"
	done
	printf '%s' "$best" | tr ' ' '\n' | grep . | sort -g | sed -n '3p' # 取中位数那一次
}
PC_TEE=1
{
	SCALEDB="$RAW/scale-tasks.db"
	cp "$PC_DB" "$SCALEDB"
	log "  库副本：$SCALEDB"
	for n in 20000 10000 5000 2000 1000; do
		${SQL3[@]} "$SCALEDB" "DELETE FROM tasks WHERE id NOT IN (SELECT id FROM tasks ORDER BY created_at DESC LIMIT $n); VACUUM; ANALYZE;" >/dev/null 2>&1
		got=$(${SQL3[@]} "$SCALEDB" "SELECT count(*) FROM tasks;")
		tm=$(sqltime "$SCALEDB" "$SQL_TASKS")
		printf '%s\t%s\n' "$got" "$tm" >>"$RAW/curve-tasks.tsv"
		log "  tasks=$got → ListTasks(limit=200) 中位耗时 $tm s"
	done
	${SQL3[@]} "$SCALEDB" "EXPLAIN QUERY PLAN $SQL_TASKS" 2>&1 | sed -E 's/^[0-9]+\|[0-9]+\|[0-9]+\|/    /' | sed 's/^/    /'
	rm -f "$SCALEDB" "$SCALEDB-wal" "$SCALEDB-shm"

	SCALEDB="$RAW/scale-events.db"
	cp "$PC_DB" "$SCALEDB"
	for n in 100000 10000 1000; do
		${SQL3[@]} "$SCALEDB" "DELETE FROM events WHERE id NOT IN (SELECT id FROM events ORDER BY created_at DESC LIMIT $n); VACUUM; ANALYZE;" >/dev/null 2>&1
		got=$(${SQL3[@]} "$SCALEDB" "SELECT count(*) FROM events;")
		tm=$(sqltime "$SCALEDB" "$SQL_MEV_BASE")
		printf '%s\t%s\n' "$got" "$tm" >>"$RAW/curve-events.tsv"
		log "  events=$got → ListEvents(limit=500) 中位耗时 $tm s"
	done
	rm -f "$SCALEDB" "$SCALEDB-wal" "$SCALEDB-shm"
} 2>&1 | tee -a "$LOG"
PC_TEE=0

hr "8. 第三档：10 万条但无统计信息（同一份数据，只差 ANALYZE）"
svc_up slow "$PORT" || exit 1
log "  服务已重启：$PC_BASE"
bench_all p3_100k_noanalyze
pc_stop

# ---------------------------------------------------------------------------
# 9. 汇总
# ---------------------------------------------------------------------------
# 最近秩法：p50 = 第 ceil(0.50*n) 个，p95 = 第 ceil(0.95*n) 个，max = 第 n 个。
summarize() {
	awk -F'\t' '
		$1 ~ /^p[0-9]/ {
			k = $1 SUBSEP $2
			n[k]++
			t[k, n[k]] = $3 * 1000      # s → ms
			b[k] = $4
			c[k] = $5
		}
		END {
			for (k in n) {
				m = n[k]
				for (i = 2; i <= m; i++) {          # 插入排序（n 只有 20，够用）
					v = t[k, i]; j = i - 1
					while (j >= 1 && t[k, j] > v) { t[k, j+1] = t[k, j]; j-- }
					t[k, j+1] = v
				}
				i50 = int(m * 0.50 + 0.999); if (i50 < 1) i50 = 1
				i95 = int(m * 0.95 + 0.999); if (i95 > m) i95 = m
				sum = 0
				for (i = 1; i <= m; i++) sum += t[k, i]
				split(k, parts, SUBSEP)
				printf "%s\t%s\t%d\t%.2f\t%.2f\t%.2f\t%.2f\t%d\t%s\n",
					parts[1], parts[2], m, sum/m, t[k, i50], t[k, i95], t[k, m], b[k], c[k]
			}
		}' "$LAT" | sort -k1,1 -k2,2 >"$SUM"
}
summarize
{
	log ""
	log "=================================================================================="
	log "## 汇总表（延时单位 ms；最近秩法取分位）"
	log "=================================================================================="
	printf '%-16s %-10s %5s %8s %8s %8s %8s %10s %6s\n' phase endpoint n avg p50 p95 max bytes http | tee -a "$LOG"
	awk -F'\t' '{printf "%-16s %-10s %5s %8s %8s %8s %8s %10s %6s\n", $1,$2,$3,$4,$5,$6,$7,$8,$9}' "$SUM" | tee -a "$LOG"
}

# 放大关系：每行 1000 行数据带来的增量（ms），以及外推到 1s 需要多少行。
scale_line() {
	local lbl="$1" ep="$2" n0="$3" n1="$4" rows0="$5" rows1="$6"
	awk -F'\t' -v lbl="$lbl" -v ep="$ep" -v n0="$n0" -v n1="$n1" -v r0="$rows0" -v r1="$rows1" '
		$2 == ep && $1 == n0 { a = $6 }
		$2 == ep && $1 == n1 { b = $6 }
		END {
			if (a == "" || b == "") { printf "  %-34s 无数据\n", lbl; exit }
			d = b - a
			slope = (r1 > r0) ? d / (r1 - r0) : 0
			cross = (slope > 0) ? 1000 / slope : 0
			printf "  %-34s p95: %8.2f ms → %8.2f ms（Δ %+8.2f ms，%+.2fx）；每行 %+.5f ms；线性外推撞 1s ≈ %.0f 行\n",
				lbl, a, b, d, (a > 0 ? b / a : 0), slope, cross
		}' "$SUM"
}


# ---------------------------------------------------------------------------
# 10. finding.md
#
# 写法：正文先落到**引号 heredoc** 的模板里（`<<'MD'`，不做任何展开——markdown 里
# 全是反引号代码跨度，非引号 heredoc 会把它当命令替换执行），动态值用 {{KEY}}
# 占位，最后用 perl 做一次替换。值里的换行在 vars 文件里写成字面 \n。
# ---------------------------------------------------------------------------
TPL="$RAW/finding.tpl"
VARS="$RAW/finding.vars"
: >"$VARS"

put_var() { # put_var KEY VALUE（多行值 → 单行 \n 转义）
	printf '%s\t%s\n' "$1" "$(printf '%s' "$2" | awk '{ if (NR > 1) printf "\\n"; printf "%s", $0 }')" >>"$VARS"
}

EV_ROWS=$(${SQL3[@]} "$PC_DB" "SELECT count(*) FROM events;")
TK_ROWS=$(${SQL3[@]} "$PC_DB" "SELECT count(*) FROM tasks;")
EDGE_ROWS=$(${SQL3[@]} "$PC_DB" "SELECT count(*) FROM edges;")
DBSZ_MB=$(( $(stat -c %s "$PC_DB") / 1024 / 1024 ))
GITREV=$(git -C "$PC_REPO" rev-parse --short HEAD 2>/dev/null || echo '?')

# --- 延迟表：端点 × 四档 ---
TBL_LAT="$(
	for ep in health mev1 mev500 mev50 cev100 cev1000 trace tasks200; do
		for ph in p0_empty p1_10k p2_100k p3_100k_noanalyze; do
			awk -F'\t' -v p="$ph" -v e="$ep" '
				$1==p && $2==e { printf "| %s | %s | %.2f | %.2f | %.2f | %s | %s |\n", e, p, $5, $6, $7, $8, $9 }' "$SUM"
		done
	done
)"
put_var TBL_LAT "$TBL_LAT"

# --- LIMIT 线性表（50 vs 500，同端点同源）---
put_var TBL_LIMIT "$(awk -F'\t' '
	$2=="mev500" { b[$1]=$5; b95[$1]=$6; bb[$1]=$8 }
	$2=="mev50"  { a[$1]=$5; a95[$1]=$6; ab[$1]=$8 }
	END {
		n=split("p0_empty p1_10k p2_100k p3_100k_noanalyze", ks, " ")
		for (i=1;i<=n;i++) { k=ks[i]; if (b[k]=="") continue
			printf "| %s | %.2f | %.2f | %+.1f ms | %.1fx | %d → %d B |\n",
				k, a[k], b[k], b[k]-a[k], bb[k]/ab[k], ab[k], bb[k] }
	}' "$SUM")"
put_var DELTA500_50 "$(awk -F'\t' '$1=="p2_100k"&&$2=="mev500"{a=$5; ab=$8} $1=="p2_100k"&&$2=="mev50"{b=$5; bb=$8} END{printf "%+.1f ms", a-b}' "$SUM")"
put_var BPS500_50 "$(awk -F'\t' '$1=="p2_100k"&&$2=="mev500"{a=$5; ab=$8} $1=="p2_100k"&&$2=="mev50"{b=$5; bb=$8} END{printf "%.1f", (a>b)?(ab-bb)/1024/(a-b):0}' "$SUM")"
put_var RATIO500_50 "$(awk -F'\t' '$1=="p2_100k"&&$2=="mev500"{a=$5} $1=="p2_100k"&&$2=="mev50"{b=$5} END{printf "%.2f", (b>0)?a/b:0}' "$SUM")"

# --- 数据量形貌表 + 外推 ---
put_var TBL_SCALE "$(awk -F'\t' '
	{ if ($1=="p0_empty") a[$2]=$6; else if ($1=="p1_10k") b[$2]=$6; else if ($1=="p2_100k") c[$2]=$6 }
	END {
		n=split("health mev1 mev500 mev50 cev100 cev1000 trace tasks200", es, " ")
		for (i=1;i<=n;i++) { e=es[i]
			shape = (b[e]>0 && c[e]>0) ? (c[e]/b[e] >= 1.3 ? "随规模单调增长" : "涨到位后平台化") : "—"
			printf "| %s | %.2f | %.2f | %.2f | %s |\n", e, a[e], b[e], c[e], shape }
	}' "$SUM")"
put_var SCALE_LINES "$(scale_line 'tasks200（tasks 表，0 → 2 万行）' tasks200 p0_empty p2_100k 0 20000)"
SC_TASKS="$(scale_line 'tasks200' tasks200 p0_empty p2_100k 0 20000 | sed 's/^  *//')"
SC_MEV="$(scale_line 'mev500' mev500 p0_empty p2_100k 0 100000 | sed 's/^  *//')"
put_var SC_TASKS "$SC_TASKS"
put_var SC_TASKS_ROWS "$(printf '%s' "$SC_TASKS" | sed -n 's/.*撞 1s ≈ \([0-9]*\) 行/约 \1 行/p' | sed 's/^/约 /; s/约 约 /约 /')"
put_var SC_MEV "$SC_MEV"

# --- ANALYZE 有/无对照 ---
put_var TBL_ANALYZE "$(awk -F'\t' '
	{ if ($1=="p2_100k") a[$2]=$6; else if ($1=="p3_100k_noanalyze") b[$2]=$6 }
	END {
		n=split("health mev1 mev500 mev50 cev100 cev1000 trace tasks200", es, " ")
		for (i=1;i<=n;i++) { e=es[i]; d=(a[e]>0)?(b[e]/a[e]):0
			printf "| %s | %.2f | %.2f | %+.2fx |\n", e, a[e], b[e], d-1 }
	}' "$SUM")"

# --- EXPLAIN 原文（从 run.log 里截出来，保证 finding 里的计划是**原始输出**）---
# eqp_block <ANALYZE-on|ANALYZE-off> <Q<n>>：按 ID 精确截取那一节的原始计划。
# awk '!seen[$0]++' 去重是因为 log() 自己 tee 了一份、外层块又 tee 了一份（同内容重复写）。
eqp_block() {
	sed -n "/### \[$1\] $2 /,/^$/p" "$LOG" | grep -vE '^$' | awk '!seen[$0]++' | head -14
}
put_var PLAN_MEV "$(eqp_block ANALYZE-on Q1)"
put_var PLAN_MEV50 "$(eqp_block ANALYZE-on Q2)"
put_var PLAN_CEV100 "$(eqp_block ANALYZE-on Q3)"
put_var PLAN_TASKS "$(eqp_block ANALYZE-on Q7)"
put_var PLAN_TRACE_EV "$(eqp_block ANALYZE-on Q4)"
put_var PLAN_TRACE_TK "$(eqp_block ANALYZE-on Q5)"
put_var PLAN_BYTRACE "$(eqp_block ANALYZE-on Q6)"
put_var PLAN_REDISPATCH "$(eqp_block ANALYZE-on Q8)"
put_var PLAN_EDGES "$(eqp_block ANALYZE-on Q9)"
put_var PLAN_GHOST "$(eqp_block ANALYZE-on Q10)"
put_var PLAN_TASKS_NOAN "$(eqp_block ANALYZE-off Q7)"
put_var PLAN_MEV_NOAN "$(eqp_block ANALYZE-off Q1)"
put_var INDEXLIST "$(sed -n '/\[ANALYZE-on\] sqlite_master 索引清单/,/^$/p' "$LOG" | grep ' | ' | awk '!seen[$0]++' | sed 's/^  *//')"
put_var SQLIDX "$(grep -n 'idx_edges_lookup' "$PC_REPO/internal/store/schema.sql" | sed 's/^/    /')"
put_var EDGECOLS "$(${SQL3[@]} "$PC_DB" "SELECT group_concat(name, ', ') FROM pragma_table_info('edges');")"
put_var QTIME "$(cat "$RAW"/qtime-*.txt 2>/dev/null | grep -E 'Run Time' | sed 's/^/    /')"
put_var PROBE_SCOPE "$(grep -A3 '附：GET /v1/events 的鉴权口径' "$LOG" | grep -E 'RECV_KEY|SEND_KEY' | awk '!seen[$0]++')"
put_var EXTRAP_TASKS "$(awk -F'\t' '
	$1==2000 { a=$2 } $1==20000 { b=$2 }
	END {
		if (a > 0 && b > a) {
			slope = (b - a) * 1000 / 18000          # ms/行
			n = 20000 + (1000 - b * 1000) / slope   # SQL 部分自己到 1s 的行数
			printf "%.0f 行（斜率 %.4f ms/行；2,000 行 %.1f ms → 20,000 行 %.1f ms）\n", n, slope, a * 1000, b * 1000
		} else { print "（数据不足，未外推）" }
	}' "$RAW/curve-tasks.tsv" 2>/dev/null)"
med() { sort -g | awk '{a[NR]=$1} END{ if(NR) print a[int((NR+1)/2)] }'; }
put_var HEALTH_P95 "$(awk -F'\t' '$1=="p2_100k"&&$2=="health"{printf "%.2f", $6}' "$SUM")"
put_var MEV1_P95 "$(awk -F'\t' '$1=="p2_100k"&&$2=="mev1"{printf "%.2f", $6}' "$SUM")"
put_var MEV500_P95 "$(awk -F'\t' '$1=="p2_100k"&&$2=="mev500"{printf "%.2f", $6}' "$SUM")"
put_var MEV500_BYTES "$(awk -F'\t' '$1=="p2_100k"&&$2=="mev500"{print $8}' "$SUM")"
put_var QT_MEV500 "$(sed -n 's/^Run Time: real //p' "$RAW/qtime-mev500.txt" 2>/dev/null | awk '{print $1*1000}' | med | awk '{printf "%.1f", $1}')"
put_var QT_TASKS "$(sed -n 's/^Run Time: real //p' "$RAW/qtime-tasks200.txt" 2>/dev/null | awk '{print $1*1000}' | med | awk '{printf "%.1f", $1}')"
put_var QT_REDISPATCH "$(sed -n 's/^Run Time: real //p' "$RAW/qtime-redispatch.txt" 2>/dev/null | awk '{print $1*1000}' | med | awk '{printf "%.1f", $1}')"
put_var TASKS200_LINE "$(awk -F'\t' '$2=="tasks200"{printf "%s %.2f ms；", $1, $6}' "$SUM")"
put_var QUEUE0 "$(${SQL3[@]} "$PC_DB" "SELECT count(*) FROM tasks WHERE status='pending' AND node_id IS NULL;")"
put_var TASKS_AT_MEASURE "$TK_ROWS"
put_var CURVE_EVENTS "$(awk -F'\t' '{printf "%s 行 → %s s；", $1, $2}' "$RAW/curve-events.tsv" 2>/dev/null | sed 's/；$//')"
put_var CURVE "$(sed -n '/7.5 缩放曲线/,/^## 8\./p' "$LOG" | grep -E 'tasks=|events=|\|--|`--' | awk '!seen[$0]++' | sed 's/^  *//')"
put_var PROBE_LIMIT "$(grep -A5 '附：limit 口径' "$LOG" | grep 'limit=' | awk '!seen[$0]++')"
put_var PORT "$PORT"
put_var CPUS "$PC_CPUS"
put_var NREQ "$N_REQ"
put_var OUTD "$OUT"
put_var RAWD "$RAW"
put_var EVROWS "$EV_ROWS"
put_var TKROWS "$TK_ROWS"
put_var EDGEROWS "$EDGE_ROWS"
put_var DBSZ "$DBSZ_MB"
put_var GITREV "$GITREV"
put_var DATEISO "$(date -Is)"

cat >"$TPL" <<'MD'
## 读路径：10 万事件 / 2 万任务下 `GET /v1/tasks` 随任务数**单调变差**（全表扫描 + 临时 B 树排序，没有任何索引能救它），而 events 侧靠 `idx_events_retention` 把代价压在 LIMIT 上、与总行数无关

- 严重度：major（`tasks` 列表是唯一一条**在线读路径上的全表扫描**，且随数据量单调增长，叠加 C2 的「无保留期」会一直恶化）；events 侧 minor
- 场景：C5 慢查询（契约 §3）· 分支 `{{GITREV}}` · {{DATEISO}}
- 复现命令：
  ```bash
  bash scripts/perf/c/s5-slowquery.sh     # 端口 {{PORT}}，分核 {{CPUS}}，自己 pc_start/pc_seed/pc_cap/造数/重启/测量
  ```
  数据量三档由脚本内部完成（events 0 → 10,000 → 100,000；tasks 0 → 2,000 → 20,000），
  每档 8 条读路径（6 业务 + 2 底噪对照）× {{NREQ}} 次 `curl -w '%{time_total}\t%{size_download}\t%{http_code}'`。
- 数字：见第 1/3 节两张纵向对比表（0 / 1 万 / 10 万 + ANALYZE 有/无）
- 原始证据：`{{RAWD}}/run.log`（全过程 + 每条 `EXPLAIN QUERY PLAN` 原文）、`{{RAWD}}/latency.tsv`（每档每端点 {{NREQ}} 条原始取样）、
  `{{RAWD}}/summary.tsv`（汇总）、`{{RAWD}}/qtime-*.txt`（sqlite3 直跑 `.timer` 原始输出）
- 造数方式：**不走接口**（接口实测 ~35 ev/s，10 万条要 48 分钟）。停服务后用 `sqlite3` + recursive CTE 单语句直插；
  插完 `PRAGMA wal_checkpoint(TRUNCATE)` + `ANALYZE`（插入耗时在 run.log 第 2、3 节）。
  **ANALYZE 的有/无两态都测了**（p2_100k vs p3_100k_noanalyze），结论见第 3 节末。
- 库规模：events={{EVROWS}} / tasks={{TKROWS}} / edges={{EDGEROWS}}，DB 文件 {{DBSZ}} MB

---

## 1. 各读路径的延迟（四档 × 8 端点，各 {{NREQ}} 次串行取样）

p50/p95/max 用最近秩法（第 ⌈p·n⌉ 个样本）；单位 ms。每项先跑 3 次预热不计入。

| 端点 | 数据量档 | p50 | p95 | max | 响应字节 | http |
|---|---|---|---|---|---|---|
{{TBL_LAT}}

端点含义（都带 `namespaceId=<ns>`）：
`mev500` = `GET /v1/events?limit=500`（Bearer **SEND_KEY**——这个读端点要 `notify:send`，见 §6.1；机器入口 `api.EventHandler`）· `mev50` = 同端点 `limit=50` ·
`cev100` / `cev1000` = `GET /v1/console/events?limit=100|1000`（会话 Cookie）·
`trace` = `GET /v1/traces/trace_bulk_0001`（Cookie；该 trace 有 10 条事件 + 2 个任务）·
`tasks200` = `GET /v1/tasks?limit=200`（Cookie）。
两条**底噪对照**（用来把「SQL 的代价」从「每请求都跑一遍 argon2id 的代价」里剥出来）：
`health` = `GET /health`（无鉴权、无 SQL）；`mev1` = `GET /v1/events?limit=1`（有 Bearer 鉴权 + 只取 1 行）。
`mev1` 与 `mev500` 的差就是「多取 499 行 + 多序列化 238KB」的增量，`health` 与 `mev1` 的差就是 argon2id 的固定税。
`p3_100k_noanalyze` = 同一份 10 万行数据，只是把 `sqlite_stat1` 清空（ANALYZE 关掉）后重启再测，用来做口径对照。

### 1.0 先说底噪：机器读端点的几十毫秒是 **argon2id**，不是 SQL

| 对照 | p95（10 万档） | 说明 |
|---|---|---|
| `GET /health`（无鉴权、无 SQL） | {{HEALTH_P95}} ms | HTTP + 路由的纯底噪 |
| `GET /v1/events?limit=1`（Bearer + 1 行） | {{MEV1_P95}} ms | + argon2id 验证（每请求一次，**这才是大头**） |
| `GET /v1/events?limit=500`（Bearer + 500 行 / {{MEV500_BYTES}} B） | {{MEV500_P95}} ms | + 多 499 行 + 238KB 序列化 |
| 同一条 SQL 在 sqlite3 里直跑（500 行） | **{{QT_MEV500}} ms** | 真正的库内耗时（run.log 第 7 节 `qtime-mev500` 中位） |

也就是说：**`GET /v1/events` 的 p95 里 95%+ 是 API Key 的 argon2id 校验**（与契约 §6 记录的现象同一个根因），
SQL 只占 {{QT_MEV500}} ms。所以本场景报 events 侧的「慢查询」是**没有依据**的——它的慢与数据量无关、与鉴权方式有关。
（200 行任务列表 `GET /v1/tasks` 走的是会话 Cookie，**没有 argon2**，所以它的耗时里 SQL 占 {{QT_TASKS}} ms，是真账。）

### 1.1 LIMIT 的线性：**字节**与 limit 成正比（10x），**耗时**不成比例（{{RATIO500_50}}x）

| 档位 | limit=50 p50 | limit=500 p50 | 差 | 字节比（500/50） | 响应字节 |
|---|---|---|---|---|---|
{{TBL_LIMIT}}

- 同端点、同源、只改 LIMIT：10 万档的 p50 差 **{{DELTA500_50}}**，约合 **{{BPS500_50}} MB/s** 的
  「序列化 + 传输」速率（= 字节差 / 时间差）—— 也就是说这点差值是**响应体变大**的钱，不是库变慢的钱。
- p95 上两列甚至互有胜负（argon2 的 ±10ms 抖动比这 10ms 差别还大），
  所以**不能拿 HTTP p95 去论证 SQL 的代价**——这正是 §3.1 要单开一节在 sqlite3 里量的原因。
- 库内证据：`ListEvents(limit=500)` 在 **1 千 / 1 万 / 10 万** 三档行数下 sqlite3 直跑分别是
  `{{CURVE_EVENTS}}`（§3.1）—— 一条平线。`idx_events_retention(namespace_id, created_at)` 让 SQLite
  从索引尾部反向扫，LIMIT 一到就停，**代价与 events 表总行数无关**。

---

## 2. EXPLAIN QUERY PLAN（原文，ANALYZE 之后）

判定口径：`SEARCH … USING INDEX idx_x` = 走索引；`SCAN xxx` = 顺序扫描（不带 INDEX 就是整个 namespace 扫）；
`USE TEMP B-TREE FOR ORDER BY` = 排序交给临时 B 树，代价 O(N log N)、**与总行数同阶**。

| # | 真实 SQL 出处 | 走哪个索引 | 排序 |
|---|---|---|---|
| Q1 | `internal/store/event.go:233` ListEvents（事件流，无 traceId） | `idx_events_retention` | TEMP B-TREE（分块） |
| Q2 | 同上 + LIMIT=50 | 同上 | 同上 |
| Q3 | `internal/server/console_handlers.go:296` → 同一 ListEvents | 同上 | 同上 |
| Q4 | ListEvents 带 traceId（trace 视图，`console_handlers.go:450`） | `idx_events_trace` | TEMP B-TREE，但只排**单 trace 的那几行**（本场景 10 行） |
| Q5 | ListTasks 带 traceId（`console_handlers.go:456`） | `idx_tasks_trace` | 同上（2 行） |
| Q6 | `internal/store/event.go:268` GetEventByTrace | `idx_events_trace` | 同上（LIMIT 1） |
| Q7 | `internal/store/task.go:318` ListTasks（任务列表，无过滤条件） | **无（`SCAN tasks`）** | **TEMP B-TREE** |
| Q8 | `internal/store/task.go:1072` ListRedispatchable（后台巡检） | 见 §Q8 | **TEMP B-TREE**（按 COALESCE 表达式） |
| Q9 | `internal/store/graph.go:211` EdgesFrom（派发查边） | **COVERING INDEX `sqlite_autoindex_edges_1`**（PK 前四列与 `idx_edges_lookup` 相同） | ✓ 无 |

### Q1 / Q2 / Q3 事件流 `GET /v1/events?limit=500` / `?limit=50` / `/v1/console/events?limit=100`

```
{{PLAN_MEV}}
```

```
{{PLAN_MEV50}}
```

控制台那条（`GET /v1/console/events?limit=100`，`console_handlers.go:296` → 同一个 `ListEvents`）计划一模一样，
只是 LIMIT 是 100：

```
{{PLAN_CEV100}}
```

读法：`SEARCH events USING INDEX idx_events_retention (namespace_id=?)` = 用索引定位到本命名空间的尾部；
`USE TEMP B-TREE FOR ORDER BY` 是因为 `ORDER BY created_at DESC, id DESC` 的**第二排序键 `id` 不在索引里**
（索引只有 `(namespace_id, created_at)`，`internal/store/migrate.go:513`）。
但 SQLite 走的是「分块排序」：先按索引拿到有序的 `created_at` 块，块内再按 id 排，所以**代价压在 LIMIT 行数上**，
不是整表——这正是 §1.1 里「字节 10x、耗时 {{RATIO500_50}}x」的来源。

### Q7 任务列表 `GET /v1/tasks?limit=200`（唯一的全表扫描）

```
{{PLAN_TASKS}}
```

`SCAN tasks` —— 没有索引参与，扫的是**本命名空间的任务行**（`WHERE namespace_id = ?` 只能靠行上过滤或走
`idx_tasks_retention` 的窄前缀，然后 `USE TEMP B-TREE FOR ORDER BY` 把命中的行全排一遍）。
根因是 `tasks` 表**没有任何 `(namespace_id, created_at)` 索引**：
现存的五条都是 `(namespace_id, trace_id)` / `(namespace_id, parent_task_id)` / `(status, lease_until)` /
`(node_id, status)` / `(namespace_id, capability_id, status, finished_at)`（见 §4 的 `sqlite_master` 清单），
全都提供不了 `ORDER BY created_at DESC` 的序。

关掉统计信息后计划**没有本质变化**（见 §3 末的两态表）：

```
{{PLAN_TASKS_NOAN}}
```

### Q8 后台巡检 `ListRedispatchable`（`internal/store/task.go:1072`）

```
{{PLAN_REDISPATCH}}
```

`ORDER BY COALESCE(not_before, 0), created_at, id` —— **表达式排序天然吃不到索引**，
所以 `USE TEMP B-TREE FOR ORDER BY` 是必然的。但**它没到要报的程度**，实测三点：
① 等值条件 `node_id IS NULL AND status='pending'` 落在 `idx_tasks_node(node_id, status)` 上，是选择性的；
② 代价只与**待派队列长度**（`node_id IS NULL` 的 pending 行数）成正比，**与 tasks 表总量无关**——
本场景 {{TASKS_AT_MEASURE}} 个任务里待派队列（`status='pending' AND node_id IS NULL`）是 **{{QUEUE0}} 行**，
sqlite3 直跑中位 **{{QT_REDISPATCH}} ms**（run.log 第 7 节 `qtime-redispatch`）；
③ 它**不带 namespaceId**（注释写明是刻意的：全库重派职责），所以是跨命名空间的一次索引查找。
结论：**info 级**。只有当「同一次派发积压了几万个待派任务」时，这条的排序才会变成尖峰
（那是队列深度问题，不是表规模问题）。
真正跟表规模有关的后台动作是同一条 ticker 上的保留清理（`internal/lease/sweeper.go:176` 起的
`maybeSweepRetention`，低频门默认 1 小时一轮，随后按批 DELETE）——它已经在 #143 里被专门加过索引。

### Q4 / Q5 / Q6 trace 视图与 GetEventByTrace（唯一健康的一组）

```
{{PLAN_TRACE_EV}}
```

```
{{PLAN_TRACE_TK}}
```

```
{{PLAN_BYTRACE}}
```

三条都靠 `(namespace_id, trace_id)` 等值定位到 `idx_events_trace` / `idx_tasks_trace`。
注意它们**仍然有 `USE TEMP B-TREE FOR ORDER BY`**——因为这两条索引提供不了 `created_at` 的序；
但排序的对象只是**单条 trace 命中的那几行**（本场景 10 事件 / 2 任务），
代价与 events/tasks 表的**总量无关**（实测 1.4–2.1ms 一条平线，见第 3 节）。
这也说明「有 TEMP B-TREE」本身不等于问题：**要看排序的输入是不是随规模增长**。

### Q9 派发查边 `EdgesFrom`（`internal/store/graph.go:211`）

```
{{PLAN_EDGES}}
```

实际走的是 `COVERING INDEX sqlite_autoindex_edges_1` —— 即 `edges` 的主键
`PRIMARY KEY (namespace_id, from_is_source, from_key, from_port, to_capability)` 的自动索引，
前四列与 WHERE 逐字对齐，而且**覆盖**（`to_capability` 就在索引里，不用回表）。
也就是说 `idx_edges_lookup` 在查询计划里其实是冗余的（详见 §4.1）。

---

## 3. 数据量放大：谁"平台化"、谁"单调变差"

| 端点 | 0 条 p95 | 1 万条 p95 | 10 万条 p95 | 1万→10万 的形貌 |
|---|---|---|---|---|
{{TBL_SCALE}}

（任务表对应 0 / 2 千 / 2 万行；events 端点为 0 / 1 万 / 10 万行。）

外推 —— **只对 tasks 做**。events 侧不能这么外推：`GET /v1/events` 的 p95 里有个 40ms 量级的
argon2id 常数项（§1.0），拿它当斜率会把「1s 需要 2900 万条事件」这种没意义的数字算出来；
events 侧的正确口径是 §3.1 的**纯 SQL 曲线**（贴着 2ms 的平线）：

```
{{SCALE_LINES}}
```

- `tasks200` 是**单调增长**：每多一个任务，列表查询就多扫一行、多排一行。
  按契约 §5 的判据，它属于「随时间/数据量单调增长」，不是「一次性涨到位后平台化」。
- `mev500` / `cev1000` 基本持平：`idx_events_retention` 提供了 `created_at` 的序，LIMIT 一到就停。
- `cev100` 那一行虽然被判定成「随规模单调增长」（1.82 → 2.41 ms），但它的**响应字节在 1 万/10 万档几乎一样**
  （37406 → 37502 B，+0.3%），差值属于测量噪声；`cev1000` 同理（186846 → 187300 B）。别把这两个 0.3% 当规模效应。
- 两条外推口径的差异（p95 斜率 {{SC_TASKS_ROWS}} vs 纯 SQL 曲线 {{EXTRAP_TASKS}}）来自
  「0 行基线里混着 ~1ms 的固定开销」——分母被摊薄，所以 p95 斜率口径**偏乐观**。
  以纯 SQL 曲线为准（§4.2）。

### 3.1 行数 → **纯 SQL** 耗时的缩放曲线（把 argon2 与 JSON 序列化剥掉）

HTTP 侧的数字被两件事盖住了：`GET /v1/events` 每请求要跑一遍 argon2id（`GET /health` {{HEALTH_P95}}ms vs
`GET /v1/events?limit=1` {{MEV1_P95}}ms，见 §1.0），以及 500 行的 JSON 序列化。所以这里在**库的副本上**
按行数删减 + `VACUUM`，直接量 store 的那条 SQL（中位耗时，run.log 第 7.5 节）：

```
{{CURVE}}
```

两条曲线的形状完全不同：**tasks 随行数近似线性（甚至略超线性：临时 B 树排序要物化所有命中行），
events 基本是一条平线（LIMIT 一到就停）**。这就是「同一台机器、同一份 DB、同一个 sqlite3，
差别只在有没有覆盖 ORDER BY 的索引」的直接证据。

**ANALYZE 的有/无对照**（同一份数据，只清掉 `sqlite_stat1`）：

| 端点 | p2_100k（ANALYZE 后）p95 | p3_100k_noanalyze（无统计）p95 | 差 |
|---|---|---|---|
{{TBL_ANALYZE}}

```
{{PLAN_MEV_NOAN}}
```

结论：**ANALYZE 不是这条链路的解药**。events 侧两态都走 `idx_events_retention`（差异在噪声内）；
tasks 侧两态都是 `SCAN tasks` + `TEMP B-TREE`——缺的是**索引**，不是统计信息。
（这也是"公平起见两态都测"的意义：若不测无统计态，很容易把「有/无 ANALYZE 没差别」误读成"与数据量无关"。）

### sqlite3 直跑的真实执行时间（剥离 HTTP + argon2 底噪）

```
{{QTIME}}
```

---

## 4. 索引缺口：这是「为什么随数据量变差」的根

`sqlite_master` 实测清单（原文见 `{{RAWD}}/run.log` 第 4 节）：

```
{{INDEXLIST}}
```

三条具体缺口：

1. **`tasks` 没有 `(namespace_id, created_at)` 索引** —— `internal/store/task.go:318` 的
   `ORDER BY created_at DESC, id DESC LIMIT ?` 只能整表（按 namespace）扫 + 临时 B 树排序。
   现存 `idx_tasks_retention(namespace_id, capability_id, status, finished_at)` 的第二列是
   `capability_id`，拿不到 `created_at` 的序，所以等于没用。
2. **`events` 的 `idx_events_retention` 覆盖 `created_at` 但不覆盖 `id`** ——
   `internal/store/event.go:241` 的 `ORDER BY created_at DESC, id DESC` 仍需排序，
   只是被 SQLite 的分块排序压在 LIMIT 行数内。属于 minor（现在不疼，但排序键不完整，
   换 `PRAGMA` / SQLite 版本时要重新验一遍）。
3. **`source != 'route'` 无索引**（`internal/store/event.go:236`）—— 它是索引反向扫行的**行上过滤**。
   本场景刻意掺了 2% 的 `source='route'` 行（模拟 #142 的剪枝事件），仍只多扫约 2% 的行，不是问题；
   但它意味着「返回字节数」与「索引扫描行数」不是 1:1。

### 4.1 `idx_edges_lookup` 声明了两次：第二条**不存在**，而且**连列都没有了**

`internal/store/schema.sql` 里同名索引出现两次，列不同：

```
{{SQLIDX}}
```

实测（run.log 第 5 节）：

- 库里 `edges` 的索引**只有一条** `idx_edges_lookup`，列是 `(namespace_id, from_is_source, from_key, from_port)`，
  即第 288 行那条（注意 schema.sql 里写的是历史列名 `org_id`，migration 12 重建时换成了 `namespace_id`；
  见 `internal/store/migrate.go` 的 edges 重建段与 `idx_events_retention` 那条迁移）。
- 第 290 行那条 `(namespace_id, from_capability, from_port)` **不存在，且已不可能存在**：
  migration 12（`internal/store/migrate.go` 的 `migrateNamespaces`，edges 重建段）把 `edges` 换成了
  `from_is_source + from_key` 方案，`from_capability` 列被删掉。`edges` 现在的列是：
  `{{EDGECOLS}}`。直接执行第 290 行那条 `CREATE INDEX` 会报 `no such column: from_capability`
  （错误原文在 run.log 第 5 节）。
- 于是「派发时按 `from_capability` 查边会不会全表扫描」这个问题**本身已经不成立**：
  全仓唯一的查边入口是 `internal/store/graph.go:211 EdgesFrom`，条件是
  `namespace_id = ? AND from_is_source = ? AND from_key = ? AND from_port = ?`，
  与第 288 行那条索引的前四列逐字对齐，实测走索引（§Q9）。
  也就是说：**这条索引缺口在当前代码里不存在**，只留下两条非性能隐患——
  ① `schema.sql` 里那句已经跑不通的 `CREATE INDEX`（谁把它捡回来当迁移跑就会炸）；
  ② 同名第二次 `IF NOT EXISTS` 会被静默跳过，正是它藏了这么久的原因。

### 4.2 影响判断：单机多少数据量下哪个接口先撞 1s

- **`GET /v1/tasks` 先撞**。实测 HTTP p95（任务数 0 / 2 千 / 2 万 → 对应四个测量档）：
  `{{TASKS200_LINE}}` 同一份库上 sqlite3 直跑 `ListTasks(limit=200)` 中位 **{{QT_TASKS}} ms**
  （20k 行，`qtime-tasks200`）。纯 SQL 缩放曲线见 §3.1。
  按 **2 千 → 2 万档的实测斜率**线性外推，**SQL 自己**撞 1s 需要 {{EXTRAP_TASKS}}
  （JSON 序列化那 108KB 是常量 ~14ms，摊薄后可忽略）。
- events 侧（`/v1/events`、`/v1/console/events`）走索引 + 块排序，**与总行数无关**：
  纯 SQL 曲线 1 千 → 10 万行都是 `{{CURVE_EVENTS}}`（§3.1，一条平线）。
  它的 p95 绝对值仍有 {{MEV500_P95}} ms，但那是 argon2id 的固定税（§1.0），**不是「撞 1s 的慢查询」**，
  而且**不会随数据量增长**——这两件事在排障时不能混为一谈。
- 形状判据（契约 §5）：tasks 列表 = **随时间/数据量单调增长**；events 列表 = **平台化**；
  `GET /v1/traces/{id}` = 与总量无关（单 trace 查询）。
- 与 C2 的结论叠加：代码里没有保留期/清理机制，任务表只增不减 → 这条 p95 会一直往上走，
  不会自我收敛。
- 另有一条**不进 HTTP** 的读路径：`ListRedispatchable`（§Q8）每 5 秒重排一次待派队列
  （`internal/store/task.go:38` 的 `LeaseSweepIntervalSec = 5`）。队列为空时它只要 {{QT_REDISPATCH}} ms，
  但**积压几万个待派任务**时这里会先卡——表现为「派发变慢 / CPU 底噪升高」，从接口延迟上看不出来。

---

## 5. 修复建议（只给建议，本场景不改代码）

1. `tasks` 补 `CREATE INDEX idx_tasks_list ON tasks(namespace_id, created_at DESC, id DESC)`，
   让 `store/task.go:318` 的 ORDER BY 直接由索引满足（这是本次唯一 major 的直接解）。
2. `events` 补 `(namespace_id, created_at, id)`，把块内排序也消掉（minor，可与上一条同一条迁移）。
3. `store/task.go:1072` 的 `ORDER BY COALESCE(not_before, 0)` 建议落成实体列
   （`not_before` 改 NOT NULL DEFAULT 0 即可去掉 COALESCE）后建
   `(status, node_id, not_before, created_at)`，否则**待派队列一长**，每 tick 的排序就会变成尖峰
   （现在队列为 0 时只要 {{QT_REDISPATCH}} ms，所以是 info 级）。
4. 删掉 `schema.sql:290` 那条已无法执行的 `idx_edges_lookup`，并给 schema 加一条「索引名全局唯一」
   的自检（或迁移里断言 `sqlite_master` 无重名）。
5. `GET /v1/console/events?limit=1000` **静默降级成 500 行**（`console_handlers.go:49` 的
   `listLimitCap = 500`，且 `internal/store/event.go:230` 里还有一份 500 的重复常量）：
   建议响应里带 `limitApplied`，或把两处 500 收成一处常量——现在调用方无法从响应看出自己被削了一半。

---

## 6. 附带发现（测量过程中撞到的两个契约问题，独立成条）

### 6.1 `GET /v1/events`（机器读端点）要求的是 **`notify:send`** scope，用 `notify:receive` 的 key 拿 403

实测（本脚本每次运行都会打这两行，见 `{{RAWD}}/run.log`）：

```
{{PROBE_SCOPE}}
```

- 出处：`internal/api/events.go:77` —— `authn.Authenticate(r, h.Keys, authn.ScopeNotifySend)`。
  这个 handler 同时挂着 `POST /v1/events`（写入）与 `GET /v1/events`（读取）（`internal/server/mux.go:262`），
  而鉴权在方法分支之前统一按 **send** 校验，于是「只该读事件」的 `notify:receive` key 读不了事件流。
- 语义方向是反的：`cmd/devseed/main.go` 给 recv key 的注释就是 `notify:receive`，
  它连最基础的「拉自己事件的」都做不到，只能写不能读。
- 严重度：minor（有 workaround：换 send key；但 scope 模型与端点语义对不上，外部集成方会踩）。
- 影响判断：**与数据量无关**，是一次性的鉴权判定（但仍要跑一遍 argon2id，见 §1 注）。

### 6.2 `limit` 的上限在三处口径不一致：机器入口 `limit=501..1000` 会**静默变回 50 行**

实测（`{{RAWD}}/run.log` 第 4 节前的「附：limit 口径」段）：

```
{{PROBE_LIMIT}}
```

- 三处上限：`internal/api/events.go:196-201` 允许到 **1000**；`internal/store/event.go:230-232`
  的硬上限是 **500**，且超过时**不是钳到 500 而是打回 50**；`internal/server/console_handlers.go:49`
  的 `listLimitCap` 是 **500**（控制台先钳到 500，所以控制台侧最多少给、不会崩到 50）。
- 后果：机器入口传 `limit=1000`（一个完全合法的、被 handler 接受的参数）实际只返回 **50** 行 ——
  比传 `limit=100` 还少一半。调用方无法从响应里看出自己被降级了（响应里没有 `limitApplied`）。
- 严重度：minor（不会 500，但会静默丢数据；分页/巡检类客户端会误判「只有 50 条事件」）。
- 归因 file:line：`internal/api/events.go:196`（`if n > 0 && n <= 1000`） vs
  `internal/store/event.go:230`（`if limit <= 0 || limit > 500 { limit = 50 }`）。
MD

perl -e '
	my ($tpl, $vf) = @ARGV;
	open my $V, "<", $vf or die "vars: $!";
	my %v;
	while (my $l = <$V>) {
		chomp $l;
		next unless length $l;
		my ($k, $val) = split /\t/, $l, 2;
		$val = "" unless defined $val;
		$val =~ s/\\n/\n/g;
		$v{$k} = $val;
	}
	close $V;
	open my $T, "<", $tpl or die "tpl: $!";
	local $/;
	my $s = <$T>;
	my @missing;
	$s =~ s/\{\{([A-Z0-9_]+)\}\}/exists $v{$1} ? $v{$1} : do { push @missing, $1; "{{$1}}" }/ge;
	print $s;
	print STDERR "s5: 未替换的占位符: @missing\n" if @missing;
' "$TPL" "$VARS" >"$OUT/finding.md"

hr "完成"
log "finding : $OUT/finding.md"
log "raw     : $RAW/run.log ($(wc -l <"$LOG") 行) / latency.tsv ($(wc -l <"$LAT") 行) / summary.tsv ($(wc -l <"$SUM") 行)"
printf 's5: finding.md → %s\n' "$OUT/finding.md"
printf 's5: 汇总 → %s\n' "$SUM"
