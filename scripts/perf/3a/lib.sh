#!/usr/bin/env bash
# 批次 3A（#237 tasks 列表索引 + #242 store 层语句缓存）的压测公共底座。
#
# 只做「怎么把环境搭起来 + 怎么采样 + 怎么把数字抠出来」，不含任何结论。
# 集中到一处的原因和 b/lib.sh、c/common.sh 一样：三个配置（基线 / 只加索引 /
# 索引+语句缓存）必须跑在**同一台机器的同一批核**上，端口段、分核、库路径、
# 采样命令必须逐字一致，否则三组数字之间没法比较。
#
# 用法：
#   source scripts/perf/3a/lib.sh
#   p3_require_env
#   p3_build_all                 # A/B 两个二进制（C 要等 /tmp/perf-3a/GO-AFTER）
#   p3_seed 20k 20000 100000     # 播种（只做一次，之后各配置 cp 副本）
#   p3_start_server ... / p3_load ... / p3_stop_server
#
# 纪律（issue #217 的约定表，写死在常量里，场景脚本不得绕开）：
#   * 分核：一切压测/采样/构建命令都要在 `taskset -c $P3_CORES` 下跑（默认 0-3）
#   * 端口：只用 8020-8039（段外端口 p3_check_port 直接炸）
#   * 每个场景各自一份库文件；副本之间只允许差「迁移」造成的结构差异
#   * 二进制来源必须可追溯：每个二进制都记 sha256 + `go version -m` 的 build 行，
#     因为本仓**踩过**「默认不重建二进制、拿旧二进制当新版本量」的坑（issue #257）
#
# 本文件不改产品代码；所有运行期产物都落在 $P3_ROOT（默认 /tmp/perf-3a）。

set -uo pipefail

P3_CORES=${P3_CORES:-0-3}
P3_PORT_LO=8020
P3_PORT_HI=8039
P3_ROOT=${P3_ROOT:-/tmp/perf-3a}
# 仓库根：优先问 git（lib.sh 在 scripts/perf/3a/，靠相对路径数 .. 很容易数错一层）
P3_REPO=${P3_REPO:-$(git -C "$(dirname "${BASH_SOURCE[0]}")" rev-parse --show-toplevel 2>/dev/null)} \
	|| P3_REPO=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)

P3_BIN=$P3_ROOT/bin
P3_RAW=$P3_ROOT/raw
P3_DBROOT=$P3_ROOT/db
P3_SRC=$P3_ROOT/src

# 三个配置的源码身份（写死，别让脚本"猜当前 HEAD"——那会让报告里的 sha 与实物脱钩）。
#   A = 改动前（3433eef 是 #237 索引提交的父提交）
#   B = 只有索引（dfcf97b = 迁移 16）
#   C = 索引 + 语句缓存（当前 worktree 工作树状态，含未提交改动，身份用树指纹锚定）
P3_COMMIT_BASE=3433eef8ccd1b36764cff430997bc6e9e73158d6
P3_COMMIT_INDEX=dfcf97b5f6c0f2ed103be7ab06f966c1d9519db4

# go 命令的统一环境（见 AGENTS.md §1：直连不走镜像，官方校验和，自动补工具链）。
P3_GO_ENV="GOPROXY=${GOPROXY:-direct} GOSUMDB=${GOSUMDB:-sum.golang.org} GOTOOLCHAIN=${GOTOOLCHAIN:-auto}"

# ---------- 通用小工具 ----------

p3_log() { printf '[%s] %s\n' "$(date +%H:%M:%S)" "$*" >&2; }
p3_say() { printf '\n\033[1;36m▶ %s\033[0m\n' "$*" >&2; }
p3_die() { printf '\n\033[1;31m✗ %s\033[0m\n' "$*" >&2; exit 1; }

# p3_check_port：端口必须在 8020-8039（越界直接退出，防止跟 B/C 路抢端口）。
p3_check_port() {
	local p=$1
	if ! [[ "$p" =~ ^[0-9]+$ ]] || [ "$p" -lt "$P3_PORT_LO" ] || [ "$p" -gt "$P3_PORT_HI" ]; then
		p3_die "端口 $p 不在 3A 段 $P3_PORT_LO-$P3_PORT_HI 内"
	fi
}

p3_require_env() {
	local c
	for c in taskset curl jq sqlite3 go awk sed; do
		command -v "$c" >/dev/null || p3_die "缺少 $c"
	done
	mkdir -p "$P3_BIN" "$P3_RAW" "$P3_DBROOT" "$P3_SRC"
	p3_log "root=$P3_ROOT repo=$P3_REPO cores=$P3_CORES ports=$P3_PORT_LO-$P3_PORT_HI"
}

