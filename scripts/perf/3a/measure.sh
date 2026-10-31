#!/usr/bin/env bash
# 批次 3A 压测执行器：#237（tasks 列表索引）+ #242（store 层语句缓存）。
#
# 三个配置、一份数据、一台机器、同一批核，交替测量 A/B/C/A/B/C 两轮：
#
#   A = 基线        anotify-base   ← git archive 3433eef（#237 索引提交的父提交）
#   B = 只有索引    anotify-index  ← git archive dfcf97b（迁移 16）
#   C = 索引+缓存   anotify-after  ← 当前 worktree 的工作树状态（含未提交改动）
#
# 相位纪律（为什么分两半跑）：
#   相位 1 = 构建 A/B + 播种 + EXPLAIN + 只读机制冒烟。这些都是**结构性事实**，
#            不受同期 A/B 两路 worker 跑 go test 的 CPU 争用影响。
#   相位 2 = 所有计时数字（loadgen / pprof / 写路径 / store 基准）。
#            必须等 `/tmp/perf-3a/GO-AFTER` 信号，因为 A/B 两路 worker 一提交
#            （它们会跑 go test -race、make ci，吃满 CPU）数字就废了。
#
# 用法：
#   scripts/perf/3a/measure.sh phase1     # 现在就能跑（构建 A/B + 播种 + EXPLAIN）
#   scripts/perf/3a/measure.sh phase2     # 等 GO-AFTER 后跑（C + 全部计时）
#   scripts/perf/3a/measure.sh all        # 上面两步
#   scripts/perf/3a/measure.sh bins       # 只构建/校验二进制 + 打印溯源表
#   scripts/perf/3a/measure.sh seed       # 只播种 + 派生副本
#   scripts/perf/3a/measure.sh bench      # 只跑 store 层写路径基准（需 phase1 的 after 源码）
#   scripts/perf/3a/measure.sh stopall    # 收掉本脚本记录过的所有 server 进程
#
# 产物：/tmp/perf-3a/raw/**（server.log / loadgen 输出 / pprof / EXPLAIN / summary.tsv）
# 结论写进 scripts/perf/3a/REPORT.md；本脚本只负责产出可复现的数字与原文。

set -uo pipefail
cd "$(dirname "$0")" || exit 1
# shellcheck source=./lib.sh
. ./lib.sh

GO_SIGNAL=$P3_ROOT/GO-AFTER
SUMMARY=$P3_RAW/summary.tsv
SUMMARY242=$P3_RAW/summary-242.tsv

# 每份库的「数据规模」标签 → (tasks 行数, events 行数)。
# events 固定 10 万行：它是另一个 issue 的表，但同一个库、同一份 page cache，
# 这样量出来的 tasks 列表成本更接近真实部署（库里不是只有一张表）。
SCALE_20K="20000 100000"
SCALE_100K="100000 100000"

# 端口按配置固定分配：A/B/C 各自的 server.log 里看到同一个端口，便于人肉核对。
PORT_A=8020
PORT_B=8021
PORT_C=8022

# ---------- 二进制身份 ----------

# ---------- 二进制备份 / 产物回仓 ----------
#
# /tmp/perf-3a 在 2026-11-01 01:54 被外部进程整目录删除过一次（连带二进制、DB、raw），
# 所以加两条防再发：二进制往 /var/tmp 备份并在缺失时优先恢复；测量产物落一份到仓库。
P3_BACKUP=${P3_BACKUP:-/var/tmp/anotify3a-backup}

# p3_bin_backup：把当前 bin 目录整体备份（含 sha 清单）。
p3_bin_backup() {
	mkdir -p "$P3_BACKUP" || return 0
	cp -a "$P3_BIN" "$P3_BACKUP/" 2>/dev/null || true
	( cd "$P3_BIN" && sha256sum anotify-* devseed loadgen 2>/dev/null ) >"$P3_BACKUP/bin.sha256" 2>/dev/null || true
	p3_log "二进制已备份到 $P3_BACKUP/bin（sha 清单 bin.sha256）"
}

# p3_bin_restore <name>：从备份恢复一个二进制（存在且自洽才用），返回 0 = 已恢复。
p3_bin_restore() {
	local name=$1
	local src="$P3_BACKUP/bin/anotify-$name"
	local dst="$P3_BIN/anotify-$name"
	[ -x "$src" ] || return 1
	[ -x "$dst" ] && cmp -s "$src" "$dst" && return 0
	cp -a "$src" "$dst" 2>/dev/null || return 1
	p3_log "从备份恢复 anotify-$name（$(sha256sum "$dst" | cut -c1-16)…）"
	return 0
}

