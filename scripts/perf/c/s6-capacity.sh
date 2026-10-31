#!/usr/bin/env bash
# 批次 5 · 容量与背压（#229 事件保留期 / #228 派发背压）的测量台。
#
# 与 scripts/perf/c 下其它脚本的关系：复用 common.sh 的底座（分核 / 起服务 / 播种 /
# 采样），但**端口段换成 8140-8159**（批次 5 的约定，避开 7940-7959 那批）。
# 所以本脚本 source 完 common.sh 之后会改写 PC_PORT_LO/HI —— 只影响本进程。
#
# 用法：
#   bash scripts/perf/c/s6-capacity.sh           # 全部区段 a-f（约 10 分钟）
#   bash scripts/perf/c/s6-capacity.sh a b       # 只跑指定区段
#   S6_SKIP_BUILD=1 bash scripts/perf/c/s6-capacity.sh
#
# 产物：$PC_ROOT（默认 /home/<user>/perf-b5 —— **不落 /tmp**：那是 7.5G tmpfs，
# 这里的库能到几百 MB，会把别的进程一起挤死）
#   report.txt     汇总（本脚本打印的全部结论，可直接贴 issue）
#   a/ b/ c/ d/ e/ 各段的原始输出

set -uo pipefail

source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/common.sh"

# ---- 批次 5 的端口段（覆盖 common.sh 的 C 路段；只在本进程内生效）----
PC_PORT_LO=8140
PC_PORT_HI=8159
# ---- 产物落 /home ----
# 注意顺序：common.sh 已经把 PC_ROOT 设成 /tmp/perf-c，而 /tmp 在这台机上是 7.5G tmpfs
# （本段的库能到几百 MB，会把别的进程一起挤死）。所以这里**直接覆盖**，
# 覆盖源用独立的 S6_ROOT 变量，避免「env 里给了 PC_ROOT 但被 common.sh 抢先定义」的歧义。
PC_ROOT="${S6_ROOT:-/home/$(id -un)/perf-b5}"
S6_OUT="$PC_ROOT/out"
# 汇总报告的落盘策略（#228 复审 MINOR）：**不再每次 truncate**。
#   $S6_OUT/report.txt         本次进程内**所有**跑过的区段（每次运行重置一次）
#   $S6_OUT/report-<section>.txt 每个区段一份（跨运行保留，供事后逐段对账）
# 踩过的坑：原来是 `: >"$S6_REPORT"`（每次运行清空），而文档教人分段跑
# （`s6-capacity.sh a` … `s6-capacity.sh f`）→ 跑完最后一段，前面所有数字的证据
# 就从汇总里消失了，只剩各段目录里的原始文件。
S6_REPORT="$S6_OUT/report.txt"
S6_SECTION_REPORT=""
# s6_section_tee：把区段结论同时写进「本次汇总」与「该区段的专属报告」。
s6_section_tee() {
	tee -a "$S6_REPORT" | tee -a "$S6_SECTION_REPORT"
}
mkdir -p "$PC_ROOT/bin" "$PC_ROOT/logs" "$PC_ROOT/runs" "$S6_OUT"

S6_EVENTS="${S6_EVENTS:-1000}"      # 空 payload 档每批条数
S6_EVENTS_1K="${S6_EVENTS_1K:-500}" # 1KB payload 档条数
S6_CONC="${S6_CONC:-8}"
S6_BACKLOG="${S6_BACKLOG:-26000}"   # 保留期实验的积压条数（> 单轮预算 20×1000 → 多轮）
S6_READ_SECS="${S6_READ_SECS:-20}"
S6_BP_SECS="${S6_BP_SECS:-40}"
S6_RPS_SECS="${S6_RPS_SECS:-20}"

LG_BIN="$PC_ROOT/bin/loadgen"

log() { echo "[s6 $(date +%H:%M:%S)] $*" | tee -a "$S6_REPORT"; }
die() {
	log "❌ $*"
	exit 1
}

# ---------------------------------------------------------------- sqlite 小工具
# 带 busy timeout：服务的 5s 巡检会写库，撞上了不该让整段测量失败。
s6_sq() { sqlite3 -cmd '.timeout 8000' "${S6_DB:-$PC_DB}" "$1" 2>/dev/null || true; }
s6_sq1() {
	local v
	v=$(s6_sq "$1" | head -1)
	echo "${v:-0}"
}

# 量体积前先 checkpoint，否则量到的是「主库 + 一大堆未回写 WAL」（#229 踩过）。
s6_checkpoint() {
	local tries=0 out busy
	while [ $tries -lt 8 ]; do
		tries=$((tries + 1))
		out=$(sqlite3 -cmd '.timeout 8000' "${S6_DB:-$PC_DB}" 'PRAGMA wal_checkpoint(TRUNCATE);' 2>&1)
		busy="${out%%|*}"
		[ "$busy" = "0" ] && return 0
		sleep 0.3
	done
	return 0
}

s6_size_header() { echo "label,db_bytes,pages,page_size,pages_x_size,freelist,events_rows,tasks_rows,events_sent"; }

# s6_size <label>：一行 CSV 快照。
s6_size() {
	local label="$1" db pages psz freelist ev tk
	s6_checkpoint
	db=$(stat -c %s "${S6_DB:-$PC_DB}" 2>/dev/null || echo 0)
	pages=$(s6_sq1 'PRAGMA page_count;')
	psz=$(s6_sq1 'PRAGMA page_size;')
	freelist=$(s6_sq1 'PRAGMA freelist_count;')
	ev=$(s6_sq1 'select count(*) from events;')
	tk=$(s6_sq1 'select count(*) from tasks;')
	echo "$label,$db,$pages,$psz,$((pages * psz)),$freelist,$ev,$tk,${S6_SENT:-0}"
}

