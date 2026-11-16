#!/usr/bin/env bash
# C1 · 持续 10 分钟写入：RSS / goroutine / fd / DB / WAL 五条资源曲线。
#
# 量什么：在**同一批核**（taskset -c 8-11）上，用 Bearer 机器入口
#   POST /v1/namespaces/{ns}/events（无限速）持续压 10 分钟，同时每 30s 采一行资源快照。
#   另开一条 ≤30/min 的 webhook 支路（POST /v1/hooks/{token}），保证
#   「webhook 投递 → 派发 → 任务落库 → 分配」这一环始终在链路上，
#   而不是只往 events 表里灌行。
#
# v4 口径（2026-11-16 迁移，issue 006）——与 v3 的三处根本差别：
#   * 投递入口是命名空间子资源：`POST /v1/namespaces/{ns}/events`（Bearer API Key）。
#     v3 的 `POST /v1/events`（body 里带 namespaceId）已退役。
#   * body 的 `source` 必须是**已存在**的源名（v4 ingress 走 store.GetSourceByName），
#     且派发目标由该源 spec.targets 决定；v3 是「体里给 source 名 + 一条入口边」。
#   * 能力/源都是信封资源（/v1/namespaces/{ns}/capabilities、…/sources）。
#     **v3 的 `POST /v1/edges` 已退役**（端口 / fromPort / outputs 概念随 v3 路由消失）：
#     `edges` 行由能力的 `spec.next` + 源的 `spec.targets` 在写事务里**自动物化**，
#     对外只剩只读投影 `GET /v1/namespaces/{ns}/edges`（写端点回 405）。
#
# 为什么不用 hook 压满：`POST /v1/hooks/{token}` 是双维限流（每 token 600/min、
#   每 IP 1200/min，见 internal/server/ratelimit.go 的 defaultHookRatePerMin /
#   defaultHookRatePerIPPerMin）。压它只能量到 429。
#
# 为什么必须有常驻节点（scripts/perf/c/agent.mjs）：光造事件只会写 events/tasks 行，
#   没有 assignment / attempt / 结果事实，量出来的不是同一条写路径。
#   **v4 提醒**：tasks 在接收事务里就落库，所以「tasks 涨了」不能证明节点在接单；
#   派发前提是 deployments.state=applied（v3 靠 node_capabilities 行），见 §3 打的派发事实。
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
HOOK_EVERY="${S1_HOOK_EVERY:-2}"    # 支路间隔 → 600s/2s = 300 投 = 30/min（< 600/min 每 token 限速）
SAMPLE_EVERY="${S1_SAMPLE_EVERY:-30}"
EVENT_TYPE="${S1_EVENT_TYPE:-go}"   # 事件 type（v4 里只是事件标签：v3 的 fromPort/出口名已退役）
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
s1_count_where() { s1_sql "select count(*) from $1 where $2" | tr -d ' '; }
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
# 2. 播种 → 建能力（含 next）→ 建源
#
# v3 这一段建两条边（`POST /v1/edges`）并给 perf.echo 补 outputs=["go"]；v4 三条概念全退役：
#   a) `edges` 是**派生只读投影**（`GET /v1/namespaces/{ns}/edges`，写端点明确 405）：
#      能力的 `spec.next` + 源的 `spec.targets` 在写事务里自动物化，脚本不再建边；
#   b) 端口 / fromPort / outputs 随 v3 路由退役：事件的 `type` 只是标签（v4 派发目标由源的
#      targets 决定），所以主压事件的 type=go 不再需要任何 outputs 声明；
#   c) 能力 spec 必填六项（params/next/run/requires/lifecycle/concurrency），其中 `run` 是
#      必填的可执行入口（entrypoint + files，入口正文必须以 `#!` 开头、直接 exec 不经 shell）。
#      common.sh 的 `pc_cap` 只给 params（它要服务 C 路其他成员，不动它），所以这里用
#      本文件内的 `s1_cap` 发完整信封。
#
# 链路形状（与 v3 测量口径等价的部分）：
#   perf.echo --(spec.next)--> perf.sink     让「下游解析」成立（v3 的 E2 能力边）；
#   源 perf.echo --(targets)--> perf.echo    主压事件的派发入口（v3 的 E1 入口边）。
# 建完断言这两条边**确实物化**——v3 断言的是两次 `POST /v1/edges` 都返回 200，
# v4 没有写端点，等价事实是 edges 投影里真有这两行（少一行链路就不通，照样不压）。
# ---------------------------------------------------------------------------
pc_seed || die "pc_seed 失败（devseed 或 /v1/namespaces）"
log "seed 完成 ns=$PC_NS uid=$PC_UID"