# export_artifacts：把「小而关键」的产物复制到 scripts/perf/3a/raw/。
#
# 为什么产物要回仓库：/tmp 是被外部进程清过一次的（见上），而 perf 报告必须能追溯原文。
# 只拷文本与几 KB 级别的文件（.pprof protobuf 不拷，靠 -top/-list 的文本摘要留证）。
export_artifacts() {
	local dst="$P3_REPO/scripts/perf/3a/raw"
	mkdir -p "$dst/explain" "$dst/pprof" "$dst/load" "$dst/bench" || return 0
	cp -a "$P3_RAW"/summary*.tsv "$P3_RAW"/phase1-summary.tsv "$P3_RAW"/bin-provenance.tsv 2>/dev/null
	cp -a "$P3_RAW"/summary.tsv "$P3_RAW"/summary-242.tsv "$P3_RAW"/phase1-summary.tsv \
		"$P3_RAW"/bin-provenance.tsv "$dst/" 2>/dev/null
	[ -f "$P3_RAW/after-provenance.txt" ] && cp -a "$P3_RAW/after-provenance.txt" "$dst/"
	cp -a "$P3_RAW"/explain/*.txt "$dst/explain/" 2>/dev/null
	cp -a "$P3_RAW"/bench/*.txt "$dst/bench/" 2>/dev/null
	local d
	for d in "$P3_RAW"/cpu-*; do
		[ -d "$d" ] || continue
		mkdir -p "$dst/pprof/$(basename "$d")"
		cp -a "$d"/pprof-top.txt "$d"/pprof-list-prepareV2.txt "$d"/pprof-prepareV2-row.txt \
			"$dst/pprof/$(basename "$d")/" 2>/dev/null
	done
	for d in "$P3_RAW"/*/load.log; do
		[ -f "$d" ] || continue
		cp -a "$d" "$dst/load/$(basename "$(dirname "$d")").log" 2>/dev/null
	done
	# 库模板的 sha256（换批次要重新记，不能混着比）
	{ echo "# db sha256（每次重新播种都是新批次，跨批次不要混表比较）"; local dbl
		for dbl in "$P3_DBROOT"/*/perf.db; do
			[ -f "$dbl" ] || continue
			printf '%s\t%s\t%s\t%s\n' "$(basename "$(dirname "$dbl")")" \
				"$(sha256sum "$dbl" | cut -d' ' -f1)" "$(stat -c %s "$dbl")" \
				"$(sqlite3 "$dbl" 'select max(version) from schema_version;' 2>/dev/null)"
		done; } >"$dst/provenance-db.txt" 2>/dev/null
	p3_log "产物已回仓：$dst（$(find "$dst" -type f | wc -l) 个文件）"
}

bins() {
	p3_require_env
	# 先保留 centurion 已经验证过的基线二进制，再自己重建一份做「可重建性」核对。
	if [ -x "$P3_BIN/anotify-base" ] && [ ! -f "$P3_BIN/anotify-base.centurion.sha256" ]; then
		cp -a "$P3_BIN/anotify-base" "$P3_BIN/anotify-base.centurion" 2>/dev/null
		sha256sum "$P3_BIN/anotify-base.centurion" | tee "$P3_BIN/anotify-base.centurion.sha256" >&2
	fi
	# 优先从 /var/tmp 备份恢复（重建会产生新的构建产物，provenance 得重记；能恢复就恢复）
	p3_bin_restore base || p3_build_archive "$P3_COMMIT_BASE" base
	p3_bin_restore index || p3_build_archive "$P3_COMMIT_INDEX" index
	p3_build_tools
	# 可重建性核对：重编的基线二进制应与 centurion 先前构建的那份 sha256 相同
	# （源码 + Go 版本 + 构建参数一致时 Go 构建是可重现的）。不同也不潜崩溃，
	# 但必须在报告里声明，因为它意味着「同一份源码得到不同二进制」这件事需要解释。
	if [ -f "$P3_BIN/anotify-base.centurion" ]; then
		local a b
		a=$(sha256sum "$P3_BIN/anotify-base.centurion" | cut -d' ' -f1)
		b=$(sha256sum "$P3_BIN/anotify-base" | cut -d' ' -f1)
		if [ "$a" = "$b" ]; then
			p3_log "可重建性核对：重编的 anotify-base 与 centurion 那份逐字节相同（$b）"
		else
			p3_log "注意：重编的 anotify-base 与 centurion 那份不同（$a → $b），见 bin-provenance.tsv"
		fi
	fi
	if [ -f "$GO_SIGNAL" ] || [ -f "$P3_ROOT/GO-FINAL" ]; then
		p3_bin_restore after || p3_build_after
	else
		p3_log "GO-AFTER 未出现 → 暂不构建 C（anotify-after）"
	fi
	p3_say "二进制溯源（报告里每张表都要能对上这里的 sha）"
	{
		p3_summary_header_bins
		p3_bin_provenance base "$P3_COMMIT_BASE"
		p3_bin_provenance index "$P3_COMMIT_INDEX"
		[ -f "$GO_SIGNAL" ] && p3_bin_provenance after "worktree-HEAD:$(git -C "$P3_REPO" rev-parse HEAD)"
	} | tee "$P3_RAW/bin-provenance.tsv" >&2
	p3_bin_backup
}

p3_summary_header_bins() {
	printf 'config\tcommit\tbinary\tsha256_16\tbytes\tbuild_tags\ttrimpath\tdriver\n'
}

# p3_bin_provenance <name> <source-id>：一行 = 一个二进制「是从哪份源码编出来的」。
p3_bin_provenance() {
	local name=$1 srcid=$2
	local bin="$P3_BIN/anotify-$name"
	[ -x "$bin" ] || { printf '%s\t%s\tMISSING\t-\t-\t-\t-\n' "$name" "$srcid"; return 1; }
	local tags trim drv
	tags=$(go version -m "$bin" 2>/dev/null | sed -n 's/^\s*build\t-tags=//p' | tr '\n' ',')
	trim=$(go version -m "$bin" 2>/dev/null | sed -n 's/^\s*build\t-trimpath=//p')
	# 链接进去的驱动版本：C 配置（索引+缓存+驱动升级）与 A/B（v1.34.5）的差异之一，
	# 从**二进制本身**取（仓库 go.mod 可被另两路 worker 改，二进制不会）。
	drv=$(go version -m "$bin" 2>/dev/null | sed -n 's/^\s*dep\tmodernc.org\/sqlite\t\([^\t]*\).*/\1/p')
	printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' "$name" "$srcid" "${bin#$P3_ROOT/}" \
		"$(sha256sum "$bin" | cut -c1-16)" "$(stat -c %s "$bin")" "${tags:-none}" "${trim:-false}" "${drv:-?}"
}

# ---------- 播种 + 派生副本 ----------

# seed_only <scale> <tasks> <events>：只播种，不派生副本（给 hook 源的写入留出窗口）。
seed_only() {
	local scale=$1 ntasks=$2 nevents=$3
	if [ ! -f "$P3_DBROOT/seed-$scale/perf.db" ]; then
		p3_seed "seed-$scale" "$ntasks" "$nevents"
	else
		p3_log "复用已有 seed 库 seed-$scale（不重新播种：重播会换掉页面布局，破坏后续可比性）"
	fi
}

# derive <scale> <tasks> <events>：seed → 派生 A/B/C 三份副本。
#
# 顺序很重要：所有副本都是从**同一个** seed 库 cp 出来的，所以「行数据逐字节相同」
# 是 cp 保证的，不是我另外校验出来的（报告里贴 sha256 作证）。
#
# 注意 seed 库里先要有 webhook 源（写路径场景要打 POST /v1/hooks/<token>）：
# 源行只在 seed 上建一次，三份副本共享同一个 token —— 否则「A/B/C 打不同 token」
# 会让 b-tree 里的键分布不同，把被测变量之外的东西混进写路径数字。
# 所以调用顺序是：p3_seed → ensure_hook_source → derive。
derive() {
	local scale=$1 ntasks=$2 nevents=$3
	local seed="seed-$scale"
	[ -f "$P3_DBROOT/$seed/perf.db" ] || p3_die "derive: 缺 seed 库 $seed（先 seed_only）"
	# A = 基线：直接拷 seed（v15，无 idx_tasks_ns_created）
	p3_copy_db "$seed" "$scale-A"
	# A 也要 VACUUM/ANALYZE：p3_copy_db 的 checkpoint 会把 WAL 写回主文件，页里会留下
	# 空闲/碎片；如果只给 B 做 VACUUM，那两者的文件大小差就不是「索引占的空间」
	# 而是「碎片 + 索引」，尺寸表直接失真（本轮真踩过）。
	sqlite3 "$P3_DBROOT/$scale-A/perf.db" "PRAGMA wal_checkpoint(TRUNCATE); VACUUM; ANALYZE;"
	# 硬护栏：A 必须停在 v15 且**没有**新索引。本轮真踩过：一个误用 index 二进制打开
	# seed 库的小实验把 seed 升到了 v16，于是「基线」拖着一个新索引去跑，对比直接作废
	# （而且两个配置的 RPS 都很漂亮，从日志里根本看不出来）。
	[ "$(p3_db_version "$scale-A")" = "15" ] || p3_die "$scale-A 不是 v15（seed 库被别的二进制升过）"
	[ "$(p3_db_index_state "$scale-A")" = "absent" ] || p3_die "$scale-A 竟然有 idx_tasks_ns_created（基线不纯）"
	# B = 只有索引：拷 A，然后让「只加了迁移 16」的二进制 Open 一次触发迁移
	p3_copy_db "$scale-A" "$scale-B"
	p3_say "用 anotify-index 打开 $scale-B，触发迁移 16"
	p3_start_server "migrate-$scale-B" index "$scale-B" "$PORT_B" || p3_die "迁移失败"
	p3_stop_server "migrate-$scale-B"
	local ver idx
	ver=$(p3_db_version "$scale-B")
	idx=$(p3_db_index_state "$scale-B")
	p3_log "$scale-B：schema v$ver index=$idx"
	[ "$ver" = "16" ] || p3_die "$scale-B 没有升到 v16（实际 $ver）"
	case "$idx" in present*) ;; *) p3_die "$scale-B 缺 idx_tasks_ns_created（$idx）" ;; esac
	# 迁移后 VACUUM/ANALYZE：让文件大小是「索引撑起来的真实体积」而不是空闲页残留，
	# 同时给 B 一份和 A 同口径的 statistics（两侧都有 sqlite_stat1 → 计划可比）。
	sqlite3 "$P3_DBROOT/$scale-B/perf.db" "PRAGMA wal_checkpoint(TRUNCATE); VACUUM; ANALYZE;"
	# C = 索引 + 语句缓存：与 B 结构完全相同（同一个二进制之外，库必须一模一样）
	p3_copy_db "$scale-B" "$scale-C"
}

# 读路径冒烟：一条 GET /v1/tasks 确认端点可用、返回条数对得上。
# 这不是计时，是**机制校验**：数字之前先证明「测的是同一条读路径」。
smoke_read() {
	local tag=$1 label=$2 binname=$3
	local st body n
	st=$(curl -s -o "$P3_RAW/$tag/tasks.json" -w '%{http_code}' \
		-H "Cookie: anotify_session=$P3_SESSION" \
		"$P3_BASE/v1/tasks?namespaceId=$P3_NS&limit=200")
	n=$(jq -r '(.tasks // []) | length' "$P3_RAW/$tag/tasks.json" 2>/dev/null)
	printf 'read-smoke\t%s\t%s\thttp=%s\trows=%s\tdb_rows=%s\n' \
		"$label" "$(p3_bin_sha "$binname")" "$st" "${n:-NA}" \
		"$(sqlite3 "$P3_DBROOT/$label/perf.db" "select count(*) from tasks;")" \
		| tee -a "$P3_RAW/phase1-summary.tsv"
	[ "$st" = "200" ] || p3_log "读冒烟失败 label=$label http=$st"
}

# ---------- 相位 1 ----------

