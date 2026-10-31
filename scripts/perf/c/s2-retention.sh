#!/usr/bin/env bash
# C2 · 事件保留未开 → 增长速率与外推（#217 / C 路）
#
# 要回答三个问题：
#   1. 一条事件在磁盘上到底值多少字节？空 payload 与 1KB payload 两档差多少？
#   2. 「行数 → 文件大小」是线性的吗？（500/1000/2000 三个点 + dbstat 拆表）
#   3. 代码里有没有上限机制？没有的话，按 38 / 1 / 100 ev/s 三档外推
#      一小时 / 一天 / 一周 / 一个月要多少磁盘、多大盘能撑多久。
#
# 测量口径（为什么这么干）：
#   * **测体积前先 `PRAGMA wal_checkpoint(TRUNCATE)`**：不这么做量到的全是 WAL 噪声。
#     实测本机基线 DB 4KB 而 WAL 1.39MB（pc_build+pc_seed 的产物），3 条事件
#     在未 checkpoint 的口径下"增长"了 340KB/条 —— 纯属自己吓自己。
#   * **条数必须精确**：用 `seq N | xargs -P` 发定长批，而不是 loadgen 的
#     "跑 N 秒"（后者只知道大概发了多少条，算不出 bytes/event）。
#   * 压测命令一律 `taskset -c $PC_CPUS`（契约 §1：不抢另外两路的核）。
#   * 走 `POST /v1/events`（Bearer send key）而不是 hook：hook 有 60/min/IP
#     硬限速（internal/server/ratelimit.go: rlHookPerMin，契约 §6 已实测），
#     灌 2000 条要 33 分钟。
#
# 用法：
#   bash scripts/perf/c/s2-retention.sh           # 全流程，产物落 /tmp/perf-c/s2/
#   S2_EVENTS=200 bash scripts/perf/c/s2-retention.sh   # 快速自检（缩短每批条数）
#
# 产物（都在 $PC_ROOT/s2，默认 /tmp/perf-c/s2）：
#   points.csv            每个测量点的机器可读汇总（行数/文件大小/页数/dbstat）
#   measure-<label>.txt   每个点的原始命令输出（checkpoint + stat + sqlite 查询）
#   post-<label>.codes    每批的逐请求 HTTP 码（用来核对"发了 N 条 = 收下 N 条"）
#   wal-watch.csv         灌数据期间的 WAL 尺寸采样（看 autocheckpoint 压不压得住）
#   code-facts.txt        "有没有上限机制"的 grep 原文（文件:行可直接引用）
#   extrapolation.txt     外推表（脚本自己算，不是手填）

set -uo pipefail

source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/common.sh"

S2_OUT="$PC_ROOT/s2"
S2_PORT="${S2_PORT:-7941}"
S2_ALT_PORT="${S2_ALT_PORT:-7946}" # 契约 §3：撞端口顺延到保留重试位
S2_CONC="${S2_CONC:-8}"            # 并发连接数（argon2 是 CPU 瓶颈，8 路足够打满 4 核）
S2_EVENTS="${S2_EVENTS:-2000}"     # 空 payload 档的目标条数
S2_EVENTS_1K="${S2_EVENTS_1K:-1000}" # 1KB payload 档追加条数
S2_PAYLOAD_1K="${S2_PAYLOAD_1K:-1024}"
S2_SEQ=0
S2_LAST_TIER="base" # 上一个测量点之前灌的那一批属于哪一档（写进 points.csv 供切片）

log() { echo "[s2 $(date +%H:%M:%S)] $*"; }

# ---------------------------------------------------------------- sqlite 小工具
# sq：所有查询都带 busy timeout —— 服务的租约巡检每 5s 会写一次库，撞上了不该
# 让测量点整段失败（更不该把它读成"查询出错所以没数据"）。
sq() { sqlite3 -cmd '.timeout 5000' "$PC_DB" "$1" 2>/dev/null || true; }
sq1() {
	local v
	v=$(sq "$1" | head -1)
	echo "${v:-0}"
}