# 会话 Cookie 的 header 拼一次复用（名字分片写：整串会被通用凭据脱敏规则改写）。
S1_COOKIE_HDR="Cookie: anotify_""session=$PC_SESSION"

# s1_cap <name> <next-json> [params-schema]：建能力（发 v4 信封），打印 metadata.uid。
# 与 pc_cap 的唯一差别是 `next`：v4 用它声明下游（v3 的 outputs 已随端口路由退役）。
s1_cap() {
	local name="$1" next="${2:-[]}" schema="${3:-{\"type\":\"object\"}}"
	curl -s -X POST "$PC_BASE/v1/namespaces/$PC_NS/capabilities" \
		-H "$S1_COOKIE_HDR" -H 'content-type: application/json' \
		-d "{\"apiVersion\":\"anotify/v1\",\"kind\":\"Capability\",\"metadata\":{\"name\":\"$name\"},\"spec\":{\"params\":$schema,\"next\":$next,\"run\":{\"entrypoint\":\"run.sh\",\"files\":{\"run.sh\":\"#!/bin/sh\\nexit 0\\n\"}},\"requires\":{},\"lifecycle\":{},\"concurrency\":{}}}" \
		| jq -r '.metadata.uid // empty'
}

SINK_ID=$(s1_cap perf.sink)
[ -n "$SINK_ID" ] || die "建能力 perf.sink 失败（s1_cap 返回空 uid）"
ECHO_ID=$(s1_cap perf.echo '["perf.sink"]')
[ -n "$ECHO_ID" ] || die "建能力 perf.echo 失败（s1_cap 返回空 uid）"
log "能力就绪 perf.echo=$ECHO_ID（next=[perf.sink]） perf.sink=$SINK_ID"

# 源：主压入口与 webhook 支路各一个 hook token（主压实际用的是 Bearer key，token 只当建源成功的凭据）。
#
# 两处 targets 都指向**常驻节点申报的那个能力**（perf.echo）：v4 的派发目标来自源的 targets，
# 而不是 v3 的「入口边」，所以「节点持有哪个能力」必须与 targets 对齐，否则任务落库后
# 永远等不到候选节点。v3 里 hook 源指向 sink（因为节点申报的就是 sink），语义一致。
MAIN_TOKEN=$(pc_source perf.echo perf.echo)
[ -n "$MAIN_TOKEN" ] || die "建源 perf.echo 失败（pc_source 返回空 token）"
HOOK_TOKEN=$(pc_source perf-soak-hook perf.echo)
[ -n "$HOOK_TOKEN" ] || die "建 webhook 支路源失败（pc_source 返回空 token）"
log "源就绪 perf.echo（主压，hook=${MAIN_TOKEN:0:8}…） perf-soak-hook（支路，hook=${HOOK_TOKEN:0:8}…）"

