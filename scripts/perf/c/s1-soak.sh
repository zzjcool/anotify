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
#   派发前提是 deployments.state=applied（v3 靠 node_capabilities 行）——§3 现在会真的
#   调一次 POST …/capabilities/{name}/deploy，并在节点回执收敛后断言 NodesAvailable=True。
#   节点侧的帧也必须走 v4（task.dispatch/task.accept/lease.grant/task.result）：
#   issue 007 之前 agent.mjs 还是 v3 的 assign/ack/result，节点**一帧都收不到**，
#   而脚本照样跑完 600s —— 所以 §3 与每个主压段都断言 acked/resulted 必须随 tasks 涨。
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
# 跨轮污染：raw/ 下是本轮产物，开跑前清空（profiles/ 与 runs/ 保留，那是历史证据）。
# 不清的后果：重跑同一时长会读到上一轮的数字（`s1_seg_status` 取第一行「状态码」）。
mkdir -p "$RAW"
: >"$RAW/agent.log"; : >"$RAW/hook.log"; : >"$RAW/ingest.log"
rm -f "$RAW"/ingest_*.log
PROF="$RAW/profiles"

# 独立可跑：PC_ROOT 指到本场景目录，和其他 C 路成员（s2/s3…）的 bin/runs 互不干扰。
export PC_ROOT="$S1_DIR"
# shellcheck source=./common.sh
source "$HERE/common.sh"

DURATION="${S1_DURATION:-600}"      # 主压总时长
# 主压限速（请求/秒，所有并发共享）。0 = 不限速。
#
# 为什么默认限速（issue 007）：v4 的派发吞吐上限实测 40-90 任务/s（每任务 5 次读 +
# 1 次 assignment 写事务，与 ingest 抢同一把写锁）。不限速时摄取 330-670/s 会把最老
# pending 推过 120s，背压介入后整段 100% 503 —— 那时量到的不是稳态，是「积压+背压」。
# 40 是给上限留了余量的稳态档；要量「极限吞吐」用 S1_RATE=0，并清楚那一段的 503 是设计行为。
S1_RATE="${S1_RATE:-40}"
SEG_A="${S1_SEG_A:-120}"            # 0-2min 段
SEG_C="${S1_SEG_C:-120}"            # 最后 2min 段（即 8-10min）
SEG_B=$((DURATION - SEG_A - SEG_C)) # 中间段
# 短跑（如 S1_DURATION=180 冒烟）装不下三段：中间段收成 0，末段压到总时长的 1/3。
# 默认 600s 的口径（120 / 360 / 120）一个数都不变，短跑仍保留「前段 + 后段」两个测点，
# 否则 SEG_B 会变成负数（`s1_seg 120 60` → loadgen 收到 `-d -60s`）。
if [ "$SEG_B" -lt 0 ]; then
	SEG_C=$(( DURATION / 3 ))
	SEG_B=$(( DURATION - SEG_A - SEG_C ))
fi
if [ "$SEG_B" -lt 0 ]; then
	SEG_A=$(( DURATION - SEG_C ))
	SEG_B=0
fi
HOOK_EVERY="${S1_HOOK_EVERY:-2}"    # 支路间隔 → 600s/2s = 300 投 = 30/min（< 600/min 每 token 限速）
SAMPLE_EVERY="${S1_SAMPLE_EVERY:-30}"
EVENT_TYPE="${S1_EVENT_TYPE:-go}"   # 事件 type（v4 里只是事件标签：v3 的 fromPort/出口名已退役）
SLOTS="${S1_SLOTS:-8}"             # 假节点的物理槽位数（= 本机能同时跑几个；中枢上限 20，超了 hello 被拒成 too_many_instances）
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
#      本文件内的 `s1_cap` 发完整信封；`concurrency.maxPerNode` 必须对齐节点槽位数（§s1_cap）。
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

