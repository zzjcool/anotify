#!/usr/bin/env bash
# S1b · 派发热路径的 CPU profile：8 槽单节点 + 持续 assign→result churn 20s。
#
#   bash scripts/perf/b/s1b-profile.sh
#
# 为什么要单独一条：S1/S2 的负载只持续几百毫秒，采不到有意义的 profile。
# 这里用「反复打满 8 个槽」把 CPU 采样窗口填满，回答「一次派发 ~14ms 到底花在哪」。
set -uo pipefail
. "$(dirname "$0")/lib.sh"

PORT=${PORT:-7927}
SECS=${SECS:-20}
pb_init
pb_build
pb_start_server
pb_seed
pb_ns

PB_CAP_NAME=perf.noop
pb_cap "$PB_CAP_NAME"
pb_source perf-prof "$CAP"
MARK=$OUT/marks.txt; : >"$MARK"
pb_node node-prof 8 1 'echo "$ANOTIFY_TASK_ID" >> '"$MARK" 60
pb_wait_online 1

echo "=== 基线（空转 5s）==="
c0=$(pb_cputime); sleep 5; c1=$(pb_cputime)
echo "idle cpu 5s: $(pb_sub "$c1" "$c0")s"

pb_say "并发采样：profile ${SECS}s + 持续 $((SECS))s 的 8 槽 churn"
( pb_pprof "$SECS" dispatch >"$OUT/pprof.txt" 2>&1 ) &
PP=$!
sleep 1
rounds=0
end=$((SECONDS + SECS - 2))
while [ $SECONDS -lt $end ]; do
	pb_fire_many 32 8
	rounds=$((rounds + 1))
done
wait "$PP" 2>/dev/null
c2=$(pb_cputime)
done_n=$(pb_finished_count)
{
	echo "=== 派发热路径 profile ==="
	printf 'churn %s 轮 × 32 任务 = %s 次派发；完成 %s；采样窗口 CPU %.2fs\n' \
		"$rounds" "$((rounds * 32))" "$done_n" "$(pb_sub "$c2" "$c1")"
	printf '吞吐 = %.1f task/s（%.2fms/task，含 8 客户端并发）\n' \
		"$(awk -v n="$done_n" -v t="$SECS" 'BEGIN{printf "%.1f", n/t}')" \
		"$(awk -v n="$done_n" -v t="$SECS" 'BEGIN{printf "%.2f", t*1000/n}')"
	printf 'loadavg(1/5/15) %s\n' "$(pb_loadavg)"
} | tee "$OUT/s1b.txt"
echo '--- CPU profile -top -nodecount=25 ---'
sed -n '/### CPU profile/,$p' "$OUT/pprof.txt" | head -32
echo '--- 按累计时间（cum）---'
taskset -c "$PB_CORES" go tool pprof -top -nodecount=20 -cum "$OUT/dispatch.pprof" 2>&1 | head -26
pb_say "S1b 完成"
