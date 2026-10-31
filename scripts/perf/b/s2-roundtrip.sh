#!/usr/bin/env bash
# S2 · assign → result 往返：单槽串行 vs 声明的并发槽（run.concurrency:8）vs 真多槽（instances:8）。
#
#   for c in a b c; do CONFIG=$c bash scripts/perf/b/s2-roundtrip.sh; done
#
# CONFIG:
#   a = instances:1 concurrency:1  → 1 个执行槽（串行基准）
#   b = instances:1 concurrency:8  → **中枢只看见 1 个槽**（见结论），concurrency 只是 hello 里的声明
#   c = instances:8 concurrency:1 → 8 个槽
#   d = instances:8 concurrency:8 → 8 个槽 + 声明 8（对照 c，看声明有没有用）
#
# 两个阶段：
#   串行  P=1，10 个任务  → 单任务端到端（不含排队）
#   并发  P=8，32 个任务  → 有排队时的端到端分布
# PPROF=1 时在并发阶段同时采 20s CPU profile（profile 与压测同核，污染已知）。
set -uo pipefail
. "$(dirname "$0")/lib.sh"

CONFIG=${CONFIG:-a}
PORT=${PORT:-7929}
PPROF=${PPROF:-0}
case "$CONFIG" in
	a) INST=1;  CONC=1 ;;
	b) INST=1;  CONC=8 ;;
	c) INST=8;  CONC=1 ;;
	d) INST=8;  CONC=8 ;;
	*) pb_die "CONFIG 只能是 a/b/c/d（收到 $CONFIG）" ;;
esac
SERIAL_N=${SERIAL_N:-10}
PAR_N=${PAR_N:-32}
PAR=${PAR:-8}

pb_init
pb_build
pb_start_server
pb_seed
pb_ns

PB_CAP_NAME=perf.noop
pb_cap "$PB_CAP_NAME"
pb_source perf-rt "$CAP"

MARK=$OUT/marks.txt
: >"$MARK"
CLIENT_FILE=$OUT/client.txt
: >"$CLIENT_FILE"
CMD='printf "%s %s\n" "$ANOTIFY_TASK_ID" "$EPOCHREALTIME" >> '"$MARK"

pb_say "CONFIG=$CONFIG：instances=$INST concurrency=$CONC"
pb_node node-rt "$INST" "$CONC" "$CMD" 60
pb_wait_online 1
g0=$(pb_gsnap before); rss0=$(pb_rss before)

REPORT=$OUT/s2-$CONFIG.txt
: >"$REPORT"

pb_say "串行阶段：$SERIAL_N 任务 / P=1"
t0=$(pb_secs)
for _ in $(seq 1 "$SERIAL_N"); do pb_fire_timed "$SRC" '{"i":1}' >/dev/null; done
before=$(pb_finished_count)
pb_wait_until "串行 $SERIAL_N 个任务 finished" 300 bash -c \
	"[ \$(grep -c '\"node.ws.result\"' '$OUT/server.log') -ge $SERIAL_N ]"
t_serial=$(pb_sub "$(pb_secs)" "$t0")
LAT_SERIAL=$(pb_latency "$CLIENT_FILE" "$MARK" "S2-$CONFIG 串行")

pb_say "并发阶段：$PAR_N 任务 / P=$PAR"
: >"$CLIENT_FILE"
t0=$(pb_secs)
if [ "$PPROF" = "1" ]; then
	# profile 与压测并行跑（同核），20s 采样覆盖整段并发阶段
	( pb_pprof 20 "s2-$CONFIG" >"$OUT/s2-$CONFIG-pprof.log" 2>&1 ) &
	PPROF_PID=$!
fi
pb_fire_timed_many "$PAR_N" "$PAR"
pb_wait_until "并发 $PAR_N 个任务 finished" 600 bash -c \
	"[ \$(grep -c '\"node.ws.result\"' '$OUT/server.log') -ge $((SERIAL_N + PAR_N)) ]"
t_par=$(pb_sub "$(pb_secs)" "$t0")
[ "${PPROF_PID:-}" ] && wait "$PPROF_PID" 2>/dev/null
LAT_PAR=$(pb_latency "$CLIENT_FILE" "$MARK" "S2-$CONFIG 并发 P=$PAR")

g1=$(pb_gsnap after); rss1=$(pb_rss after)

{
	echo "== S2 CONFIG=$CONFIG（instances=$INST concurrency=$CONC）=="
	printf '串行 %s 任务 墙钟 %ss → %.2f task/s（%.1fms/task）\n' "$SERIAL_N" "$t_serial" \
		"$(awk -v t="$t_serial" -v n="$SERIAL_N" 'BEGIN{printf "%.2f", n/t}')" \
		"$(awk -v t="$t_serial" -v n="$SERIAL_N" 'BEGIN{printf "%.1f", t*1000/n}')"
	echo "$LAT_SERIAL"
	printf '并发 %s 任务 墙钟 %ss → %.2f task/s\n' "$PAR_N" "$t_par" \
		"$(awk -v t="$t_par" -v n="$PAR_N" 'BEGIN{printf "%.2f", n/t}')"
	echo "$LAT_PAR"
	printf 'goroutine %s → %s；RSS(KB) %s → %s\n' "$g0" "$g1" "$rss0" "$rss1"
	printf 'loadavg(1/5/15) %s\n' "$(pb_loadavg)"
} | tee "$REPORT"
[ "$PPROF" = "1" ] && { echo "--- pprof top ---"; head -30 "$OUT/s2-$CONFIG-pprof.log"; }

pb_say "S2 完成（CONFIG=$CONFIG）"