# s1_cap <name> <next-json> [params-schema] [concurrency-json]：建能力（发 v4 信封），打印 metadata.uid。
# 与 pc_cap 的两处差别：
#   * `next`：v4 用它声明下游（v3 的 outputs 已随端口路由退役）；
#   * `concurrency`：v4 的「同一能力在这台机器上最多并发几个」是**能力 spec** 的一部分
#     （internal/artifact：concurrency.maxPerNode，省略 = 1）。默认 1 时一台 8 槽的节点
#     只能同时跑 1 个 perf.echo，其余 accept 全被 grantBudgetOn 拒成
#     slot_unavailable ⇒ 节点吞吐被钉在「1 个在飞任务」上（实测 ≈56 任务/s），
#     摄取一高 pending 就永远涨 —— 那是配置出来的假瓶颈，不是被测链路的容量。
#     所以这里把它对齐到 SLOTS。
s1_cap() {
	local name="$1" next="${2:-[]}" schema="${3:-"{\"type\":\"object\"}"}" conc="${4:-"{}"}"
	curl -s -X POST "$PC_BASE/v1/namespaces/$PC_NS/capabilities" \
		-H "$S1_COOKIE_HDR" -H 'content-type: application/json' \
		-d "{\"apiVersion\":\"anotify/v1\",\"kind\":\"Capability\",\"metadata\":{\"name\":\"$name\"},\"spec\":{\"params\":$schema,\"next\":$next,\"run\":{\"entrypoint\":\"run.sh\",\"files\":{\"run.sh\":\"#!/bin/sh\\nexit 0\\n\"}},\"requires\":{},\"lifecycle\":{},\"concurrency\":$conc}}" \
		| jq -r '.metadata.uid // empty'
}

SINK_ID=$(s1_cap perf.sink)
[ -n "$SINK_ID" ] || die "建能力 perf.sink 失败（s1_cap 返回空 uid）"
ECHO_ID=$(s1_cap perf.echo '["perf.sink"]' '{"type":"object"}' "{\"maxPerNode\":$SLOTS}")
[ -n "$ECHO_ID" ] || die "建能力 perf.echo 失败（s1_cap 返回空 uid）"
log "能力就绪 perf.echo=$ECHO_ID（next=[perf.sink] concurrency.maxPerNode=$SLOTS） perf.sink=$SINK_ID"

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
# 3. 常驻节点 → 部署（v4 的派发前提）→ 链路自检
#
# 顺序是刻意的：deploy 必须在自检之前。v4 的派发要求 deployments.state=applied，
# 而 deploy 的收敛是「中枢下发 capability.deploy → 节点回 capability.deployed」一个
# WS 往返；没收敛前派发判据是 NotDeployed，任务只会堆在 pending。
# ---------------------------------------------------------------------------
# 节点申报 **perf.echo**（源的 targets）：v4 派发判据按 name@version 匹配 hello.capabilities。
# --tick-ms 5s：采样器每 30s 一行，tick 太稀会让 extra.csv / 段断言读到 30s 前的旧数。
node "$PC_REPO/scripts/perf/c/agent.mjs" --base "$PC_BASE" --db "$PC_DB" --ns "$PC_NS" \
	--cap-name perf.echo --slots "$SLOTS" --tick-ms 5000 >"$RAW/agent.log" 2>&1 &
AGENT_PID=$!
for _ in $(seq 1 50); do grep -q '"ev":"ready"' "$RAW/agent.log" 2>/dev/null && break; sleep 0.2; done
grep -q '"ev":"ready"' "$RAW/agent.log" || { tail -20 "$RAW/agent.log" | tee -a "$LOGF"; die "常驻节点没 ready"; }
log "节点就绪 $(head -1 "$RAW/agent.log")"

# s1_agent_counter <field>：agent.log 最后一条 tick 里的某个计数器（没有则 0）。
# 读 tick 而不是另开查库/HTTP：压测节点不能给被测服务再添一条读路径。
s1_agent_counter() {
	local n
	n=$(grep -o "\"$1\":[0-9]*" "$RAW/agent.log" 2>/dev/null | tail -1 | cut -d: -f2)
	printf '%s' "${n:-0}"
}