# pc_start_db <name> <port> <dbpath> [VAR=VAL ...]：用指定库起服务。
#
# 为什么需要它：pc_start 的 PC_DB 恒等于 $PC_ROOT/runs/<name>/perf.db，
# 想复用一份**造好的**库（保留期实验要同一个库跑多轮）就得让 PC_DB 跟着走，
# 否则 s6_sq/pc_seed 会去操作一个服务从未打开过的空库。
pc_start_db() {
	local name="$1" port="$2" db="$3"
	shift 3
	pc_start "$name" "$port" ANOTIFY_DB="$db" "$@" >/dev/null || return 1
	export PC_DB="$db"
	# S6_DB 是「当前要量哪个库」的显式覆盖（同一份库要被多轮复用）。
	# 它是 export 的，所以**用完之后必须 unset**：不然后续用普通 pc_start 起的段
	# 会继续对着上一段的库做 s6_sq/s6_size，症状是「造了数据但测到的是旧库」。
	export S6_DB="$db"
	return 0
}

s6_build() {
	if [ -n "${S6_SKIP_BUILD:-}" ] && [ -x "$PC_BIN" ] && [ -x "$LG_BIN" ]; then return 0; fi
	pc_build || return 1
	( cd "$PC_REPO" && taskset -c "$PC_CPUS" go build -o "$LG_BIN" ./scripts/perf/loadgen ) || return 1
	return 0
}

# s6_payload <seq> <bytes>：定长 payload。
# 长度必须不随 seq 漂移（定宽 6 位 seq），否则 bytes/event 会假性偏移。
s6_payload() {
	local seq6 pad
	printf -v seq6 '%06d' "$1"
	if [ "$2" -le 2 ]; then
		printf '{}'
		return 0
	fi
	pad=$(printf 'x%.0s' $(seq 1 "$2"))
	printf '{"seq":"%s","pad":"%s"}' "$seq6" "$pad"
}

# s6_ingest <from> <count> <payload_bytes> <type> <dir>
# 预生成 body 文件再并发 curl：body 内容各不相同（seq），xargs 只做 curl 最省事。
s6_ingest() {
	local from="$1" count="$2" plen="$3" etype="$4" dir="$5"
	mkdir -p "$dir"
	local idx
	for idx in $(seq "$from" $((from + count - 1))); do
		printf '{"namespaceId":"%s","source":"perfgen","type":"%s","payload":%s}' \
			"$PC_NS" "$etype" "$(s6_payload "$idx" "$plen")" >"$dir/b.$idx"
	done
	seq "$from" $((from + count - 1)) | xargs -P "$S6_CONC" -I{} curl -s -o /dev/null \
		-w '%{http_code}\n' --max-time 60 \
		-X POST "$PC_BASE/v1/events" \
		-H "Authorization: Bearer $PC_SEND_KEY" \
		-H 'content-type: application/json' \
		--data-binary "@$dir/b.{}" >>"$dir/codes.txt"
	rm -f "$dir"/b.*
}

# 数某个 HTTP 码：grep -c 无匹配时输出 0 但退出码非 0，所以不能 `|| echo 0`（会拼出两行）。
s6_codecount() { local n; n=$(grep -cx "$2" "$1" 2>/dev/null); printf '%s' "${n:-0}"; }

# s6_post_edge <fromPort> <toCapId>：建入口边（fromIsSource=true，/v1/events 首轮派发查的就是它）。
s6_post_edge() {
	curl -s -o /dev/null -w '%{http_code}' -X POST "$PC_BASE/v1/edges" \
		-H "Cookie: anotify_session=$PC_SESSION" -H 'content-type: application/json' \
		-d "{\"namespaceId\":\"$PC_NS\",\"fromIsSource\":true,\"fromCapability\":\"perfgen\",\"fromPort\":\"$1\",\"toCapability\":\"$2\"}"
}

# s6_make_backlog <dbpath> <n>：造一个「n 条 40 天前事件」的库（直接 SQL 写，秒级）。
s6_make_backlog() {
	local dbpath="$1" n="$2" ns
	mkdir -p "$(dirname "$dbpath")"
	rm -f "$dbpath" "$dbpath-wal" "$dbpath-shm"
	pc_start_db s6seed 8142 "$dbpath" >/dev/null || return 1
	pc_seed >/dev/null || { pc_stop; return 1; }
	pc_stop
	ns="$PC_NS"
	sqlite3 -cmd '.timeout 8000' "$dbpath" <<SQL >/dev/null
PRAGMA journal_mode=WAL;
WITH RECURSIVE seq(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM seq WHERE i < $n)
INSERT INTO events (id, namespace_id, trace_id, source, type, payload, idem_key, created_at)
SELECT 'evt_bk' || i, '$ns', 'trace_bk' || i, 'perfgen', 's6.ret', '{}', NULL,
       CAST(strftime('%s','now') AS INTEGER) - 40*86400
  FROM seq;
SQL
	PC_NS="$ns"
	return 0
}

