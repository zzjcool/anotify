#!/usr/bin/env bash
# C 路（资源/泄漏/前端）性能评估的公共底座。
#
# 只做「怎么把环境搭起来 + 怎么采样」，不含任何场景判断。之所以集中到一处：
# 5 个场景要跑在**同一台机器的同一批核**上，测量口径（端口段、分核、库路径、
# 采样命令）必须完全一致，否则数字之间没法比较。
#
# 用法：
#   source scripts/perf/c/common.sh
#   pc_require_env                      # 检查端口段/分核/依赖
#   pc_build                            # 构建带 embeddist 的服务二进制（幂等）
#   pc_start soak 7940                  # 起服务，等 /health，返回 base
#   pc_seed "$PC_DB"                    # devseed 出 SESSION/SEND_KEY
#   ...
#   pc_stop soak
#
# 纪律（issue #217）：
#   * 分核：一切压测/采样命令都要在 `taskset -c $PC_CPUS` 下跑（默认 8-11）
#   * 端口：只用 7940-7959（本文件会拒绝段外端口）
#   * 库：每个场景各自 `mktemp -d`
#
# 本文件不改产品代码；所有运行期产物都落在 $PC_ROOT（默认 /tmp/perf-c）。

set -uo pipefail

PC_CPUS="${PC_CPUS:-8-11}"
PC_PORT_LO=7940
PC_PORT_HI=7959
PC_ROOT="${PC_ROOT:-/tmp/perf-c}"
PC_REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
PC_BIN="$PC_ROOT/bin/anotify"
PC_DEVSEED="$PC_ROOT/bin/devseed"

# pc_require_env：早期把「纪律」变成硬失败。
# 这里宁可炸也不静默降级 —— 抢核/抢端口会污染另外两路的数字，而且事后
# 从日志里看不出来（只会看到"今天有点慢"）。
pc_require_env() {
	command -v taskset >/dev/null || { echo "pc_require_env: 缺少 taskset" >&2; return 1; }
	command -v curl >/dev/null || { echo "pc_require_env: 缺少 curl" >&2; return 1; }
	command -v jq >/dev/null || { echo "pc_require_env: 缺少 jq" >&2; return 1; }
	if ! echo "$PC_CPUS" | grep -Eq '^[0-9]+(-[0-9]+)?(,[0-9]+(-[0-9]+)?)*$'; then
		echo "pc_require_env: PC_CPUS 非法：$PC_CPUS" >&2
		return 1
	fi
	mkdir -p "$PC_ROOT/bin" "$PC_ROOT/logs" "$PC_ROOT/runs"
}

# pc_port_ok <port>：段外端口一律拒绝（issue #217 的约定表）。
pc_port_ok() {
	local p="$1"
	if ! [[ "$p" =~ ^[0-9]+$ ]] || [ "$p" -lt "$PC_PORT_LO" ] || [ "$p" -gt "$PC_PORT_HI" ]; then
		echo "pc_port_ok: 端口 $p 不在 C 路段 $PC_PORT_LO-$PC_PORT_HI 内（会跟 A/B 路抢端口）" >&2
		return 1
	fi
}

# pc_build：构建带前端 embed 的服务（静态资源场景要指纹文件名 → 必须 embeddist）。
# 先跑 `make fe` 生成 internal/server/dist；它 1 秒内完成且不会弄脏工作区。
pc_build() {
	mkdir -p "$PC_ROOT/bin"
	if [ ! -x "$PC_BIN" ] || [ -n "${PC_FORCE_BUILD:-}" ]; then
		( cd "$PC_REPO" && make fe >"$PC_ROOT/logs/fe.log" 2>&1 ) || {
			echo "pc_build: make fe 失败，见 $PC_ROOT/logs/fe.log" >&2
			return 1
		}
		( cd "$PC_REPO" && go build -tags embeddist -o "$PC_BIN" ./cmd/server ) || return 1
		( cd "$PC_REPO" && go build -o "$PC_DEVSEED" ./cmd/devseed ) || return 1
	fi
	return 0
}