phase1() {
	p3_require_env
	bins
	p3_say "播种 + 派生副本（20k / 100k 两档）"
	# 先播种 20k 并在 seed 上建 webhook 源，再派生副本 —— 顺序不能反（见 derive 的注释）
	seed_only 20k $SCALE_20K
	ensure_hook_source 20k
	derive 20k $SCALE_20K
	# 100k 只量读路径（写路径在 20k 上量，规模对写放大的结论没有增量信息）
	seed_only 100k $SCALE_100K
	derive 100k $SCALE_100K

	p3_say "EXPLAIN QUERY PLAN + 库体积（A vs B）"
	: >"$P3_RAW/phase1-summary.tsv"
	cat "$P3_RAW/bin-provenance.tsv" >>"$P3_RAW/phase1-summary.tsv"
	local s
	for s in 20k 100k; do
		p3_explain "$s-A" "$P3_RAW/explain/$s-A.txt"
		p3_explain "$s-B" "$P3_RAW/explain/$s-B.txt"
		printf 'db-size\t%s-A\t%s\tbytes\tschema=%s\tindex=%s\n' \
			"$s" "$(p3_db_size "$s-A")" "$(p3_db_version "$s-A")" "$(p3_db_index_state "$s-A")" \
			| tee -a "$P3_RAW/phase1-summary.tsv"
		printf 'db-size\t%s-B\t%s\tbytes\tschema=%s\tindex=%s\n' \
			"$s" "$(p3_db_size "$s-B")" "$(p3_db_version "$s-B")" "$(p3_db_index_state "$s-B")" \
			| tee -a "$P3_RAW/phase1-summary.tsv"
	done
	# C 的库体积（C 与 B 结构一致，数字应逐字节相同——这是「隔离变量」的自证）
	printf 'db-size\t20k-C\t%s\tbytes\tschema=%s\tindex=%s\n' \
		"$(p3_db_size 20k-C)" "$(p3_db_version 20k-C)" "$(p3_db_index_state 20k-C)" \
		| tee -a "$P3_RAW/phase1-summary.tsv"
	printf 'db-sha256\t20k-A\t%s\n' "$(sha256sum "$P3_DBROOT/20k-A/perf.db" | cut -c1-16)" | tee -a "$P3_RAW/phase1-summary.tsv"
	printf 'db-sha256\t20k-B\t%s\n' "$(sha256sum "$P3_DBROOT/20k-B/perf.db" | cut -c1-16)" | tee -a "$P3_RAW/phase1-summary.tsv"
	printf 'db-sha256\t20k-C\t%s\n' "$(sha256sum "$P3_DBROOT/20k-C/perf.db" | cut -c1-16)" | tee -a "$P3_RAW/phase1-summary.tsv"

	# 读路径机制冒烟：三个配置各一条请求，确认 200 且 rows=200（limit 生效）
	p3_say "只读机制冒烟（每配置一条 GET /v1/tasks?limit=200）"
	local cfg
	for cfg in A B; do
		load_stamp "20k-$cfg"
		p3_start_server "smoke-20k-$cfg" "$(bin_for "$cfg")" "20k-$cfg" "$(port_for "$cfg")" || p3_die "冒烟启动失败 $cfg"
		smoke_read "smoke-20k-$cfg" "20k-$cfg" "$(bin_for "$cfg")"
		p3_stop_server "smoke-20k-$cfg"
	done
	export_artifacts
	p3_log "相位 1 完成。原始产物在 $P3_RAW/"
}

# ---------- 等待信号 ----------

# 等 A/B/D 三路 worker 收工。最多 90 分钟（540×10s）。
#
# 为什么不限 45 分钟：worker D 在跑 `make ci` + `make e2e`（整仓构建 + 浏览器套件，
# 吃满 CPU）。计时窗口只有一次，被 e2e 污染就得整体重跑，窄了不如等久一点。
# 信号文件由 centurion 在另两路交付后放；本脚本不自己去猜「他们大概跑完了」。
wait_go() {
	if [ -f "$GO_SIGNAL" ]; then
		p3_log "GO-AFTER 已存在（$(tr '\n' ' ' <"$GO_SIGNAL" 2>/dev/null)）"
		return 0
	fi
	p3_say "等 $GO_SIGNAL（最多 90 分钟）"
	local i
	for i in $(seq 1 540); do
		[ -f "$GO_SIGNAL" ] && {
			p3_log "等到了（第 $((i * 10))s）"
			return 0
		}
		[ $((i % 30)) -eq 0 ] && p3_log "仍在等 GO-AFTER（$((i * 10))s / 5400s）"
		sleep 10
		done
	p3_log "等满 90 分钟仍无 GO-AFTER：只提交相位 1 的结构性结论"
	return 1
}

# e2e_guard：就算拿到了 GO-AFTER，也可能还有 e2e 在跑（信号早于进程退出）。
# 看到 run_all.sh / parallel_runner / 前端套件就再等 60s，最多 5 次；还沾就硬拒——
# 在 e2e 的负载上量 p95，等于把别人的 CPU 算进本批的「改进」里。
e2e_guard() {
	local i n
	# 锚定 cmdline 开头：只认「真正在跑 e2e」的进程。
	# 用宽 pattern（.*run_all\.sh.*）会把「命令行里恰好提到这个路径」的 shell（包括本脚本的
	# 调用者、编辑器、甚至一条 heredoc）都算进去 —— 本轮真踩过，白等了 5 分钟。
	local pat='^(bash|/bin/bash) .*scripts/e2e/run_all\.sh|^node .*scripts/e2e/(parallel_runner\.mjs|suites/)'
	for i in $(seq 1 5); do
		n=$(pgrep -fc "$pat" 2>/dev/null || true)
		[ -z "${n:-}" ] && n=0
		if [ "$n" -eq 0 ]; then
			p3_log "e2e 已清空（第 $i 次检查）"
			return 0
		fi
		p3_log "还有 $n 个 e2e 进程在跑（$(pgrep -af "$pat" 2>/dev/null | head -2 | tr '\n' ' '))，等 60s"
		sleep 60
	done
	p3_die "e2e 一直没退，计时会被污染（拒绝出数）"
}

# ---------- 相位 2：计时 ----------

# bin_for / port_for：配置 → 二进制名 / 端口
bin_for() {
	case "$1" in
	A) echo base ;;
	B) echo index ;;
	C) echo after ;;
	*) p3_die "未知配置 $1" ;;
	esac
}
port_for() {
	case "$1" in
	A) echo "$PORT_A" ;;
	B) echo "$PORT_B" ;;
	C) echo "$PORT_C" ;;
	*) p3_die "未知配置 $1" ;;
	esac
}

# load_stamp <label>：从库的 stamp.env 里取 SESSION/NS/CAP（三份副本的会话是同一行数据）。
load_stamp() {
	local f="$P3_DBROOT/$1/stamp.env"
	[ -f "$f" ] || p3_die "缺 $f"
	P3_SESSION=$(sed -n 's/^SESSION=//p' "$f")
	P3_NS=$(sed -n 's/^NS=//p' "$f")
	P3_CAP=$(sed -n 's/^CAP=//p' "$f")
	[ -n "$P3_SESSION" ] && [ -n "$P3_NS" ] || p3_die "$f 里没有 SESSION/NS"
}

# read_round <cfg> <scale> <round>：#237 的读路径一轮（c=32，20s）。
read_round() {
	local cfg=$1 scale=$2 round=$3
	local tag="tasks-$scale-$cfg-r$round"
	local label="$scale-$cfg"
	load_stamp "$label"
	p3_start_server "$tag" "$(bin_for "$cfg")" "$label" "$(port_for "$cfg")" || p3_die "启动失败 $tag"
	# 冷启动别直接开压：先打一条请求，让连接池/语句缓存进稳态（改后代码首次 prepare 也在这里发生）。
	curl -s -o /dev/null -H "Cookie: anotify_session=$P3_SESSION" "$P3_BASE/v1/tasks?namespaceId=$P3_NS&limit=200"
	p3_load "$P3_RAW/$tag/load.log" \
		-url "$P3_BASE/v1/tasks?namespaceId=$P3_NS&limit=200" \
		-header "Cookie: anotify_session=$P3_SESSION" -c 32 -d 20s >/dev/null
	p3_stop_server "$tag"
	p3_checkpoint "$label"   # 先 fold WAL：行数快照才准，-wal 也不会吃掉临时盘
	p3_summary_row "$SUMMARY" "$cfg" "$scale" "$(bin_for "$cfg")" "$label" "$round" \
		"$P3_RAW/$tag/load.log" "get /v1/tasks limit=200 c=32 20s"
}

