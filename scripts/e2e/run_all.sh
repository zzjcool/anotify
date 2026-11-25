#!/usr/bin/env bash
# Anotify 端到端测试总编排：机器级互斥 → 环境准备 → 构建二进制 → 委托 Node 并行执行器
# 用法：
#   ./scripts/e2e/run_all.sh                 # 全量（并行模式）
#   ./scripts/e2e/run_all.sh --serial        # 全量（串行模式）
#   ./scripts/e2e/run_all.sh auth_flow        # 只跑指定套件（串行）
#   ./scripts/e2e/run_all.sh --serial auth_flow  # 同上（显式串行）
#   ./scripts/e2e/run_all.sh --serial node_v4    # v4 真实节点核心链（真 anode 二进制）
# 环境变量（锁的口径与风险见 E2E_TESTING.md「机器级互斥」）：
#   E2E_CONCURRENCY  Chrome 类套件并发上限（默认 4）
#   E2E_LOCK=0       跳过机器级锁（**只在确认没有共享资源冲突时**用；并发会撞端口/DB）
#   E2E_LOCK_WAIT    等锁上限秒数（默认 900）；超时 exit 75 并打印持锁者，绝不静默并发
#   E2E_LOCK_PROBE   只拿锁不干活：持有 N 秒后退出（自测用，它跑不了任何套件 ⇒ 不是绕过口）
set -uo pipefail
cd "$(dirname "$0")/../.." # 仓库根
ROOT=$(pwd)
export GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto

# 机器级互斥（#529）：同机两份矩阵并发时，findFreePort 的「探测→使用」窗口会让两个矩阵
# 都通过探测（随后 bind 失败或连到别人的 server），叠加共享的 /tmp、浏览器 profile 与 CPU，
# 症状是**随机套件红**（capacity/routing/vars_ui 都中过）。那种红最贵：人学会「红了就重跑」，
# 直到某次真红也被当成抖动。所以串行化的粒度是**矩阵**而不是套件 —— 一个矩阵拿锁，它内部的
# --parallel 套件照旧并发；shard.mjs 的每个分片各是一个矩阵（不同机器上互不影响）。
# 锁由内核持有：进程退出/崩溃（含 kill -9）时 flock 立刻释放，因此不会留死锁；子命令一律
# 9>&-，锁的生命周期严格等于本进程，残留子进程不许替我们延长它。
LOCK_FILE="${TMPDIR:-/tmp}/anotify-e2e-$(id -u 2>/dev/null).lock"
LOCK_WAIT="${E2E_LOCK_WAIT:-900}"
lockHolder() {
	local out pid
	out=$(head -c 400 "$LOCK_FILE" 2>/dev/null | tr '\n' ' ')
	[ -n "$out" ] || return
	pid=${out#*pid=}
	pid=${pid%% *}
	# 崩溃残留的文本不该被读成「有人正在跑」：内核早已释放，下一个持有者会覆盖这行。
	[ -n "$pid" ] && ! ps -p "$pid" >/dev/null 2>&1 && out="$out（pid $pid 已不在：锁已由内核释放，这行只是残留文本）"
	printf '%s' "$out"
}
# 锁文件常驻、不删：删掉会让「已打开的等待者」与「新建者」指向不同 inode ⇒ 互斥静默失效。
# 逃生口必须显式：E2E_LOCK 的默认值永远是「上锁」，静默降级成并发是不允许的。
if [ "${E2E_LOCK:-1}" = "0" ]; then
	echo "⚠️  E2E_LOCK=0：跳过机器级 e2e 互斥 —— 同机另一份矩阵会与本份抢端口/DB/CPU（随机假红就是这么来的）" >&2
else
	# 拿不到锁也绝不降级成并发：缺 flock / 锁文件打不开 / 等超时，三种都是硬失败。
	command -v flock >/dev/null 2>&1 || { echo "❌ 缺少 flock（util-linux）⇒ 无法保证机器级互斥；装它，或显式 E2E_LOCK=0 接受并发风险" >&2; exit 1; }
	case "$LOCK_WAIT" in '' | *[!0-9]*) echo "❌ E2E_LOCK_WAIT 必须是整秒数（现在是 '$LOCK_WAIT'）" >&2; exit 2 ;; esac
	# >> 而不是 >：等待者不许截断持锁者写下的诊断行（拿到锁之后才覆盖它）。
	exec 9>>"$LOCK_FILE" || { echo "❌ 打不开锁文件 $LOCK_FILE（TMPDIR 可写吗？）" >&2; exit 1; }
	if ! flock -n 9; then
		echo "▶ 本机已有 e2e 矩阵在跑（holder: $(lockHolder)）；等锁最多 ${LOCK_WAIT}s…" >&2
		if ! flock -w "$LOCK_WAIT" 9; then
			echo "❌ 等锁 ${LOCK_WAIT}s 超时，另一个 e2e 矩阵仍占着 $LOCK_FILE（holder: $(lockHolder)）；等它结束或换台机器，确认无共享资源冲突时才可显式 E2E_LOCK=0 强行并发" >&2
			exit 75
		fi
		echo "  ✅ 等到锁（本进程已跑 ${SECONDS}s）" >&2
	fi
	# pid + start 是排障时唯一能回答「谁在占着」的字段（`ps -o lstart= -p <pid>` 可续查）。
	: >"$LOCK_FILE"
	printf 'pid=%s start=%s cwd=%s suites=%s\n' "$$" "$(date -Is)" "$ROOT" "${*:-（全量）}" >&9
	echo "🔒 机器级 e2e 锁：$LOCK_FILE（pid=$$）"