# ---------- 前端 embed 产物 ----------
#
# 三个配置都要 -tags embeddist（契约 §3 要求两边同口径），而 internal/server/dist
# 是 gitignore 的生成物，git archive 出来的源码树里没有。
#
# **不在 worktree 里跑 `make fe`**：fe = check-classes + sitegen + hash，sitegen 会往
# web/ 写 *.html。本 worker 的红线是「一行产品代码都不许改」，把生成物写进仓库
# 也会污染 git status、干扰 A/B 两路与 centurion 的提交。
# 所以 dist 一律在 /tmp 里备好，再 cp 进各源码快照 —— 三份二进制内嵌**同一份** dist，
# 唯一的差异就只有被测的代码本身。
#
# 取 dist 的顺序：
#   ① 已有 $P3_ROOT/dist（本次运行已经备好）；
#   ② 复用 centurion 跑过 `make fe` 的基线快照里的 dist（内容与当前 web/ 一致：
#      web/、web-src/ 在 3433eef..dfcf97b 间零改动，另两路 worker 只动 internal/store）；
#   ③ 都没有就在 /tmp 里跑一遍 sitegen + hash 现生成。
p3_ensure_dist() {
	if [ ! -f "$P3_ROOT/dist/manifest.json" ]; then
		if [ -f "$P3_ROOT/base-src/internal/server/dist/manifest.json" ]; then
			p3_say "复用基线快照里已生成的 dist（make fe 产物，只读拷贝）"
			rm -rf "$P3_ROOT/dist"
			cp -a "$P3_ROOT/base-src/internal/server/dist" "$P3_ROOT/dist" || return 1
		else
			p3_say "生成前端指纹 dist（只写 /tmp，不碰 worktree）"
			rm -rf "$P3_ROOT/web-gen"
			cp -a "$P3_REPO/web" "$P3_ROOT/web-gen" || return 1
			(
				cd "$P3_REPO" || exit 1
				go run ./cmd/sitegen -src web-src -out "$P3_ROOT/web-gen" -langs zh-CN,en,ja,es &&
					node scripts/hash.mjs "$P3_ROOT/web-gen" "$P3_ROOT/dist"
			) 2>&1 | tee "$P3_RAW/hash-dist.log" >&2 || return 1
		fi
	fi
	# 自证：dist 里必须有 HTML。只有 assets 的 dist 是伪 embeddist（静态页全 404）——
	# 二进制能跑、API 也不受影响，但它不是「make build 那个二进制」，不能拿来当同口径证据。
	if [ -z "$(find "$P3_ROOT/dist" -maxdepth 1 -name '*.html' -print -quit)" ]; then
		p3_die "dist 里没有 .html（见 $P3_RAW/hash-dist.log）：不能当作 embeddist 构建输入"
	fi
	p3_log "dist = $(find "$P3_ROOT/dist" -type f | wc -l) 个文件（$(find "$P3_ROOT/dist" -maxdepth 1 -name '*.html' | wc -l) 个 HTML）"
}

# ---------- 二进制 ----------

# p3_build_archive <commit> <name>：从历史提交构建一个二进制（git archive → /tmp 快照 → go build）。
p3_build_archive() {
	local commit=$1 name=$2
	local src="$P3_SRC/$name" out="$P3_BIN/anotify-$name"
	p3_ensure_dist
	p3_say "构建 anotify-$name（git archive $commit → $src）"
	rm -rf "$src" || return 1
	mkdir -p "$src" || return 1
	git -C "$P3_REPO" archive "$commit" | tar -x -C "$src" || p3_die "git archive $commit 失败"
	# 先删掉目标 dir：worktree 快照里可能已经有 internal/server/dist（别人跑过 make fe），
	# 而 `cp -a X Y` 在 Y 已存在时会把 X 拷成 Y/X —— 于是嵌入的是**另一个** dist，
	# 编出来的二进制与其它配置不同源（本轮真踩过：多出 8KB 掉嵌目录，sha 对不上）。
	rm -rf "$src/internal/server/dist"
	mkdir -p "$src/internal/server"
	cp -a "$P3_ROOT/dist" "$src/internal/server/dist" || p3_die "cp dist 失败"
	(
		cd "$src" || exit 1
		env $P3_GO_ENV taskset -c "$P3_CORES" go build -trimpath -tags embeddist -o "$out" ./cmd/server
	) || p3_die "构建 $name 失败"
	[ -x "$out" ] || p3_die "$out 不存在"
	p3_log "anotify-$name = $(sha256sum "$out" | cut -c1-16)… $(stat -c %s "$out") 字节"
}