# cpu_round <cfg> <scale> <round>：#242 的「同一负载 + 重叠 CPU profile」一轮。
#   loadgen c=32 跑 25s；pprof 在 +2s 时开抓 20s —— 采样窗口完整落在负载稳态里，
#   这样 CPU/请求 = profile seconds ÷ (RPS × 20) 才有意义（分母是同一段时间的请求数）。
cpu_round() {
	local cfg=$1 scale=$2 round=$3
	local tag="cpu-$scale-$cfg-r$round"
	local label="$scale-$cfg"
	load_stamp "$label"
	p3_start_server "$tag" "$(bin_for "$cfg")" "$label" "$(port_for "$cfg")" || p3_die "启动失败 $tag"
	curl -s -o /dev/null -H "Cookie: anotify_session=$P3_SESSION" "$P3_BASE/v1/tasks?namespaceId=$P3_NS&limit=200"
	p3_load "$P3_RAW/$tag/load.log" \
		-url "$P3_BASE/v1/tasks?namespaceId=$P3_NS&limit=200" \
		-header "Cookie: anotify_session=$P3_SESSION" -c 32 -d 25s >/dev/null &
	local loadpid=$!
	sleep 2
	p3_pprof_cpu "$P3_RAW/$tag/cpu.pprof" 20
	wait "$loadpid"
	p3_stop_server "$tag"
	p3_checkpoint "$label"   # 先 fold WAL：行数快照才准，-wal 也不会吃掉临时盘

	local parsed n rps p50 p95 p99 total cpur
	parsed=$(p3_parse_load "$P3_RAW/$tag/load.log") || parsed="NA	NA	NA	NA	NA	NA"
	IFS=$'\t' read -r n rps p50 p95 p99 _ <<<"$parsed"
	total=$(p3_pprof_total_secs "$P3_RAW/$tag/cpu.pprof") || total="NA"
	# CPU/请求（毫秒）：profile 里 20 秒窗口的总 CPU 秒 ÷ 同期请求数
	if [ "$total" != "NA" ] && [ "$rps" != "NA" ]; then
		cpur=$(awk -v t="$total" -v r="$rps" 'BEGIN{ if (r>0) printf "%.3f", t*1000/(r*20); else print "NA" }')
	else
		cpur="NA"
	fi
	printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' \
		"$cfg" "$scale" "$(bin_for "$cfg")" "$(p3_bin_sha "$(bin_for "$cfg")")" "$label" "$round" \
		"$n" "$rps" "$p50" "$p95" "$p99" "$total" "$cpur" >>"$SUMMARY242"
	printf 'cpu-round\t%s\t%s\trps=%s\tp50=%sms\tp95=%sms\ttotal_cpu=%ss\tcpu/req=%sms\n' \
		"$cfg" "$label" "$rps" "$p50" "$p95" "$total" "$cpur" | tee -a "$P3_RAW/phase2-summary.tsv"
	# pprof 原文摘要（-top 30 与 prepareV2 行级）
	p3_pprof_top "$P3_RAW/$tag/cpu.pprof" "$P3_RAW/$tag/pprof-top.txt" 30 >/dev/null
	p3_pprof_list "$P3_RAW/$tag/cpu.pprof" prepareV2 "$P3_RAW/$tag/pprof-list-prepareV2.txt" >/dev/null
	# prepareV2 很可能不在 top30 里（它的 flat 小、cum 才看得出来），所以
	# 另拉一份 nodecount=200 的 -top 专门 grep 这个符号。
	p3_pprof_find "$P3_RAW/$tag/cpu.pprof" 'prepareV2' >"$P3_RAW/$tag/pprof-prepareV2-row.txt"
	p3_log "prepareV2 -top 行：$(tr '\n' ' ' <"$P3_RAW/$tag/pprof-prepareV2-row.txt")"
}

# write_round <cfg> <scale> <round> <extra-env...>：webhook 写路径一轮（c=8）。
#
# 打的是 POST /v1/hooks/<token>：限流器 → GetSourceByWebhookToken（读）→ **插 events 行**
# → Fire 派发 → 建 tasks 行。这就是 #237 索引写放大的观测点（新索引在 tasks 上）。
write_round() {
	local cfg=$1 scale=$2 round=$3
	shift 3
	local tag="hook-$scale-$cfg-r$round" label="$scale-$cfg"
	load_stamp "$label"
	p3_start_server "$tag" "$(bin_for "$cfg")" "$label" "$(port_for "$cfg")" "$@" || p3_die "启动失败 $tag"
	# 写前/写后的行数：证明请求真的落到了 DB（而不是被限流器挡在门外）
	local ev0 tk0 ev1 tk1
	ev0=$(sqlite3 "$P3_DBROOT/$label/perf.db" "select count(*) from events;")
	tk0=$(sqlite3 "$P3_DBROOT/$label/perf.db" "select count(*) from tasks;")
	p3_load "$P3_RAW/$tag/load.log" \
		-url "$P3_BASE/v1/hooks/$P3_HOOK_TOKEN" -method POST \
		-body-file "$P3_RAW/hook-body.json" \
		-header "content-type: application/json" -c 8 -d 20s >/dev/null
	p3_stop_server "$tag"
	p3_checkpoint "$label"   # 先 fold WAL：行数快照才准，-wal 也不会吃掉临时盘
	ev1=$(sqlite3 "$P3_DBROOT/$label/perf.db" "select count(*) from events;")
	tk1=$(sqlite3 "$P3_DBROOT/$label/perf.db" "select count(*) from tasks;")
	# 硬护栏：一行都没写进去 → 这个数字不是「写路径成本」，是 404/429 的成本。
	# 本轮真踩过（拷贝库漏了 WAL 里的源行 → 全部 404），所以现在卡死在这里。
	[ "$((ev1 - ev0))" -ge 1 ] || p3_die "$tag：events 没涨（全部 404/429？），拒绝出数：见 $P3_RAW/$tag/load.log"
	p3_summary_row "$SUMMARY" "$cfg" "$scale" "$(bin_for "$cfg")" "$label" "$round" \
		"$P3_RAW/$tag/load.log" "hook c=8 20s events+$((ev1 - ev0)) tasks+$((tk1 - tk0)) env=${*:-default}"
}

# hook_default_round：默认 env 下的写路径（10s）。预期撞 600/min 的 token 维限流，
# 量到的只是限流器天花板 —— 它**证明**「默认配置下写路径根本没到 DB 成本」。
hook_default_round() {
	local cfg=$1 scale=$2
	local tag="hook-default-$scale-$cfg"
	local label="$scale-$cfg"
	load_stamp "$label"
	p3_start_server "$tag" "$(bin_for "$cfg")" "$label" "$(port_for "$cfg")" || p3_die "启动失败 $tag"
	local ev0 tk0 ev1 tk1
	ev0=$(sqlite3 "$P3_DBROOT/$label/perf.db" "select count(*) from events;")
	tk0=$(sqlite3 "$P3_DBROOT/$label/perf.db" "select count(*) from tasks;")
	p3_load "$P3_RAW/$tag/load.log" \
		-url "$P3_BASE/v1/hooks/$P3_HOOK_TOKEN" -method POST \
		-body-file "$P3_RAW/hook-body.json" \
		-header "content-type: application/json" -c 8 -d 10s >/dev/null
	p3_stop_server "$tag"
	p3_checkpoint "$label"   # 先 fold WAL：行数快照才准，-wal 也不会吃掉临时盘
	ev1=$(sqlite3 "$P3_DBROOT/$label/perf.db" "select count(*) from events;")
	tk1=$(sqlite3 "$P3_DBROOT/$label/perf.db" "select count(*) from tasks;")
	[ "$((ev1 - ev0))" -ge 1 ] || p3_die "$tag：events 没涨（默认 env 下也应该有几十行写入），拒绝出数：见 $P3_RAW/$tag/load.log"
	p3_summary_row "$SUMMARY" "$cfg" "$scale" "$(bin_for "$cfg")" "$label" "default" \
		"$P3_RAW/$tag/load.log" "hook 默认 env c=8 10s（预期 429 限流）events+$((ev1 - ev0)) tasks+$((tk1 - tk0))"
	# 日志瘦身：默认 env 下 429 洪流会写出几十 MB server.log（issue #245 里记的就是这个），
	# 全文留在 /tmp 里会把临时盘吃掉。只留头 30 行 + 行数与体积（证据足够）。
	local slog="$P3_RAW/$tag/server.log"
	if [ -f "$slog" ]; then
		{
			echo "# 默认 env 写路径：429 限流日志洪流，只保留头 30 行（原文见 run log 行数与体积）"
			echo "# 行数=$(wc -l <"$slog")  体积=$(stat -c %s "$slog") 字节"
			head -30 "$slog"
		} >"$slog.trim" && mv "$slog.trim" "$slog"
	fi
}