# s2_payload：生成**长度精确可控**的 payload。
# seq 用定宽 6 位十进制（保证 payload 长度恒定：否则每换一个数量级 payload 就变长，
# bytes/event 会随 seq 漂移 —— 2000 条差 4000 字节，虽然小但会让线性度看起来是假象）。
s2_payload() {
	local seq6
	printf -v seq6 '%06d' "$1"
	if [ "${S2_PAYLOAD:-0}" -le 2 ]; then
		printf '{}'
		return 0
	fi
	printf '{"seq":"%s","pad":"%s"}' "$seq6" "$S2_PAD"
}

# s2_one：发一条事件，只打印 HTTP 码（xargs 的每个 worker 调它一次）。
s2_one() {
	local body
	body=$(printf '{"namespaceId":"%s","source":"perfgen","type":"%s","payload":%s}' \
		"$PC_NS" "$S2_TIER" "$(s2_payload "$1")")
	curl -s -o /dev/null -w '%{http_code}\n' --max-time 30 \
		-X POST "$PC_BASE/v1/events" \
		-H "Authorization: Bearer $PC_SEND_KEY" \
		-H 'content-type: application/json' \
		--data-binary "$body"
}

# s2_measure <label>：一个测量点。顺序很重要 —— 先 checkpoint 再 stat，
# 否则读到的是"还没回写的主库 + 一大堆 WAL"。
s2_measure() {
	local label="$1"
	local raw="$S2_OUT/measure-$label.txt"
	local ck busy="" tries=0
	{
		echo "## label=$label  ts=$(date -Is)  events_posted=$S2_SEQ"
		echo "--- PRAGMA wal_checkpoint(TRUNCATE) ---"
	} >"$raw"
	# TRUNCATE 模式要等所有读者退出；服务进程可能正持着一笔写事务，
	# 于是返回 (busy=1,...)。重试几次而不是将就 —— 将就量到的 WAL 是噪声。
	while [ $tries -lt 6 ]; do
		tries=$((tries + 1))
		ck=$(sqlite3 -cmd '.timeout 5000' "$PC_DB" 'PRAGMA wal_checkpoint(TRUNCATE);' 2>&1)
		echo "  try$tries: $ck" >>"$raw"
		busy="${ck%%|*}"
		[ "$busy" = "0" ] && break
		sleep 0.3
	done

	local db wal
	db=$(stat -c %s "$PC_DB" 2>/dev/null || echo 0)
	wal=$(stat -c %s "$PC_DB-wal" 2>/dev/null || echo 0)
	{
		echo "--- stat ---"
		stat -c '%n %s' "$PC_DB" 2>/dev/null
		stat -c '%n %s' "$PC_DB-wal" 2>/dev/null || echo "  (no wal file)"
		echo "--- 行数 ---"
		sq "select 'events_rows', count(*) from events;"
		echo "--- 按 source 分行（perfgen=真事件，route=派发剪枝行）---"
		sq "select source, type, count(*) from events group by 1, 2 order by 1, 2;"
		echo "--- payload 长度 ---"
		sq "select 'avg_payload_len', cast(avg(length(payload)) as int), 'sum_payload_len', sum(length(payload)) from events;"
		sq "select source, cast(avg(length(payload)) as int), sum(length(payload)) from events group by 1;"
		echo "--- tasks ---"
		sq "select 'tasks_rows', count(*) from tasks;"
		echo "--- 页面/空洞 ---"
		sq 'PRAGMA page_size;'
		sq 'PRAGMA page_count;'
		sq 'PRAGMA freelist_count;'
		echo "--- dbstat 按表/索引拆（events 相关）---"
		sq "select name, count(*) pages, sum(pgsize) bytes from dbstat group by name
		    having name like '%events%' order by bytes desc;"
		echo "--- dbstat 全库合计 ---"
		sq "select 'dbstat_total', sum(pgsize) from dbstat;"
		echo "--- 页数×页大小 vs 实际文件字节（checkpoint(TRUNCATE) 后应当相等）---"
		sq "select 'page_bytes', (select * from pragma_page_count()) * (select * from pragma_page_size()), 'db_bytes', $(stat -c %s "$PC_DB" 2>/dev/null || echo 0);"
	} >>"$raw" 2>&1

	local rows prune tasks page_count freelist page_size avg_pl dbst_ev dbst_ret page_bytes
	rows=$(sq1 "select count(*) from events;")
	prune=$(sq1 "select count(*) from events where source='route';")
	tasks=$(sq1 "select count(*) from tasks;")
	page_size=$(sq1 'PRAGMA page_size;')
	page_count=$(sq1 'PRAGMA page_count;')
	freelist=$(sq1 'PRAGMA freelist_count;')
	avg_pl=$(sq1 "select cast(coalesce(avg(length(payload)),0) as int) from events;")
	dbst_ev=$(sq1 "select coalesce(sum(pgsize),0) from dbstat where name='events';")
	dbst_ret=$(sq1 "select coalesce(sum(pgsize),0) from dbstat where name='idx_events_retention';")
	page_bytes=$((page_size * page_count))

	[ -f "$S2_OUT/points.csv" ] || \
		echo "label,tier,posted,event_rows,prune_rows,tasks_rows,db_bytes,wal_bytes,total_bytes,page_size,page_count,page_bytes,freelist,avg_payload_len,dbstat_events,dbstat_idx_events_retention" >"$S2_OUT/points.csv"
	echo "$label,$S2_LAST_TIER,$S2_SEQ,$((rows - prune)),$prune,$tasks,$db,$wal,$((db + wal)),$page_size,$page_count,$page_bytes,$freelist,$avg_pl,$dbst_ev,$dbst_ret" \
		>>"$S2_OUT/points.csv"
	log "measure $label: rows=$rows (perfgen=$((rows - prune)) route=$prune) tasks=$tasks db=$db wal=$wal pages=$page_count (page_bytes=$page_bytes) freelist=$freelist"
}

