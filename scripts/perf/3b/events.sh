#!/usr/bin/env bash
# #243 测量：GET /v1/console/events?limit=500 —— 响应字节 / CPU 千请求 / RPS。
#
#   PORT=8043 bash scripts/perf/3b/events.sh /tmp/p3b/bin/anotify-base base
#   PORT=8043 bash scripts/perf/3b/events.sh /tmp/p3b/bin/anotify-new  new
#
# 灌事件走 POST /v1/events（API Key），与 issue #243 的复现口径一致；
# payload 形状也照抄（title + body 的填充文本），这样响应体量级可比。
# identity 视角 = 旧行为（不发 Accept-Encoding，Go transport 不会自动加）；
# gzip 视角 = 浏览器 / undici 的真实路径。
set -uo pipefail
. "$(dirname "$0")/lib.sh"

BIN=${1:?用法: events.sh <server 二进制> <tag>}
TAG=${2:?缺少 tag}
PORT=${PORT:-8043}
EVENTS=${EVENTS:-550}
REPS=${REPS:-3}
BASE="http://127.0.0.1:$PORT"
REPORT=$P3B_OUT/events-$TAG.txt

p3b_port "$PORT"
p3b_ensure_bins
mkdir -p "$P3B_OUT"
: >"$REPORT"

WORK=$(mktemp -d "${TMPDIR:-/tmp}/p3b-ev-XXXXXX")
DB=$WORK/a.db

cleanup() {
	[ -n "${SRVPID:-}" ] && kill -9 "$SRVPID" 2>/dev/null
	[ -n "${LG:-}" ] && kill -9 "$LG" 2>/dev/null
	wait 2>/dev/null
	rm -rf "$WORK"
}
trap cleanup EXIT

p3b_say "起 server（$TAG）port=$PORT bin=$BIN"
SRVPID=$(p3b_start_server "$BIN" "$PORT" "$DB" "$WORK/server.log")
p3b_log "server pid=$SRVPID loadavg=$(awk '{print $1" "$2" "$3}' /proc/loadavg)"

p3b_seed "$DB"

p3b_say "灌 $EVENTS 条事件（POST /v1/events，串行）"
t0=$(date +%s)
for i in $(seq 1 "$EVENTS"); do
	curl -sS -o /dev/null -X POST "$BASE/v1/events" \
		-H "Authorization: Bearer $SEND_KEY" -H 'content-type: application/json' \
		-d "{\"source\":\"perfseed\",\"type\":\"perf.event\",\"namespaceId\":\"$NS\",\"payload\":{\"i\":$i,\"title\":\"perf seed event $i — filler payload for realistic row width\",\"body\":\"lorem ipsum dolor sit amet consectetur adipiscing elit sed do eiusmod tempor\"}}" \
		|| p3b_die "灌事件失败 i=$i"
	if [ $((i % 100)) -eq 0 ]; then p3b_log "已灌 $i/$EVENTS（$(( $(date +%s) - t0 ))s）"; fi
done
tseed=$(( $(date +%s) - t0 ))
rows=$(sqlite3 "$DB" "select count(*) from events;")
printf '灌入 %s 条，%ss（%.1f rps）；events 表行数=%s\n' "$EVENTS" "$tseed" \
	"$(awk -v n="$EVENTS" -v t="$tseed" 'BEGIN{ if(t>0) printf "%.1f", n/t; else print "NA"}')" "$rows" | tee -a "$REPORT"

p3b_say "参照 /health"
taskset -c "$P3B_CORES" "$P3B_BIN/loadgen" -url "$BASE/health" -c 32 -d 3s 2>&1 | tee "$P3B_OUT/events-$TAG-health.log" | sed -n '4,5p' | tee -a "$REPORT"

COOKIE="Cookie: anotify_session=$SESSION"
evurl() { echo "$BASE/v1/console/events?namespaceId=$NS&limit=$1"; }

