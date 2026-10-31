#!/usr/bin/env bash
# 批次 3B（#223 / #243）性能测量 —— 公共库。
#
# 纪律（与 scripts/perf/b 一致，别绕开）：
#   - server 与 loadgen 都绑 4-7 核（taskset），端口只用 8040-8059
#   - 库用 mktemp -d，跑完删
#   - 前后对比必须**同场次交替跑**：base → new → (需要时) base，
#     并把 /proc/loadavg 一并记下来（本机常年有别的路在压，绝对数字会漂）
#
# 用法：
#   bash scripts/perf/3b/nodes.sh  <server 二进制> <tag>     # #223，GET /v1/nodes
#   bash scripts/perf/3b/events.sh <server 二进制> <tag>     # #243，GET /v1/console/events
#
# 基线二进制怎么来（对比用）：
#   git stash / 另建 worktree 到本 PR 的 base 提交，然后
#   go build -o /tmp/p3b/bin/anotify-base ./cmd/server
set -uo pipefail

P3B_CORES=${CORES:-4-7}
P3B_BIN=${PB_BIN:-/tmp/p3b/bin}
P3B_ROOT=$(git -C "$(dirname "${BASH_SOURCE[0]}")" rev-parse --show-toplevel 2>/dev/null) \
	|| P3B_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)
P3B_OUT=${OUT:-/tmp/p3b/logs}

p3b_say() { printf '\n\033[1;36m▶ %s\033[0m\n' "$*" >&2; }
p3b_log() { printf '[%s] %s\n' "$(date +%H:%M:%S)" "$*" >&2; }
p3b_die() { printf '\n\033[1;31m✗ %s\033[0m\n' "$*" >&2; exit 1; }

# p3b_port <port>：只允许 8040-8059（与另两路 0-3 / 8-11 核的端口段错开）
p3b_port() {
	local p=$1
	[ "$p" -ge 8040 ] && [ "$p" -le 8059 ] || p3b_die "端口 $p 不在本路允许区间 8040-8059"
}

# p3b_ensure_bins：只构建辅助二进制（devseed/agent/loadgen）；server 由调用方传。
# 存在即复用（换 commit 复现时请 PB_REBUILD=1 或 rm -rf $P3B_BIN）。
p3b_ensure_bins() {
	mkdir -p "$P3B_BIN" "$P3B_OUT"
	local need=0 b
	for b in devseed agent loadgen; do [ -x "$P3B_BIN/$b" ] || need=1; done
	[ "${PB_REBUILD:-0}" = "1" ] && need=1
	[ "$need" = "0" ] && return 0
	p3b_say "构建 devseed/agent/loadgen → $P3B_BIN"
	(
		cd "$P3B_ROOT" || exit 1
		export GOPROXY=${GOPROXY:-direct} GOSUMDB=${GOSUMDB:-sum.golang.org} GOTOOLCHAIN=${GOTOOLCHAIN:-auto}
		taskset -c "$P3B_CORES" go build -o "$P3B_BIN/devseed" ./cmd/devseed || exit 1
		taskset -c "$P3B_CORES" go build -o "$P3B_BIN/agent" ./cmd/agent || exit 1
		taskset -c "$P3B_CORES" go build -o "$P3B_BIN/loadgen" ./scripts/perf/loadgen || exit 1
	) || p3b_die "辅助二进制构建失败"
}

# p3b_cputime <pid>：进程累计 CPU 秒（utime+stime，clock ticks=100/秒）
p3b_cputime() { awk '{printf "%.2f", ($14+$15)/100}' "/proc/$1/stat" 2>/dev/null; }

p3b_wait_http() { # $1=base
	local base=$1 i
	for i in $(seq 1 200); do curl -sf -o /dev/null "$base/health" && return 0; sleep 0.1; done
	return 1
}

# p3b_start_server <bin> <port> <db> <log>：绑核心 + pprof + json 日志，pid 打到 stdout
p3b_start_server() {
	local bin=$1 port=$2 db=$3 log=$4 base="http://127.0.0.1:$2"
	taskset -c "$P3B_CORES" env \
		ANOTIFY_ADDR=":$port" ANOTIFY_DB="$db" ANOTIFY_STATIC= ANOTIFY_PPROF=1 \
		ANOTIFY_LOG_LEVEL=warn ANOTIFY_LOG_FORMAT=json ANOTIFY_RP_ORIGIN="$base" \
		"$bin" >"$log" 2>&1 &
	local pid=$!
	p3b_wait_http "$base" || { tail -20 "$log" >&2; p3b_die "server 起不来（$bin :$port）"; }
	echo "$pid"
}

# p3b_seed：devseed 造用户/会话/API Key，导出 SESSION / SEND_KEY / NS
p3b_seed() { # $1=db
	local seed
	seed=$(taskset -c "$P3B_CORES" "$P3B_BIN/devseed" -db "$1" -username perf 2>&1) || p3b_die "devseed 失败"
	SESSION=$(printf '%s\n' "$seed" | sed -n 's/^SESSION=//p')
	SEND_KEY=$(printf '%s\n' "$seed" | sed -n 's/^SEND_KEY=//p')
	NS=$(printf '%s\n' "$seed" | grep -o 'ns_[A-Za-z0-9]*' | head -1)
	[ -n "$SESSION" ] && [ -n "$NS" ] || p3b_die "devseed 输出异常：$seed"
	export SESSION SEND_KEY NS
	p3b_log "ns=$NS session=${SESSION:0:10}…"
}

# p3b_load <url> <cookie> <accept-encoding> <conc> <dur> <outfile>
#   → 打印 "请求 RPS p50 cpu/千请求" 一行，并追加到 $REPORT
#   accept-encoding=identity 时 Go transport 不会自动加 gzip（等价「没有 gzip」的旧行为）；
#   gzip 时走浏览器/undici 的真实路径。
p3b_load() {
	local url=$1 cookie=$2 ae=$3 conc=$4 dur=$5 out=$6
	local reqs rps p50 c0 c1 cpu per1k
	c0=$(p3b_cputime "$SRVPID")
	taskset -c "$P3B_CORES" "$P3B_BIN/loadgen" -url "$url" -header "$cookie" \
		-header "Accept-Encoding: $ae" -c "$conc" -d "$dur" >"$out" 2>&1
	c1=$(p3b_cputime "$SRVPID")
	reqs=$(awk '/^总请求/{print $2}' "$out")
	rps=$(awk '/^RPS/{print $2}' "$out")
	p50=$(sed -n 's/.*p50=\([0-9.]*ms\).*/\1/p' "$out")
	cpu=$(awk -v a="$c0" -v b="$c1" 'BEGIN{printf "%.2f", b-a}')
	per1k=$(awk -v c="$cpu" -v r="$reqs" 'BEGIN{ if (r>0) printf "%.2f", c/r*1000; else print "NA"}')
	printf '%-18s 请求 %-6s RPS %-8s p50 %-10s server CPU %ss  CPU/千请求 %ss\n' \
		"$ae-c$conc" "${reqs:-?}" "${rps:-?}" "${p50:-?}" "$cpu" "$per1k"
}
