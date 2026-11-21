set -uo pipefail
cd "$(dirname "$0")/../.."
ROOT=$(pwd)
RUN="$ROOT/scripts/e2e/run_all.sh"
WORK=$(mktemp -d "${TMPDIR:-/tmp}/.anotify-lock-selftest.XXXXXX") || exit 1
trap 'rm -rf "$WORK"' EXIT
export TMPDIR="$WORK"
LOCK="$TMPDIR/anotify-e2e-$(id -u).lock"
FAILED=0
ok() { echo "  ✅ $1"; }
bad() { echo "  ❌ $1" >&2; FAILED=$((FAILED + 1)); }
expect() {
	if [ "$3" = "$2" ]; then ok "$1"; else bad "$1（期望 exit=$2，实际 exit=$3）"; fi
}
has() {
	case "$2" in
	*"$3"*) ok "$1" ;;
	*) bad "$1（输出里没有「$3」）" ;;
	esac
}
hasRe() {
	if printf '%s' "$2" | grep -qE "$3"; then ok "$1"; else bad "$1（输出里没有匹配 /$3/ 的行）"; fi
}
holderPid() { sed -n 's/.*pid=\([0-9][0-9]*\).*/\1/p' "$LOCK" 2>/dev/null | head -1; }
waitWritten() {
	local i
	for i in $(seq 1 300); do
		[ -n "$(holderPid)" ] && return 0
		sleep 0.02
	done
	return 1
}
noWait() {
	if [ "$2" -le 1 ]; then ok "$1（${2}s）"; else bad "$1（等了 ${2}s，锁没被内核释放）"; fi
}

echo "▶ e2e 机器级锁自测（私有 TMPDIR=$TMPDIR，不碰本机真实 e2e 锁）"

echo "① 无争用：立刻拿锁（锁就在 TMPDIR 里）"
out=$(E2E_LOCK_PROBE=0 bash "$RUN" 2>&1)
rc=$?
expect "① exit 0" 0 "$rc"
has "① 打印锁路径" "$out" "lock-probe: lock=$LOCK"
has "① 打印 🔒 横幅（含持锁 pid）" "$out" "🔒 机器级 e2e 锁：$LOCK（pid="

echo "② 争用：等待后成功跑（不是静默并发）"
: >"$LOCK"
E2E_LOCK_PROBE=2 bash "$RUN" >"$WORK/holder2.log" 2>&1 &
h=$!
waitWritten || bad "② 持锁进程没写身份行"
out=$(E2E_LOCK_WAIT=120 E2E_LOCK_PROBE=0 bash "$RUN" 2>&1)
rc=$?
wait "$h" 2>/dev/null
expect "② exit 0" 0 "$rc"
has "② 报出「已有矩阵在跑 + holder」" "$out" "本机已有 e2e 矩阵在跑（holder: pid="
has "② 报出等到锁" "$out" "✅ 等到锁"

echo "③ 超时：等不到就 exit 75，并打印持锁者的 pid/start"
: >"$LOCK"
E2E_LOCK_PROBE=6 bash "$RUN" >"$WORK/holder3.log" 2>&1 &
h=$!
waitWritten || bad "③ 持锁进程没写身份行"
hp=$(holderPid)
out=$(E2E_LOCK_WAIT=1 E2E_LOCK_PROBE=0 bash "$RUN" 2>&1)
rc=$?
kill "$h" 2>/dev/null
wait "$h" 2>/dev/null
expect "③ exit 75（EX_TEMPFAIL，不静默并发）" 75 "$rc"
hasRe "③ 打印持锁者 pid + start" "$out" "holder: pid=$hp start=[0-9]"
hasRe "③ holder 身份字段齐全" "$out" "holder: pid=[0-9]+ start=[^ ]+ cwd="
has "③ 打印等锁上限与超时事实" "$out" "等锁 1s 超时"
has "③ 给出逃生口提示" "$out" "E2E_LOCK=0"

echo "④ 崩溃不留锁：kill -9 持锁进程后立刻可再取"
: >"$LOCK"
E2E_LOCK_PROBE=10 bash "$RUN" >"$WORK/holder4.log" 2>&1 &
h=$!
waitWritten || bad "④ 持锁进程没写身份行"
dead=$(holderPid)
kill -9 "$h" 2>/dev/null
wait "$h" 2>/dev/null
t0=$SECONDS
out=$(E2E_LOCK_WAIT=1 E2E_LOCK_PROBE=0 bash "$RUN" 2>&1)
rc=$?
noWait "④ kill -9 之后没有等锁" $((SECONDS - t0))
expect "④ exit 0" 0 "$rc"
reborn=$(holderPid)
if [ -n "$reborn" ] && [ "$reborn" != "$dead" ]; then
	ok "④ 残留身份被新持有者覆盖（$dead → $reborn）"
else
	bad "④ 锁文件里的 pid 还是 $dead（新持有者没覆盖）"
fi

echo "⑤ 残留文本：不在的 pid 要标注，外部 flock 持锁也算占用"
printf 'pid=%s start=%s cwd=%s suites=%s\n' 999999 '2000-01-01T00:00:00+00:00' "$ROOT" '（残留）' >"$LOCK"
flock -x "$LOCK" sleep 3 &
h=$!
sleep 0.2
out=$(E2E_LOCK_WAIT=1 E2E_LOCK_PROBE=0 bash "$RUN" 2>&1)
rc=$?
kill "$h" 2>/dev/null
wait "$h" 2>/dev/null
expect "⑤ exit 75（非 run_all 的持锁者也认）" 75 "$rc"
has "⑤ 标注 pid 已不在" "$out" "pid 999999 已不在"

echo "⑥ 逃生口：E2E_LOCK=0 有人持锁时也能跑，但必须吵一句"
: >"$LOCK"
E2E_LOCK_PROBE=4 bash "$RUN" >"$WORK/holder6.log" 2>&1 &
h=$!
waitWritten || bad "⑥ 持锁进程没写身份行"
t0=$SECONDS
out=$(E2E_LOCK=0 E2E_LOCK_PROBE=0 bash "$RUN" 2>&1)
rc=$?
noWait "⑥ E2E_LOCK=0 不等锁" $((SECONDS - t0))
kill "$h" 2>/dev/null
wait "$h" 2>/dev/null
expect "⑥ exit 0" 0 "$rc"
has "⑥ 打印并发风险说明" "$out" "E2E_LOCK=0：跳过机器级 e2e 互斥"

echo "⑦ 缺 flock：硬失败，不静默降级成并发"
mkdir -p "$WORK/nobin"
out=$(PATH="$WORK/nobin" /bin/bash "$RUN" 2>&1)
rc=$?
expect "⑦ exit 1" 1 "$rc"
has "⑦ 说明缺 flock 并给逃生口" "$out" "缺少 flock（util-linux）"

echo "⑧ 粒度：锁只在矩阵入口，没落到执行器/套件层"
if grep -rq 'flock' "$ROOT/scripts/e2e/parallel_runner.mjs" "$ROOT/scripts/e2e/lib" "$ROOT/scripts/e2e/suites"; then
	bad "⑧ 执行器/套件层出现了锁 —— --parallel 的 N 个套件会被串行化"
else
	ok "⑧ 执行器/套件层没有锁（--parallel 仍并发）"
fi

if [ "$FAILED" = 0 ]; then
	echo "✅ e2e 机器级锁自测通过（8 项；锁=$LOCK）"
	exit 0
fi
echo "❌ e2e 机器级锁自测失败：$FAILED 项" >&2
exit 1
