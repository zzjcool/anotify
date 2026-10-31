#!/usr/bin/env bash
# S5 · 慢节点与租约重派 —— 重点验「同一个任务是否被重复执行」。
#
# 两个子实验，各自独立的**能力 + 源 + 节点**（互不抢派发，任务只会落到自己的节点上）：
#
#   5a 有超时保护（预期**不**重复）
#      command = `sleep 60`，timeoutSec = 5（→ internal/store/task.go:LeaseTTLSec=30 的租约
#      根本来不及过期）。预期：agent 在 5s 时掐掉命令**进程组**，回报
#      failed + error="timed out after 5s"；任务**不**被重派、不重复执行。
#
#   5b 心跳饥饿（预期**重复执行** —— 这是要抓的产品行为）
#      command = marker + `sleep 60`，timeoutSec = 200（远大于租约 30s）。
#      链路（常量见 internal/store/task.go 20-60 行）：
#        t≈0     派发 → ack（租约 = now+30）
#        t≈2     kill -STOP agent（心跳停摆，但它的 sleep 60 孙进程还活着）
#        t≈20-25 server watchdog：>2×心跳(10s)=20s 无上行帧 → 关连接（node.ws.timeout）
#        t≈30-35 租约过期 → lease.sweeper 回收回 pending，退避 RetryBackoff(1)=10s
#        t≈45    用**同一份 node.yaml / 同一 token** 起第二个 agent（顶掉旧连接）
#        t≈45-55 巡检重派 → 第二次执行 → 同一 task_id 在 marker 里出现 2 行，
#                同一时刻有 **2 个 sleep 60 进程**（重复执行的铁证）
#
# 用法：
#   bash scripts/perf/b/s5-lease-dup.sh          # 端口 7923
#   PORT=7930 KEEP=1 bash scripts/perf/b/s5-lease-dup.sh   # 7925 是 s3 的端口，别撞
#
# 纪律：本脚本**只产出证据**，不做「必须通过」的硬断言——与预期不符的行以 `!!` 开头，
#       由 human 复核后写进 REPORT（发现产品 bug 不改断言迁就）。
#
# 5a 的 marker 说明：5a 的 command 冻结为 `sleep 60`（不写 marker），所以「该 task 执行了几次」
# 用 agent 日志里的 agent.task.finished 计数（等价证据），marker 行数那一条只在 5b 有。
set -uo pipefail
. "$(dirname "$0")/lib.sh"

PORT=${PORT:-7923}
pb_init           # pb_init → pb_check_port（PORT 必须在 7920-7939）
pb_build
pb_start_server
pb_seed
pb_ns

MARK=$OUT/marks.txt
: >"$MARK"
CLIENT_FILE=$OUT/client.txt   # 本脚本不用延迟分布，但保持与其它场景脚本一致
: >"$CLIENT_FILE"

# ---------- 本地小工具（只读 lib 的行为，不改 lib） ----------

# s5_poll <desc> <secs> <cmd...>：带超时的轮询，**超时只返回 1 不退出**。
# 为什么不用 pb_wait_until：它超时走 pb_die → exit → trap pb_cleanup 删掉 $WORK，
# 全部证据一起消失。而「等不到」对压测结论本身就是证据，必须落盘。
s5_poll() {
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

# s5_task <taskId>：任务行的紧凑 JSON（空 = 该任务还没出现在列表里）
s5_task() {
	pb_api GET "/v1/tasks?namespaceId=$NS&limit=20" |
		jq -c --arg t "$1" '(.tasks // [])[] | select(.id == $t)'
}

# s5_field <taskId> <字段名>
s5_field() { s5_task "$1" | jq -r ".$2 // \"\""; }

# s5_terminal <taskId>：是否已进终态（lease_expired 是**中间态**，不算落地）
s5_terminal() {
	local st
	st=$(s5_field "$1" status)
	case "$st" in
	failed | succeeded | canceled | abandoned) return 0 ;;
	*) return 1 ;;
	esac
}

# s5_log_count <file> <event> <taskId>：日志里同时含该 event 与该 task_id 的行数
s5_log_count() {
	local n
	n=$(grep -F "\"$2\"" "$1" 2>/dev/null | grep -cF -- "$3")
	echo "${n:-0}"
}