# ensure_hook_source <scale>：在**源库（seed）**里建一个 webhook 源，再把 token 记下来。
#
# 为什么在 seed 上建、而不是每个配置各建一个：token 是随机生成的，各建一个就变成
# 「A/B/C 打的是不同的 token」，而 token 在 DB 里是唯一键（不同的 b-tree 位置）。
# 在 seed 上建一次 → 三份副本带着同一个源行 → 差异只剩被测的索引/缓存。
ensure_hook_source() {
	local scale=$1
	local label="seed-$scale"
	local stamp="$P3_DBROOT/seed-$scale/stamp.env"
	[ -f "$stamp" ] || p3_die "缺 $stamp"
	[ -f "$P3_RAW/hook-body.json" ] || printf '%s' '{"type":"perf","payload":{"n":1}}' >"$P3_RAW/hook-body.json"
	local token
	token=$(sqlite3 "$P3_DBROOT/$label/perf.db" \
		"select json_extract(config,'\$.token') from sources where kind='webhook' limit 1;" 2>/dev/null)
	if [ -n "${token:-}" ] && [ "$token" != "null" ]; then
		printf '%s\n' "$token" >"$P3_RAW/hook-token.txt"
		p3_log "复用已有 webhook token（$label）"
		return 0
	fi
	load_stamp "$label"
	# 能力参数的「满足值」：从 params_schema 的 required 里逐个填 "perf"。
	#
	# 为什么不随便塞一个 payload：内置能力（如 notify.phone）要求 to/title，
	# 不带参数的出口会在 dispatch 阶段被 **params_schema 剪枝**——那样每个请求只写
	# 一行 events、不建 tasks，写路径就测不到 tasks 上的索引维护成本（#237 的写放大
	# 就发生在 tasks 上）。所以先按 schema 把参数填满，让派发真的落地。
	local tparams uid
	# 注意 `to` 要的是 **user id**（usr_xxx），不是用户名：dispatch 的
	# checkUserScoped 用 GetUserByID 直查（契约 §10.1.1），填用户名会被剪成 foreign_user。
	uid=$(sqlite3 "$P3_DBROOT/$label/perf.db" "select id from users limit 1;")
	tparams=$(sqlite3 "$P3_DBROOT/$label/perf.db" \
		"select params_schema from capability_versions where id='$P3_CAP';" |
		jq -c --arg uid "$uid" 'reduce ((.required // [])[]) as $k ({}; .[$k] = (if $k=="to" then $uid else "perf" end))' 2>/dev/null)
	[ -n "${tparams:-}" ] || tparams='{}'
	p3_say "在 $label 上建 webhook 源（targets → $P3_CAP，params=$tparams）"
	p3_start_server "mk-source-$scale" base "$label" "$PORT_A" || p3_die "启动失败（建源）"
	local resp tkbefore tkafter
	tkbefore=$(sqlite3 "$P3_DBROOT/$label/perf.db" "select count(*) from tasks;")
	resp=$(curl -s -X POST "$P3_BASE/v1/sources" \
		-H "Cookie: anotify_session=$P3_SESSION" -H 'content-type: application/json' \
		-d "{\"namespaceId\":\"$P3_NS\",\"name\":\"perf-hook\",\"kind\":\"webhook\",\"config\":{\"token\":\"perf-token-$scale\"},\"enabled\":true,\"targets\":[{\"capabilityId\":\"$P3_CAP\",\"params\":$tparams}]}")
	printf '%s\n' "$resp" >"$P3_RAW/mk-source-$scale.json"
	# 源行落库后直接把 token 从库里读回来：不依赖响应体的字段名（契约 §6 的
	# 报告字段是 camelCase，但**配置 blob 内部**是 store 自己的 WebhookConfig）。
	local i
	for i in $(seq 1 20); do
		token=$(sqlite3 "$P3_DBROOT/$label/perf.db" \
			"select json_extract(config,'\$.token') from sources where kind='webhook' limit 1;" 2>/dev/null)
		[ -n "${token:-}" ] && [ "$token" != "null" ] && break
		sleep 0.2
	done
	# 冒烟：投一条，确记 200（不是 404/503）**且真建了 task**。
	# 后者是关键：如果派发被剪枝，写路径就只剩 events 一行，量到的不是 #237 的写放大。
	local st
	st=$(curl -s -o "$P3_RAW/hook-smoke.json" -w '%{http_code}' -X POST "$P3_BASE/v1/hooks/$token" \
		-H 'content-type: application/json' -d '{"type":"perf","payload":{"n":1}}')
	tkafter=$(sqlite3 "$P3_DBROOT/$label/perf.db" "select count(*) from tasks;")
	p3_log "hook 冒烟 http=$st token=${token:0:12}… tasks +$((tkafter - tkbefore)) body=$(head -c 180 "$P3_RAW/hook-smoke.json")"
	p3_stop_server "mk-source-$scale"
	[ "$st" = "200" ] || p3_die "webhook 冒烟不是 200（http=$st）：见 $P3_RAW/hook-smoke.json"
	[ "$((tkafter - tkbefore))" -ge 1 ] || p3_die "webhook 派发没建 task（tasks 没涨）：写路径测不到 tasks 索引，见 $P3_RAW/hook-smoke.json"
	printf '%s\n' "$token" >"$P3_RAW/hook-token.txt"
	P3_HOOK_TOKEN=$token
}

# store_bench：改后代码上量 BenchmarkCreateTasks（带索引 vs 去掉索引）。
#
# 这是契约修订 ② 的「主判据」：同一份 Go 代码、同一台机器、同一批核，
# 唯一差异是 schema 里那一行 CREATE INDEX。做法是把改后的源码树拷到 /tmp，
# 再在副本里删掉那一行（**不改仓库里的任何文件**，也不改任何测试断言）。
store_bench() {
	local out="$P3_RAW/bench"
	prepare_bench_sources || p3_die "准备基准源码失败"
	mkdir -p "$out"
	p3_say "契约原始命令：带索引（$BSRC_WITH）"
	(
		cd "$BSRC_WITH" || exit 1
		env $P3_GO_ENV taskset -c "$P3_CORES" go test ./internal/store/ \
			-run '^$' -bench 'BenchmarkCreateTasks' -benchmem -count=5
	) 2>&1 | tee "$out/create-tasks-with-index.txt" >&2
	p3_say "契约原始命令：去掉 idx_tasks_ns_created（$BSRC_NODIX）"
	(
		cd "$BSRC_NODIX" || exit 1
		env $P3_GO_ENV taskset -c "$P3_CORES" go test ./internal/store/ \
			-run '^$' -bench 'BenchmarkCreateTasks' -benchmem -count=5
	) 2>&1 | tee "$out/create-tasks-no-index.txt" >&2
	# 顺手用 sqlite3 复核「索引对插入的代价」是什么量级（独立于 Go，纯 SQL 口径）
	sql3_insert_cost
}

# prepare_bench_sources：为写路径基准准备两份**只差一行 CREATE INDEX** 的源码树。
#
# 为什么要从 `git archive <实测 commit>` 取，而不是直接用 worktree：
#   ① worktree 可能还在被别的 worker 改（本轮实测：扫描时刻里面多了一个还没写完的
#      _test.go，`go test` 直接编译失败，基准跑空却不报错）；
#   ② 实测的 anotify-after 是由某个确定的源码状态编出来的，基准必须测同一份代码。
# 所以：取 after-provenance.txt 里记下的 commit → archive → **重编 server 并与实测
# 二进制比 sha256**。对不上就拒出数（那说明基准测的不是压测用的代码）。
prepare_bench_sources() {
	local out="$P3_RAW/bench" head
	head=$(sed -n 's/^git rev-parse HEAD=//p' "$P3_RAW/after-provenance.txt" 2>/dev/null)
	[ -n "${head:-}" ] || { p3_log "缺 $P3_RAW/after-provenance.txt（先跑 phase2 的 bins）"; return 1; }
	BSRC_WITH="$P3_SRC/bench-with"
	BSRC_NODIX="$P3_SRC/bench-without"
	[ -d "$BSRC_WITH/internal/store" ] && [ -d "$BSRC_NODIX/internal/store" ] && [ -f "$out/schema-diff.txt" ] && return 0
	p3_say "准备基准源码 = git archive $head（不用 worktree：它可能正被别人改）"
	rm -rf "$BSRC_WITH" "$BSRC_NODIX" || return 1
	mkdir -p "$BSRC_WITH" || return 1
	git -C "$P3_REPO" archive "$head" | tar -x -C "$BSRC_WITH" || return 1
	# 自证：这份 archive 编出的二进制必须与实测的 anotify-after 逐字节相同
	rm -rf "$BSRC_WITH/internal/server/dist"
	cp -a "$P3_ROOT/dist" "$BSRC_WITH/internal/server/dist" || return 1
	(
		cd "$BSRC_WITH" || exit 1
		env $P3_GO_ENV taskset -c "$P3_CORES" go build -trimpath -tags embeddist -o "$P3_BIN/verify-after" ./cmd/server
	) || return 1
	local got want
	got=$(sha256sum "$P3_BIN/verify-after" | cut -d' ' -f1)
	want=$(sha256sum "$P3_BIN/anotify-after" | cut -d' ' -f1)
	[ "$got" = "$want" ] || { p3_log "archive($head) 重编的二进制 ≠ 实测 anotify-after（$got ≠ $want）：拒出数"; return 1; }
	p3_log "自证通过：archive $head 重编 == 实测 anotify-after（${got:0:16}…）"
	rm -rf "$BSRC_WITH/internal/server/dist"   # 基准不需要 embed，删掉省临时盘
	mkdir -p "$out"
	cp -a "$BSRC_WITH" "$BSRC_NODIX" || return 1
	sed -i '/CREATE INDEX IF NOT EXISTS idx_tasks_ns_created/d' "$BSRC_NODIX/internal/store/schema.sql"
	diff -u "$BSRC_WITH/internal/store/schema.sql" "$BSRC_NODIX/internal/store/schema.sql" >"$out/schema-diff.txt"
	[ "$?" = 1 ] || { p3_log "两份 schema.sql 应该只差 1 处"; return 1; }
	return 0
}

