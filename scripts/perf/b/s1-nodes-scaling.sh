#!/usr/bin/env bash
# S1 · N 个 agent 并发接单：握手成本、GET /v1/nodes 随 N 的曲线、assign 扇出与超槽后的排空速度。
#
#   for n in 1 4 16 64; do N=$n bash scripts/perf/b/s1-nodes-scaling.sh; done
#
# 每个 N 用独立库（端口由 PORT 指定，默认 7920），跑完即清理。
#
# 测四件事：
#   1) 握手：先建好 N 个节点（HTTP），再**一次性**拉起 N 个 agent，量「到全部 online」的墙钟
#   2) GET /v1/nodes -c 8 -d 10s（读路径每节点 2 条 SQL 的 N+1）→ RPS/延迟曲线 + 响应体大小
#   3) assign 扇出两波：
#        波 A：N 个任务 / P=N（正好铺满每节点 1 槽）→ 理想扇出耗时
#        波 B：4N 个任务 / P=16 → 超出槽位的部分掉进 pending，量「排空」速度
#        （排空的量化与归因见 S3，这里只取墙钟数字做 N 维对比）
#   4) 空闲期成本：N 条心跳（10s 间隔）在 30s 窗口吃掉多少 server CPU
set -uo pipefail
. "$(dirname "$0")/lib.sh"

N=${N:-1}
PORT=${PORT:-7920}
pb_init
pb_build
pb_start_server
pb_seed
pb_ns

PB_CAP_NAME=perf.noop
pb_cap "$PB_CAP_NAME"
pb_source perf-bulk "$CAP"

pb_say "N=$N：建 $N 个节点（instances=1 concurrency=1）"
REPORT=$OUT/s1-N$N.txt
: >"$REPORT"
MARK=$OUT/marks.txt
: >"$MARK"
CLIENT_FILE=$OUT/client.txt
: >"$CLIENT_FILE"
CMD='printf "%s %s\n" "$ANOTIFY_TASK_ID" "$EPOCHREALTIME" >> '"$MARK"

t0=$(pb_secs)
for i in $(seq 1 "$N"); do pb_node_create "n$i" 1 1 "$CMD" 60; done
t_node_create=$(pb_sub "$(pb_secs)" "$t0")

g0=$(pb_gsnap before-nodes)
rss0=$(pb_rss before-nodes)

pb_say "一次性拉起 $N 个 agent，量握手"
t0=$(pb_secs)
for i in $(seq 1 "$N"); do pb_node_start "n$i" >/dev/null; done
pb_wait_online "$N" 180
t_handshake=$(pb_sub "$(pb_secs)" "$t0")
g1=$(pb_gsnap after-nodes)
rss1=$(pb_rss after-nodes)

pb_say "GET /v1/nodes 压测（-c 8 -d 10s）"
taskset -c "$PB_CORES" "$PB_BIN/loadgen" -url "$BASE/v1/nodes?namespaceId=$NS" \
	-header "$COOKIE" -c 8 -d 10s 2>&1 | tee "$OUT/s1-nodes-loadgen-N$N.txt"
BYTES=$(curl -s -o "$OUT/nodes-N$N.json" -w '%{size_download}' -H "$COOKIE" "$BASE/v1/nodes?namespaceId=$NS")

pb_say "波 A：$N 个任务 / P=$N（铺满所有槽）"
t0=$(pb_secs)
pb_fire_timed_many "$N" "$N"
pb_wait_until "波 A $N 个任务 finished" 180 bash -c \
	"[ \$(grep -c '\"node.ws.result\"' '$OUT/server.log') -ge $N ]"
t_burst=$(pb_sub "$(pb_secs)" "$t0")

pb_say "波 B：$((N * 4)) 个任务 / P=16（超槽 → 掉 pending）"
before_b=$(pb_finished_count)
t0=$(pb_secs)
pb_fire_many "$((N * 4))" 16
pb_wait_until "波 B 再完成 $((N * 4)) 个" 300 bash -c \
	"[ \$(grep -c '\"node.ws.result\"' '$OUT/server.log') -ge $((before_b + N * 4)) ]"
t_drain=$(pb_sub "$(pb_secs)" "$t0")

g2=$(pb_gsnap after-load)
rss2=$(pb_rss after-load)

pb_say "空闲期 CPU（$N 条心跳 / 30s 窗口）"
pb_cpu_over 30 "N=$N idle-heartbeat"

{
	echo "== S1 N=$N =="
	printf '建节点(HTTP×N)     %ss\n' "$t_node_create"
	printf '握手到全 online    %ss（%.1fms/节点）\n' "$t_handshake" \
		"$(awk -v t="$t_handshake" -v n="$N" 'BEGIN{printf "%.1f", t*1000/n}')"
	printf 'GET /v1/nodes 响应 %s bytes\n' "$BYTES"
	printf '波 A %s 任务       墙钟 %ss → %.1f task/s（%.1fms/task）\n' "$N" "$t_burst" \
		"$(awk -v t="$t_burst" -v n="$N" 'BEGIN{printf "%.2f", n/t}')" \
		"$(awk -v t="$t_burst" -v n="$N" 'BEGIN{printf "%.1f", t*1000/n}')"
	printf '波 B %s 任务       墙钟 %ss → %.2f task/s（超槽部分靠 5s 巡检排空）\n' "$((N*4))" "$t_drain" \
		"$(awk -v t="$t_drain" -v n="$((N*4))" 'BEGIN{printf "%.2f", n/t}')"
	printf 'goroutine          %s → %s → %s\n' "$g0" "$g1" "$g2"
	printf 'RSS(KB)            %s → %s → %s\n' "$rss0" "$rss1" "$rss2"
	printf 'loadavg(1/5/15)    %s\n' "$(pb_loadavg)"
} | tee -a "$REPORT"

pb_goroutine_dump "N$N"
pb_latency "$CLIENT_FILE" "$MARK" "S1 N=$N 波 A（满槽）"

pb_say "S1 完成（N=$N）"
cat "$REPORT"