# s2_post_batch <count> <payload_bytes> <tier> <label>
# 定长批：发多少条脚本自己知道（seq 区间就是条数），且逐请求状态码全部留档。
s2_post_batch() {
	local count="$1" plen="$2" tier="$3" label="$4"
	local start t0 t1 dur codes ok

	export PC_BASE PC_SEND_KEY PC_NS
	export S2_TIER="$tier" S2_PAYLOAD="$plen"
	# pad 长度一次性算好并导出：让 worker 里不再有子进程开销，payload 长度恒定。
	#   总长 = 8("{\"seq\":\"") + 6(序号) + 9("\",\"pad\":\"") + P + 2("\"}") = 25 + P
	if [ "$plen" -gt 2 ]; then
		S2_PAD=$(head -c $((plen - 25)) /dev/zero | tr '\0' 'x')
		export S2_PAD
	fi
	export -f s2_payload s2_one

	start=$((S2_SEQ + 1))
	codes="$S2_OUT/post-$label.codes"

	# WAL 观测：灌的同时每 0.2s 记一次尺寸，看 autocheckpoint 有没有压住。
	(
		while :; do
			[ -f "$PC_DB-wal" ] && echo "$label,$(date +%s.%N),$(stat -c %s "$PC_DB-wal")"
			sleep 0.2
		done
	) >>"$S2_OUT/wal-watch.csv" 2>/dev/null &
	local watcher=$!

	log "post $label: $count 条, payload=${plen}B, c=$S2_CONC, taskset -c $PC_CPUS"
	t0=$(date +%s.%N)
	seq "$start" $((start + count - 1)) \
		| taskset -c "$PC_CPUS" xargs -P "$S2_CONC" -n1 bash -c 's2_one "$1"' _ \
			>"$codes"
	t1=$(date +%s.%N)
	dur=$(awk -v a="$t0" -v b="$t1" 'BEGIN{printf "%.2f", b-a}')
	kill "$watcher" 2>/dev/null
	wait "$watcher" 2>/dev/null

	S2_SEQ=$((start + count - 1))
	S2_LAST_TIER="$tier"
	ok=$(grep -c '^200$' "$codes" 2>/dev/null)
	ok=${ok:-0}
	log "post $label: 200×$ok / $count, ${dur}s, $(awk -v n="$count" -v d="$dur" 'BEGIN{printf "%.1f", (d>0? n/d : n)}') req/s"
	echo "$label,$count,$ok,$dur" >>"$S2_OUT/post-summary.csv"
	if [ "$ok" != "$count" ]; then
		log "警告：$label 有 $((count - ok)) 条非 200，明细见 $codes"
		sort "$codes" | uniq -c | sed 's/^/    /' >&2
	fi
}