# s5_log_has <file> <event> <taskId>：日志里是否已有「该 event + 该 task_id」的行
s5_log_has() { grep -F "\"$2\"" "$1" 2>/dev/null | grep -qF -- "$3"; }

# s5_agent_count <file> <event> <taskId>：**agent 侧**日志的行数。
# 注意两边日志格式不同：server 是 JSON（pb_start_server 传了 ANOTIFY_LOG_FORMAT=json，能 jq），
# agent 是 slog 的**文本** handler（cmd/agent/main.go:newLogger → NewTextHandler），
# 形如 `time=… level=INFO msg="task finished" event=agent.task.finished task_id=tsk_x …`，
# 所以这里用 event=/task_id= 两个键去数，不能拿 jq 解。
s5_agent_count() {
	local n
	n=$(grep -F "event=$2" "$1" 2>/dev/null | grep -cE "(^| )task_id=$3( |$)")
	echo "${n:-0}"
}

# s5_agent_lines <file> <event> <taskId>：原样打出那几行（文本格式，不能 jq）
s5_agent_lines() {
	grep -F "event=$2" "$1" 2>/dev/null | grep -E "(^| )task_id=$3( |$)" | head -5
}

# s5_mark_count <taskId>：agent 侧 marker 里该 task 的行数（>1 = 重复执行）
s5_mark_count() {
	local n
	n=$(grep -cF -- "$1" "$MARK" 2>/dev/null)
	echo "${n:-0}"
}
# s5_mark_ge <taskId> <n>：marker 里该 task 至少出现 n 行
s5_mark_ge() { [ "$(s5_mark_count "$1")" -ge "$2" ]; }

# s5_node_online <name>：该**名字**的节点是否 online（比 pb_online 的「任意一台在线」精确：
# 本脚本有两个节点，必须确认是 5b 那台连上了）
s5_node_online() {
	local st
	st=$(pb_api GET "/v1/nodes?namespaceId=$NS" |
		jq -r --arg n "$1" '(.nodes // [])[] | select(.name == $n) | .status' | head -1)
	[ "$st" = "online" ]
}

# s5_hello_count / s5_hello_gt：server.log 里握手成功次数（确认「第二个 agent 真的连上了」）
s5_hello_count() {
	local n
	n=$(grep -cF '"node.ws.hello"' "$OUT/server.log" 2>/dev/null)
	echo "${n:-0}"
}
s5_hello_gt() { [ "$(s5_hello_count)" -gt "$1" ]; }

# s5_sleeps / s5_sleep_count：当前活着的、cmdline **恰好**是 `sleep 60` 的进程。
# 用 ^...$ 精确匹配：`sh -c 'printf …; sleep 60'` 的父 sh 的 cmdline 不同，不会被重复计数
# （否则每次执行会被算成 2 个进程，2 次执行看起来像 4 个）。
s5_sleeps() { pgrep -af '^sleep 60$' 2>/dev/null; }
s5_sleep_count() {
	local n
	n=$(pgrep -cf '^sleep 60$' 2>/dev/null)
	echo "${n:-0}"
}

# s5_backoff_done <taskId>：租约回收时设的 not_before 是否已到期（没设 = 立刻算到期）
s5_backoff_done() {
	local nb now
	nb=$(s5_field "$1" notBefore)
	case "$nb" in '' | null | *[!0-9]*) return 0 ;; esac
	now=$(date +%s)
	[ "$nb" -le "$now" ]
}

# ============================================================
# 5a · 有超时保护（预期不重复执行）
# ============================================================
pb_say "5a：超时保护下的慢命令（sleep 60 + timeoutSec=5 < 租约 30s，预期不重复执行）"

PB_CAP_NAME=perf.slow.timeout
pb_cap "$PB_CAP_NAME"
pb_source perf-slow-timeout "$CAP"

NAME5A=s5a-slow
g_a0=$(pb_gsnap s5a-before)
rss_a0=$(pb_rss s5a-before)
pb_node "$NAME5A" 1 1 'sleep 60' 5
s5_poll "5a 节点 online" 40 s5_node_online "$NAME5A" || pb_die "5a 节点没连上（agent 日志见 $OUT/$NAME5A.log）"