for lim in 500 100; do
	echo "=== limit=$lim ===" | tee -a "$REPORT"
	for r in $(seq 1 "$REPS"); do
		echo "--- rep $r（loadavg $(awk '{print $1}' /proc/loadavg)）---" >>"$REPORT"
		p3b_say "limit=$lim c=8 rep$r"
		p3b_load "$(evurl $lim)" "$COOKIE" identity 8 5s "$P3B_OUT/events-$TAG-id-l$lim-c8-r$r.log" | tee -a "$REPORT"
		p3b_load "$(evurl $lim)" "$COOKIE" gzip 8 5s "$P3B_OUT/events-$TAG-gz-l$lim-c8-r$r.log" | tee -a "$REPORT"
	done
	if [ "$lim" = "500" ]; then
		for r in 1 2; do
			p3b_say "limit=$lim c=16 rep$r"
			p3b_load "$(evurl $lim)" "$COOKIE" identity 16 5s "$P3B_OUT/events-$TAG-id-l$lim-c16-r$r.log" | tee -a "$REPORT"
			p3b_load "$(evurl $lim)" "$COOKIE" gzip 16 5s "$P3B_OUT/events-$TAG-gz-l$lim-c16-r$r.log" | tee -a "$REPORT"
		done
	fi
done

p3b_say "响应字节数"
for lim in 500 100; do
	plain=$(curl -s -o /dev/null -w '%{size_download}' -H "$COOKIE" -H 'Accept-Encoding: identity' "$(evurl $lim)")
	wire=$(curl -s -o /dev/null -w '%{size_download}' -H "$COOKIE" -H 'Accept-Encoding: gzip' "$(evurl $lim)")
	printf 'limit=%-3s identity %s B；gzip 线上字节 %s B\n' "$lim" "$plain" "$wire" | tee -a "$REPORT"
done
msize=$(curl -s -o /dev/null -w '%{size_download}' -H "Authorization: Bearer $SEND_KEY" -H 'Accept-Encoding: identity' "$BASE/v1/events?namespaceId=$NS&limit=500")
mgzip=$(curl -s -o /dev/null -w '%{size_download}' -H "Authorization: Bearer $SEND_KEY" -H 'Accept-Encoding: gzip' "$BASE/v1/events?namespaceId=$NS&limit=500")
printf 'GET /v1/events?limit=500（机器入口，无缩进）identity %s B；gzip %s B\n' "$msize" "$mgzip" | tee -a "$REPORT"

prof() { # $1=ae $2=suffix
	local ae=$1 sfx=$2
	taskset -c "$P3B_CORES" "$P3B_BIN/loadgen" -url "$(evurl 500)" -header "$COOKIE" \
		-header "Accept-Encoding: $ae" -c 8 -d 22s >"$P3B_OUT/events-$TAG-prof-load$sfx.log" 2>&1 &
	LG=$!
	sleep 2
	curl -s -o "$P3B_OUT/events-$TAG$sfx.pprof" "$BASE/debug/pprof/profile?seconds=15"
	kill -9 "$LG" 2>/dev/null
	taskset -c "$P3B_CORES" go tool pprof -top -nodecount=30 "$BIN" "$P3B_OUT/events-$TAG$sfx.pprof" \
		>"$P3B_OUT/events-$TAG.pprof-top$sfx.txt" 2>&1
}

p3b_say "profile：limit=500 identity 8 并发（看 ListEvents / appendCompact 的占比）"
prof identity ""
{ grep -E "Total samples" "$P3B_OUT/events-$TAG.pprof-top.txt"
  grep -E "consoleHandler\).events|DB\).ListEvents|appendCompact|structEncoder|sortSlice|mergeEvents|writeJSON" \
	"$P3B_OUT/events-$TAG.pprof-top.txt" || true; } | tee -a "$REPORT"

p3b_say "profile：limit=500 gzip 8 并发（看 flate 自己吃掉多少）"
prof gzip "-gz"
{ grep -E "Total samples" "$P3B_OUT/events-$TAG.pprof-top-gz.txt"
  grep -E "flate|gzip|appendCompact|consoleHandler\).events" "$P3B_OUT/events-$TAG.pprof-top-gz.txt" || true; } | tee -a "$REPORT"
echo "（完整 top 见 $P3B_OUT/events-$TAG.pprof-top{,-gz}.txt）" | tee -a "$REPORT"

p3b_say "TAG=$TAG 完成"
cat "$REPORT"