# ===========================================================================
# a 段：每事件字节数 + 实测吞吐 + 30 天稳态外推
# ===========================================================================
sec_a() {
	local dir="$S6_OUT/a"
	rm -rf "$dir"
	mkdir -p "$dir"
	unset S6_DB # 见 pc_start_db 的注释：跨段必须清掉，否则 s6_sq 会读上一段的库
	pc_start s6a 8141 ANOTIFY_EVENT_RETENTION_DAYS=0 >/dev/null || die "a 段服务起不来"
	pc_seed >/dev/null || die "a 段 seed 失败"
	local pts="$dir/points.csv"
	s6_size_header >"$pts"

	local sink_id
	sink_id=$(pc_cap perf.sink)
	[ -n "$sink_id" ] || die "a 段建能力失败"
	local edge_code
	edge_code=$(s6_post_edge s6.empty "$sink_id")
	log "a: 入口边 HTTP $edge_code"

	S6_SENT=0
	s6_size m0 >>"$pts"
	local t0 t1
	t0=$(date +%s%3N)
	s6_ingest 1 "$S6_EVENTS" 2 s6.empty "$dir/ing1"
	t1=$(date +%s%3N)
	S6_SENT=$((S6_SENT + S6_EVENTS))
	log "a: 空 payload 第 1 批 ${S6_EVENTS} 条 200=$(s6_codecount "$dir/ing1/codes.txt" 200) 耗时 $((t1 - t0))ms"
	s6_size m1000 >>"$pts"

	t0=$(date +%s%3N)
	s6_ingest $((S6_EVENTS + 1)) "$S6_EVENTS" 2 s6.empty "$dir/ing2"
	t1=$(date +%s%3N)
	S6_SENT=$((S6_SENT + S6_EVENTS))
	log "a: 空 payload 第 2 批 ${S6_EVENTS} 条 200=$(s6_codecount "$dir/ing2/codes.txt" 200) 耗时 $((t1 - t0))ms"
	s6_size m2000 >>"$pts"

	t0=$(date +%s%3N)
	s6_ingest $((2 * S6_EVENTS + 1)) "$S6_EVENTS_1K" 1024 s6.kb "$dir/ing3"
	t1=$(date +%s%3N)
	S6_SENT=$((S6_SENT + S6_EVENTS_1K))
	log "a: 1KB 批 ${S6_EVENTS_1K} 条 200=$(s6_codecount "$dir/ing3/codes.txt" 200) 耗时 $((t1 - t0))ms"
	s6_size m2000_1k >>"$pts"

	# 实测稳态吞吐（conc 8 定速压 20s）——30 天外推用这个 ev/s
	local body="$dir/body.json"
	jq -nc --arg ns "$PC_NS" '{namespaceId:$ns, source:"perfgen", type:"s6.rps", payload:{n:1}}' >"$body"
	taskset -c "$PC_CPUS" "$LG_BIN" -url "$PC_BASE/v1/events" -method POST \
		-body-file "$body" -header "Authorization: Bearer $PC_SEND_KEY" \
		-c "$S6_CONC" -d "${S6_RPS_SECS}s" >"$dir/rps.txt" 2>&1
	local rps
	rps=$(awk '/^RPS[ \t]+/ {print $2; exit}' "$dir/rps.txt")
	[ -n "$rps" ] || rps=0
	# m_end 的 events_sent 只到 1KB 批为止（RPS 段发出多少条不确定），
	# 所以它单独只用来做「大样本 B/event」的旁证，不参与斜率表。
	s6_size m_end >>"$pts"
	pc_stop

	# 斜率：用 events 行数（一条 ingest 可能落 2 行：本体 + 剪枝留痕）
	# 关键口径：一条 ingest 在 events 里落 **2 行**（本体 + 派发剪枝留痕，见 #229），
	# 所以 B/event 必须按「发出的事件条数」除，不能按行数除（按行除会刚好差一半）。
	awk -F, '
		NR==1{next}
		{lab[NR]=$1; pxs[NR]=$5; rows[NR]=$7; sent[NR]=$9; n=NR}
		END{
			for(i=3;i<=n;i++){
				dd=pxs[i]-pxs[i-1]; ds=sent[i]-sent[i-1]; dr=rows[i]-rows[i-1];
				if(ds>0) printf "%s→%s  events_sent=%d delta_rows=%d delta_bytes=%d rows/event=%.2f B/event=%.2f\n",
					lab[i-1],lab[i],ds,dr,dd,dr/ds,dd/ds;
			}
		}' "$pts" >"$dir/deltas.txt"
	# 空 payload 档取两批的均值（单批会有页分配抖动）；1KB 档只有一段。
	local bp bp1k
	bp=$(awk -F'[ =]' '/B\/event=/ && !/1k/ {k=$NF+0; s+=k; n++} END{printf "%.2f", (n?s/n:0)}' "$dir/deltas.txt")
	bp1k=$(awk -F'[ =]' '/B\/event=/ && /1k/ {k=$NF+0; s+=k; n++} END{printf "%.2f", (n?s/n:0)}' "$dir/deltas.txt")
	[ -n "$bp" ] || bp=0
	[ -n "$bp1k" ] || bp1k=0

	{
		echo "## a 段 · 每事件字节数与 30 天稳态"
		echo "--- 测量点（每点先 wal_checkpoint(TRUNCATE)）"
		cat "$pts"
		echo "--- 边际斜率"
		cat "$dir/deltas.txt"
		echo "--- 实测稳态吞吐（conc=$S6_CONC, ${S6_RPS_SECS}s）"
		cat "$dir/rps.txt"
		echo
		echo "空 payload 档 B/event = $bp；1KB payload 档 B/event = $bp1k"
		awk -v bp="$bp" -v rps="$rps" 'BEGIN{
			printf "实测 ev/s ≈ %s\n", rps;
			printf "%-10s %-12s %-12s %-14s\n", "ev/s", "1 天", "30 天(保留窗口)", "10GiB 盘撑";
			split("1 10 30.6 38 100 " rps, rate, " ");
			for(i in rate){
				r=rate[i]+0; if(r<=0) continue;
				day=r*bp*86400; month=r*bp*2592000; days10=(10*1024*1024*1024)/day;
				printf "%-10.1f %-12s %-12s %-14s\n", r, gib(day), gib(month), sprintf("%.1f 天", days10);
			}
			printf "\n10GiB 盘 + 30 天保留窗口 → 可持续写入上限 = %.2f ev/s\n", (10*1024*1024*1024)/(bp*2592000);
			printf "（auto_vacuum=0：删除只把页放进 freelist，文件不会缩小——稳态是「页数到顶不再涨」，不是「变小」）\n";
		}
		function gib(b){ return sprintf("%.2f GiB", b/1024/1024/1024) }'
	} | s6_section_tee
	log "a 段完成 → $dir"
}