t_fire_a=$(pb_secs)
TID_A=$(pb_fire "$SRC" '{"i":"5a"}' | head -1)
[ -n "$TID_A" ] || pb_die "5a 触发没拿到 task id"
pb_log "5a task=$TID_A（timeoutSec=5，命令 sleep 60）"

s5_poll "5a 任务进终态（timeoutSec=5 → 应在 10s 内）" 40 s5_terminal "$TID_A" || true
t_done_a=$(pb_secs)
elapsed_a=$(pb_sub "$t_done_a" "$t_fire_a")
g_a1=$(pb_gsnap s5a-after)
rss_a1=$(pb_rss s5a-after)

{
	echo "== S5a 超时保护：command='sleep 60' + timeoutSec=5 + 租约 30s（预期：不重复执行）=="
	printf '采集时间             %s\n' "$(date '+%F %T')"
	printf 'task                 %s\n' "$TID_A"
	printf '节点 / agent pid     %s / %s\n' "$NAME5A" "$(pb_node_pid "$NAME5A")"
	printf '墙钟 fire→终态       %ss\n' "$elapsed_a"
	printf 'status / error       %s / %s\n' "$(s5_field "$TID_A" status)" "$(s5_field "$TID_A" error)"
	printf 'attempts             %s（期望 1）\n' "$(s5_field "$TID_A" attempts)"
	printf 'leaseUntil/notBefore %s / %s\n' "$(s5_field "$TID_A" leaseUntil)" "$(s5_field "$TID_A" notBefore)"
	echo '--- 任务行（原文 jq）---'
	s5_task "$TID_A" |
		jq '{id,status,attempts,error,nodeId,leaseUntil,notBefore,createdAt,startedAt,finishedAt}' 2>/dev/null || true
	echo '--- server.log 证据 ---'
	printf 'node.ws.result 行数            %s（期望 1）\n' \
		"$(s5_log_count "$OUT/server.log" node.ws.result "$TID_A")"
	printf 'lease.sweeper.reclaimed 行数   %s（期望 0：5s 就失败，租约没机会过期）\n' \
		"$(s5_log_count "$OUT/server.log" lease.sweeper.reclaimed "$TID_A")"
	echo '--- server.log 该 task 的 node.ws.result（jq 出来）---'
	grep -F '"node.ws.result"' "$OUT/server.log" | grep -F -- "$TID_A" |
		jq -c '{time,task_id,status,node_id,reported,msg}' 2>/dev/null || true
	echo '--- agent 侧执行次数（5a 的 command 冻结为 sleep 60、不写 marker → 用 agent 日志计数）---'
	printf 'agent.task.finished 行数       %s（期望 1）\n' \
		"$(s5_agent_count "$OUT/$NAME5A.log" agent.task.finished "$TID_A")"
	s5_agent_lines "$OUT/$NAME5A.log" agent.task.finished "$TID_A"
	echo '--- timeout 文案（agent 文本日志）---'
	printf "grep -c 'timed out after'      %s（期望 ≥1）\n" \
		"$(grep -cF 'timed out after' "$OUT/$NAME5A.log" 2>/dev/null || true)"
	grep -F 'timed out after' "$OUT/$NAME5A.log" 2>/dev/null | head -3 || true
	echo '--- 命令进程是否真的被杀（期望空）---'
	printf "pgrep -af '^sleep 60$' →\n"
	s5_sleeps || echo '  （空：sleep 60 已被杀干净）'
	echo '--- goroutine / RSS / load ---'
	printf 'goroutine %s → %s\n' "$g_a0" "$g_a1"
	printf 'RSS(KB)   %s → %s\n' "$rss_a0" "$rss_a1"
	printf 'loadavg   %s\n' "$(pb_loadavg)"
} 2>&1 | tee "$OUT/s5a.txt"

# ============================================================
# 5b · 心跳饥饿 → 租约过期 → 重派 → 重复执行
# ============================================================
pb_say "5b：心跳饥饿 + 租约重派（sleep 60 + timeoutSec=200 > 租约 30s，预期重复执行）"

PB_CAP_NAME=perf.slow.lease
pb_cap "$PB_CAP_NAME"
pb_source perf-lease-5b "$CAP"

