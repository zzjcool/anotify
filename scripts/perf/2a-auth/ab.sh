#!/usr/bin/env bash
# 批次 2A（#227 / #245 / #241）鉴权热路径修复前后对比。
#
# 纪律（任务卡）：`taskset -c 4-7`、端口 7980-7999、同一命令跑 before 与 after。
# 用法：
#   bash scripts/perf/2a-auth/ab.sh <二进制路径> <标签>
#   bash scripts/perf/2a-auth/ab.sh /tmp/perf-2a/bin/anotify-before before
#
# 产出：/tmp/perf-2a/out/<标签>/{bearer,rss,read,hooks,hooks-eq}.log
# 本脚本只读产品代码，不写仓库；所有产物落在 /tmp/perf-2a。
set -uo pipefail

BIN="${1:?用法: ab.sh <二进制> <标签>}"
LABEL="${2:?用法: ab.sh <二进制> <标签>}"
CPUS="${CPUS:-4-7}"
PORT="${PORT:-7980}"
BASE="http://127.0.0.1:$PORT"
ROOT=/tmp/perf-2a
OUT="$ROOT/out/$LABEL"
LOADGEN="$ROOT/bin/loadgen"
DEVSEED="$ROOT/bin/devseed"
mkdir -p "$OUT" "$ROOT/logs"

[ -x "$BIN" ] || { echo "缺二进制：$BIN" >&2; exit 1; }
[ -x "$LOADGEN" ] || { echo "缺 loadgen" >&2; exit 1; }
command -v jq >/dev/null || { echo "缺 jq" >&2; exit 1; }
case "$PORT" in 798[0-9]|799[0-9]) ;; *) echo "端口必须在 7980-7999" >&2; exit 1;; esac

PIDF="$ROOT/run-$LABEL.pid"
SRVLOG="$ROOT/logs/server-$LABEL.log"

restart() { # $1=log level  $2...=额外 env
  if [ -f "$PIDF" ]; then kill "$(cat "$PIDF")" 2>/dev/null; sleep 0.6; fi
  local lvl="$1"; shift
  : > "$SRVLOG"
  taskset -c "$CPUS" env ANOTIFY_ADDR=":$PORT" ANOTIFY_DB="$DB" \
    ANOTIFY_PPROF=1 ANOTIFY_LOG_FORMAT=json ANOTIFY_LOG_LEVEL="$lvl" \
    "$@" "$BIN" >>"$SRVLOG" 2>&1 &
  echo $! > "$PIDF"
  for _ in $(seq 1 60); do
    curl -sf "$BASE/health" >/dev/null 2>&1 && return 0
    sleep 0.2
  done
  echo "服务起不来（$LABEL），见 $SRVLOG" >&2; tail -20 "$SRVLOG" >&2; return 1
}
stop() { [ -f "$PIDF" ] && { kill "$(cat "$PIDF")" 2>/dev/null; rm -f "$PIDF"; }; sleep 0.4; }
trap stop EXIT

rss_mb() { awk '/VmRSS/{printf "%.1f", $2/1024}' "/proc/$(cat "$PIDF")/status" 2>/dev/null; }

# ---------- 种子 ----------
DB="$(mktemp -d /tmp/perf-2a/db.XXXXXX)/a.db"
taskset -c "$CPUS" "$DEVSEED" -db "$DB" -username "p2a$RANDOM" >"$ROOT/logs/devseed-$LABEL.log" 2>&1
SESSION=$(grep -o 'SESSION=[^ ]*' "$ROOT/logs/devseed-$LABEL.log" | cut -d= -f2 | head -1)
SEND_KEY=$(grep -o 'SEND_KEY=[^ ]*' "$ROOT/logs/devseed-$LABEL.log" | cut -d= -f2 | head -1)
NS=$(grep -o 'ns_[A-Za-z0-9]*' "$ROOT/logs/devseed-$LABEL.log" | head -1)
[ -n "$SESSION" ] && [ -n "$SEND_KEY" ] && [ -n "$NS" ] || {
  echo "devseed 产物缺失（SESSION/SEND_KEY/NS）：$ROOT/logs/devseed-$LABEL.log" >&2
  cat "$ROOT/logs/devseed-$LABEL.log" >&2; exit 1; }

restart info || exit 1

# 能力 + webhook 源（拿 hookUrl）
CAP=$(curl -s -X POST -H "Cookie: anotify_session=$SESSION" -H 'content-type: application/json' \
  -d "{\"namespaceId\":\"$NS\",\"name\":\"perf.write\"}" "$BASE/v1/capabilities" | jq -r '.capability.id // empty')