fi
# 自测钩子（scripts/e2e/lock_selftest.sh 用）：拿锁后只持有 N 秒就退出，跑不了任何套件 ——
# 所以它不是「绕过并发保护」的口子（真跑矩阵时没人设这个变量）。
if [ -n "${E2E_LOCK_PROBE:-}" ]; then
	echo "lock-probe: lock=$LOCK_FILE holder=$(lockHolder)"
	sleep "${E2E_LOCK_PROBE:-0}" 9>&- 2>/dev/null || true
	echo "lock-probe: released"
	exit 0
fi

echo "=========================================="
echo " Anotify E2E 全量测试"
echo "=========================================="

# 1. 前置：dist 缺失就重建（embed 依赖，gitignore 产物）；手动删过 web/ 时先恢复
#    git 追踪的源文件（partials.js/tokens.css 等，sitegen 不生成它们）。
NEED_FE=0
if [ ! -d "internal/server/dist" ] || [ -z "$(ls -A internal/server/dist 2>/dev/null)" ]; then
	NEED_FE=1
fi
if [ ! -f "web/partials.js" ]; then
	NEED_FE=1
	git checkout HEAD -- web 9>&- 2>/dev/null || true
fi
if [ "$NEED_FE" = "1" ]; then
	echo "▶ 生成前端产物（dist/web 不完整）…"
	make fe 9>&- || { echo "❌ make fe 失败"; exit 1; }
	echo "  ✅ 前端产物就绪"
fi

# 1b. 前置：dist 必须是**这次源**的产物。陈旧就硬失败、不自动重建 —— 自动重建会把
#     「你忘了 make fe」重新变成静默（重建后永远绿），也让 e2e 启动时间翻倍。
#     三次假绿的历史见 DEVELOPMENT.md「改了 web/ 源码后跑 make e2e 用的是旧产物」。
if ! node scripts/dist-stamp.mjs --check 9>&-; then
	echo "❌ dist 与 web-src 不一致（前端产物陈旧）→ 先 make fe，再跑 e2e" >&2
	exit 1
fi

# 2. 构建三个真实产物（没有 mock）：anotify（cmd/server，带 embeddist）/ devseed（播种身份/Key）
#    / anode（v4 节点）。v4 已退役 cmd/agent 与旧协议（#331/#332/#333）：节点侧只有 cmd/anode，
#    帧契约唯一来源是 internal/protocol，旧 AGENT_BIN 随之改名 ANODE_BIN。
echo "▶ 构建二进制…"
mkdir -p .e2e-bin
go build -tags embeddist -o .e2e-bin/anotify ./cmd/server 9>&- || { echo "❌ server 构建失败"; exit 1; }
go build -o .e2e-bin/devseed ./cmd/devseed 9>&- || { echo "❌ devseed 构建失败"; exit 1; }
go build -o .e2e-bin/anode ./cmd/anode 9>&- || { echo "❌ anode 构建失败"; exit 1; }
export ANOTIFY_BIN="$ROOT/.e2e-bin/anotify" DEVSEED_BIN="$ROOT/.e2e-bin/devseed" ANODE_BIN="$ROOT/.e2e-bin/anode"
echo "  ✅ 二进制就绪"

# 3. 委托 Node 并行执行器（go test 已拆到 `make test`，不再阻塞 e2e）。外面套临时目录守卫
#    （#326）：/tmp 是 tmpfs，守卫给被包命令私有 TMPDIR 并断言没有 anotify-* 残留（见 E2E_TESTING.md）。
echo ""
node scripts/check-tmp-leaks.mjs --lint 9>&- || exit $?
node scripts/check-tmp-leaks.mjs -- node scripts/e2e/parallel_runner.mjs "$@" 9>&-
exit $?