NAME5B=s5b-lease
# 注意 $MARK 由**外层** shell 展开成绝对路径（单引号断开），$ANOTIFY_TASK_ID /
# $EPOCHREALTIME 留到 agent 的 sh -c 里才展开（与 s1 同写法）。
# shellcheck disable=SC2016
CMD5B='printf "%s %s\n" "$ANOTIFY_TASK_ID" "start $EPOCHREALTIME" >> '"$MARK"'; sleep 60'

g_b0=$(pb_gsnap s5b-before)
rss_b0=$(pb_rss s5b-before)
pb_node "$NAME5B" 1 1 "$CMD5B" 200
s5_poll "5b 节点 online" 40 s5_node_online "$NAME5B" || pb_die "5b 节点没连上（agent 日志见 $OUT/$NAME5B.log）"

h_hello0=$(s5_hello_count)
t_fire_b=$(pb_secs)
TID_B=$(pb_fire "$SRC" '{"i":"5b"}' | head -1)
[ -n "$TID_B" ] || pb_die "5b 触发没拿到 task id"
pb_log "5b task=$TID_B（timeoutSec=200，命令 marker+sleep 60）"

# 计划里写的是「等 ~2s（确认 marker 有 1 行）→ kill -STOP」。这里把那个固定 2s 换成
# **等 marker 出现**（通常 ~0.3s）：marker 是命令真的跑起来的证据（agent 是先 ack 再 exec，
# 所以 marker 存在 ⇒ ack/租约已在服务端落定），而且早一点 STOP 能让「第一个 sleep 60」
# 活得更久 → 与第二次执行的并发窗口更大（重复执行的两进程证据更好抓）。
s5_poll "5b marker 出现第 1 行（命令真的跑起来了）" 20 s5_mark_ge "$TID_B" 1 ||
	pb_log '5b 警告：marker 20s 内没出现，继续观察（可能派发更慢）'
mark_b1=$(s5_mark_count "$TID_B")
sleep_b1=$(s5_sleep_count)

OLD_PID=$(pb_node_pid "$NAME5B")
[ -n "$OLD_PID" ] || pb_die "5b 拿不到 agent pid"
pb_log "5b agent1 pid=$OLD_PID marker=$mark_b1 个 sleep60 进程=$sleep_b1 → kill -STOP（模拟心跳饥饿）"
kill -STOP "$OLD_PID" || pb_die "5b kill -STOP 失败"
t_stop=$(pb_secs)

# ① server watchdog：> 2×心跳(10s) = 20s 无上行帧 → 关连接
s5_poll "server 判死（server.log 出现 node.ws.timeout，约 20s）" 45 \
	grep -qF '"node.ws.timeout"' "$OUT/server.log" || true
# ② 租约 30s 过期 → 巡检（5s 一轮）回收回 pending，退避 RetryBackoff(1) = 10s
s5_poll "租约回收（lease.sweeper.reclaimed 里出现该 task）" 70 \
	s5_log_has "$OUT/server.log" lease.sweeper.reclaimed "$TID_B" || true
t_reclaim=$(pb_secs)
s5_poll "退避到期（not_before ≤ now）" 25 s5_backoff_done "$TID_B" || true

# ③ 第二个 agent：**同一份 node.yaml / 同一 token**（pb_node_start 复用 $OUT/<name>.yaml）。
#    服务端 hub.Register 会顶掉该节点的旧连接（internal/ws/node_hub.go：superseded by a
#    newer connection）；旧 agent 是 SIGSTOP 状态、它那条连接早被 watchdog 关掉了，
#    所以顶掉不会丢任何东西——只有它手上的任务会被重派出去。
#    注意 pb_node_start 会 >覆盖同名日志，先把 agent1 的日志另存一份（证据）。
cp "$OUT/$NAME5B.log" "$OUT/$NAME5B.agent1.log" 2>/dev/null || true
NEW_PID=$(pb_node_start "$NAME5B")
pb_log "5b agent2 pid=$NEW_PID（agent1 pid=$OLD_PID 仍 STOP 着；agent1 日志已存为 $NAME5B.agent1.log）"
s5_poll "第二个 agent 握手成功（node.ws.hello 计数 > $h_hello0）" 40 s5_hello_gt "$h_hello0" || true
s5_poll "5b 节点重新 online" 30 s5_node_online "$NAME5B" || true
t_agent2=$(pb_secs)