# ===========================================================================
# b 段：保留期稳态（页数平台化）+ 清理吞吐 + 可重入
# ===========================================================================
sec_b() {
	local dir="$S6_OUT/b"
	rm -rf "$dir"
	mkdir -p "$dir"
	local db="$PC_ROOT/runs/s6b-db/perf.db"
	mkdir -p "$PC_ROOT/runs/s6b-db"

	log "b: 造 $S6_BACKLOG 条「40 天前」的事件…"
	s6_make_backlog "$db" "$S6_BACKLOG" || die "b 段造积压失败"

	# ---- 第 1 轮：retention=1 天 → 首轮 Tick（5s）就开删，追赶轮 30s 一轮排空 ----
	pc_start_db s6b 8142 "$db" ANOTIFY_EVENT_RETENTION_DAYS=1 ANOTIFY_LOG_LEVEL=info \
		|| die "b 段服务起不来"
	s6_size before >"$dir/sizes.txt"
	local waited=0 left
	while [ $waited -lt 180 ]; do
		sleep 5
		waited=$((waited + 5))
		left=$(s6_sq1 "select count(*) from events where type='s6.ret';")
		[ "${left:-0}" -eq 0 ] && break
	done
	log "b: 积压排空耗时 ≈ ${waited}s（剩余 $left 行）"
	s6_size after >>"$dir/sizes.txt"
	grep -h 'lease.sweeper.retention' "$PC_RUN/server.log" >"$dir/sweep-round1.log" 2>/dev/null || true
	pc_stop

	# ---- 第 2 轮：同一个库、已无过期数据 → 再跑一轮必须删 0 条且不报错（可重入）----
	pc_start_db s6b2 8143 "$db" ANOTIFY_EVENT_RETENTION_DAYS=1 ANOTIFY_LOG_LEVEL=info \
		|| die "b 段第 2 轮服务起不来"
	sleep 14 # 越过首轮 Tick（5s）+ 一次追赶门（30s 内的第一拍不会来，这里只看「不该有删除」）
	local r2_failed r2_deleted
	r2_failed=$(grep -c 'lease.sweeper.retention_failed' "$PC_RUN/server.log" 2>/dev/null); r2_failed=${r2_failed:-0}
	r2_deleted=$(grep -h 'lease.sweeper.retention' "$PC_RUN/server.log" 2>/dev/null | grep -oE 'events_deleted=[0-9]+' | awk -F= '{s+=$2} END{print s+0}')
	cp "$PC_RUN/server.log" "$dir/second-round.log"
	pc_stop

	# ---- 第 3 轮：删空后再灌同样多行 → 看页数是否被 freelist 复用（平台化）----
	local ns_refill
	ns_refill=$(sqlite3 -cmd '.timeout 8000' "$db" 'SELECT id FROM namespaces ORDER BY created_at, id LIMIT 1;')
	sqlite3 -cmd '.timeout 8000' "$db" <<SQL >/dev/null
WITH RECURSIVE seq(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM seq WHERE i < $S6_BACKLOG)
INSERT INTO events (id, namespace_id, trace_id, source, type, payload, idem_key, created_at)
SELECT 'evt_re' || i, '$ns_refill', 'trace_re' || i, 'perfgen', 's6.refill', '{}', NULL,
       CAST(strftime('%s','now') AS INTEGER)
  FROM seq;
SQL
	S6_DB="$db" s6_size refilled >>"$dir/sizes.txt"

	{
		echo "## b 段 · 保留期稳态与清理（积压 $S6_BACKLOG 条 40 天前事件，retention=1 天）"
		echo "--- 尺寸 / 页数：before=满积压  after=删空  refilled=重新灌同样多行"
		cat "$dir/sizes.txt"
		echo "--- 清理轮次（events_deleted / elapsed_ms / truncated）"
		if [ -s "$dir/sweep-round1.log" ]; then cat "$dir/sweep-round1.log"; else echo "(无 retention 日志)"; fi
		echo "--- 第 2 轮（同库、已无过期数据）"
		echo "retention_failed 条数 = $r2_failed（必须是 0）"
		echo "本进程 events_deleted 合计 = $r2_deleted（必须是 0）"
		awk -F, '{db[$1]=$2;pg[$1]=$3;fl[$1]=$6}
			END{
				printf "\n文件字节 : before=%s after=%s refilled=%s\n", db["before"],db["after"],db["refilled"];
				printf "page_count: before=%s after=%s refilled=%s\n", pg["before"],pg["after"],pg["refilled"];
				printf "freelist  : before=%s after=%s refilled=%s\n", fl["before"],fl["after"],fl["refilled"];
				if(db["before"]+0>0) printf "删空后文件剩 %.1f%%（auto_vacuum=0 → 不压缩，页进 freelist）\n", db["after"]*100.0/db["before"];
				if(db["before"]+0>0) printf "重灌同样多行后回到 %.1f%%（freelist 复用 → 稳态是**平台化**，不是变小）\n", db["refilled"]*100.0/db["before"];
			}' "$dir/sizes.txt"
		echo "单轮删除吞吐见上面 elapsed_ms（每轮预算 = batches × batch = 20 × 1000 行）"
	} | s6_section_tee
	log "b 段完成 → $dir"
}