# pc_start <name> <port> [VAR=VAL ...]
# 起一个独立服务实例（独立 DB 目录），等 /health 通过后打印 base。
# 额外环境变量通过后面参数给（例如 ANOTIFY_STATIC=...）。
pc_start() {
	local name="$1" port="$2"; shift 2
	pc_port_ok "$port" || return 1
	local dbdir="$PC_ROOT/runs/$name"
	rm -rf "$dbdir"; mkdir -p "$dbdir"
	export PC_DB="$dbdir/perf.db"
	export PC_RUN="$dbdir"
	# 分核：服务进程本身也要绑在 C 路核上，否则 profile 里会混进别人的 CPU。
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
		"$@" \
		"$PC_BIN" >"$dbdir/server.log" 2>&1 &
	local pid=$!
	echo "$pid" >"$dbdir/server.pid"
	export PC_PID="$pid"
	export PC_PORT="$port"
	export PC_BASE="http://localhost:$port"
	# 等健康：用 boot 做身份校验（#215 的教训：同一个端口上别人的服务也返回 200）。
	local deadline=$((SECONDS + 15))
	while [ $SECONDS -lt $deadline ]; do
		local boot
		boot=$(curl -s --max-time 1 "$PC_BASE/health" 2>/dev/null | jq -r '.boot // empty' 2>/dev/null)
		if [ "$boot" = "perf-c-$name" ]; then
			echo "$PC_BASE"
			return 0
		fi
		if ! kill -0 "$pid" 2>/dev/null; then
			echo "pc_start: 服务启动即退出，日志：" >&2
			tail -20 "$dbdir/server.log" >&2
			return 1
		fi
		sleep 0.2
	done
	echo "pc_start: 15s 内 /health 未就绪（或端口 $port 被别人的服务占用），日志：" >&2
	tail -20 "$dbdir/server.log" >&2
	return 1
}

# pc_stop：收掉 pc_start 起的进程（SIGTERM → 等 5s → SIGKILL）。
pc_stop() {
	local pid="${PC_PID:-}"
	[ -z "$pid" ] && { [ -f "${PC_RUN:-}/server.pid" ] && pid=$(cat "$PC_RUN/server.pid"); }
	[ -z "$pid" ] && return 0
	kill "$pid" 2>/dev/null
	for _ in $(seq 1 50); do
		kill -0 "$pid" 2>/dev/null || return 0
		sleep 0.1
	done
	kill -9 "$pid" 2>/dev/null
	return 0
}

# pc_seed [username]：devseed 造用户 + API Key + 会话，导出 PC_SESSION/PC_SEND_KEY/PC_RECV_KEY/PC_NS。
# 注意 PC_NS 需要服务已在跑（走 /v1/namespaces 解析默认命名空间）。
pc_seed() {
	local user="${1:-perfc}"
	local out
	out=$("$PC_DEVSEED" -db "$PC_DB" -username "$user" 2>&1) || { echo "$out" >&2; return 1; }
	PC_SESSION=$(echo "$out" | sed -n 's/^SESSION=//p')
	PC_SEND_KEY=$(echo "$out" | sed -n 's/^SEND_KEY=//p')
	PC_RECV_KEY=$(echo "$out" | sed -n 's/^RECV_KEY=//p')
	PC_UID=$(echo "$out" | sed -n 's/^UID=//p')
	export PC_SESSION PC_SEND_KEY PC_RECV_KEY PC_UID
	# 默认命名空间：走契约端点，不查库（库是 store 的私有实现）。
	PC_NS=$(curl -s -H "Cookie: anotify_session=$PC_SESSION" "$PC_BASE/v1/namespaces" \
		| jq -r '(.namespaces // [])[0].id // empty')
	export PC_NS
	if [ -z "$PC_NS" ]; then
		echo "pc_seed: 取不到默认命名空间（会话是否有效？）" >&2
		return 1
	fi
	return 0
}

