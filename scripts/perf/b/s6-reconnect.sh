#!/usr/bin/env bash
# S6 · 断连重连 ×10 —— 查 goroutine / 会话 / fd 是否泄漏，以及断连期间的任务是否重复执行。
#
# **事实（别把这里读成「agent 会重连」）**：cmd/agent 没有重连逻辑 —— serveNode 一旦读到
# 连接错误就返回 `connection closed` 并让进程退出（cmd/agent/client.go，契约 §3.4.7 明确
# v1 不做退避重连，交给进程管理器重启）。所以本脚本里的「重连」= kill -9 进程后**用同一份
# node.yaml / 同一个 token** 重新拉起一个新进程；服务端侧 NodeHub.Register 会顶掉旧连接
# （internal/ws/node_hub.go：old.kill("superseded by a newer connection")）。
#
# 十轮循环，每轮：
#   触发 6 个短任务（P6）→ sleep 0.2 → kill -9 agent → 等 server 收尸（node offline）
#   → 用同一 yaml 重启 agent → 等握手 + online → 记 goroutine / RSS / server fd 数
#
# 收尾再等 40s + 「全部任务落地」轮询：让被杀的 agent 手上的任务走完
# 租约过期（30s）→ 退避（10s）→ 巡检重派 的完整路径，然后数 marker 里
# **同一 task_id 出现 >1 次**的任务数（重复执行 = 本脚本要抓的产品行为）。
#
# 用法：
#   bash scripts/perf/b/s6-reconnect.sh           # 端口 7924
#   PORT=7932 ROUNDS=10 KEEP=1 bash scripts/perf/b/s6-reconnect.sh   # 7926 是 s4 的端口，别撞
set -uo pipefail
. "$(dirname "$0")/lib.sh"

PORT=${PORT:-7924}
pb_init           # pb_init → pb_check_port（PORT 必须在 7920-7939）
pb_build
pb_start_server
pb_seed
pb_ns

ROUNDS=${ROUNDS:-10}        # 轮数（每轮 kill+重启一次）
PER_ROUND=${PER_ROUND:-6}   # 每轮触发的任务数
NAME=s6-node

MARK=$OUT/marks.txt
: >"$MARK"

PB_CAP_NAME=perf.reconnect
pb_cap "$PB_CAP_NAME"
pb_source perf-reconnect "$CAP"

# 短命令：执行 ~0.3s 就结束（marker 只用来查「同一 task 被执行了几次」）。
# $MARK 由外层 shell 展开；$ANOTIFY_TASK_ID / $EPOCHREALTIME 留到 agent 的 sh -c 里展开。
# shellcheck disable=SC2016
CMD='printf "%s %s\n" "$ANOTIFY_TASK_ID" "$EPOCHREALTIME" >> '"$MARK"'; sleep 0.3'

# ---------- 本地小工具（只读 lib 的行为，不改 lib） ----------

# s6_poll <desc> <secs> <cmd...>：带超时的轮询，**超时只返回 1 不退出**。
# 理由同 s5：pb_wait_until 超时会 pb_die → exit → 临时目录（含全部证据）被删。
s6_poll() {
	local desc=$1 secs=$2
	shift 2
	local deadline=$((SECONDS + secs))
	while [ $SECONDS -lt $deadline ]; do
		if "$@" >/dev/null 2>&1; then return 0; fi
		sleep 0.2
	done
	printf '!! TIMEOUT（%ss）等不到：%s\n' "$secs" "$desc"
	return 1
}

# s6_fd：server 进程当前的 fd 数（leak 的另一个面：连接/日志/DB 句柄）
# shellcheck disable=SC2012  # /proc 下的 fd 都是纯数字软链，不存在特殊字符问题
s6_fd() { ls "/proc/$PBPID/fd" 2>/dev/null | wc -l | tr -d ' '; }

# s6_node_online <name>：该名字的节点是否 online（pb_online 数的是「任意一台在线」，
# 单节点场景等价，但按名字查能顺带确认「重启后还是同一台机器」）
s6_node_online() {
	local st
	st=$(pb_api GET "/v1/nodes?namespaceId=$NS" |
		jq -r --arg n "$1" '(.nodes // [])[] | select(.name == $n) | .status' | head -1)
	[ "$st" = "online" ]
}