# ④ 观察窗口：巡检每 5s 一轮 → 期望 5~15s 内重派并第二次执行。
#    期间每 0.3s 采一次 sleep 60 的**并发进程数**（第一次执行的孙进程还活着，
#    第二次执行起来的那一刻会有 2 个 → 重复执行的铁证），并等 marker 第 2 行。
MAX_SLEEP=0
mark_b2=0
deadline=$((SECONDS + 60))
while [ $SECONDS -lt $deadline ]; do
	c=$(s5_sleep_count)
	if [ "$c" -gt "$MAX_SLEEP" ]; then MAX_SLEEP=$c; fi
	mark_b2=$(s5_mark_count "$TID_B")
	if [ "$mark_b2" -ge 2 ]; then break; fi
	sleep 0.3
done
t_reexec=$(pb_secs)
PGREP_SNAPSHOT=$(s5_sleeps)
if [ -z "$PGREP_SNAPSHOT" ]; then PGREP_SNAPSHOT='（空）'; fi
pb_log "5b 重派后 marker=$mark_b2 行，sleep60 峰值并发=$MAX_SLEEP"

# ⑤ 第二次执行本身是 sleep 60（timeoutSec=200 不会掐）→ 等它自己跑完进终态
s5_poll "5b 第二次执行进终态（约 60s）" 100 s5_terminal "$TID_B" || true
t_done_b=$(pb_secs)
g_b1=$(pb_gsnap s5b-after)
rss_b1=$(pb_rss s5b-after)

{
	echo '== S5b 心跳饥饿：SIGSTOP agent → 判死 → 租约过期 → 重派 → 期望「同一 task 执行两次」==' 
	printf '采集时间                 %s\n' "$(date '+%F %T')"
	printf 'task                     %s\n' "$TID_B"
	printf '节点                     %s\n' "$NAME5B"
	printf 'agent1(被 STOP) pid      %s\n' "$OLD_PID"
	printf 'agent2(顶掉旧连接) pid   %s\n' "$NEW_PID"
	printf 'SIGSTOP→回收             %ss\n' "$(pb_sub "$t_reclaim" "$t_stop")"
	printf '回收→agent2 在线          %ss\n' "$(pb_sub "$t_agent2" "$t_reclaim")"
	printf 'agent2 在线→第二次执行    %ss\n' "$(pb_sub "$t_reexec" "$t_agent2")"
	printf 'fire→终态（含重派+重跑）  %ss\n' "$(pb_sub "$t_done_b" "$t_fire_b")"
	printf 'status / error           %s / %s\n' "$(s5_field "$TID_B" status)" "$(s5_field "$TID_B" error)"
	printf 'attempts                 %s（期望 ≥2，证据：被重派过）\n' "$(s5_field "$TID_B" attempts)"
	echo '--- 任务行（原文 jq）---'
	s5_task "$TID_B" |
		jq '{id,status,attempts,error,nodeId,leaseUntil,notBefore,createdAt,startedAt,finishedAt}' 2>/dev/null || true
	echo '--- agent 侧 marker（同一 task_id 的行数 = 实际执行次数）---'
	printf 'marker 行数              %s（key 证据：期望 2 = 重复执行）\n' "$(s5_mark_count "$TID_B")"
	nl -ba "$MARK" 2>/dev/null || echo '  （marker 为空）'
	echo '--- 重复执行的铁证：同一时刻活着的 sleep 60 进程 ---'
	printf '观察窗口内 sleep60 峰值并发 %s（期望 2）\n' "$MAX_SLEEP"
	printf '突破发生时 pgrep -af "^sleep 60$" 快照：\n%s\n' "$PGREP_SNAPSHOT"
	echo '--- server.log 证据 ---'
	printf 'node.ws.result 行数            %s\n' "$(s5_log_count "$OUT/server.log" node.ws.result "$TID_B")"
	printf 'lease.sweeper.reclaimed 行数   %s（期望 ≥1）\n' \
		"$(s5_log_count "$OUT/server.log" lease.sweeper.reclaimed "$TID_B")"
	printf 'node.ws.timeout 行数（判死）   %s（期望 ≥1）\n' \
		"$(grep -cF '"node.ws.timeout"' "$OUT/server.log" 2>/dev/null || true)"
	printf 'node.ws.closed 行数            %s\n' \
		"$(grep -cF '"node.ws.closed"' "$OUT/server.log" 2>/dev/null || true)"
	echo '--- 判死 / 回收 / 重派 的原始日志行（jq 出来，去掉只有人工信息的 msg）---'
	grep -F -e '"node.ws.timeout"' -e '"node.ws.closed"' -e '"lease.sweeper.reclaimed"' \
		-e '"lease.sweeper.redispatch"' "$OUT/server.log" |
		jq -c 'del(.msg)' 2>/dev/null | head -20 || true
	echo '--- 该 task 的 node.ws.result（第二次执行的回报）---'
	grep -F '"node.ws.result"' "$OUT/server.log" | grep -F -- "$TID_B" |
		jq -c '{time,task_id,status,node_id,reported}' 2>/dev/null || true
	echo '--- agent 侧执行计数（agent 日志是文本格式，不能 jq）---'
	printf 'agent1 log  agent.task.finished %s（期望 0：被 STOP，收不了尾）\n' \
		"$(s5_agent_count "$OUT/$NAME5B.agent1.log" agent.task.finished "$TID_B")"
	printf 'agent2 log  agent.task.finished %s（期望 1）\n' \
		"$(s5_agent_count "$OUT/$NAME5B.log" agent.task.finished "$TID_B")"
	s5_agent_lines "$OUT/$NAME5B.log" agent.task.finished "$TID_B"
	echo '--- goroutine / RSS / load ---'
	printf 'goroutine %s → %s\n' "$g_b0" "$g_b1"
	printf 'RSS(KB)   %s → %s\n' "$rss_b0" "$rss_b1"
	printf 'loadavg   %s\n' "$(pb_loadavg)"
} 2>&1 | tee "$OUT/s5b.txt"

