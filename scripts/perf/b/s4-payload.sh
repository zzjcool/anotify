#!/usr/bin/env bash
# S4 · 大 payload：params 1KB / 100KB / 512KB / 900KB / 1MB / 2MB → 帧大小限制、内存峰值、失败行为。
#
#   bash scripts/perf/b/s4-payload.sh
#
# 已知的上限（读代码得到的假设，本脚本负责实测）：
#   - 入口 body：hook 4MiB（source_handlers.go:748），fire 1MiB，/v1/events 1MiB
#   - 下行 assign 帧：agent 侧读上限 1MiB（cmd/agent/client.go:56 SetReadLimit）
#   - 上行帧：服务端**没设**读上限 → coder/websocket 默认 32KiB（所以大 result 会直接断连）
#
# 分两段：
#   4a 逐个尺寸打一次（走 hook，4MiB 上限），记录 HTTP、任务终态、agent 是否掉线、RSS 峰值
#   4b **已退役**：原来靠 node.yaml 里写 40KB 命令把 hello 帧撞大；v2 的 hello 不再带命令
#      （只上挈 applied[]，见 internal/ws.HelloFrame），那个测量面不存在了（N3）。
set -uo pipefail
. "$(dirname "$0")/lib.sh"

PORT=${PORT:-7926}
SIZES=${SIZES:-1024 102400 524288 900000 1048576 2000000}
pb_init
pb_build
pb_start_server
pb_seed
pb_ns

PB_CAP_NAME=perf.noop
MARK=$OUT/marks.txt
: >"$MARK"
CMD='printf "%s %s\n" "$ANOTIFY_TASK_ID" "$EPOCHREALTIME" >> '"$MARK"
# v2：命令进能力产物（节点不再自报执行方式，见 N3）。
pb_cap "$PB_CAP_NAME" "$CMD" 60
pb_source perf-big "$CAP"

REPORT=$OUT/s4.txt
: >"$REPORT"
echo '== S4 大 payload ==' | tee -a "$REPORT"
echo '--- 4a：hook 入口逐个尺寸（agent 侧读上限 1MiB）---' | tee -a "$REPORT"

pb_node node-big 1 1 "$CMD" 60
pb_wait_online 1
echo "基线：goroutine=$(pb_gsnap before) RSS=$(pb_rss before)KB" | tee -a "$REPORT"

for sz in $SIZES; do
	pb_say "payload ${sz}B"
	body=$OUT/body-$sz.json
	python3 - "$sz" "$body" <<'PY'
import json, sys
sz, path = int(sys.argv[1]), sys.argv[2]
with open(path, 'w') as f:
    json.dump({"type": "perf.event", "payload": {"blob": "x" * sz}}, f)
PY
	actual=$(stat -c%s "$body")
	rss_before=$(pb_rss "before-$sz")
	g_before=$(pb_gsnap "before-$sz")
	# 走 hook（4MiB 上限）；-w 取状态码与耗时
	resp=$(curl -sS -o "$OUT/resp-$sz.json" -w '%{http_code} %{time_total}' -X POST \
		-H 'content-type: application/json' --data-binary @"$body" "$BASE$HOOK")
	code=${resp%% *}; secs=${resp##* }
	task=$(jq -r '.dispatch.tasks[0].id // empty' "$OUT/resp-$sz.json" 2>/dev/null)
	sleep 2
	online=$(pb_online)
	st='-'; err='-'; att=0
	if [ -n "$task" ]; then
		json=$(pb_api GET "/v1/tasks/$task?namespaceId=$NS")
		st=$(printf '%s' "$json" | jq -r '.task.status // "-"')
		err=$(printf '%s' "$json" | jq -r '(.task.error // "") | .[0:90]')
		att=$(printf '%s' "$json" | jq -r '.task.attempts // 0')
	fi
	dropped=$(grep -c '"node.ws.closed"' "$OUT/server.log" 2>/dev/null || echo 0)
	rss_after=$(pb_rss "after-$sz")
	{
		printf '%-9s body=%-9s HTTP=%s %.3fs task=%s status=%-9s attempts=%s online=%s closed=%s RSS=%s→%sKB\n' \
			"${sz}B" "$actual" "$code" "$secs" "${task:--}" "$st" "$att" "$online" "$dropped" "$rss_before" "$rss_after"
		printf '          error=%s\n' "${err:--}"
	} | tee -a "$REPORT"

	# agent 掉线就把它拉回来（大 payload 让连接被 1009 关掉后，巡检会重派 → 反复打死）
	if [ "$online" = "0" ]; then
		echo '          → agent 掉线，重启（模拟运维/进程管理器行为）' | tee -a "$REPORT"
		pb_node_start node-big >/dev/null
		pb_wait_online 1 60 || echo '          → 重启后仍未 online' | tee -a "$REPORT"
	fi
done

# ---------- 4b：已退役（v2）----------
#
# 原来的 4b 量的是「hello 帧 > 32KiB 会不会被库默认读上限打断」：它靠 node.yaml 里
# 写一条 40KB 的 `command` 把 hello 撞大。
# v2 里 hello 已经**不再带命令**（只上挈 applied[]，见 internal/ws.HelloFrame），
# 所以这个测量面不存在了：命令现在走 deploy 帧（产物 spec），它的体积上限是
# internal/artifact 的 MaxFileBytes/MaxTotalBytes，属于另一个问题。
# 真要继续测帧上限，应该在“部署一份大产物”上做，而不是在 hello 上（N3）。
echo '--- 4b 已退役：v2 的 hello 不再带命令，帧尺寸测量面不存在（见 N3）---' | tee -a "$REPORT"

{
	printf '结束：goroutine=%s RSS=%sKB\n' "$(pb_gsnap end)" "$(pb_rss end)"
	printf 'loadavg(1/5/15) %s\n' "$(pb_loadavg)"
} | tee -a "$REPORT"

pb_say "S4 完成"