# ===========================================================================
# c 段：清理不影响读路径 p95
# ===========================================================================
# 为什么用「两个命名空间」：直接在同一个命名空间里造过期行再删掉，会产生一个
# 与被测目标无关的巨大混淆项——**表变小了，读变快了**。实测（第一版脚本）删完之后
# p95 反而更好（111ms → 64ms），那证明的是「数据少了读得快」，不是「清理不伤读」。
# 所以：读压测固定打命名空间 A（行数全程不变），清理删的是命名空间 B 的 26000 行过期事件。
# 两者共用同一个 SQLite 库、同一张表、同一把写锁——正是要找的那个交互。
sec_c() {
	local dir="$S6_OUT/c"
	rm -rf "$dir"
	mkdir -p "$dir"
	unset S6_DB
	local base_db="$PC_ROOT/runs/s6c-base/perf.db"
	mkdir -p "$PC_ROOT/runs/s6c-base"
	log "c: 造两命名空间库（A=24000 条新事件供读，B=26000 条 40 天前事件供删）…"

	rm -f "$base_db" "$base_db-wal" "$base_db-shm"
	pc_start_db s6cseed 8144 "$base_db" >/dev/null || die "c 段建库服务起不来"
	pc_seed >/dev/null || { pc_stop; die "c 段 seed 失败"; }
	local ns_a="$PC_NS"
	local ns_b
	ns_b=$(curl -s -X POST "$PC_BASE/v1/namespaces" -H "Cookie: anotify_session=$PC_SESSION" \
		-H 'content-type: application/json' -d '{"slug":"cleanup","name":"清理专用"}' | jq -r '.namespace.id // empty')
	pc_stop
	[ -n "$ns_b" ] || die "c 段建第二个命名空间失败"
	log "c: nsA=$ns_a nsB=$ns_b"

	sqlite3 -cmd '.timeout 8000' "$base_db" <<SQL >/dev/null
PRAGMA journal_mode=WAL;
WITH RECURSIVE seq(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM seq WHERE i < 24000)
INSERT INTO events (id, namespace_id, trace_id, source, type, payload, idem_key, created_at)
SELECT 'evt_a' || i, '$ns_a', 'trace_a' || i, 'perfgen', 'c.read', '{}', NULL,
       CAST(strftime('%s','now') AS INTEGER) - i
  FROM seq;
WITH RECURSIVE seq(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM seq WHERE i < 26000)
INSERT INTO events (id, namespace_id, trace_id, source, type, payload, idem_key, created_at)
SELECT 'evt_b' || i, '$ns_b', 'trace_b' || i, 'perfgen', 'c.sweep', '{}', NULL,
       CAST(strftime('%s','now') AS INTEGER) - 40*86400
  FROM seq;
SQL
	sqlite3 -cmd '.timeout 8000' "$base_db" 'PRAGMA wal_checkpoint(TRUNCATE);' >/dev/null 2>&1
	local off_db="$PC_ROOT/runs/s6c-off/perf.db" on_db="$PC_ROOT/runs/s6c-on/perf.db"
	mkdir -p "$PC_ROOT/runs/s6c-off" "$PC_ROOT/runs/s6c-on"
	# **必须**连 -wal/-shm 一起清掉再拷。踩过的坑：上一轮跑剩的 perf.db-wal 会被
	# 新拷进来的主库「重放」，帧页号与新的主库对不上 → 服务端立刻报
	# `database disk image is malformed (11)`，整个读压测 100% 500。
	# 这个坑只在「重复跑同一个区段」时出现，很容易被误读成产品把库写坏了。
	rm -f "$off_db" "$off_db-wal" "$off_db-shm" "$on_db" "$on_db-wal" "$on_db-shm"
	cp "$base_db" "$off_db"
	cp "$base_db" "$on_db"

	# s6_c_read <run-name> <port> <db> <retention-days> <outfile>
	# 先跑 4s 预热（丢弃），再量 ${S6_READ_SECS}s：两次的冷热状态才可比。
	s6_c_read() {
		local name="$1" port="$2" db="$3" days="$4" out="$5"
		pc_start_db "$name" "$port" "$db" ANOTIFY_EVENT_RETENTION_DAYS="$days" ANOTIFY_LOG_LEVEL=info || return 1
		pc_seed >/dev/null || { pc_stop; return 1; }
		local url="$PC_BASE/v1/console/events?limit=20&namespaceId=$ns_a"
		taskset -c "$PC_CPUS" "$LG_BIN" -url "$url" -header "Cookie: anotify_session=$PC_SESSION" \
			-c 4 -d 4s >/dev/null 2>&1
		taskset -c "$PC_CPUS" "$LG_BIN" -url "$url" -header "Cookie: anotify_session=$PC_SESSION" \
			-c 4 -d "${S6_READ_SECS}s" >"$out" 2>&1
		return 0
	}

	s6_c_read s6coff 8144 "$off_db" 0 "$dir/read-baseline.txt" || die "c 段基线起不来"
	grep -h 'lease.sweeper' "$PC_RUN/server.log" >"$dir/sweep-baseline.log" 2>/dev/null || true
	pc_stop
	s6_c_read s6con 8145 "$on_db" 1 "$dir/read-under-sweep.txt" || die "c 段对照起不来"
	grep -h 'lease.sweeper.retention' "$PC_RUN/server.log" >"$dir/sweep.log" 2>/dev/null || true
	pc_stop

	{
		echo "## c 段 · 清理对读路径 p95 的影响（读 A 命名空间，删 B 命名空间 26000 行）"
		echo "读目标：GET /v1/console/events?limit=20（A 的行数全程 24000，不随清理变化）"
		echo "--- baseline（retention=0，不跑清理）"
		cat "$dir/read-baseline.txt"
		echo "--- under sweep（retention=1，删除发生在压测窗口内）"
		cat "$dir/read-under-sweep.txt"
		echo "--- baseline 那台的清理日志（retention=0 ⇒ 应是 permanent / events 一条不删）"
		if [ -s "$dir/sweep-baseline.log" ]; then cat "$dir/sweep-baseline.log"; else echo "(baseline 无 retention 日志)"; fi
		echo "--- under sweep 这一轮删了多少 / 花了多久"
		if [ -s "$dir/sweep.log" ]; then cat "$dir/sweep.log"; else echo "(无 retention 日志：清理可能没落在窗口内)"; fi
		echo "--- WAL 语义：读者不阻塞写者、写者不阻塞读者，两侧没有共享锁；"
		echo "    所以预期是「无可测退化」，上面两个 p95 就是这句话的实测。"
	} | s6_section_tee
	log "c 段完成 → $dir"
}