# 部署（v4 新增的一关，v3 没有）：
#   v3：node_capabilities 里有一行就算「这台机器装着该能力」，mock 节点由 seed 造好即可派单；
#   v4：候选节点还要求 deployments 里 (node, capability, digest) 是 state=applied —— 只能由
#       POST …/capabilities/{name}/deploy 下发 + 节点回 capability.deployed 产生（seed 不能替）。
# 不部署的后果是**静默停摆**而不是报错（issue 007 根因链第 1 条）：NodesAvailable=False/
# NotDeployed，节点一帧都收不到，soak 照样跑完。所以这里部署完必须断言。
s1_deploy() {  # s1_deploy <cap-name>：下发部署，把逐节点结论摊到 run.log
	local name="$1" out
	out=$(curl -s -X POST "$PC_BASE/v1/namespaces/$PC_NS/capabilities/$name/deploy" \
		-H "$S1_COOKIE_HDR" -H 'content-type: application/json' -d '{}')
	log "部署 $name → $(printf '%s' "$out" | jq -c '{version, deployed:(.deployed//[]|map({node,state,sent}))}' 2>/dev/null || printf '%s' "$out")"
}
# s1_applied_deploys <cap-name>：该能力在 deployments 里 applied 的节点数（= 真的持有 digest）。
s1_applied_deploys() {
	s1_count_where "deployments d join capability_versions c on c.id = d.capability_id" \
		"d.state='applied' and c.name='$1'"
}
# s1_nodes_available <cap-name>：能力的 NodesAvailable 条件（"True/Valid" / "False/NotDeployed" / …）。
s1_nodes_available() {
	curl -s -H "$S1_COOKIE_HDR" "$PC_BASE/v1/namespaces/$PC_NS/capabilities/$1" \
		| jq -r '(.status.conditions // [])[] | select(.type=="NodesAvailable") | "\(.status)/\(.reason)"' 2>/dev/null
}

# 源 perf.echo 的 targets 指向 perf.echo，主压事件派出去的就是它；perf.sink 是
# spec.next 的下游，一并部署（没有 emit 就不会有下游任务，但它同样是「网关→下一跳」的前提）。
s1_deploy perf.echo
s1_deploy perf.sink
APPLIED_ECHO=0
for _ in $(seq 1 50); do
	APPLIED_ECHO=$(s1_applied_deploys perf.echo)
	[ "${APPLIED_ECHO:-0}" -ge 1 ] && break
	sleep 0.2
done
APPLIED_SINK=$(s1_applied_deploys perf.sink)
NA_ECHO=""
for _ in $(seq 1 50); do
	NA_ECHO=$(s1_nodes_available perf.echo)
	[ "${NA_ECHO%%/*}" = "True" ] && break
	sleep 0.2
done
log "派发前提：deployments.applied perf.echo=${APPLIED_ECHO:-0} perf.sink=${APPLIED_SINK:-0}；NodesAvailable(perf.echo)=${NA_ECHO:-?}"
if [ "${APPLIED_ECHO:-0}" -lt 1 ] || [ "${NA_ECHO%%/*}" != "True" ]; then
	log "部署没收敛：capability.deployed 回执没上来 ⇒ 节点不会成为候选，任务只会停在 pending。"
	tail -20 "$RAW/agent.log" | tee -a "$LOGF"
	tail -20 "$PC_RUN/server.log" | tee -a "$LOGF"
	die "派发前提不成立（applied=${APPLIED_ECHO:-0} NodesAvailable=${NA_ECHO:-?}）"
fi

TASKS_BEFORE=$(s1_count tasks)
ACKED_BEFORE=$(s1_agent_counter acked)
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
# 节点**真的接住了**这一条：v4 的 tasks 在接收事务里就落库，tasks 涨了不能证明节点
# 收得到帧（issue 007 现场：手工插 deployments.state='applied' 后 tasks 照涨、acked 恒 0）。
# 等一个派发周期（250ms）+ 一次 accept/grant/result 往返 + 一条 tick 落盘。
for _ in $(seq 1 40); do
	[ "$(s1_agent_counter acked)" -gt "${ACKED_BEFORE:-0}" ] && break
	sleep 0.25