# sql3_insert_cost：用 sqlite3 CLI 在**真实 20k 库**的副本上灌 2 万行，量纯 SQL 侧代价。
#
# 跟 Go 基准互补：Go 基准测的是 store 层一整个事务（含 ID 生成、FK 校验、驱动开销），
# 这里排除掉所有 Go 侧噪声，只看 SQLite 维护索引要多久。两边方向一致才可信。
#
# passes 参数：交替重复几轮（with/without/with/without…）。单轮 120ms 量级的数字
# 在共享机器上噪声很大，必须多轮取中位数，否则「+10%」可能只是预热差异。
sql3_insert_cost() {
	local out="$P3_RAW/bench/sql3-insert.txt" base="$P3_DBROOT/20k-B/perf.db"
	local passes=${1:-1}
	[ -f "$base" ] || { p3_log "跳过 sql3 复核（缺 $base）"; return 0; }
	local tmp="$P3_ROOT/bench-tmp"
	mkdir -p "$tmp"
	# 在**已有 2 万行的真实库副本**上追加 2 万行：真实库里 tasks 一直在涨，
	# 空表上的插入代价不能代表它（b-tree 高度/页缓存都已经稳态）。
	local ns cap
	ns=$(sed -n 's/^NS=//p' "$P3_DBROOT/20k-B/stamp.env")
	cap=$(sed -n 's/^CAP=//p' "$P3_DBROOT/20k-B/stamp.env")
	local b p
	for p in $(seq 1 "$passes"); do
		for b in with without; do
			cp "$base" "$tmp/$b.db"
			if [ "$b" = "without" ]; then
				sqlite3 "$tmp/$b.db" "DROP INDEX IF EXISTS idx_tasks_ns_created;"
			fi
			local t0 t1
			t0=$(date +%s.%N)
			sqlite3 "$tmp/$b.db" "
				BEGIN;
				INSERT INTO tasks (id, namespace_id, trace_id, path, capability_id, capability, version,
					params, status, attempts, reported, summary, artifacts, error, cap_hash, graph_id, created_at)
				SELECT 'ins_'||printf('%06d_%05d',$p,value), '$ns', 'ins_'||(value/10), '[]', '$cap', 'bench.echo', 1,
					'{}', 'pending', 0, 0, '', '[]', '', 'h', 'g', 1800000000+value
				FROM generate_series(1,20000);
				COMMIT;" >/dev/null
			t1=$(date +%s.%N)
			printf 'sql3-insert\t%s-index\tpass=%s\t20000rows\t%.1fms\n' "$b" "$p" \
				"$(awk -v a="$t0" -v b="$t1" 'BEGIN{printf "%.1f", (b-a)*1000}')" | tee -a "$out"
		done
	done
	rm -rf "$tmp"
}

# store_bench_controlled：契约修订 ② 的**受控版**（主判据的最终数字用这一份）。
#
# 为什么不直接拿 `-count=5` 那一份当结论：默认的 `-benchtime=1s` 让每轮的迭代次数
# 不同（b.N 自适应），而 BenchmarkCreateTasks 的内存库是**随迭代次数长行数**的——
# 后面的样本跑在更大的表上，自然更慢。两个进程前后跑，就把「表变大」的斜率当成了
# 「索引代价」（本轮实测原始数字确实呈上升趋势，中位数不可比）。
# 受控版做两件事：
#   ① -benchtime=300x：固定迭代次数 → 两侧表规模完全相同；
#   ② 同一进程只跑一个子用例、with/without **交替**启动，各两轮 → 抵消预热/顺序偏置。
store_bench_controlled() {
	local out="$P3_RAW/bench"
	prepare_bench_sources || p3_die "准备基准源码失败"
	mkdir -p "$out"
	: >"$out/controlled.txt"
	local pass v dir
	for pass in 1 2 3; do
		for v in with without; do
			if [ "$v" = with ]; then dir=$BSRC_WITH; else dir=$BSRC_NODIX; fi
			p3_log "受控基准 pass=$pass variant=$v"
			(
				cd "$dir" || exit 1
				env $P3_GO_ENV taskset -c "$P3_CORES" go test ./internal/store/ \
					-run '^$' -bench 'BenchmarkCreateTasks/n=20' -benchtime=300x -benchmem -count=5
			) 2>&1 | grep -E '^Benchmark' | sed "s/^/pass=$pass variant=$v\t/" \
				| tee -a "$out/controlled.txt"
		done
	done
	# 纯 SQL 多轮交替复核（5 轮 with/without 交替，各 5 个样本）
	: >"$out/sql3-insert.txt"
	sql3_insert_cost 5
}

# ---------- round-2：最终形态（迁移 16 带 ANALYZE + Open 末尾 PRAGMA optimize） ----------
#
# 只重跑 C 侧（A/B 复用 round-1）：最终代码改的是**迁移与 Open 的统计**，与 A/B 无关。
# 另外补一组 round-1 没做的测量：4 种过滤形状的 EXPLAIN + 单发延迟（稀有 status 的计划是审查点）。

# job_round <binname> <label> <scale> <round>：显式指定二进制与库的一轮读测量。
job_round() {
	local binname=$1 label=$2 scale=$3 round=$4
	local tag="r2-tasks-$scale-$label-r$round"
	load_stamp "$label"
	p3_start_server "$tag" "$binname" "$label" "$(port_for C)" || p3_die "启动失败 $tag"
	curl -s -o /dev/null -H "Cookie: anotify_session=$P3_SESSION" "$P3_BASE/v1/tasks?namespaceId=$P3_NS&limit=200"
	p3_load "$P3_RAW/$tag/load.log" \
		-url "$P3_BASE/v1/tasks?namespaceId=$P3_NS&limit=200" \
		-header "Cookie: anotify_session=$P3_SESSION" -c 32 -d 20s >/dev/null
	p3_stop_server "$tag"
	p3_checkpoint "$label"
	p3_summary_row "$SUMMARY" "${label##*-}" "$scale" "$binname" "$label" "$round" \
		"$P3_RAW/$tag/load.log" "round-2 get /v1/tasks limit=200 c=32 20s"
}

# job_pprof <binname> <label> <round>：显式二进制的「负载 + 重叠 CPU 采样」一轮。
job_pprof() {
	local binname=$1 label=$2 round=$3
	local tag="r2-cpu-$label-r$round"
	load_stamp "$label"
	p3_start_server "$tag" "$binname" "$label" "$(port_for C)" || p3_die "启动失败 $tag"
	curl -s -o /dev/null -H "Cookie: anotify_session=$P3_SESSION" "$P3_BASE/v1/tasks?namespaceId=$P3_NS&limit=200"
	p3_load "$P3_RAW/$tag/load.log" -url "$P3_BASE/v1/tasks?namespaceId=$P3_NS&limit=200" \
		-header "Cookie: anotify_session=$P3_SESSION" -c 32 -d 25s >/dev/null &
	local lp=$!
	sleep 2
	p3_pprof_cpu "$P3_RAW/$tag/cpu.pprof" 20
	wait "$lp"
	p3_stop_server "$tag"
	local parsed n rps p50 p95 p99 total cpur
	parsed=$(p3_parse_load "$P3_RAW/$tag/load.log") || parsed="NA	NA	NA	NA	NA	NA"
	IFS=$'\t' read -r n rps p50 p95 p99 _ <<<"$parsed"
	total=$(p3_pprof_total_secs "$P3_RAW/$tag/cpu.pprof") || total="NA"
	cpur=$(awk -v t="$total" -v r="$rps" 'BEGIN{ if (r>0) printf "%.3f", t*1000/(r*20); else print "NA" }')
	printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' \
		round-2 "$label" "$binname" "$(p3_bin_sha "$binname")" "$label" "$round" \
		"$n" "$rps" "$p50" "$p95" "$p99" "$total" "$cpur" >>"$SUMMARY242"
	p3_pprof_top "$P3_RAW/$tag/cpu.pprof" "$P3_RAW/$tag/pprof-top.txt" 30 >/dev/null
	p3_pprof_list "$P3_RAW/$tag/cpu.pprof" prepareV2 "$P3_RAW/$tag/pprof-list-prepareV2.txt" >/dev/null
	p3_pprof_find "$P3_RAW/$tag/cpu.pprof" 'prepareV2' >"$P3_RAW/$tag/pprof-prepareV2-row.txt"
	sed -n '3p' "$P3_RAW/$tag/pprof-list-prepareV2.txt" >"$P3_RAW/$tag/pprof-prepareV2-cum.txt"
	p3_log "round-2 pprof：rps=$rps p95=$p95 cpu/req=$cpur ms prepareV2=$(cat "$P3_RAW/$tag/pprof-prepareV2-cum.txt")"
}