# s2_deltas：从 points.csv 算（a）相邻点之间的**边际** bytes/event（这是最干净的
# 线性度凭证：整体斜率会被基线里那几十个 free page 和 seed 表的四舍五入弄脏）、
# （b）两档 payload 的总斜率，并写进 bpe.env 供外推用。
s2_deltas() {
	awk -F, -v pl1k="$S2_PAYLOAD_1K" '
	NR==1{next}
	{
		n++; lab[n]=$1; tier[n]=$2; posted[n]=$3; rows[n]=$4+$5;
		bytes[n]=$9; pages[n]=$10; free[n]=$12; avgpl[n]=$14;
	}
	END{
		print "# 相邻测量点的边际成本（bytes/event）与线性度"
		printf "%-12s %-8s %8s %8s %12s %12s %12s\n", "label","tier","posted","ev_rows","total_bytes","d_bytes","B/event"
		for(i=1;i<=n;i++){
			if(i==1){ printf "%-12s %-8s %8d %8d %12d %12s %12s\n", lab[i],tier[i],posted[i],rows[i],bytes[i],"-","-"; continue }
			dp=posted[i]-posted[i-1]; db=bytes[i]-bytes[i-1];
			printf "%-12s %-8s %8d %8d %12d %12d %12.1f\n", lab[i],tier[i],posted[i],rows[i],bytes[i],db,(dp>0?db/dp:0)
		}
		print ""
		print "# 两档总斜率"
		# 基线 = 第一个点；空档末尾 = 最后一个 tier 为 perf.e0 的点；1KB 末尾 = 最后一个点
		base=1; last0=0; lastall=n
		for(i=1;i<=n;i++){
			if(tier[i]=="base") base=i
			if(tier[i]=="perf.e0") last0=i
		}
		if(last0>0 && last0>base){
			nn=posted[last0]-posted[base]; dd=bytes[last0]-bytes[base]
			bpe0=dd/nn
			printf "空 payload 档: (%d - %d) B / (%d - %d) 条 = %.1f B/event  => %.2f MiB/万条\n", bytes[last0],bytes[base],posted[last0],posted[base],bpe0,bpe0*10000/1048576
		} else bpe0=0
		if(last0>0 && lastall>last0){
			nn=posted[lastall]-posted[last0]; dd=bytes[lastall]-bytes[last0]
			bpe1=dd/nn
			printf "1KB payload 档: (%d - %d) B / (%d - %d) 条 = %.1f B/event  => %.2f MiB/万条\n", bytes[lastall],bytes[last0],posted[lastall],posted[last0],bpe1,bpe1*10000/1048576
			printf "两档差: %.1f B/event（payload 净增 %d B 的写入成本，含索引/页开销的放大系数 %.2f）\n", bpe1-bpe0,pl1k,(pl1k>0? (bpe1-bpe0)/pl1k : 0)
		} else bpe1=bpe0
		print ""
		print "# 行级（含派发剪枝行）"
		if(last0>base){
			nn=rows[last0]-rows[base]; dd=bytes[last0]-bytes[base]
			printf "每行 %.1f B（%d 行 / %d B）；注意一条事件在本版写 2 行（事件 + 剪枝留痕）\n", (nn>0?dd/nn:0), nn, dd
		}
		# 同一档内最后一个区间的斜率 = 稳态边际成本。为什么不只用上面的“总斜率”：
		# 基线里含 seed 建表的 417KB 与 20 个 free page，会把斜率往下稀释 ~3%；
		# 而运维关心的「再写 1 万条要多少盘」只能用边际值。
		bpe0m=bpe0
		for(i=2;i<=n;i++){
			if(tier[i]=="perf.e0" && tier[i-1]=="perf.e0" && posted[i]>posted[i-1]){
				bpe0m=(bytes[i]-bytes[i-1])/(posted[i]-posted[i-1])
				printf "稳态边际（%s→%s）: %.1f B/event\n", lab[i-1], lab[i], bpe0m
			}
		}
		printf "BPE_EMPTY=%.1f\nBPE_1K=%.1f\nBPE_EMPTY_MARG=%.1f\nBPE_1K_MARG=%.1f\n", bpe0, bpe1, bpe0m, bpe1
	}
	' "$S2_OUT/points.csv" >"$S2_OUT/deltas.txt"
	grep '^BPE_' "$S2_OUT/deltas.txt" >"$S2_OUT/bpe.env"
	sed -i '/^BPE_/d' "$S2_OUT/deltas.txt"
	cat "$S2_OUT/deltas.txt"
}

