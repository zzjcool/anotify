#!/usr/bin/env bash
# S0 冒烟：验证 lib.sh 的整条路径（建服/播种/建能力+源/建节点+agent/触发/采样/延迟汇总）。
# 任何场景脚本挂了，先跑这个定位是「环境坏了」还是「场景坏了」。
#
#   PORT=7920 bash scripts/perf/b/s0-smoke.sh    # 默认 7920，与 s1 相同 → 别并行跑
set -uo pipefail
. "$(dirname "$0")/lib.sh"
PORT=7920
pb_init
pb_build
pb_start_server
pb_seed
pb_ns

PB_CAP_NAME=perf.noop
pb_cap perf.noop
pb_source perf-hook "$CAP"
pb_source_edge perf-hook "$CAP"
SRC_NAME=perf-hook

MARK=$OUT/marks.txt
: >"$MARK"
CLIENT_FILE=$OUT/client.txt
: >"$CLIENT_FILE"

pb_node node-smoke 1 1 'printf "%s %s\n" "$ANOTIFY_TASK_ID" "$EPOCHREALTIME" >> '"$MARK"
pb_wait_online 1
pb_log "节点 online"

pb_say "触发 5 个任务（串行，fire 路径）"
baseline_g=$(pb_gsnap before)
baseline_rss=$(pb_rss before)
for _ in 1 2 3 4 5; do pb_fire_timed "$SRC" '{"i":1}' >/dev/null; done
pb_wait_until "5 个任务 finished" 30 bash -c "grep -c '\"node.ws.result\"' '$OUT/server.log' | grep -qx 5"

echo "--- server.log 里的 task finished ---"
grep '"node.ws.result"' "$OUT/server.log" | jq -c '{time,task_id,status,node_id}' 2>/dev/null || true
echo "--- agent 侧 marker ---"
cat "$MARK"
echo "--- 任务视图 ---"
pb_api GET "/v1/tasks?namespaceId=$NS&limit=3" | head -c 600; echo
echo "--- goroutine / rss ---"
pb_gsnap after; pb_rss after
pb_latency "$CLIENT_FILE" "$MARK" "smoke 5 任务串行"

echo "--- 目录内容 ---"
ls -la "$OUT"
pb_say "冒烟通过（human 复核上面数字）"
