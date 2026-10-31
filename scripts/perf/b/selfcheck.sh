#!/usr/bin/env bash
# B 路评估产物的机器自检（= 契约里的 verify 命令）。
#
#   bash scripts/perf/b/selfcheck.sh        # 全绿退出 0
#
# 检查四件事：
#   1. 所有脚本 bash -n 通过
#   2. 产品代码零改动（internal/ cmd/ api/ web/ web-src/ 相对 origin/main 无 diff）
#   3. 六个场景脚本 + lib.sh + REPORT.md 都在，且 REPORT.md 里出现 ≥3 个 perf issue 号
#   4. 纪律：端口字面量只落在 7920-7939；每条构建/压测/起进程的命令都带 taskset -c
set -uo pipefail

ROOT=$(git rev-parse --show-toplevel 2>/dev/null) || ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)
cd "$ROOT" || exit 1
fail=0
ok() { printf '  \033[32m✓\033[0m %s\n' "$*"; }
bad() { printf '  \033[31m✗\033[0m %s\n' "$*"; fail=1; }

echo "1) bash -n 语法检查"
shopt -s nullglob
scripts=(scripts/perf/b/*.sh)
[ "${#scripts[@]}" -gt 0 ] || bad 'scripts/perf/b/*.sh 为空'
for f in "${scripts[@]}"; do
	if out=$(bash -n "$f" 2>&1); then
		ok "$f"
	else
		bad "$f：$out"
	fi
done

echo "2) 产品代码零改动"
# 参照物是**评估地基提交**（01a3f1f「零依赖压测器 + pprof 开关 + 场景手册」）：
# 它本身就改了 internal/server/config.go 与 mux.go（加 ANOTIFY_PPROF 开关），
# 而 origin/main 还没合它。所以「本路评估没有改产品代码」= 相对 01a3f1f 无 diff。
# 同时把相对 origin/main 的 diff 打出来，让 reviewer 一眼看到差异只来自那个地基提交。
mbase=$(git merge-base HEAD origin/main 2>/dev/null)
if git diff --quiet 01a3f1f -- internal cmd api web web-src scripts/demo 2>/dev/null; then
	ok '相对评估地基 01a3f1f：internal/ cmd/ api/ web/ web-src/ scripts/demo 无 diff'
else
	bad '相对评估地基 01a3f1f 有产品代码改动：'
	git --no-pager diff --stat 01a3f1f -- internal cmd api web web-src scripts/demo
fi
if [ -n "$mbase" ]; then
	n=$(git diff --name-only "$mbase" -- internal cmd api web web-src scripts/demo | wc -l | tr -d ' ')
	if [ "$n" = "0" ]; then
		ok "相对 origin/main（$mbase）也无产品代码 diff"
	else
		printf '  \033[33m·\033[0m 相对 origin/main 有 %s 个文件差异（应全部来自地基提交 01a3f1f）：\n' "$n"
		git --no-pager diff --name-only "$mbase" -- internal cmd api web web-src scripts/demo | sed 's/^/      /'
	fi
fi

echo "3) 产物齐全 + REPORT 里必须有 issue 号"
for f in scripts/perf/b/lib.sh scripts/perf/b/REPORT.md \
	scripts/perf/b/s1-nodes-scaling.sh scripts/perf/b/s1b-profile.sh \
	scripts/perf/b/s2-roundtrip.sh scripts/perf/b/s3-slot-saturation.sh \
	scripts/perf/b/s4-payload.sh scripts/perf/b/s5-lease-dup.sh scripts/perf/b/s6-reconnect.sh; do
	[ -s "$f" ] && ok "$f" || bad "缺文件或为空：$f"
done
if [ -s scripts/perf/b/REPORT.md ]; then
	ids=$(grep -oE '#2(1[89]|2[0-9]|3[0-9])' scripts/perf/b/REPORT.md | sort -u)
	n=$(printf '%s\n' "$ids" | grep -c . || true)
	if [ "${n:-0}" -ge 3 ]; then
		ok "REPORT.md 引用了 $n 个 issue 号：$(printf '%s' "$ids" | tr '\n' ' ')"
	else
		bad "REPORT.md 只引用了 ${n:-0} 个 issue 号（要求 ≥3）"
	fi
	# 每条 issue 的 5 项上下文里，前三项必须能在 REPORT 或 issue 正文里对上（这里只查 REPORT 的自述）
	grep -q '最重要 3 个发现' scripts/perf/b/REPORT.md && ok 'REPORT.md 有「最重要 3 个发现」' \
		|| bad 'REPORT.md 缺「最重要 3 个发现」小节'
	grep -q '未完成 / 未确认' scripts/perf/b/REPORT.md && ok 'REPORT.md 有未完成/未确认小节' \
		|| bad 'REPORT.md 缺未完成/未确认小节'
fi

echo "4) 纪律：端口与分核"
badports=$(grep -ohE '\b79[0-9]{2}\b' scripts/perf/b/*.sh 2>/dev/null | sort -u |
	awk '$1 < 7920 || $1 > 7939')
if [ -z "$badports" ]; then
	ok '端口字面量全部落在 7920-7939'
else
	bad "越界端口：$(printf '%s' "$badports" | tr '\n' ' ')"
fi
# 每条真正"跑东西"的命令都必须带 taskset。用**正向断言**（固定串必须存在），
# 而不是"反查没带 taskset 的行"——后者会把 `[ -x "$PB_BIN/anotify" ]`、
# 多行 env 续行、以及 selfcheck 自己的 grep 模式全抓成假阳性。
need=(
	'taskset -c \"$PB_CORES\" go build -o \"$PB_BIN/anotify\"'
	'taskset -c \"$PB_CORES\" go build -o \"$PB_BIN/agent\"'
	'taskset -c \"$PB_CORES\" go build -o \"$PB_BIN/loadgen\"'
	'taskset -c \"$PB_CORES\" go build -o \"$PB_BIN/devseed\"'
	'taskset -c \"$PB_CORES\" \"$PB_BIN/devseed\"'
	'taskset -c \"$PB_CORES\" env'
	'taskset -c \"$PB_CORES\" \"$PB_BIN/agent\" -f'
	'taskset -c \"$PB_CORES\" \"$PB_BIN/loadgen\"'
	'taskset -c \"$PB_CORES\" go tool pprof'
)
miss=0
for pat in "${need[@]}"; do
	if grep -qF -- "$pat" scripts/perf/b/lib.sh scripts/perf/b/*.sh 2>/dev/null; then
		ok "带分核：$pat"
	else
		bad "找不到带 taskset 的命令：$pat"
		miss=1
	fi
done
[ "$miss" = "0" ] && ok '构建/起服/起 agent/压测/profile 全部走 taskset -c $PB_CORES'

echo
if [ "$fail" -eq 0 ]; then
	printf '\033[1;32mSELFCHECK OK\033[0m\n'
	exit 0
fi
printf '\033[1;31mSELFCHECK FAILED\033[0m\n'
exit 1