# s2_extrapolate：外推表。**脚本自己算**，不许手填 —— 手填的数字没人能复核。
s2_extrapolate() {
	local bpe_empty="$1" bpe_1k="$2" suffix="${3:-}"
	local head="load           ev/s     bytes/ev   1h(MiB)     1d(MiB)     1w(MiB)    1mo(MiB)  10GiB能撑   100GiB能撑    1TiB能撑"
	{
		echo "# 外推（1 MiB = 1048576 B，1 GiB = 1073741824 B，1 月 = 30 天）"
		echo "# bytes/ev 来源：空 payload 档 / 1KB payload 档 的实测增量（见 points.csv）"
		echo
		for pair in "empty_payload:$bpe_empty" "1KB_payload:$bpe_1k"; do
			local name="${pair%%:*}" bpe="${pair##*:}"
			echo "## payload=$name  (bytes/event=$bpe)"
			printf '%s\n' "$head"
			for rps in 1 38 100; do
				awk -v r="$rps" -v b="$bpe" 'BEGIN{
					split("3600 86400 604800 2592000", sec, " ");
					lab[1]="1h"; lab[2]="1d"; lab[3]="1w"; lab[4]="1mo";
					mib=1048576; gib=1073741824;
					rate=r*b;                      # bytes/s
					printf "%-12s %6d %11d", "load", r, b;
					for(i=1;i<=4;i++){
						v=rate*sec[i];
						printf " %11.1f", v/mib;    # 列头写 MiB 口径
					}
					split("10737418240 107374182400 1099511627776", cap, " ");
					for(i=1;i<=3;i++){
						if (rate <= 0) { printf " %11s", "-"; continue }
						s=cap[i]/rate;             # 秒
						if (s >= 86400*365) printf " %9.1f年", s/(86400*365);
						else if (s >= 86400) printf " %9.1f天", s/86400;
						else printf " %9.1f时", s/3600;
					}
					printf "\n";
				}'
			done
			echo
		done
	} >"$S2_OUT/extrapolation${suffix}.txt"
	cat "$S2_OUT/extrapolation${suffix}.txt"
}

# s2_code_facts：把"有没有上限机制"的 grep 原文留档（文件:行可直接引用）。
s2_code_facts() {
	{
		echo '# grep -rn "retention\|prune\|VACUUM\|DELETE FROM events" internal/ --include=*.go'
		grep -rn 'retention\|prune\|VACUUM\|DELETE FROM events' "$PC_REPO/internal/" --include=*.go || true
		echo
		echo '# 结论要看的三处'
		grep -n 'EventRetentionDisabled = 0\|DefaultEventRetentionSec = EventRetentionDisabled\|if opts.EventRetentionSec <= 0' \
			"$PC_REPO/internal/store/retention.go" || true
		grep -n 'SweepRetention(ctx, store.RetentionOptions{' "$PC_REPO/internal/lease/retention.go" || true
		grep -rn 'idx_events_retention' "$PC_REPO/internal/store/" || true
		grep -rn 'DELETE FROM events' "$PC_REPO/internal/store/" || true
	} >"$S2_OUT/code-facts.txt" 2>&1
	log "code-facts → $S2_OUT/code-facts.txt（$(wc -l <"$S2_OUT/code-facts.txt") 行）"
}