# p3_source_fingerprint：当前 worktree **工作树状态**的指纹（HEAD + diff + 未跟踪文件内容）。
#
# 只用 HEAD 的 sha 不够：C 配置要看的是「A/B 两路 worker 改完、还没提交的那一坨」。
# 所以把 `git diff` 原文与每个未跟踪文件的内容哈希一起卷进 sha256。
# 报告里贴这个指纹，就能确认量的是哪份源码。
p3_source_fingerprint() {
	(
		cd "$P3_REPO" || exit 1
		echo "HEAD=$(git rev-parse HEAD)"
		git diff
		local f
		for f in $(git ls-files -o --exclude-standard | sort); do
			echo "== untracked $f $(sha256sum "$f" | cut -d' ' -f1)"
		done
	) | sha256sum | cut -d' ' -f1
}

# p3_snapshot_worktree <dst>：把 worktree 的工作树状态（含未提交改动）拷到 /tmp。
#
# 为什么不直接在 worktree 里 `go build -tags embeddist`：那要先往
# internal/server/dist 写 dist（gitignore 里，但仍属于「动了仓库」），
# 而且 dist 目录在 worktree 里出现/消失会让并行的 A/B 两路看到不同的 `git status`。
# 快照到 /tmp 再构建 = 完全隔离。
p3_snapshot_worktree() {
	local dst=$1
	rm -rf "$dst" || return 1
	mkdir -p "$dst" || return 1
	# 只带源码相关的东西：.git 不要（不是要在里面跑 git），node_modules 不要（几个 G），
	# .pi-legion / .pi-subagents 是本批的编排产物，跟二进制无关。
	# 顶层 anotify 是被 `make build` 生成过的旧二进制（若有），不要带。
	tar -c -C "$P3_REPO" \
		--exclude=.git --exclude=node_modules --exclude=.pi-legion \
		--exclude=.pi-subagents --exclude=anotify \
		. 2>/dev/null | tar -x -C "$dst" || p3_die "快照 worktree 失败"
}

# p3_build_after：C 配置的二进制（索引 + 语句缓存），从当前 worktree 的工作树状态构建。
p3_build_after() {
	local src="$P3_SRC/after" out="$P3_BIN/anotify-after"
	p3_ensure_dist
	local fp head drv
	fp=$(p3_source_fingerprint)
	head=$(git -C "$P3_REPO" rev-parse HEAD)
	p3_say "构建 anotify-after（worktree 快照 → $src，HEAD $head，树指纹 $fp）"
	p3_snapshot_worktree "$src"
	# 先删掉目标 dir：worktree 快照里可能已经有 internal/server/dist（别人跑过 make fe），
	# 而 `cp -a X Y` 在 Y 已存在时会把 X 拷成 Y/X —— 于是嵌入的是**另一个** dist，
	# 编出来的二进制与其它配置不同源（本轮真踩过：多出 8KB 掉嵌目录，sha 对不上）。
	rm -rf "$src/internal/server/dist"
	mkdir -p "$src/internal/server"
	cp -a "$P3_ROOT/dist" "$src/internal/server/dist" || p3_die "cp dist 失败"
	# C 配置 = 索引 + 语句缓存 + **驱动升级**（modernc.org/sqlite v1.34.5 → v1.45.0）。
	# 驱动版本是 #242 的根因之一（v1.34.5 的 (*stmt).query 每次都 prepareV2，
	# v1.45.0 起缓存了编译句柄），所以它必须成为可验的数字，而不是一句描述：
	# 从**快照的 go.mod**取（仓库可能在构建后又被别人改），再用二进制里的 dep 交叉验证。
	drv=$(cd "$src" && go list -m modernc.org/sqlite 2>/dev/null | awk '{print $2}')
	(
		cd "$src" || exit 1
		env $P3_GO_ENV taskset -c "$P3_CORES" go build -trimpath -tags embeddist -o "$out" ./cmd/server
	) || p3_die "构建 after 失败"
	printf '%s\n' "$fp" >"$P3_RAW/after-source-fingerprint.txt"
	{
		echo "config=C（索引 + 语句缓存 + 驱动升级）"
		echo "git rev-parse HEAD=$head"
		echo "worktree-tree-fingerprint=$fp  # sha256(HEAD + git diff + 每个未跟踪文件内容)"
		echo "driver-in-go.mod=modernc.org/sqlite ${drv:-UNKNOWN}  # A/B 是 v1.34.5"
		echo "binary=$out"
		echo "binary-sha256=$(sha256sum "$out" | cut -d' ' -f1)"
		echo "linked-driver=$(go version -m "$out" | sed -n 's/^\s*dep\tmodernc.org\/sqlite\t\([^\t]*\).*/\1/p')"
	} >"$P3_RAW/after-provenance.txt"
	p3_log "anotify-after = $(sha256sum "$out" | cut -c1-16)… $(stat -c %s "$out") 字节（HEAD $head，驱动 ${drv:-?}）"
}