# s6_hello_count / s6_hello_gt：server.log 里的握手成功次数
# （每轮 kill 之前先记一次，用它确认「新起的进程真的被服务端接上了」）
s6_hello_count() {
	local n
	n=$(grep -cF '"node.ws.hello"' "$OUT/server.log" 2>/dev/null)
	echo "${n:-0}"
}
s6_hello_gt() { [ "$(s6_hello_count)" -gt "$1" ]; }

# s6_unsettled：非终态任务数（pending/assigned/acked/running/lease_expired）
s6_unsettled() {
	pb_api GET "/v1/tasks?namespaceId=$NS&limit=1000" |
		jq -r '[.tasks // [] | .[]
			| select(.status == "pending" or .status == "assigned" or .status == "acked"
				or .status == "running" or .status == "lease_expired")] | length'
}
s6_settled() { [ "$(s6_unsettled)" = "0" ]; }

# s6_offline：当前没有任何节点在线（kill -9 后 server 已收尸）
s6_offline() { [ "$(pb_online)" = "0" ]; }

# ============================================================
# 基线 + 节点
# ============================================================
pb_say "建节点 $NAME（instances=2 concurrency=1，命令 sleep 0.3）"
pb_node "$NAME" 2 1 "$CMD" 30
s6_poll "节点 online" 40 s6_node_online "$NAME" || pb_die "节点没连上（agent 日志见 $OUT/$NAME.log）"

g0=$(pb_gsnap s6-baseline)
rss0=$(pb_rss s6-baseline)
fd0=$(s6_fd)
on0=$(pb_online)
ROWS=$OUT/s6-rounds.txt
: >"$ROWS"
printf '%-6s %-10s %-10s %-5s %s\n' round goroutine 'RSS(KB)' fd online | tee -a "$ROWS"
printf '%-6s %-10s %-10s %-5s %s\n' 0 "$g0" "$rss0" "$fd0" "$on0" | tee -a "$ROWS"

# ============================================================
# 10 轮 kill + 重启
# ============================================================
pb_say "$ROUNDS 轮：每轮触发 $PER_ROUND 个任务（P$PER_ROUND）→ kill -9 → 同一 yaml 重启"
t_rounds0=$(pb_secs)
for r in $(seq 1 "$ROUNDS"); do
	pb_fire_many "$PER_ROUND" "$PER_ROUND"
	sleep 0.2

	old=$(pb_node_pid "$NAME")
	[ -n "$old" ] || pb_die "第 $r 轮拿不到 agent pid"
	kill -9 "$old" 2>/dev/null || true
	# 等 server 真的收尸（node offline）：保证旧连接的 cleanup（MarkInstancesDead）不会
	# 晚于新连接的 hello —— 否则槽位会被误标成 dead，重派就找不到容量。
	s6_poll "第 $r 轮旧连接被收尸（online=0）" 5 s6_offline || true

	# pb_node_start 会 >覆盖同名日志，先把这一轮的 agent 日志另存（证据）
	cp "$OUT/$NAME.log" "$OUT/$NAME.round$r.log" 2>/dev/null || true
	h0=$(s6_hello_count)
	pb_node_start "$NAME" >/dev/null
	s6_poll "第 $r 轮新 agent 握手（node.ws.hello 计数 > $h0）" 20 s6_hello_gt "$h0" || true
	pb_wait_online 1 20

	g=$(pb_gsnap "round-$r")
	rss=$(pb_rss "round-$r")
	fd=$(s6_fd)
	on=$(pb_online)
	printf '%-6s %-10s %-10s %-5s %s\n' "$r" "$g" "$rss" "$fd" "$on" | tee -a "$ROWS"
done
t_rounds=$(pb_sub "$(pb_secs)" "$t_rounds0")

# ============================================================
# 收尾：等租约过期 + 重派走完，再取最终快照
# ============================================================
pb_say '收尾：等 40s（租约 30s 过期 + 退避 10s）后轮询「全部任务落地」'
sleep 40
s6_poll "所有任务进终态（巡检重派走完）" 60 s6_settled || true

g1=$(pb_gsnap s6-final)
rss1=$(pb_rss s6-final)
fd1=$(s6_fd)
on1=$(pb_online)
pb_goroutine_dump s6-final

