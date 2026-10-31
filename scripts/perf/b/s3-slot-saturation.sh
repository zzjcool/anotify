#!/usr/bin/env bash
# S3 · 槽位饱和：instances:1 concurrency:1 下发 100 个任务 → 是排队还是丢？内存涨多少？队列上限在哪？
#
#   bash scripts/perf/b/s3-slot-saturation.sh
#
# 三问：
#   1) 超槽的任务去哪了（pending？丢帧？）+ 排空速度（这是 B 路的核心瓶颈之一）
#   2) 积压 100 / 1000 条 pending 时，服务端要付多少代价（巡检 tick、CPU、RSS、DB 体积）
#   3) 队列有没有上限（会不会 OOM / 被限流 / 任务被弃置 abandoned）
set -uo pipefail
. "$(dirname "$0")/lib.sh"

PORT=${PORT:-7925}
N1=${N1:-100}
N2=${N2:-900}
pb_init
pb_build
pb_start_server
pb_seed
pb_ns

PB_CAP_NAME=perf.noop
pb_cap "$PB_CAP_NAME"
pb_source perf-sat "$CAP"

MARK=$OUT/marks.txt
: >"$MARK"
CMD='printf "%s %s\n" "$ANOTIFY_TASK_ID" "$EPOCHREALTIME" >> '"$MARK"

pb_say "建 1 个节点：instances=1 concurrency=1（1 个执行槽）"
pb_node node-sat 1 1 "$CMD" 60
pb_wait_online 1
g0=$(pb_gsnap before); rss0=$(pb_rss before)
cpu0=$(pb_cputime)

REPORT=$OUT/s3.txt
: >"$REPORT"
{
	echo '== S3 槽位饱和（instances=1 concurrency=1）=='
	printf '基线：goroutine=%s RSS=%sKB cpu=%ss db=%sB\n' "$g0" "$rss0" "$cpu0" "$(stat -c%s "$DB")"
} | tee -a "$REPORT"

# ---------- 阶段 1：100 个任务同时打进来 ----------
pb_say "阶段 1：$N1 个任务 / P=10 一次性打进来"
t0=$(pb_secs)
pb_fire_many "$N1" 10
t_fire=$(pb_sub "$(pb_secs)" "$t0")
sleep 1
# 派发瞬间看任务状态分布（服务端视角）
STATUS1=$(pb_tasks_dist)
g1=$(pb_gsnap after-burst); rss1=$(pb_rss after-burst)
{
	printf '阶段1 触发 %s 个任务（P=10）耗时 %ss；1s 后状态分布：%s\n' "$N1" "$t_fire" "$STATUS1"
	printf '阶段1 后 goroutine=%s RSS=%sKB\n' "$g1" "$rss1"
} | tee -a "$REPORT"

# 量前 30 秒的排空速率（这段最能说明问题）
pb_say "量排空速率（30s 窗口）"
d0=$(pb_finished_count); c0=$(pb_cputime)
sleep 30
d1=$(pb_finished_count); c1=$(pb_cputime)
rate=$(awk -v a="$d0" -v b="$d1" 'BEGIN{printf "%.2f", (b-a)/30}')
{
	printf '排空速率：30s 内完成 %s 个 → %.2f task/s（单槽理想值应接近 1/单任务延迟≈30~40 task/s）\n' "$((d1-d0))" "$rate"
	printf '同期 server CPU：%.2fs（%.1f%%）\n' "$(pb_sub "$c1" "$c0")" \
		"$(awk -v a="$c0" -v b="$c1" 'BEGIN{printf "%.1f", (b-a)/30*100}')"
	echo '--- 巡检日志（每 5s 一轮：dispatched 才是真派出去的）---'
	grep 'lease.sweeper.redispatch' "$OUT/server.log" | jq -r '(.time|.[11:19])+" dispatched="+(.dispatched|tostring)+" skipped="+(.skipped|tostring)' | tail -8
} | tee -a "$REPORT"
stat_after_first=$(pb_tasks_dist)
echo "阶段1 30s 后状态：$stat_after_first" | tee -a "$REPORT"

# ---------- 阶段 2：再压 900 条，凑到 ~1000 pending ----------
pb_say "阶段 2：再打 $N2 个任务（凑到 ~1000 条积压）"
t0=$(pb_secs)
pb_fire_many "$N2" 20
t_fire2=$(pb_sub "$(pb_secs)" "$t0")
sleep 2
PEND=$(pb_tasks_count pending)
g2=$(pb_gsnap backlog); rss2=$(pb_rss backlog)
dbsize=$(stat -c%s "$DB")
{
	printf '阶段2 触发 %s 个（P=20）耗时 %ss；pending=%s；DB=%sB；goroutine=%s RSS=%sKB\n' \
		"$N2" "$t_fire2" "$PEND" "$dbsize" "$g2" "$rss2"
} | tee -a "$REPORT"

pb_say "积压期 CPU（30s 窗口，$PEND 条 pending 每 5s 被 ListRedispatchable 扫一遍）"
pb_cpu_over 30 "backlog-$PEND"

d2=$(pb_finished_count)
sleep 20
d3=$(pb_finished_count)
{
	printf '积压期排空速率：20s 内完成 %s 个 → %.2f task/s\n' "$((d3-d2))" \
		"$(awk -v a="$d2" -v b="$d3" 'BEGIN{printf "%.2f", (b-a)/20}')"
	echo '--- 积压期巡检日志 ---'
	grep 'lease.sweeper.redispatch' "$OUT/server.log" | jq -r '(.time|.[11:19])+" dispatched="+(.dispatched|tostring)+" skipped="+(.skipped|tostring)' | tail -6
	g3=$(pb_gsnap end); rss3=$(pb_rss end)
	printf '结束：goroutine=%s RSS=%sKB DB=%sB\n' "$g3" "$rss3" "$(stat -c%s "$DB")"
	printf 'loadavg(1/5/15) %s\n' "$(pb_loadavg)"
	echo '--- 队列上限检查：有没有 429/5xx/被弃置的任务 ---'
	printf 'abandoned=%s  failed=%s  pending=%s\n' \
		"$(pb_tasks_count abandoned)" "$(pb_tasks_count failed)" "$(pb_tasks_count pending)"
} | tee -a "$REPORT"

pb_goroutine_dump s3
pb_say "S3 完成"