# p3_build_tools：devseed / loadgen（都从**基线**提交构建：devseed 要造 v15 的库，
# 那是所有配置的共同起点；loadgen 只依赖标准库，用基线版和用改后版没有差别）。
p3_build_tools() {
	local src="$P3_SRC/base"
	[ -x "$P3_BIN/devseed" ] && [ -x "$P3_BIN/loadgen" ] && return 0
	[ -d "$src" ] || p3_die "缺少 $src（先跑 p3_build_archive base）"
	p3_say "构建 devseed / loadgen（基线提交）"
	(
		cd "$src" || exit 1
		env $P3_GO_ENV taskset -c "$P3_CORES" go build -o "$P3_BIN/devseed" ./cmd/devseed &&
			env $P3_GO_ENV taskset -c "$P3_CORES" go build -o "$P3_BIN/loadgen" ./scripts/perf/loadgen
	) || p3_die "构建 tools 失败"
}

# p3_bin_info <name>：二进制溯源行（报告里每张表都要能对上号）。
p3_bin_info() {
	local name=$1
	local bin="$P3_BIN/anotify-$name"
	[ -x "$bin" ] || { echo "$name MISSING"; return 1; }
	printf '%s\t%s\t%s\t%s\n' "$name" \
		"$(sha256sum "$bin" | cut -c1-16)" \
		"$(stat -c %s "$bin")" \
		"$(go version -m "$bin" 2>/dev/null | sed -n 's/^\s*build\t-tags=//p' | tr '\n' ',')"
}

# p3_bin_sha <name>：报告表格里填的短 sha。
p3_bin_sha() {
	local bin="$P3_BIN/anotify-$1"
	[ -x "$bin" ] && sha256sum "$bin" | cut -c1-12 || echo "MISSING"
}

# ---------- 播种 ----------

# p3_seed <label> <tasks> <events>：造一份「基线（v15）»的库，并把它当成唯一的数据源。
#
# devseed（基线二进制）建用户/命名空间/能力 → 再用 generate_series 批量灌 tasks/events。
# 之所以走 SQL 批量而不是循环调 HTTP：我们要的是**数据规模**，不是写入路径的耗时；
# 20 万行 0.1s 量级，且行内容完全确定（同 label 同规模 = 逐字节相同的库）。
#
# 最后 VACUUM + ANALYZE：前者让文件大小可比（否则比的是空闲页残留），
# 后者让 A/B 两侧都有 sqlite_stat1（否则 B 的索引统计是缺的，计划会不可比）。
p3_seed() {
	local label=$1 ntasks=$2 nevents=$3
	local dir="$P3_DBROOT/$label"
	local db="$dir/perf.db"
	p3_say "播种 $label：tasks=$ntasks events=$nevents → $db"
	mkdir -p "$dir" || return 1
	rm -f "$db" "$db-wal" "$db-shm"
	"$P3_BIN/devseed" -db "$db" -username perf >"$dir/devseed.log" 2>&1 || p3_die "devseed 失败"
	local session ns cap
	session=$(sed -n 's/^SESSION=//p' "$dir/devseed.log")
	ns=$(sqlite3 "$db" "select id from namespaces limit 1")
	cap=$(sqlite3 "$db" "select id from capability_versions limit 1")
	[ -n "$session" ] && [ -n "$ns" ] && [ -n "$cap" ] || p3_die "devseed 产出不完整（见 $dir/devseed.log）"

	# tasks：状态 pending、无节点、无租约 —— 就是 ListTasks 会扫到的形状。
	# created_at 递增（同一秒会有多行，正好覆盖 created_at 相等时靠 id 破平的情况）。
	sqlite3 "$db" "
		INSERT INTO tasks (id, namespace_id, trace_id, path, capability_id, capability, version,
			params, status, attempts, reported, summary, artifacts, error, cap_hash, graph_id, created_at)
		SELECT 'tsk_'||printf('%010d',value), '$ns', 'tr_'||(value/10), '[]', '$cap', 'bench.echo', 1,
			'{}', 'pending', 0, 0, '', '[]', '', 'h', 'g', 1700000000+value
		FROM generate_series(1,$ntasks);" || p3_die "灌 tasks 失败"

	# events：只灌行，不给 idem_key（NULL 允许重复）——量的是读路径的扫表成本。
	sqlite3 "$db" "
		INSERT INTO events (id, namespace_id, trace_id, source, type, payload, idem_key, created_at)
		SELECT 'evt_'||printf('%010d',value), '$ns', 'tr_'||(value/10), 'bench', 'bench.echo',
			'{\"n\":1}', NULL, 1700000000+value
		FROM generate_series(1,$nevents);" || p3_die "灌 events 失败"

	sqlite3 "$db" "PRAGMA wal_checkpoint(TRUNCATE); VACUUM; ANALYZE;" || p3_die "VACUUM/ANALYZE 失败"

	local ver n1 n2
	ver=$(sqlite3 "$db" "select max(version) from schema_version;")
	n1=$(sqlite3 "$db" "select count(*) from tasks;")
	n2=$(sqlite3 "$db" "select count(*) from events;")
	cat >"$dir/stamp.env" <<-EOF
		# 由 p3_seed 生成：这份库的「出生证明」，任何报告表格都要引用它
		LABEL=$label
		DB=$db
		SCHEMA_VERSION=$ver
		SESSION=$session
		NS=$ns
		CAP=$cap
		TASKS=$n1
		EVENTS=$n2
		BYTES=$(stat -c %s "$db")
	EOF
	p3_log "播种完成：schema v$ver tasks=$n1 events=$n2 文件=$(stat -c %s "$db") 字节"
}