# ---------- 统计 ----------
EXEC_TOTAL=$(wc -l <"$MARK" 2>/dev/null | tr -d ' ')
EXEC_TOTAL=${EXEC_TOTAL:-0}
UNIQ=$(awk '{ if (!($1 in seen)) { seen[$1] = 1; n++ } } END { print n + 0 }' "$MARK")
DUP=$(awk '{ c[$1]++ } END { d = 0; for (k in c) if (c[k] > 1) d++; print d + 0 }' "$MARK")
STATUS_HIST=$(pb_tasks_dist)   # lib 的函数：一次拉 1000 条按状态分组
TASK_TOTAL=$(pb_api GET "/v1/tasks?namespaceId=$NS&limit=1000" | jq -r '(.tasks // []) | length')
HELLO_N=$(s6_hello_count)
RESULT_N=$(
	grep -cF '"node.ws.result"' "$OUT/server.log" 2>/dev/null || true
)
RECLAIM_N=$(
	grep -cF '"lease.sweeper.reclaimed"' "$OUT/server.log" 2>/dev/null || true
)
CLOSED_N=$(
	grep -cF '"node.ws.closed"' "$OUT/server.log" 2>/dev/null || true
)

# ============================================================
# 报告
# ============================================================
{
	echo "== S6 断连重连 ×$ROUNDS（instances=2 concurrency=1，每轮 $PER_ROUND 个短任务后 kill -9 → 同一 yaml 重启）=="
	printf '采集时间       %s\n' "$(date '+%F %T')"
	printf '轮次墙钟       %ss（%s 轮，平均 %ss/轮）\n' "$t_rounds" "$ROUNDS" \
		"$(awk -v t="$t_rounds" -v n="$ROUNDS" 'BEGIN{printf "%.2f", t/n}')"
	echo
	printf '%-6s %-10s %-10s %-5s %s\n' round goroutine 'RSS(KB)' fd online
	tail -n +2 "$ROWS"
	echo
	echo '--- 最终 goroutine 聚合（前 25 行，按 pprof 的排序）---'
	cat "$OUT/s6-final.goroutines.agg.txt" 2>/dev/null || true
	echo
	echo '--- 泄漏判读（baseline → final）---'
	printf 'goroutine  %s → %s（Δ%s）\n' "$g0" "$g1" "$((g1 - g0))"
	printf 'RSS(KB)    %s → %s（Δ%s）\n' "$rss0" "$rss1" "$((rss1 - rss0))"
	printf 'server fd  %s → %s（Δ%s）\n' "$fd0" "$fd1" "$((fd1 - fd0))"
	printf 'online     %s → %s\n' "$on0" "$on1"
	echo
	echo '--- 重复执行（marker 里同一 task_id 出现 >1 次）---'
	printf '触发任务 %s 个 / 库里任务 %s 个，marker 记录 %s 行，唯一 task %s 个，**重复执行 %s 个**\n' \
		"$((ROUNDS * PER_ROUND))" "$TASK_TOTAL" "$EXEC_TOTAL" "$UNIQ" "$DUP"
	echo '重复执行的任务（task_id x次数，最多 10 个）：'
	DUP_LIST=$(awk '{ c[$1]++ } END { for (k in c) if (c[k] > 1) printf "  %s x%d\n", k, c[k] }' "$MARK" | head -10)
	if [ -n "$DUP_LIST" ]; then
		printf '%s\n' "$DUP_LIST"
	else
		echo '  （无：每个 task 都只执行了一次）'
	fi
	echo
	echo '--- 任务状态分布（GET /v1/tasks）---'
	printf '%s\n' "${STATUS_HIST:-（空）}"
	echo
	echo '--- server.log 计数 ---'
	printf 'node.ws.hello %s 次（= 1 次初始 + %s 次重启）/ node.ws.closed %s 次 / node.ws.result %s 次 / lease.sweeper.reclaimed %s 次\n' \
		"$HELLO_N" "$ROUNDS" "$CLOSED_N" "$RESULT_N" "$RECLAIM_N"
	echo
	printf 'loadavg %s\n' "$(pb_loadavg)"
} 2>&1 | tee "$OUT/s6.txt"

pb_say "S6 完成 —— 表格与 goroutine 聚合见 $OUT/s6.txt"