# pc_cap <name>：建一个能力，打印 capability id。
pc_cap() {
	local name="$1" schema="${2:-{\"type\":\"object\"}}"
	curl -s -X POST "$PC_BASE/v1/capabilities" \
		-H "Cookie: anotify_session=$PC_SESSION" -H 'content-type: application/json' \
		-d "{\"namespaceId\":\"$PC_NS\",\"name\":\"$name\",\"paramsSchema\":$schema}" \
		| jq -r '.capability.id // empty'
}

# pc_source <name> <capId...>：建 webhook 源，指向给定出口能力，打印 hook token。
pc_source() {
	local name="$1"; shift
	local targets='[]'
	local cap
	targets="["
	local first=1
	for cap in "$@"; do
		[ $first -eq 1 ] || targets+=","
		targets+="{\"capabilityId\":\"$cap\"}"
		first=0
	done
	targets+="]"
	curl -s -X POST "$PC_BASE/v1/sources" \
		-H "Cookie: anotify_session=$PC_SESSION" -H 'content-type: application/json' \
		-d "{\"name\":\"$name\",\"kind\":\"webhook\",\"namespaceId\":\"$PC_NS\",\"config\":{},\"targets\":$targets}" \
		| jq -r '.source.hookUrl // empty' | sed 's#^/v1/hooks/##'
}

# pc_hook <token> <json-body>：投递一条事件，打印 http 码。
pc_hook() {
	local token="$1" body="$2"
	curl -s -o /dev/null -w '%{http_code}' -X POST "$PC_BASE/v1/hooks/$token" \
		-H 'content-type: application/json' -d "$body"
}

# pc_sample <label>：采一行资源快照（CSV，带表头由调用方打印）。
#   字段：label,elapsed_s,rss_kb,goroutines,fds,db_bytes,wal_bytes
# goroutine 数取自 /debug/pprof/goroutine?debug=1 的 "total N" 行（pprof 自带口径），
# 比数线程/栈更稳；fd 数数 /proc/<pid>/fd 的条目数（Linux 上含 socket + 库文件）。
pc_sample() {
	local label="${1:-sample}"
	local pid="${PC_PID:?pc_sample 需要 PC_PID（先 pc_start）}"
	local rss g fds db wal
	rss=$(ps -o rss= -p "$pid" 2>/dev/null | tr -d ' ')
	g=$(curl -s --max-time 3 "$PC_BASE/debug/pprof/goroutine?debug=1" 2>/dev/null \
		| sed -n 's/^goroutine profile: total //p' | head -1)
	fds=$(ls "/proc/$pid/fd" 2>/dev/null | wc -l | tr -d ' ')
	db=0; wal=0
	[ -f "$PC_DB" ] && db=$(stat -c %s "$PC_DB")
	[ -f "$PC_DB-wal" ] && wal=$(stat -c %s "$PC_DB-wal")
	echo "$label,${PC_ELAPSED:-0},${rss:-0},${g:-0},${fds:-0},$db,$wal"
}

# pc_sample_header：CSV 表头。
pc_sample_header() { echo "label,elapsed_s,rss_kb,goroutines,fds,db_bytes,wal_bytes"; }

# pc_pprof <name> <kind>：抓一份 profile 到 $PC_RUN/<name>.<kind>.pprof。
# kind: heap | goroutine | mutex | cpu:<seconds>
pc_pprof() {
	local name="$1" kind="$2"
	case "$kind" in
	cpu:*)
		local secs="${kind#cpu:}"
		curl -s -o "$PC_RUN/$name.cpu.pprof" "$PC_BASE/debug/pprof/profile?seconds=$secs"
		;;
	*)
		curl -s -o "$PC_RUN/$name.$kind.pprof" "$PC_BASE/debug/pprof/$kind"
		;;
	esac
	echo "$PC_RUN/$name.${kind//:/-}.pprof"
}

# pc_pprof_top <file>：给 issue 用的 -top 摘要（前 15 行）。
pc_pprof_top() {
	local f="$1" n="${2:-15}"
	go tool pprof -top -nodecount="$n" "$f" 2>&1 | head -"$((n + 6))"
}