HOOKURL=$(curl -s -X POST -H "Cookie: anotify_session=$SESSION" -H 'content-type: application/json' \
  -d "{\"namespaceId\":\"$NS\",\"name\":\"perf-hook\",\"kind\":\"webhook\",\"targets\":[{\"capabilityId\":\"$CAP\"}]}" \
  "$BASE/v1/sources" | jq -r '.source.hookUrl // empty')
[ -n "$HOOKURL" ] || { echo "hookUrl 未取到（CAP=$CAP）" >&2; exit 1; }
printf '{"type":"perf.hook","payload":{"i":1}}' > "$ROOT/logs/hook-$LABEL.json"
printf '{"type":"perf.write","namespaceId":"%s","payload":{"i":1}}' "$NS" > "$ROOT/logs/evt-$LABEL.json"

echo "== $LABEL ==  NS=$NS  hook=$HOOKURL" | tee "$OUT/summary.txt"

# ---------- ① Bearer 写路径（#227 主指标）：c=8, 10s ----------
restart info
taskset -c "$CPUS" "$LOADGEN" -url "$BASE/v1/events" -method POST -c 8 -d 10s \
  -body-file "$ROOT/logs/evt-$LABEL.json" -header 'content-type: application/json' \
  -header "Authorization: Bearer $SEND_KEY" 2>&1 | tee "$OUT/bearer.log"
echo "RSS_MB_after_bearer=$(rss_mb)" | tee -a "$OUT/bearer.log"

# ---------- ② RSS 采样：c=8 Bearer 持续 15s，中途每 0.5s 采一次 ----------
restart info
( taskset -c "$CPUS" "$LOADGEN" -url "$BASE/v1/events" -method POST -c 8 -d 15s \
    -body-file "$ROOT/logs/evt-$LABEL.json" -header 'content-type: application/json' \
    -header "Authorization: Bearer $SEND_KEY" >"$OUT/rss-load.log" 2>&1 ) &
LP=$!
: > "$OUT/rss.log"
sleep 1.5
while kill -0 $LP 2>/dev/null; do rss_mb >> "$OUT/rss.log"; sleep 0.5; done
wait $LP 2>/dev/null
{
  echo "samples=$(wc -l < "$OUT/rss.log")"
  sort -n "$OUT/rss.log" | awk 'BEGIN{n=0}{a[n++]=$1}END{if(n==0){print "EMPTY";exit}
    printf "RSS_min=%.1fMB  RSS_p50=%.1fMB  RSS_max=%.1fMB\n", a[0], a[int(n/2)], a[n-1]}'
} | tee "$OUT/rss-mb.log"

# ---------- ③ 读路径（#241 主指标）：带 Cookie, c=32, 15s ----------
restart info
taskset -c "$CPUS" "$LOADGEN" -url "$BASE/v1/tasks?namespaceId=$NS&limit=100" -method GET -c 32 -d 15s \
  -header "Cookie: anotify_session=$SESSION" 2>&1 | tee "$OUT/read.log"

# ---------- ④ hooks 吞吐（#245）：默认配置, c=16, 10s ----------
restart info
taskset -c "$CPUS" "$LOADGEN" -url "$BASE$HOOKURL" -method POST -c 16 -d 10s \
  -body-file "$ROOT/logs/hook-$LABEL.json" -header 'content-type: application/json' 2>&1 | tee "$OUT/hooks.log"

# ---------- ⑤ hooks 日志放大 A/B：把阈值钉成 60/60，只比「429 打几行日志」 ----------
#    after 构建支持 ANOTIFY_HOOK_RATE_PER_MIN / _PER_IP_PER_MIN；before 构建忽略它们
#    （所以 before 这一组等价于它的默认 60/min/IP）。两边限流形状一致 → 可直接比日志量。
restart info ANOTIFY_HOOK_RATE_PER_MIN=60 ANOTIFY_HOOK_RATE_PER_IP_PER_MIN=60
BYTES0=$(stat -c %s "$SRVLOG")
taskset -c "$CPUS" "$LOADGEN" -url "$BASE$HOOKURL" -method POST -c 16 -d 10s \
  -body-file "$ROOT/logs/hook-$LABEL.json" -header 'content-type: application/json' 2>&1 | tee "$OUT/hooks-eq.log"
sleep 0.5
BYTES1=$(stat -c %s "$SRVLOG")
{
  echo "log_bytes_10s=$((BYTES1-BYTES0))"
  echo "ratelimit_warn_lines=$(grep -c '"ratelimit.hit"\|"msg":"rate limited"' "$SRVLOG" || true)"
  echo "request_warn_429_lines=$(grep -c '"status":429' "$SRVLOG" || true)"
} | tee "$OUT/hooks-log.log"

stop
echo "== $LABEL 完成，产物 $OUT =="