# filter_shapes <binname> <label>：4 种过滤形状的计划 + 单发延迟（20 次取中位）。
#
# 这是审查点：带 `status=` 时，新索引可能把计划从 `idx_tasks_lease(status=?)` 抢走。
# 判据：稀有 status（canceled）→ 必须 `idx_tasks_lease`；常见/无过滤 → `idx_tasks_ns_created`。
filter_shapes() {
	local binname=$1 label=$2
	local tag="r2-shapes-$label"
	local out="$P3_RAW/$tag"
	mkdir -p "$out"
	load_stamp "$label"
	local trace
	trace=$(sqlite3 "$P3_DBROOT/$label/perf.db" "select trace_id from tasks order by id limit 1;")
	p3_start_server "$tag" "$binname" "$label" "$(port_for C)" || p3_die "启动失败 $tag"
	# 统计学：最终形态应当在 stat1 里有 tasks 的行
	local stat1
	stat1=$(sqlite3 "$P3_DBROOT/$label/perf.db" "select count(*) from sqlite_stat1 where tbl='tasks';" 2>/dev/null || echo NA)
	printf 'stat1_tasks=%s\n' "$stat1" | tee -a "$out/shapes.txt"
	local cols q shape url times i t
	cols=$(sed -n '/^const taskColumns = `/,/`$/p' "$P3_REPO/internal/store/task.go" |
		sed -e 's/^const taskColumns = `//' -e 's/`$//' | tr '\n' ' ' | sed -e 's/  */ /g' -e 's/^ //' -e 's/ $//')
	for shape in 'none|' 'pending|&status=pending' 'canceled|&status=canceled' "traceId|&traceId=$trace"; do
		local name=${shape%%|*} qs=${shape#*|}
		q="SELECT $cols FROM tasks WHERE namespace_id = '$P3_NS'"
		case "$name" in
		pending) q="$q AND status = 'pending'" ;;
		canceled) q="$q AND status = 'canceled'" ;;
		traceId) q="$q AND trace_id = '$trace'" ;;
		esac
		q="$q ORDER BY created_at DESC, id DESC LIMIT 200"
		{
			echo "== shape=$name"
			echo "# SQL: $q"
			sqlite3 "$P3_DBROOT/$label/perf.db" "EXPLAIN QUERY PLAN $q;"
		} >>"$out/shapes.txt"
		url="$P3_BASE/v1/tasks?namespaceId=$P3_NS&limit=200$qs"
		curl -s -o /dev/null -H "Cookie: anotify_session=$P3_SESSION" "$url"  # 预热
		times=""
		for i in $(seq 1 20); do
			t=$(curl -s -o /dev/null -w '%{time_total}' -H "Cookie: anotify_session=$P3_SESSION" "$url")
			times="$times $t"
		done
		printf 'latency_ms\t%s\tmedian=%s\tvalues=%s\n' "$name" \
			"$(printf '%s\n' $times | sort -n | awk '{a[NR]=$1} END{printf "%.3f", (a[10]+a[11])/2*1000}')" "$times" >>"$out/shapes.txt"
	done
	p3_stop_server "$tag"
	p3_log "过滤形状结果：$out/shapes.txt"
	cat "$out/shapes.txt" >&2
}

# write_round_clean <cfg> <round>：**修正数据增长混淆**的写路径一轮。
#
# 为什么需要它：普通写轮每轮往库追加 ~2.2-2.4 万行 tasks，A→B→C 后测的配置天然跑在更大的表上，
# RPS 单调下降里混着「表变大」。这里每轮**把库重置回各自的初始拷贝**（w-A / w-B 逐字节同源），
# 于是 4 轮（A,B,A,B）都在同样大小的表上，差值才是索引的写代价。
write_round_clean() {
	local cfg=$1 round=$2
	local label="w-$cfg" pristine="w-$cfg-pristine"
	local tag="r2-write-clean-$cfg-r$round"
	p3_checkpoint "$pristine"
	rm -f "$P3_DBROOT/$label/perf.db" "$P3_DBROOT/$label/perf.db-wal" "$P3_DBROOT/$label/perf.db-shm"
	cp "$P3_DBROOT/$pristine/perf.db" "$P3_DBROOT/$label/perf.db" || p3_die "重置 $label 失败"
	cp "$P3_DBROOT/$pristine/stamp.env" "$P3_DBROOT/$label/stamp.env" 2>/dev/null || true
	load_stamp "$label"
	# 端口冷却：上一个 server 退出时可能还在收尾（本轮踩过 bind: address already in use）
	sleep 3
	p3_start_server "$tag" "$(bin_for "$cfg")" "$label" "$(port_for "$cfg")" \
		ANOTIFY_HOOK_RATE_PER_MIN=200000 ANOTIFY_HOOK_RATE_PER_IP_PER_MIN=200000 || p3_die "启动失败 $tag"
	local ev0 tk0 ev1 tk1
	ev0=$(sqlite3 "$P3_DBROOT/$label/perf.db" "select count(*) from events;")
	tk0=$(sqlite3 "$P3_DBROOT/$label/perf.db" "select count(*) from tasks;")
	p3_load "$P3_RAW/$tag/load.log" -url "$P3_BASE/v1/hooks/$P3_HOOK_TOKEN" -method POST \
		-body-file "$P3_RAW/hook-body.json" -header "content-type: application/json" -c 8 -d 20s >/dev/null
	p3_stop_server "$tag"
	p3_checkpoint "$label"
	ev1=$(sqlite3 "$P3_DBROOT/$label/perf.db" "select count(*) from events;")
	tk1=$(sqlite3 "$P3_DBROOT/$label/perf.db" "select count(*) from tasks;")
	[ "$((ev1 - ev0))" -ge 1 ] || p3_die "$tag：events 没涨，拒绝出数"
	p3_summary_row "$SUMMARY" "$cfg" "20k-clean" "$(bin_for "$cfg")" "$label" "$round" \
		"$P3_RAW/$tag/load.log" "clean-reset hook c=8 20s（每轮重置库，无数据增长混淆）events+$((ev1 - ev0)) tasks+$((tk1 - tk0))"
}