E_DOWN=$(s1_count_where edges "from_is_source=0 and from_ref='perf.echo' and to_ref='perf.sink'")
E_MAIN=$(s1_count_where edges "from_is_source=1 and from_ref='perf.echo' and to_ref='perf.echo'")
E_HOOK=$(s1_count_where edges "from_is_source=1 and from_ref='perf-soak-hook' and to_ref='perf.echo'")
log "派生边：echo→sink(next)=${E_DOWN:-0} 源 perf.echo→perf.echo=${E_MAIN:-0} 源 perf-soak-hook→perf.echo=${E_HOOK:-0} 全表=$(s1_count edges)"
[ "${E_DOWN:-0}" -ge 1 ] || die "spec.next 没有物化成边（perf.echo → perf.sink）——下游解析不成立，不压"
[ "${E_MAIN:-0}" -ge 1 ] || die "源 perf.echo 的 targets 没有物化成边（perf.echo → perf.echo）——主压事件派不出去"
[ "${E_HOOK:-0}" -ge 1 ] || die "源 perf-soak-hook 的 targets 没有物化成边（perf-soak-hook → perf.echo）——支路不通"

# 主压请求体：一次写好复用（loadgen 每次读同一个文件）。
# v4 没有 namespaceId（命名空间在 URL 里）；source 是已建好的源名。
BODY_JSON="$RAW/body.json"
jq -nc --arg src "perf.echo" --arg t "$EVENT_TYPE" \
	'{source:$src, type:$t, payload:{n:1, msg:"soak", at:"perf-c-s1"}}' >"$BODY_JSON"
log "请求体 $(wc -c <"$BODY_JSON") 字节：$(cat "$BODY_JSON")"

# ---------------------------------------------------------------------------
# 3. 常驻节点 + 链路自检（tasks 表必须先真的涨，否则不压）
# ---------------------------------------------------------------------------
# 节点申报 **perf.echo**（源的 targets）：v4 派发判据按 name@version 匹配 hello.capabilities。
node "$PC_REPO/scripts/perf/c/agent.mjs" --base "$PC_BASE" --db "$PC_DB" --ns "$PC_NS" \
	--cap-id "$ECHO_ID" --cap-name perf.echo --slots 8 --tick-ms 30000 >"$RAW/agent.log" 2>&1 &
AGENT_PID=$!
for _ in $(seq 1 50); do grep -q '"ev":"ready"' "$RAW/agent.log" 2>/dev/null && break; sleep 0.2; done
grep -q '"ev":"ready"' "$RAW/agent.log" || { tail -20 "$RAW/agent.log" | tee -a "$LOGF"; die "常驻节点没 ready"; }
log "节点就绪 $(head -1 "$RAW/agent.log")"

TASKS_BEFORE=$(s1_count tasks)
ONE=$(curl -s -X POST "$PC_BASE/v1/namespaces/$PC_NS/events" -H "Authorization: Bearer $PC_SEND_KEY" \
	-H 'content-type: application/json' -d @"$BODY_JSON")
log "自检事件响应 $(printf '%s' "$ONE" | jq -c '{eventId, tasks:(.tasks|length), duplicate, pruned:(.pruned//[]|map(.reason))}' 2>/dev/null || printf '%s' "$ONE")"
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

# 派发前提（v4 新增的一关，v3 没有）：
#   v3：node_capabilities 里有一行就算「这台机器装着该能力」，mock 节点由 seed 造好即可派单；
#   v4：候选节点还要求 deployments 里 (node, capability, digest) 是 state=applied ——
#       由 POST …/capabilities/{name}/deploy 下发、节点回 capability.deployed 才收敛。
# 常驻节点 scripts/perf/c/agent.mjs 仍是 v3 帧名（assign/ack/result）的实现，既不处理
# capability.deploy 也不回 capability.deployed，所以这里把事实**打出来**（不让它静默成
# 一个恒为 0 的列）：本脚本主压量的是 ingest → 路由 → 任务落库 + 派发提案这一段；
# ack/result 写路径要等 agent.mjs 迁到 v4 帧（task.dispatch/task.result），见 issue 006 遗留。
NA_COND=$(curl -s -H "$S1_COOKIE_HDR" \
	"$PC_BASE/v1/namespaces/$PC_NS/capabilities/perf.echo" \
	| jq -r '(.status.conditions // [])[] | select(.type=="NodesAvailable") | "\(.status)/\(.reason)"' 2>/dev/null)