# p3_copy_db <from-label> <to-label>：把一个库原样拷成另一份（含 stamp，便于溯源）。
#
# 三份库必须「行数据逐字节相同、唯一差异是结构」：所以一律 cp 而不是重新播种，
# 播种时的任何随机性（id 时间戳、页面布局）都不该混进 A/B/C 之间。
p3_copy_db() {
	local from="$P3_DBROOT/$1" to="$P3_DBROOT/$2" db
	[ -f "$from/perf.db" ] || p3_die "p3_copy_db: 源库不存在 $from/perf.db"
	# **必顶**：先把 WAL fold 回主文件。只 cp perf.db 而不 checkpoint，会把
	# 上一次服务进程遗在 -wal 里的写入静默丢掉——本轮真踩过：webhook 源行就是这么丢的，
	# 结果整个写路径场景全 404（而读路径照常 200，很容易看不见）。
	sqlite3 "$from/perf.db" "PRAGMA wal_checkpoint(TRUNCATE);" >/dev/null 2>&1
	if [ -s "$from/perf.db-wal" ]; then
		p3_die "p3_copy_db: checkpoint 后 -wal 仍非空（$from），拒绝拷贝（否则会丢写入）"
	fi
	mkdir -p "$to" || return 1
	rm -f "$to/perf.db" "$to/perf.db-wal" "$to/perf.db-shm"
	cp "$from/perf.db" "$to/perf.db" || p3_die "cp 库失败"
	# stamp 也跟着改：报告里引用的是「哪份库」，副本里的 DB= 行必须指向副本自己，
	# 否则读者会以为三张表量的是同一个文件。
	if [ -f "$from/stamp.env" ]; then
		sed -e "s|^LABEL=.*|LABEL=$2|" -e "s|^DB=.*|DB=$to/perf.db|" \
			"$from/stamp.env" >"$to/stamp.env"
	fi
	p3_log "库拷贝 $1 → $2（$(stat -c %s "$to/perf.db") 字节，sha256 $(sha256sum "$to/perf.db" | cut -c1-12)…）"
}

# p3_checkpoint <label>：把 WAL fold 回主文件。
#
# 写路径场景里 WAL 会隐式长很大（10 万个事件 + 任务），不 checkpoint 的话：
#   ① 行数统计可能读到旧快照；② 临时盘直接被吃掉（本轮真踩过 Disk quota exceeded）。
# TRUNCATE 还会把 -wal 文件截回 0，体积可控。
p3_checkpoint() {
	local db="$P3_DBROOT/$1/perf.db"
	[ -f "$db" ] || return 0
	sqlite3 "$db" "PRAGMA wal_checkpoint(TRUNCATE);" >/dev/null 2>&1
	return 0
}

# p3_db_version <label>：库的 schema 版本（用于确认 db-A 停在 15、db-B/C 升到 16）。
p3_db_version() {
	sqlite3 "$P3_DBROOT/$1/perf.db" "select max(version) from schema_version;"
}