done
ACKED_AFTER=$(s1_agent_counter acked)
RESULTED_AFTER=$(s1_agent_counter resulted)
log "链路自检通过：tasks $TASKS_BEFORE → $TASKS_AFTER（+$((TASKS_AFTER - TASKS_BEFORE))）；节点 acked $ACKED_BEFORE → $ACKED_AFTER resulted=$RESULTED_AFTER"
if [ "${ACKED_AFTER:-0}" -le "${ACKED_BEFORE:-0}" ] || [ "${RESULTED_AFTER:-0}" -le 0 ]; then
	log "节点一帧都没接住（acked $ACKED_BEFORE → ${ACKED_AFTER:-0}, resulted=${RESULTED_AFTER:-0}）："
	log "  task.dispatch 没到节点（节点不是候选：部署/配置 applied/命令探测），或节点的 accept/result 帧形状不对。"
	tail -20 "$RAW/agent.log" | tee -a "$LOGF"
	tail -40 "$PC_RUN/server.log" | tee -a "$LOGF"
	die "v4 帧链未打通（acked/resulted 不涨）—— 这正是本卡要防的静默失败"
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
# agent_tick_acked / agent_tick_resulted 是「节点真的在吃任务」的两列：v4 的 tasks 在接收
# 事务里就落库，tasks 涨不能证明帧到过节点（issue 007）。两列一直持平 = 节点没在消费。
echo 'elapsed_s,events,tasks,health_codes,loadgen_seg,loadgen_alive,hook_200,hook_429,agent_tick_acked,agent_tick_resulted' >"$RAW/extra.csv"
: >"$RAW/ingest.log"
: >"$RAW/hook.log"   # 必须在支路循环**之前**清空，否则会截掉已经开始写的那几行
echo none >"$S1_DIR/seg"   # 上一轮的分段标记（否则 t=0 那一行会写成上一轮的最后一段）
# 注意**不要**在这里清 agent.log：§3 起节点时已经用 `>` 截过，而且此刻节点还在往里写，
# 进程内 fd 偏移不会跟随 truncate（会挖出一个 NUL 洞）。

# 注意 grep -c 在 0 匹配时仍会打印 "0" 但退出码非 0，所以不能写 `|| echo 0`（会拼出两行）。
s1_count_code() { local n; n=$(grep -c "$1" "$2" 2>/dev/null); printf '%s' "${n:-0}"; }