# ===========================================================================
# d 段：未超阈时的额外开销（阈值 0 = 不启用 vs 阈值 86400 = 启用但永不触发）
# ===========================================================================
# 为什么要**交错**跑两轮 A/B（off,huge / huge,off）而不是各跑一次：
# 这台机器同时有别的路在压测，单次 20s 的 RPS 会随外部负载漂 ±10%（实测过 715 vs 811）。
# 那种噪声比「一次 atomic.Load」大三个数量级，单次 A/B 会把噪声读成结论。
# 交错后每一对都是「同一时段、只有配置不同」，看的是配对差值。
#
# 真正的机械证据在单测里（internal/server/backpressure_test.go 的 benchmark：
# 裸 handler vs 包一层，分配完全相同 32B/2allocs，ns 差在噪声带内），
# 这里量的是端到端能不能看出差别——结论应该是「看不出」。
sec_d() {
	local dir="$S6_OUT/d"
	rm -rf "$dir"
	mkdir -p "$dir"
	unset S6_DB
	local body="$dir/body.json"
	local dsec="${S6_DSEC:-20}"

	# s6_d_run <tag> <maxage> <outfile>
	s6_d_run() {
		local tag="$1" maxage="$2" out="$3"
		pc_start "s6d-$tag" 8146 ANOTIFY_PENDING_MAX_AGE_SEC="$maxage" >/dev/null || return 1
		pc_seed >/dev/null || { pc_stop; return 1; }
		jq -nc --arg ns "$PC_NS" '{namespaceId:$ns, source:"perfgen", type:"s6.bp", payload:{n:1}}' >"$body"
		taskset -c "$PC_CPUS" "$LG_BIN" -url "$PC_BASE/v1/events" -method POST \
			-body-file "$body" -header "Authorization: Bearer $PC_SEND_KEY" \
			-header 'content-type: application/json' \
			-c "$S6_CONC" -d "${dsec}s" >"$out" 2>&1
		s6_size "size-$tag" >>"$dir/sizes.txt"
		pc_stop
		return 0
	}

	local plan=(off huge huge off) i=0 pair=0
	for tag in "${plan[@]}"; do
		i=$((i + 1))
		local maxage=0
		[ "$tag" = huge ] && maxage=86400
		[ $((i % 2)) -eq 1 ] && pair=$((pair + 1))
		s6_d_run "$tag$pair" "$maxage" "$dir/run-$tag-$pair.txt" || die "d 段($tag/$pair)起不来"
	done

	{
		echo "## d 段 · 未超阈时的额外开销（同负载 ${dsec}s / conc=$S6_CONC / 交错两对 A/B）"
		for pair in 1 2; do
			echo "--- 第 $pair 对"
			echo "[off = 阈值 0 不启用]"
			cat "$dir/run-off-$pair.txt"
			echo "[huge = 阈值 86400，后台采样照跑、每请求只读 atomic]"
			cat "$dir/run-huge-$pair.txt"
		done
		echo "--- 落库行数（**不作为结论**：两轮 RPS 本身会因外部负载差很多，行数跟着差；"
		echo "    判断「有没有挡」看状态码 —— 两次都必须全是 200，出现 503 才是挡了）"
		cat "$dir/sizes.txt"
		# 只抽取两个量：RPS 与 p95（loadgen 的输出格式是固定的两行）。
		awk -v dir="$dir" '
			# 注意：awk 的 regexp **字面量**不能当函数实参传（会被求值成 0/1，
			# 表现为 "regexp constant for parameter yields boolean value" 且什么都抓不到）。
			# 这里传字符串，交给 match() 当正则解释。
			function grab(f, re,   line, m, v) {
				v = ""
				while ((getline line < f) > 0) {
					if (match(line, re)) {
						m = substr(line, RSTART, RLENGTH)
						sub(/^[^0-9]*/, "", m)   # 去掉 "p95=" / "RPS   " 这类前缀
						gsub(/[^0-9.]/, "", m)   # 去掉单位（ms）
						v = m + 0
					}
				}
				close(f)
				return v
			}
			BEGIN{
				for (p = 1; p <= 2; p++) {
					off = dir "/run-off-" p ".txt"; hug = dir "/run-huge-" p ".txt";
					ro = grab(off, "RPS[ ]+[0-9.]+"); rh = grab(hug, "RPS[ ]+[0-9.]+");
					po = grab(off, "p95=[0-9.]+"); ph = grab(hug, "p95=[0-9.]+");
					printf "第 %d 对：RPS %0.1f → %0.1f (%+.2f%%)   p95 %0.3fms → %0.3fms\n",
						p, ro, rh, (ro>0 ? (rh-ro)*100.0/ro : 0), po, ph;
				}
			}'
		echo "（p95 取不到时直接看上面两段原文；RPS 的配对差值才是可控变量下的结论）"
	} | s6_section_tee
	log "d 段完成 → $dir"
}