# p3_db_index_state <label>：新索引在不在（新库/老库升级两条验收线都靠它）。
p3_db_index_state() {
	local db="$P3_DBROOT/$1/perf.db"
	local n cols
	n=$(sqlite3 "$db" "select count(*) from sqlite_master where type='index' and name='idx_tasks_ns_created';")
	[ "$n" = 0 ] && { echo "absent"; return 0; }
	cols=$(sqlite3 "$db" "select group_concat(name,',') from pragma_index_info('idx_tasks_ns_created') order by seqno;")
	echo "present($cols)"
}

# p3_db_size <label>：库文件字节数（先 checkpoint，否则比的是 WAL 残留）。
p3_db_size() {
	local db="$P3_DBROOT/$1/perf.db"
	[ -f "$db" ] || { echo 0; return 0; }
	sqlite3 "$db" "PRAGMA wal_checkpoint(TRUNCATE);" >/dev/null 2>&1
	local pages psize
	pages=$(sqlite3 "$db" "PRAGMA page_count;" 2>/dev/null)
	psize=$(sqlite3 "$db" "PRAGMA page_size;" 2>/dev/null)
	echo "$((pages * psize))"
}

# p3_explain <label> <outfile>：把 ListTasks 的真实 SQL 计划落盘。
#
# 刻意用 task.go 里的 taskColumns **原文**：EXPLAIN 是否判定 COVERING INDEX
# 取决于 SELECT 的列清单，用 `SELECT *` 会得到另一个（更差的）计划，
# 那就不代表 /v1/tasks 实际跑的语句了。
p3_explain() {
	local label=$1 out=$2 db="$P3_DBROOT/$1/perf.db"
	local ns cols q
	ns=$(sed -n 's/^NS=//p' "$P3_DBROOT/$1/stamp.env")
	# 从 task.go 抠列清单（不要求产品代码提供 API；只读源码文本）
	cols=$(sed -n '/^const taskColumns = `/,/`$/p' "$P3_REPO/internal/store/task.go" |
		sed -e 's/^const taskColumns = `//' -e 's/`$//' | tr '\n' ' ' | sed -e 's/  */ /g' -e 's/^ //' -e 's/ $//')
	[ -n "$cols" ] || p3_die "抠不到 taskColumns（task.go 结构变了？）"
	q="SELECT $cols FROM tasks WHERE namespace_id = '$ns' ORDER BY created_at DESC, id DESC LIMIT 200"
	mkdir -p "$(dirname "$out")"
	{
		echo "# db=$db  schema=$(p3_db_version "$label")  index=$(p3_db_index_state "$label")"
		echo "# SQL: $q"
		sqlite3 "$db" "EXPLAIN QUERY PLAN $q;"
		echo "# 无过滤（真实请求会带 namespaceId + limit，这里是同一形状）"
		sqlite3 "$db" "EXPLAIN QUERY PLAN SELECT $cols FROM tasks WHERE namespace_id = '$ns' ORDER BY created_at DESC, id DESC LIMIT 200 OFFSET 0;"
		echo "# count(*) 形状（页面上限用）"
		sqlite3 "$db" "EXPLAIN QUERY PLAN SELECT count(*) FROM tasks WHERE namespace_id = '$ns';"
	} >"$out" 2>&1
	cat "$out" >&2
}

# ---------- server 生命周期 ----------

# p3_start_server <tag> <bin-name> <label> <port> [VAR=VAL ...]
#   bin-name: base | index | after（对应 $P3_BIN/anotify-<name>）
# 额外环境变量通过后面参数给（例如抬高 hook 限流的两条）。
p3_start_server() {
	local tag=$1 binname=$2 label=$3 port=$4
	shift 4
	p3_check_port "$port"
	local bin="$P3_BIN/anotify-$binname" rundir="$P3_RAW/$tag"
	mkdir -p "$rundir"
	: >"$rundir/server.log"
	# 分核：服务进程本身也要绑在 3A 的核上，否则 profile 里会混进别的 worker 的 CPU。
	taskset -c "$P3_CORES" env \
		ANOTIFY_ADDR=":$port" \
		ANOTIFY_DB="$P3_DBROOT/$label/perf.db" \
		ANOTIFY_PPROF=1 \
		ANOTIFY_LOG_LEVEL=warn \
		ANOTIFY_RP_ID=localhost \
		ANOTIFY_RP_ORIGIN="http://localhost:$port" \
		ANOTIFY_VAPID_PUBLIC_KEY=dummy \
		ANOTIFY_VAPID_PRIVATE_KEY=dummy \
		ANOTIFY_BOOT_ID="perf3a-$tag" \
		"$@" \
		"$bin" >>"$rundir/server.log" 2>&1 &
	local pid=$!
	echo "$pid" >"$rundir/server.pid"
	P3_PID=$pid
	P3_PORT=$port
	P3_BASE="http://127.0.0.1:$port"
	# 等健康：用 boot 做身份校验（#215 的教训：同一个端口上别人的服务也返回 200）。
	local deadline=$((SECONDS + 20)) boot
	while [ $SECONDS -lt $deadline ]; do
		boot=$(curl -s --max-time 1 "$P3_BASE/health" 2>/dev/null | jq -r '.boot // empty' 2>/dev/null)
		if [ "$boot" = "perf3a-$tag" ]; then
			p3_log "服务就绪 tag=$tag bin=$binname db=$label port=$port pid=$pid schema=$(p3_db_version "$label")"
			return 0
		fi
		if ! kill -0 "$pid" 2>/dev/null; then
			p3_log "服务启动即退出，日志尾部："
			tail -20 "$rundir/server.log" >&2
			return 1
		fi
		sleep 0.2
	done
	p3_log "20s 内 /health 未就绪（或端口 $port 被别人的服务占用），日志尾部："
	tail -20 "$rundir/server.log" >&2
	return 1
}