s1_sampler() {
	local e ev tk hc n200 n429 ack res seg lgp
	while [ -f "$S1_DIR/.running" ]; do
		e=$(elapsed_s)
		PC_ELAPSED=$e
		pc_sample "t$e" >>"$RAW/samples.csv"
		ev=$(s1_count events); tk=$(s1_count tasks); hc=$(s1_health)
		n200=$(s1_count_code ',200$' "$RAW/hook.log")
		n429=$(s1_count_code ',429$' "$RAW/hook.log")
		ack=$(s1_agent_counter acked); res=$(s1_agent_counter resulted)
		seg=$(cat "$S1_DIR/seg" 2>/dev/null); seg=${seg:-none}
		lgp=$(cat "$S1_DIR/lg.pid" 2>/dev/null)
		echo "$e,${ev:-0},${tk:-0},$hc,$seg,$([ -n "$lgp" ] && kill -0 "$lgp" 2>/dev/null && echo 1 || echo 0),$n200,$n429,${ack:-0},${res:-0}" \
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

	# 每段开跑前**必须**清掉同 tag 的旧文件：`s1_seg_status` 用 `grep -m1` 取第一行
	# 「状态码」，而下面用 `>>` 追加。不清的后果是「重跑同一时长（如两次 S1_DURATION=180）
	# 会读到上一轮的数字」—— 实测踩过一次：段 1 报 200×40311、实际本轮只有 4799。
	: >"$RAW/ingest_$tag.log"
	log "主压段 $tag（${secs}s，-c 8，rate=${S1_RATE}/s）开始 @ t=$(elapsed_s)"
	taskset -c "$PC_CPUS" "$LG_BIN" -url "$S1_EVENTS_URL" -method POST \
		-body-file "$BODY_JSON" -header 'content-type: application/json' \
		-header "Authorization: Bearer $PC_SEND_KEY" -c 8 -d "${secs}s" -rate "$S1_RATE" \
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
	local tag="$1" line total bad breakdown code
	line=$(grep -m1 '^状态码' "$RAW/ingest_$tag.log" 2>/dev/null)
	[ -n "$line" ] || return 0
	total=$(printf '%s' "$line" | grep -oE '[0-9]+×[0-9]+' | awk -F'×' '{s+=$2} END{print s+0}')
	bad=$(printf '%s' "$line" | grep -oE '[0-9]+×[0-9]+' | awk -F'×' '$1!=200{s+=$2} END{print s+0}')
	log "主压段 $tag ${line#状态码}"
	[ "${bad:-0}" -gt 0 ] || return 0
	# 把**实际**出现的非 200 码列出来，不只笼统写「503 = 背压」：
	# 本轮就碰到过 500（SQLITE_BUSY），写成 503 会把一个产品缺陷误读成「设计内的背压」。
	breakdown=$(printf '%s' "$line" | grep -oE '[0-9]+×[0-9]+' | awk -F'×' '$1!=200{printf "%s×%s ", $1, $2}')
	log "WARN 主压段 $tag 有 $bad/${total:-0} 个非 200：[${breakdown% }]"
	for code in $breakdown; do
		case "${code%%×*}" in
		503)
			log "      503 = 派发积压背压（最老 pending 超过 pending_max_age 就拒新事件，见 server.backpressure.engaged）。"
			;;
		500)
			log "      500 = 服务端错误（本轮实测为 SQLITE_BUSY：读事务也走 _txlock=immediate，等写锁超 5s 就 500）。"
			;;
		esac
	done
	log "      这一段里的 RPS 不是写入吞吐，raw/extra.csv 的 events/tasks 列才是实际落库行数。"
}

# s1_seg_assert <tag>：该段结束后断言「节点消费」跟着 tasks 一起涨。
#
# 为什么必须在**每一段**都断言而不是只看结尾：issue 007 的失败形态是「前 2 分钟真写、
# 后 8 分钟 0 写入，脚本还以为成功」。只看结尾的累计数会被前 2 分钟的量掩盖。
# 阈值的口径是「这一段里 acked/resulted 都**涨了**」：这不依赖派发速率跟摄取速率谁大，
# 只验证「帧链路活着」——而那正是本卡要防的回归。
s1_seg_assert() {
	local tag="$1" pa="$2" pr="$3" pt="$4" a r t da dr dt
	a=$(s1_agent_counter acked); r=$(s1_agent_counter resulted); t=$(s1_count tasks)
	da=$(( ${a:-0} - ${pa:-0} )); dr=$(( ${r:-0} - ${pr:-0} )); dt=$(( ${t:-0} - ${pt:-0} ))
	log "节点消费 $tag：acked +$da resulted +$dr tasks +$dt（累计 acked=${a:-0} resulted=${r:-0} tasks=${t:-0}）"
	if [ "${dt:-0}" -gt 0 ] && { [ "${da:-0}" -le 0 ] || [ "${dr:-0}" -le 0 ]; }; then
		log "本段写了 $dt 条任务，节点却只 acked +$da / resulted +$dr —— 帧没到节点（或 accept/result 被拒）。"
		log "看 raw/agent.log 的 errors/inflight/queued 与 raw/server.log 的 grant rejected / result rejected。"
		tail -20 "$RAW/agent.log" | tee -a "$LOGF"
		die "主压段 $tag 节点没在消费（acked/resulted 没随 tasks 涨）"
	fi
}