round2() {
	p3_require_env
	if [ ! -f "$P3_ROOT/GO-FINAL" ]; then
		p3_say "等 /tmp/perf-3a/GO-FINAL（最多 60 分钟）"
		local i
		for i in $(seq 1 360); do
			[ -f "$P3_ROOT/GO-FINAL" ] && { p3_log "等到了（第 $((i * 10))s）"; break; }
			[ $((i % 30)) -eq 0 ] && p3_log "仍在等 GO-FINAL（$((i * 10))s / 3600s）"
			sleep 10
		done
		[ -f "$P3_ROOT/GO-FINAL" ] || { p3_log "等满 60 分钟无 GO-FINAL"; return 3; }
	fi
	e2e_guard
	local parts=" ${ROUND2_PARTS:-build db read pprof shapes} "
	p2_has() { case "$parts" in *" $1 "*) return 0 ;; *) return 1 ;; esac; }
	# 1) 最终二进制（会覆盖 $P3_BIN/anotify-after；round-1 的 70614342… 另存为 anotify-after-round1）
	p2_has build || {
		p3_log "round-2 parts=$parts → 跳过 build/db/read/pprof/shapes 中的部分"
	}
	if p2_has build; then
	p3_say "round-2：构建最终 anotify-after"
	p3_build_after
	cp -a "$P3_RAW/after-provenance.txt" "$P3_RAW/round2-provenance.txt"
	# 自证：git archive HEAD 重编 == 刚构建的二进制（对不上只记录，不中断——工作树可能还有未提交改动）
	local head src2
	head=$(git -C "$P3_REPO" rev-parse HEAD)
	src2="$P3_SRC/round2-verify"
	rm -rf "$src2" && mkdir -p "$src2"
	git -C "$P3_REPO" archive "$head" | tar -x -C "$src2"
	rm -rf "$src2/internal/server/dist" && cp -a "$P3_ROOT/dist" "$src2/internal/server/dist"
	(cd "$src2" && env $P3_GO_ENV taskset -c "$P3_CORES" go build -trimpath -tags embeddist -o "$src2/v" ./cmd/server) || p3_die "archive 构建失败"
	{
		echo "round2-worktree-binary=$(sha256sum "$P3_BIN/anotify-after" | cut -d' ' -f1)"
		echo "round2-archive-binary=$(sha256sum "$src2/v" | cut -d' ' -f1)"
		echo "git-status-porcelain:"
		git -C "$P3_REPO" status --porcelain | head -20
	} | tee "$P3_RAW/round2-reproducibility.txt"
	p3_bin_backup
	fi

	# 2) db-C2 = db-B 拷贝 + 最终二进制 Open（触发 PRAGMA optimize）；db-B2 作为「B 加统计」的对照
	if p2_has db; then
	p3_say "round-2：派生 db-C2 / db-B2（并记录 sqlite_stat1）"
	p3_copy_db 20k-B 20k-C2
	p3_copy_db 20k-B 20k-B2
	p3_copy_db 100k-B 100k-C2
	local lab
	for lab in 20k-C2 20k-B2 100k-C2; do
		p3_start_server "r2-open-$lab" after "$lab" "$(port_for C)" || p3_die "Open 失败 $lab"
		p3_stop_server "r2-open-$lab"
		p3_checkpoint "$lab"
		printf '%s\tstat1_tasks=%s\tsha256=%s\tbytes=%s\n' "$lab" \
			"$(sqlite3 "$P3_DBROOT/$lab/perf.db" "select count(*) from sqlite_stat1 where tbl='tasks';" 2>/dev/null || echo NA)" \
			"$(sha256sum "$P3_DBROOT/$lab/perf.db" | cut -c1-16)" \
			"$(stat -c %s "$P3_DBROOT/$lab/perf.db")" | tee -a "$P3_RAW/round2-db.txt"
	done
	fi

	# 3) 读路径：C2 两轮（20k / 100k）+ B2 一轮（20k 对照）
	local r s
	if p2_has read; then
	p3_say "round-2：读路径（C2 20k/100k 各两轮 + B2 20k 一轮）"
	for r in 1 2; do
		for s in 20k 100k; do
			job_round after "$s-C2" "$s" "$r"
		done
	done
	job_round after 20k-B2 20k 1
	fi

	# 4) pprof：C2 20k 两轮
	if p2_has pprof; then
	p3_say "round-2：pprof（C2 20k 两轮）"
	for r in 1 2; do job_pprof after 20k-C2 "$r"; done
	fi

	# 5) 过滤形状：round-1 的 C（无统计）vs round-2 的 C2（有统计）
	if p2_has shapes; then
	p3_say "round-2：4 种过滤形状 EXPLAIN + 单发延迟（20k-C vs 20k-C2）"
	filter_shapes after-round1 20k-C
	filter_shapes after 20k-C2
	fi

	if p2_has write; then
		# 写路径要用 webhook token：round2 不走 phase2 的前置，自己保证它存在
		ensure_hook_source 20k
		P3_HOOK_TOKEN=$(cat "$P3_RAW/hook-token.txt")
		p3_say "round-2：修正混淆的写路径（w-A/w-B 逐字节同源，每轮重置库，A/B/A/B）"
		p3_copy_db seed-20k w-A-pristine
		p3_copy_db w-A-pristine w-B-pristine
		p3_start_server "r2-open-wB" index w-B-pristine "$(port_for B)" || p3_die "w-B 迁移失败"
		p3_stop_server "r2-open-wB"
		sqlite3 "$P3_DBROOT/w-B-pristine/perf.db" "PRAGMA wal_checkpoint(TRUNCATE); VACUUM; ANALYZE;"
		p3_copy_db w-A-pristine w-A
		p3_copy_db w-B-pristine w-B
		for r in 1 2; do
			write_round_clean A "$r"
			write_round_clean B "$r"
		done
	fi

	export_artifacts
	p3_log "round-2 完成"
}

phase2() {
	p3_require_env
	wait_go || {
		p3_log "相位 2 未执行（无 GO-AFTER）"
		return 2
	}
	e2e_guard
	bins
	# 相位 1 可能没跑过（比如直接 phase2）：这里补齐结构性的前提
	[ -f "$P3_DBROOT/20k-C/perf.db" ] || { p3_say "缺副本，先补相位 1"; phase1; }

	ensure_hook_source 20k
	P3_HOOK_TOKEN=$(cat "$P3_RAW/hook-token.txt")
	# 分块执行：PHASE2_PARTS="bench read cpu hooks"（默认全跑）。
	# 为什么要有：某一块的夹具坏了（比如播库时漏了 webhook 源）就只需重跑那一块，
	# 不用把已经干净的读数重建一遍（重建反而会把机器噪声重新摇一次）。
	local parts=" ${PHASE2_PARTS:-bench read cpu hooks} "
	# 汇总表：要重跑的块先清空自己的行，避免半截数据混进报告。
	# read 是第一个写 summary 的块 → 它重跑就整表重建；hooks 单跑则只剔掉旧 hook 行，
	# 保留已经采干净的读路径行（否则重跑一次写路径就把 #237 的读数一起烧了）。
	case "$parts" in *" read "*)
		p3_summary_header >"$SUMMARY"
		;;
	*)
		[ -f "$SUMMARY" ] || p3_summary_header >"$SUMMARY"
		case "$parts" in *" hooks "*)
			awk -F'\t' 'NR==1 || $14 !~ /^hook/' "$SUMMARY" >"$SUMMARY.tmp" && mv "$SUMMARY.tmp" "$SUMMARY"
			;;
		esac
		;;
	esac
	case "$parts" in *" cpu "*)
		printf 'config\tscale\tbin\tbin_sha12\tdb\tround\tn\trps\tp50ms\tp95ms\tp99ms\tpprof_total_s\tcpu_per_req_ms\n' >"$SUMMARY242"
		;;
	esac
	: >"$P3_RAW/phase2-summary.tsv"

	case "$parts" in *" bench "*)
		p3_say "① store 层写路径基准（索引纯代价，主判据）"
		store_bench
		;;
	esac

	local r s cfg
	case "$parts" in *" read "*)
		p3_say "② #237 读路径：tasks=20k / 100k，A vs B，交替两轮"
		for r in 1 2; do
			for s in 20k 100k; do
				for cfg in A B; do
					read_round "$cfg" "$s" "$r"
				done
			done
		done
		;;
	esac

	case "$parts" in *" cpu "*)
		p3_say "③ #242：B vs C（同库结构），负载 + CPU profile 重叠，交替两轮"
		for r in 1 2; do
			for cfg in B C; do
				cpu_round "$cfg" 20k "$r"
			done
		done
		;;
	esac

	case "$parts" in *" hooks "*)
		p3_say "④ 写路径 (a) 默认 env 10s —— 预期撞 600/min 限流"
		for cfg in A B C; do
			hook_default_round "$cfg" 20k
		done

		p3_say "⑤ 写路径 (b) 显式抬高双维限流后 20s c=8 —— A/B/C 交替两轮"
		# 唯一偏离默认 env 的地方：默认 600/min 的 token 维会把写路径截在 10 RPS，
		# 那样量到的是限流器不是 DB。抬到 20 万/min 后请求才真的打到 SQLite。
		local HOOK_ENV=(ANOTIFY_HOOK_RATE_PER_MIN=200000 ANOTIFY_HOOK_RATE_PER_IP_PER_MIN=200000)
		for r in 1 2; do
			for cfg in A B C; do
				write_round "$cfg" 20k "$r" "${HOOK_ENV[@]}"
			done
		done
		;;
	esac

	p3_say "汇总"
	column -t -s$'\t' "$SUMMARY" >&2 || cat "$SUMMARY" >&2
	column -t -s$'\t' "$SUMMARY242" >&2 || cat "$SUMMARY242" >&2
	export_artifacts
	p3_log "相位 2 完成"
}

stopall() {
	local f
	for f in "$P3_RAW"/*/server.pid; do
		[ -f "$f" ] || continue
		p3_stop_server "$(basename "$(dirname "$f")")"
	done
	p3_log "已收掉本脚本记录的所有 server 进程"
}

case "${1:-}" in
bins) bins ;;
seed)
	# seed [scale...]：默认两个档都做；指定档就只做那一档（重跑单档时省一半临时盘）
	p3_require_env
	[ "$#" -gt 1 ] && shift
	scales="${*:-20k 100k}"
	for one in $scales; do
		case "$one" in
		20k) seed_only 20k $SCALE_20K; derive 20k $SCALE_20K ;;
		100k) seed_only 100k $SCALE_100K; derive 100k $SCALE_100K ;;
		*) p3_die "未知档位 $one（只认 20k / 100k）" ;;
		esac
	done
	;;
phase1) phase1 ;;
phase2) phase2 ;;
bench) p3_require_env; store_bench ;;
bench2) p3_require_env; store_bench_controlled ;;
round2) round2 ;;
stopall) stopall ;;
all)
	phase1
	phase2
	;;
*)
	sed -n '2,30p' "$0"
	exit 2
	;;
esac