# ---------- 收尾：让被 STOP 的 agent 复活，清掉它留下的孙进程 ----------
kill -CONT "$OLD_PID" 2>/dev/null || true   # 复活后它会读到 connection closed 自行退出（v1 不重连）
sleep 1
# 残留 sleep 60 是旧 agent 的**孙进程**（不在 $PIDS 里，pb_cleanup 杀不到），
# 按上面那条精确 pattern 的结果清理，不用宽泛的 pkill。
LEFTOVER=$(s5_sleeps)
if [ -n "$LEFTOVER" ]; then
	pb_log "清理残留 sleep 60：$LEFTOVER"
	while read -r pid _; do
		[ -n "$pid" ] && kill -9 "$pid" 2>/dev/null
	done <<<"$LEFTOVER"
else
	pb_log '没有残留的 sleep 60'
fi

# ============================================================
# 汇总
# ============================================================
pb_say 'S5 汇总'
{
	echo '== S5 汇总（原文见同目录 s5a.txt / s5b.txt）=='
	printf '5a 超时保护   status=%s attempts=%s node.ws.result=%s 次  sleep60 残留进程=%s 个\n' \
		"$(s5_field "$TID_A" status)" "$(s5_field "$TID_A" attempts)" \
		"$(s5_log_count "$OUT/server.log" node.ws.result "$TID_A")" "$(s5_sleep_count)"
	printf '5b 心跳饥饿   status=%s attempts=%s marker 行数=%s 次  sleep60 峰值并发=%s\n' \
		"$(s5_field "$TID_B" status)" "$(s5_field "$TID_B" attempts)" \
		"$(s5_mark_count "$TID_B")" "$MAX_SLEEP"
	echo '判读：5a 期望「不重复」（node.ws.result=1 且 marker 无 2 行）；'
	echo '      5b 期望「重复执行」（marker 2 行 + 2 个 sleep 60 并发 + attempts≥2）——'
	echo '      后者是**待确认的产品行为**：租约重派 + 老进程没被中枢杀掉，导致同一 task 跑两遍。'
} 2>&1 | tee "$OUT/s5-summary.txt"

pb_say "S5 完成 —— 关键数字见 $OUT/s5-summary.txt"
cat "$OUT/s5a.txt"
cat "$OUT/s5b.txt"