# 注意：`go run` 的等价性——这里分段是为了拿到 0-2min / 8-10min 的**分位数**，
# loadgen 只在退出时打一次汇总，单段 600s 拿不到分段对比。
# 短跑（SEG_B=0）时中间那一段直接跳过：loadgen 收到 `-d 0s` 只会立刻退出。
A_SEG=$(s1_agent_counter acked)
R_SEG=$(s1_agent_counter resulted)
T_SEG=$(s1_count tasks)
s1_seg 0 "$SEG_A"
s1_seg_assert "0-$SEG_A" "$A_SEG" "$R_SEG" "$T_SEG"
A_SEG=$(s1_agent_counter acked); R_SEG=$(s1_agent_counter resulted); T_SEG=$(s1_count tasks)
if [ "$SEG_B" -gt 0 ]; then
	s1_seg "$SEG_A" $(( SEG_A + SEG_B ))
	s1_seg_assert "$SEG_A-$(( SEG_A + SEG_B ))" "$A_SEG" "$R_SEG" "$T_SEG"
	A_SEG=$(s1_agent_counter acked); R_SEG=$(s1_agent_counter resulted); T_SEG=$(s1_count tasks)
fi
s1_seg $(( SEG_A + SEG_B )) "$DURATION"
s1_seg_assert "$(( SEG_A + SEG_B ))-$DURATION" "$A_SEG" "$R_SEG" "$T_SEG"

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
echo "$(elapsed_s),$(s1_count events),$(s1_count tasks),$(s1_health),end,0,$FRESH_HOOK_200,$FRESH_HOOK_429,$(s1_agent_counter acked),$(s1_agent_counter resulted)" \
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
log "压测结束：节点 acked=$(s1_agent_counter acked) resulted=$(s1_agent_counter resulted) confirmed=$(s1_agent_counter confirmed) errors=$(s1_agent_counter errors) inflight=$(s1_agent_counter inflight) queued=$(s1_agent_counter queued)"

# 被背压拒掉的请求总数（跨段汇总）：非 0 就说明本轮写曲线被截断，不能当 10 分钟稳态读。
REJECTED=$(grep -oE '503×[0-9]+' "$RAW/ingest.log" 2>/dev/null | awk -F'×' '{s+=$2} END{print s+0}')
TOTAL_REQ=$(grep -oE '[0-9]+×[0-9]+' "$RAW/ingest.log" 2>/dev/null | awk -F'×' '{s+=$2} END{print s+0}')
if [ "${REJECTED:-0}" -gt 0 ]; then
	log "WARN 本轮共 ${REJECTED}/${TOTAL_REQ:-0} 个请求被 503 拒（写曲线自背压介入起失效）："
	log "     背压信号是最老 pending 任务年龄（默认 120s）；摄取持续高于消耗速率时它只会一直涨。"
	log "     消耗速率的天花板是**派发**而不是节点：实测 ≈87 任务/s（节点 20 个空槽、inflight 常为 0）。"
	log "     派发循环的配置上限是 250ms/轮 ×100 条（internal/ws/hub.go RunDispatchLoop），但一轮实际要 ~1.1s："
	log "     每任务 5 次读（EligibleNodes/FreeSlots/…）+ 1 次 assignment 写事务，且与 ingest 抢同一把写锁。"
	log "     对照 raw/extra.csv 的 events/tasks/agent_tick_acked/agent_tick_resulted 四列看「落了库 / 吃了多少」；"
	log "     backpressure 介入点见 raw/server.log 的 server.backpressure.engaged。"
fi

# 停服务/节点（cleanup 幂等，EXIT 时再跑一次也无害）
cleanup

log "产物：$RAW（samples.csv / extra.csv / ingest.log / hook.log / agent.log / profiles/）"
log "DONE c1-soak 耗时 $(($(date +%s) - T0))s"