# p3_stop_server <tag>：收掉自己起的那个进程（SIGTERM → 等 5s → SIGKILL）。
# 只 kill 记录在 server.pid 里的 pid —— 本仓明确禁止 pkill -f（会误杀别人的服务）。
p3_stop_server() {
	local tag=$1 pidfile="$P3_RAW/$1/server.pid" pid
	[ -f "$pidfile" ] || return 0
	pid=$(cat "$pidfile")
	[ -n "$pid" ] || return 0
	kill "$pid" 2>/dev/null
	local i
	for i in $(seq 1 50); do
		kill -0 "$pid" 2>/dev/null || { rm -f "$pidfile"; return 0; }
		sleep 0.1
	done
	kill -9 "$pid" 2>/dev/null
	rm -f "$pidfile"
	return 0
}

# ---------- 压测 ----------

# p3_load <outfile> <loadgen args...>
# 统一在 taskset 下跑，输出 tee 到 raw/（长命令只留终端等于没留证据）。
p3_load() {
	local out=$1
	shift
	mkdir -p "$(dirname "$out")"
	taskset -c "$P3_CORES" "$P3_BIN/loadgen" "$@" 2>&1 | tee "$out"
}

# p3_dur_ms <"1.234ms"|"456µs"|"1.2s">：把 loadgen 的带单位延迟换成毫秒浮点。
# 表格里混着 µs/ms/s 没法比大小，统一换算后再进 summary.tsv。
p3_dur_ms() {
	local v=$1 num unit
	num=$(printf '%s' "$v" | sed 's/[^0-9.]*$//')
	unit=${v#"$num"}
	case "$unit" in
	ns) awk -v n="$num" 'BEGIN{printf "%.3f", n/1e6}' ;;
	µs | us) awk -v n="$num" 'BEGIN{printf "%.3f", n/1e3}' ;;
	ms) awk -v n="$num" 'BEGIN{printf "%.3f", n}' ;;
	s) awk -v n="$num" 'BEGIN{printf "%.1f", n*1000}' ;;
	*) printf '%s' "$v" ;;
	esac
}

# p3_parse_load <file>：把 loadgen 输出压成一行 TSV：n rps p50ms p95ms p99ms status
# 解析失败（比如服务没起来、全是连接拒绝）时返回 1，让调用方别把空数字写进报告。
p3_parse_load() {
	local f=$1 n rps p50 p95 p99 code_cnt
	[ -s "$f" ] || return 1
	n=$(awk '/^总请求/{print $2}' "$f")
	rps=$(awk '/^RPS/{print $2}' "$f")
	p50=$(p3_dur_ms "$(sed -n 's/.*p50=\([^ ]*\).*/\1/p' "$f" | head -1)")
	p95=$(p3_dur_ms "$(sed -n 's/.*p95=\([^ ]*\).*/\1/p' "$f" | head -1)")
	p99=$(p3_dur_ms "$(sed -n 's/.*p99=\([^ ]*\).*/\1/p' "$f" | head -1)")
	code_cnt=$(awk '/^状态码/{$1=""; sub(/^ /,""); print}' "$f" | head -1)
	[ -n "$n" ] && [ -n "$rps" ] || return 1
	printf '%s\t%s\t%s\t%s\t%s\t%s\n' "$n" "$rps" "$p50" "$p95" "$p99" "${code_cnt:-none}"
}