main() {
	pc_require_env || return 1
	pc_build || return 1
	mkdir -p "$S2_OUT"
	rm -f "$S2_OUT/points.csv" "$S2_OUT/post-summary.csv" "$S2_OUT/wal-watch.csv" \
		"$S2_OUT"/*.codes "$S2_OUT"/measure-*.txt "$S2_OUT"/bpe.env
	echo "label,t,wal_bytes" >"$S2_OUT/wal-watch.csv"

	log "起服务（:$S2_PORT）"
	if ! pc_start retention "$S2_PORT"; then
		log ":$S2_PORT 起不来（多半被别人占了），顺延到 :$S2_ALT_PORT"
		pc_start retention "$S2_ALT_PORT" || return 1
	fi
	trap 'pc_stop' EXIT

	pc_seed || return 1
	local cap
	cap=$(pc_cap perfgen)
	[ -n "$cap" ] || {
		log "建能力失败"
		return 1
	}
	log "base=$PC_BASE ns=$PC_NS cap=$cap"

	# 冒烟：先发 1 条看响应，确认"一条事件 = 几行"（本版是 2 行：事件 + 剪枝留痕）。
	curl -s -X POST "$PC_BASE/v1/events" \
		-H "Authorization: Bearer $PC_SEND_KEY" -H 'content-type: application/json' \
		-d "{\"namespaceId\":\"$PC_NS\",\"source\":\"perfgen\",\"type\":\"perf.smoke\",\"payload\":{}}" \
		>"$S2_OUT/smoke-ingest.json"
	log "冒烟响应 → $S2_OUT/smoke-ingest.json"

	# 三个数据点：500 / 1000 / 2000 条，边灌边测（线性度靠这个）。
	# 标签用 S2_SEQ（实际已发条数）而不是常量，这样 S2_EVENTS 调小时标签仍然自洽。
	local half=$((S2_EVENTS / 2))
	s2_measure "m$S2_SEQ" # 基线（只有 seed + 能力，0 条事件）
	s2_post_batch "$half" 2 "perf.e0" p0-a
	s2_measure "m$S2_SEQ"
	s2_post_batch $((S2_EVENTS - half)) 2 "perf.e0" p0-b
	s2_measure "m$S2_SEQ"
	# 第三点=再翻一倍，看线性度是不是还成立（稀疏到密）。
	s2_post_batch "$S2_EVENTS" 2 "perf.e0" p0-c
	s2_measure "m$S2_SEQ"

	# 1KB payload 档：只换 payload 大小，两档之差就是 payload 的边际成本。
	s2_post_batch "$S2_EVENTS_1K" "$S2_PAYLOAD_1K" "perf.e1k" p1k
	s2_measure "m${S2_SEQ}_1k"

	s2_code_facts

	# bytes/event 与线性度都从 points.csv 算（total=db+wal，都已 checkpoint，
	# wal 只剩零字节，不会污染差值）；手填数字没人能复核。
	s2_deltas
	# shellcheck disable=SC1091
	. "$S2_OUT/bpe.env"
	# 两张表：extrapolation.txt 用「基线→末点」总斜率（含一次性基线偏移，偏乐观）；
	# extrapolation-marginal.txt 用稳态边际斜率（运维算「再写 N 条要多少盘」用这个）。
	s2_extrapolate "$BPE_EMPTY" "$BPE_1K" ""
	s2_extrapolate "$BPE_EMPTY_MARG" "$BPE_1K_MARG" "-marginal"

	log "全部测量点："
	column -s, -t "$S2_OUT/points.csv"
	log "post 汇总："
	column -s, -t "$S2_OUT/post-summary.csv"
	log "完成，产物在 $S2_OUT"
}

main "$@"
