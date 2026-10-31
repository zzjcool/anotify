#!/usr/bin/env bash
# #227 的核心断言是「RSS ≈ 并发 × 128MB」——每请求 64MB 的 argon2 缓冲 × GOGC 余量。
# 所以内存对比必须在**同一并发**下看峰值，而不是「同一时长」：
# 修复后同一个 15s 窗口能跑 50 倍的请求，绝对 RSS 自然被工作负载推高，
# 拿它跟修复前比会得出「内存变大了」的错误结论。
#
# 本脚本测的是：固定时长（5s）× 递增并发（1/4/8）下的**峰值 RSS（VmHWM）**。
# 修复前的曲线应随并发阶跃上升（并发 × 128MB）；修复后应基本平坦。
#
# 用法：bash scripts/perf/2a-auth/rss-vs-concurrency.sh <二进制> <标签>
set -uo pipefail

BIN="${1:?用法: rss-vs-concurrency.sh <二进制> <标签>}"
LABEL="${2:?用法: rss-vs-concurrency.sh <二进制> <标签>}"
CPUS="${CPUS:-4-7}"
PORT="${PORT:-7980}"
BASE="http://127.0.0.1:$PORT"
ROOT=/tmp/perf-2a
OUT="$ROOT/out/rss-$LABEL"
mkdir -p "$OUT" "$ROOT/logs"
PIDF="$ROOT/run-rss-$LABEL.pid"
SRVLOG="$ROOT/logs/server-rss-$LABEL.log"

case "$PORT" in 798[0-9]|799[0-9]) ;; *) echo "端口必须在 7980-7999" >&2; exit 1;; esac
[ -x "$BIN" ] || { echo "缺二进制：$BIN" >&2; exit 1; }

peak_mb() { awk '/VmHWM/{printf "%.1f\n", $2/1024}' "/proc/$(cat "$PIDF")/status" 2>/dev/null; }
base_mb() { awk '/VmRSS/{printf "%.1f\n", $2/1024}' "/proc/$(cat "$PIDF")/status" 2>/dev/null; }

restart() {
  [ -f "$PIDF" ] && { kill "$(cat "$PIDF")" 2>/dev/null; sleep 0.6; }
  : > "$SRVLOG"
  taskset -c "$CPUS" env ANOTIFY_ADDR=":$PORT" ANOTIFY_DB="$DB" ANOTIFY_LOG_FORMAT=json \
    ANOTIFY_LOG_LEVEL=error "$BIN" >>"$SRVLOG" 2>&1 &
  echo $! > "$PIDF"
  for _ in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20 21 22 23 24 25 26 27 28 29 30; do
    curl -sf "$BASE/health" >/dev/null 2>&1 && return 0
    sleep 0.2
  done
  echo "服务起不来，见 $SRVLOG" >&2; return 1
}
trap '[ -f "$PIDF" ] && kill "$(cat "$PIDF")" 2>/dev/null' EXIT

DB="$(mktemp -d /tmp/perf-2a/db.XXXXXX)/a.db"
taskset -c "$CPUS" "$ROOT/bin/devseed" -db "$DB" -username "rss$RANDOM" >"$ROOT/logs/devseed-rss-$LABEL.log" 2>&1
SESSION=$(grep -o 'SESSION=[^ ]*' "$ROOT/logs/devseed-rss-$LABEL.log" | cut -d= -f2 | head -1)
SEND_KEY=$(grep -o 'SEND_KEY=[^ ]*' "$ROOT/logs/devseed-rss-$LABEL.log" | cut -d= -f2 | head -1)
NS=$(grep -o 'ns_[A-Za-z0-9]*' "$ROOT/logs/devseed-rss-$LABEL.log" | head -1)
printf '{"source":"notify.phone","type":"perf.write","namespaceId":"%s","payload":{"i":1}}' "$NS" > "$ROOT/logs/evt-rss.json"

echo "concurrency  base_MB  peak_MB  requests  RPS" | tee "$OUT/rss-vs-c.log"
for c in 1 4 8; do
  restart || exit 1
  sleep 0.5
  B=$(base_mb)
  taskset -c "$CPUS" "$ROOT/bin/loadgen" -url "$BASE/v1/events" -method POST -c "$c" -d 5s \
    -body-file "$ROOT/logs/evt-rss.json" -header 'content-type: application/json' \
    -header "Authorization: Bearer $SEND_KEY" >"$OUT/c$c.log" 2>&1
  P=$(peak_mb)
  REQ=$(grep -o '总请求 *[0-9]*' "$OUT/c$c.log" | head -1 | tr -dc 0-9)
  RPS=$(grep -o 'RPS *[0-9.]*' "$OUT/c$c.log" | head -1 | tr -dc '0-9.')
  echo "$c  $B  $P  ${REQ:-?}  ${RPS:-?}" | tee -a "$OUT/rss-vs-c.log"
done
echo "== $LABEL 完成，产物 $OUT =="