# p3_summary_row <tsv> <config> <scale> <binname> <label> <round> <loadfile> [备注]
# 所有场景共用的汇总口径：一行 = 一次测量，字段顺序固定，最终拼成 raw/summary.tsv。
p3_summary_row() {
	local tsv=$1 cfg=$2 scale=$3 binname=$4 label=$5 round=$6 loadfile=$7 note=${8:-}
	local parsed
	parsed=$(p3_parse_load "$loadfile") || { p3_log "解析失败：$loadfile"; parsed="NA	NA	NA	NA	NA	NA"; }
	printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' \
		"$cfg" "$scale" "$binname" "$(p3_bin_sha "$binname")" "$label" \
		"$(p3_db_version "$label")" "$round" "$parsed" "$note" "$loadfile" >>"$tsv"
}

p3_summary_header() {
	printf 'config\tscale\tbin\tbin_sha12\tdb\tseeded_schema\tround\tn\trps\tp50ms\tp95ms\tp99ms\tstatus\tnote\traw\n'
}

# ---------- pprof ----------

# p3_pprof_cpu <outfile> <seconds>：抓一份 CPU profile（调用方负责让它与负载重叠）。
p3_pprof_cpu() {
	local out=$1 secs=$2
	mkdir -p "$(dirname "$out")"
	curl -s -o "$out" "$P3_BASE/debug/pprof/profile?seconds=$secs"
	[ -s "$out" ] || { p3_log "pprof 抓取失败：$out"; return 1; }
	p3_log "pprof → $out（$(stat -c %s "$out") 字节）"
}

# p3_pprof_top <pprof> <outfile> [nodecount]：-top 原文落盘（报告只摘几行，原文要留档）。
p3_pprof_top() {
	local f=$1 out=$2 n=${3:-30}
	go tool pprof -top -nodecount="$n" "$f" >"$out" 2>&1
	tail -n +1 "$out" | head -n 12 >&2
}

# p3_pprof_total_secs <pprof>：profile 的 Total samples（秒，浮点）。
#
# CPU/请求 = 这个数 ÷ (RPS × 采样秒数)。pprof 的 header 形如
#   Duration: 20.17s, Total samples = 1234ms ( 6.12%)
# 单位可能是 ms 或 s，所以按单位换算；解不出就返回空（报告里标 NA，绝不猜）。
p3_pprof_total_secs() {
	local f=$1 raw num unit
	raw=$(go tool pprof -top -nodecount=1 "$f" 2>&1 | sed -n 's/.*Total samples = \([0-9.]*\)\(ms\|s\).*/\1 \2/p' | head -1)
	[ -n "$raw" ] || return 1
	num=${raw% *}
	unit=${raw#* }
	case "$unit" in
	ms) awk -v n="$num" 'BEGIN{printf "%.3f", n/1000}' ;;
	s) awk -v n="$num" 'BEGIN{printf "%.3f", n}' ;;
	*) return 1 ;;
	esac
}

# p3_pprof_find <pprof> <regex>：在 -top 里找某个符号的 flat/cum（报告用）。
p3_pprof_find() {
	local f=$1 pat=$2
	go tool pprof -top -nodecount=200 "$f" 2>/dev/null | grep -E "$pat" | head -5
}

# p3_pprof_list <pprof> <func> <outfile>：行级 -list 原文。
#
# -trimpath 构建的二进制里，profile 记录的是模块路径（modernc.org/libc@v.../...），
# pprof 找不到源码就只给「Total: ... Rows: n」的空壳。按 issue #242 的办法把
# 模块缓存与 GOROOT/src 加进 -source_path，行级数字才出得来。
p3_pprof_list() {
	local f=$1 fn=$2 out=$3
	go tool pprof \
		-source_path="$(go env GOMODCACHE):$(go env GOROOT)/src" \
		-list="$fn" "$f" >"$out" 2>&1
	head -40 "$out" >&2
}

# ---------- 数字小工具 ----------

# p3_median：中位数（偶数个取中间两个的均值，并在报告里披露这是中位数口径）。
p3_median() {
	printf '%s\n' "$@" | sort -n | awk '
		{n++; a[n]=$1}
		END{
			if (n==0) { print "NA"; exit }
			if (n%2) { printf "%.3f\n", a[(n+1)/2] }
			else { printf "%.3f\n", (a[n/2]+a[n/2+1])/2 }
		}'
}

# p3_spread：极差占中位数的百分比（方差披露：交替测量两轮时用它说明抖动有多大）。
p3_spread() {
	printf '%s\n' "$@" | sort -n | awk '
		{n++; a[n]=$1}
		END{
			if (n<2) { print "0.0"; exit }
			med = (n%2) ? a[(n+1)/2] : (a[n/2]+a[n/2+1])/2
			if (med==0) { print "0.0"; exit }
			printf "%.1f\n", (a[n]-a[1])/med*100
		}'
}