# ===========================================================================
# e 段：超阈 → 503 比例与延迟；已在库里的任务不丢
# ===========================================================================
sec_e() {
	local dir="$S6_OUT/e"
	rm -rf "$dir"
	mkdir -p "$dir"
	unset S6_DB
	pc_start s6e 8147 ANOTIFY_PENDING_MAX_AGE_SEC=5 ANOTIFY_LOG_LEVEL=info >/dev/null || die "e 段起不来"
	pc_seed >/dev/null

	local sink_id
	sink_id=$(pc_cap perf.sink)
	[ -n "$sink_id" ] || die "e 段建能力失败"
	log "e: 入口边 HTTP $(s6_post_edge s6.bp "$sink_id")"

	# 极慢消费者：1 槽位 × 每单 3s = 0.33 task/s；压测远超它 → 积压迅速涨过 5s 阈值
	node "$PC_REPO/scripts/perf/c/agent.mjs" --base "$PC_BASE" --db "$PC_DB" --ns "$PC_NS" \
		--cap-id "$sink_id" --cap-name perf.sink --slots 1 --delay-ms 3000 \
		>"$dir/agent.log" 2>&1 &
	local agent_pid=$!
	local ok=0
	for _ in $(seq 1 60); do grep -q '"ev":"ready"' "$dir/agent.log" 2>/dev/null && { ok=1; break; }; sleep 0.2; done
	if [ "$ok" != "1" ]; then
		tail -20 "$dir/agent.log" | tee -a "$S6_REPORT"
		kill "$agent_pid" 2>/dev/null
		pc_stop
		die "e 段节点没 ready"
	fi

	local body="$dir/body.json"
	jq -nc --arg ns "$PC_NS" '{namespaceId:$ns, source:"perfgen", type:"s6.bp", payload:{n:1}}' >"$body"
	local tasks_before; tasks_before=$(s6_sq1 'select count(*) from tasks;')
	taskset -c "$PC_CPUS" "$LG_BIN" -url "$PC_BASE/v1/events" -method POST \
		-body-file "$body" -header "Authorization: Bearer $PC_SEND_KEY" \
		-header 'content-type: application/json' \
		-c "$S6_CONC" -d "${S6_BP_SECS}s" >"$dir/load.txt" 2>&1
	local tasks_after pending_after oldest_age
	tasks_after=$(s6_sq1 'select count(*) from tasks;')
	pending_after=$(s6_sq1 "select count(*) from tasks where status='pending';")
	oldest_age=$(s6_sq1 "select cast(strftime('%s','now') as integer) - min(created_at) from tasks where status='pending';")
	kill "$agent_pid" 2>/dev/null
	grep -h 'server.backpressure' "$PC_RUN/server.log" >"$dir/backpressure.log" 2>/dev/null || true
	pc_stop

	{
		echo "## e 段 · 超阈 503（阈值 5s / 1 槽位每单 3s / 压 ${S6_BP_SECS}s）"
		echo "--- 混合响应（前几秒 200，超阈后 503）"
		cat "$dir/load.txt"
		echo "--- 不丢证据"
		echo "tasks_before=$tasks_before tasks_after=$tasks_after pending_after=$pending_after oldest_pending_age_sec=$oldest_age"
		echo "--- 背压日志（engaged / released / still_engaged）"
		if [ -s "$dir/backpressure.log" ]; then cat "$dir/backpressure.log"; else echo "(无背压日志)"; fi
	} | s6_section_tee
	log "e 段完成 → $dir"
}