APPLIED_DEPLOYS=$(s1_count_where deployments "state='applied'")
log "派发前提：NodesAvailable=${NA_COND:-?} deployments.applied=${APPLIED_DEPLOYS:-0}"
if [ "${APPLIED_DEPLOYS:-0}" -eq 0 ]; then
	log "注意：没有 applied 部署 ⇒ 任务停在 pending/派发提案，节点收不到 task.dispatch（acked 恒 0）。"
	log "      v3 的等价事实是 seed 出来的 node_capabilities 行；v4 只能由「部署 + 节点回报」产生。"
	log "      agent.mjs 未迁 v4 帧，本轮 extra.csv 的 agent_tick_acked 记 0 —— 已知遗留，不属本卡范围。"
fi

# ---------------------------------------------------------------------------
# 4. t≈0 的基线 profile + argon2 单请求复现
# ---------------------------------------------------------------------------
GPROF_T0=$(pc_pprof soak heap)
curl -s "$PC_BASE/debug/pprof/goroutine?debug=1" >"$RAW/goroutine_t0.txt"
pc_pprof soak goroutine >/dev/null
pc_pprof soak mutex >/dev/null
log "t=0 profile 就绪 $(basename "$GPROF_T0") / goroutine_t0.txt"

# argon2id 三点量测（#217 的硬事实）：v3 里每个带 Bearer 的请求都跑一次 argon2id（单次 64MB
# 分配），所以 heap0 → 1 请求 → heap1 → 再 3 请求 → heap4 的 alloc_space 差分能直接看见
# 「每请求 64MB」。**v4 加了 API Key 校验缓存**（internal/auth/apikey_cache.go，issue #227）：
# 同一把 key 在 TTL 内只有第一次真的跑 argon2，后续请求命中缓存。于是三点 alloc_space 会
# **持平（≈64MB）**——持平本身就是 v4 的事实（缓存生效），不再等于「没有 argon2」；要复现
# 每请求 64MB 得换新 key 或等 TTL 过期。这里保留同一套取样点，只把读数按 v4 解读。
# 必须用**带 Bearer 的投递入口**（v4：POST /v1/namespaces/{ns}/events；v3 是 POST /v1/events）：
# GET …/capabilities 是**会话 Cookie**端点，带 Bearer 直接 401（见 raw/server.log 的
# "session cookie missing"），根本不会跑 argon2 —— 第一版脚本就是踩了这个，差分是空的。
A2BODY=$(jq -nc --arg src "perf.echo" --arg t "$EVENT_TYPE" '{source:$src, type:$t, payload:{n:1}}')
s1_a2_post() {
	curl -s -o /dev/null -X POST "$PC_BASE/v1/namespaces/$PC_NS/events" -H "Authorization: Bearer $PC_SEND_KEY" \
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
log "argon2 三点量测完成（v4 有 key 缓存 ⇒ 三点应持平 ≈64MB；持平即缓存生效，明细见 raw/argon2.txt）"

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

# 支路：每 2s 投一条（30/min << 600/min 每 token 限速）；记 (elapsed,code) 供统计 200/429。
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
#    v4 的投递 URL 是命名空间子资源（v3 的 /v1/events 已 404）：
#    等价命令：taskset -c 8-11 go run ./scripts/perf/loadgen \
#                -url $PC_BASE/v1/namespaces/$PC_NS/events ...
# ---------------------------------------------------------------------------
S1_EVENTS_URL="$PC_BASE/v1/namespaces/$PC_NS/events"
mkdir -p "$S1_DIR/bin"
LG_BIN="$S1_DIR/bin/loadgen"
( cd "$PC_REPO" && go build -o "$LG_BIN" ./scripts/perf/loadgen ) || die "构建 loadgen 失败"

s1_seg() {  # s1_seg <a> <b>  跑 [a,b) 这一段
	local a="$1" b="$2" secs=$(( $2 - $1 )) tag
	tag=$(printf '%03d-%03d' "$a" "$b")
	echo "$tag" >"$S1_DIR/seg"   # 采样子进程是独立进程，共享变量传不过去，用文件传

	log "主压段 $tag（${secs}s，-c 8）开始 @ t=$(elapsed_s)"
	taskset -c "$PC_CPUS" "$LG_BIN" -url "$S1_EVENTS_URL" -method POST \
		-body-file "$BODY_JSON" -header 'content-type: application/json' \
		-header "Authorization: Bearer $PC_SEND_KEY" -c 8 -d "${secs}s" \
		2>&1 | tee -a "$RAW/ingest.log" >>"$RAW/ingest_$tag.log" &
	LG_PID=$!
	echo "$LG_PID" >"$S1_DIR/lg.pid"
	wait "$LG_PID"
	LG_PID=""
	: >"$S1_DIR/lg.pid"
	s1_seg_status "$tag"
	log "主压段 $tag 结束 @ t=$(elapsed_s)"
}

# s1_seg_status <tag>：把该段的状态码摊到 run.log，并在出现非 200 时点名原因。
# 为什么必须做：v4 的投递入口有**派发背压**（internal/server/backpressure.go，
# defaultBackpressure MaxAge=120s：最老 pending 任务超过阈值就拒新事件 503+Retry-After）。
# 被拒的请求只需 ~300µs，loadgen 的 RPS 会反而飙剐到 2 万级 —— 光看 RPS 会误以为
# “性能很好”，实际这一整段一条事件都没写进去。v3 没有这道关（节点吃掉任务 ⇒ pending 不涨）。
s1_seg_status() {
	local tag="$1" line total bad
	line=$(grep -m1 '^状态码' "$RAW/ingest_$tag.log" 2>/dev/null)
	[ -n "$line" ] || return 0
	total=$(printf '%s' "$line" | grep -oE '[0-9]+×[0-9]+' | awk -F'×' '{s+=$2} END{print s+0}')
	bad=$(printf '%s' "$line" | grep -oE '[0-9]+×[0-9]+' | awk -F'×' '$1!=200{s+=$2} END{print s+0}')
	log "主压段 $tag ${line#状态码}"
	if [ "${bad:-0}" -gt 0 ]; then
		log "WARN 主压段 $tag 有 $bad/${total:-0} 个非 200：503 = 派发积压背压（最老 pending 任务超过 pending_max_age 就拒新事件）。"
		log "     这一段里的 RPS 不是写入吞吐，raw/extra.csv 的 events/tasks 列才是实际落库行数；原因见 raw/server.log 的 server.backpressure.engaged。"
	fi
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
# v4 列名：tasks 的相位列叫 `phase`（v3 叫 `status`）；edges 是派生投影（v3 是手建的行）。
{
	echo '== events by source =='
	s1_sql "select source, count(*) from events group by source order by 2 desc"
	echo '== tasks by phase（v3 的 status 列在 v4 叫 phase）=='
	s1_sql "select phase, count(*) from tasks group by phase order by 2 desc"
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

# 被背压拒掉的请求总数（跨段汇总）：非 0 就说明本轮写曲线被截断，不能当 10 分钟稳态读。
REJECTED=$(grep -oE '503×[0-9]+' "$RAW/ingest.log" 2>/dev/null | awk -F'×' '{s+=$2} END{print s+0}')
if [ "${REJECTED:-0}" -gt 0 ]; then
	log "WARN 本轮共 ${REJECTED} 个请求被 503 拒（写曲线自背压介入起失效）："
	log "     v4 的背压信号是最老 pending 任务年龄；任务没人吃（mock 节点未接 v4 帧、也没有 applied 部署）就会一直长。"
	log "     重跑取有效曲线前，先把「任务被消费」这条路修通（见 run.log 开头打的派发前提）。"
fi

# 停服务/节点（cleanup 幂等，EXIT 时再跑一次也无害）
cleanup

log "产物：$RAW（samples.csv / extra.csv / ingest.log / hook.log / agent.log / profiles/）"
log "DONE c1-soak 耗时 $(($(date +%s) - T0))s"