# ===========================================================================
# f 段：未超阈时的额外开销 —— 进程内口径（CPU 秒/请求 + CPU profile）
# ===========================================================================
# 为什么另开一段而不是只靠 d 段的 RPS/p95 对比：这台机器是共享的（批次 5 期间实测
# load average 21 / 12 核，另外两路也在压测），端到端 RPS 会漂 ±10~40%。
# 那种噪声比「一次 atomic.Load」大 8 个数量级，单看 RPS 差只会读到别人的负载。
# 所以这里用两个**进程内**口径：
#   ① 服务进程自己的 CPU 秒 / 已处理请求数（/proc/<pid>/stat 的 utime+stime）
#      —— 外部负载影响的是墙钟，CPU-per-request 稳定得多；
#   ② 20s CPU profile 里 backpressure 相关符号占多少采样。
sec_f() {
	local dir="$S6_OUT/f"
	rm -rf "$dir"
	mkdir -p "$dir"
	unset S6_DB
	local body="$dir/body.json"
	local secs="${S6_F_SECS:-25}"

	# s6_f_run <tag> <maxage>
	s6_f_run() {
		local tag="$1" maxage="$2"
		pc_start "s6f-$tag" 8148 ANOTIFY_PENDING_MAX_AGE_SEC="$maxage" >/dev/null || return 1
		pc_seed >/dev/null || { pc_stop; return 1; }
		jq -nc --arg ns "$PC_NS" '{namespaceId:$ns, source:"perfgen", type:"s6.bp", payload:{n:1}}' >"$body"
		local pid="$PC_PID" t0 t1 c0 c1 r0 r1 w0 w1
		t0=$(date +%s%N)
		c0=$(awk '{print $14+$15}' "/proc/$pid/stat" 2>/dev/null || echo 0) # utime+stime（ticks）
		# /proc/<pid>/io 的 syscr = 读系统调用次数。SQLite 取页最终落到 pread64，
		# 所以「多查一次库」在进程级是**可数**的。这是给「未超阈时请求路径 0 次 DB 往返」
		# 配的进程级旁证：真多查了，syscr/请求 会跟着涨。
		r0=$(awk '/^syscr:/{print $2}' "/proc/$pid/io" 2>/dev/null || echo 0)
		taskset -c "$PC_CPUS" "$LG_BIN" -url "$PC_BASE/v1/events" -method POST \
			-body-file "$body" -header "Authorization: Bearer $PC_SEND_KEY" \
			-header 'content-type: application/json' \
			-c "$S6_CONC" -d "${secs}s" >"$dir/load-$tag.txt" 2>&1 &
		local lgpid=$!
		sleep 3
		# 覆盖 load 的中段（pprof 至少要 1s 才有采样）
		curl -s -o "$dir/cpu-$tag.pprof" "$PC_BASE/debug/pprof/profile?seconds=$((secs - 8))" || true
		wait "$lgpid" 2>/dev/null || true
		t1=$(date +%s%N)
		c1=$(awk '{print $14+$15}' "/proc/$pid/stat" 2>/dev/null || echo 0)
		r1=$(awk '/^syscr:/{print $2}' "/proc/$pid/io" 2>/dev/null || echo 0)
		local req rps hz
		req=$(awk '/^总请求/ {print $2}' "$dir/load-$tag.txt")
		rps=$(awk '/^RPS[ \t]+/ {print $2; exit}' "$dir/load-$tag.txt")
		hz=$(getconf CLK_TCK 2>/dev/null || echo 100)
		[ -n "$req" ] && [ "${req:-0}" -gt 0 ] || { pc_stop; return 1; }
		# CPU 微秒/请求 = (ticks差 / CLK_TCK) * 1e6 / 请求数
		awk -v c0="$c0" -v c1="$c1" -v r0="$r0" -v r1="$r1" -v hz="$hz" -v req="$req" -v rps="$rps" -v tag="$tag" '
			BEGIN{
				usec = (c1 - c0) * 1000000.0 / hz;
				printf "%s,requests=%d,rps=%.1f,server_cpu_ms=%.0f,cpu_us_per_request=%.2f,read_syscalls=%d,read_syscalls_per_request=%.4f\n",
					tag, req, rps, usec/1000.0, usec/req, r1 - r0, (r1 - r0) / req;
			}' >>"$dir/cpu.csv"
		pc_stop
		return 0
	}

	s6_f_run off 0 || die "f 段(off)失败"
	s6_f_run huge 86400 || die "f 段(huge)失败"

	{
		echo "## f 段 · 进程内额外开销（${secs}s 同负载，阈值 0 vs 86400）"
		echo "读法：cpu_us_per_request / read_syscalls_per_request 是**每请求**的进程级成本。"
		echo "      后台采样器每秒多查 0~1 次库（自适应 5s/1s），摊到几百 rps 上就是 0.00x 次/请求。"
		echo "      真正的机械证据是单测：10000 条请求 → OldestPendingCreatedAt 只被调用 1 次（显式采样）。"
		cat "$dir/cpu.csv"
		echo "--- CPU profile 里 backpressure 相关符号的采样占比（huge 启用档）"
		go tool pprof -top -nodecount=40 "$dir/cpu-huge.pprof" 2>/dev/null \
			| grep -iE 'backpressure|Total samples|Dropped' | head -10 || echo "(pprof 无匹配)"
		echo "--- 对照：off 档的 Total samples（同一压测时长，用来比总量）"
		go tool pprof -top -nodecount=1 "$dir/cpu-off.pprof" 2>/dev/null | head -3 || echo "(pprof 不可用)"
		go tool pprof -top -nodecount=1 "$dir/cpu-huge.pprof" 2>/dev/null | head -3 || echo "(pprof 不可用)"
	} | s6_section_tee
	log "f 段完成 → $dir"
}

# ===========================================================================
main() {
	mkdir -p "$S6_OUT"
	: >"$S6_REPORT"
	log "分核=$PC_CPUS 端口段=$PC_PORT_LO-$PC_PORT_HI 产物=$PC_ROOT"
	pc_require_env || die "环境检查失败"
	s6_build || die "构建失败"
	local sections=("$@")
	[ ${#sections[@]} -eq 0 ] && sections=(a b c d e f)
	local s
	for s in "${sections[@]}"; do
		S6_SECTION_REPORT="$S6_OUT/report-$s.txt"
		: >"$S6_SECTION_REPORT"
		case "$s" in
		a) sec_a ;;
		b) sec_b ;;
		c) sec_c ;;
		d) sec_d ;;
		e) sec_e ;;
		f) sec_f ;;
		*) die "未知区段 $s（可用：a b c d e f）" ;;
		esac
	done
	log "全部完成 → $S6_REPORT"
}

main "$@"
