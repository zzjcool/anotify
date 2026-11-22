#!/usr/bin/env bash
# #542 实验驱动：矩阵内部并发 × 负载型假红
#
# 4 种配置 × 每格 3 次全量 `--parallel`，逐格记录：总墙钟、失败套件数、失败套件名、
# 失败套件单跑复验是否绿。数据每跑完一格就 append 一行 TSV —— 中途被打断不丢进度，
# 重跑时已存在的 (配置, 第几次) 会跳过（FORCE=1 覆盖）。
#
# 用法：
#   ./scripts/e2e/experiments/parallel-matrix.sh              # 全部 12 格，按轮次交错 A B C D
#   ./scripts/e2e/experiments/parallel-matrix.sh B C         # 只跑指定配置
#   FORCE=1 ./scripts/e2e/experiments/parallel-matrix.sh A   # 重跑已存在的格子
#
# 配置：
#   A 现状              E2E_PARALLEL=0（HTTP 类全并行 + Chrome 类并发 4，与历史行为逐字一致）
#   B 封顶 nproc/2      E2E_PARALLEL=$((nproc/2))
#   C 固定 4            E2E_PARALLEL=4
#   D 重套件串行收尾    E2E_PARALLEL=0 + E2E_SERIAL_TAIL=capacity,routing,vars_ui
#
# 为什么要「按轮次交错」而不是「A 跑完 3 次再跑 B」：交错能把「机器越跑越热/越脏」
# 摊到四种配置上；顺序跑会把系统漂移全记到最后一个配置头上。
#
# 出口：TSV 在 docs/reports/e2e-parallel-experiment.data.tsv；
#       每次运行的完整日志在 .e2e-bin/experiment-logs/（构建产物，不入库）。
set -uo pipefail
cd "$(dirname "$0")/../../.." # 仓库根
ROOT=$(pwd)
export PATH=/usr/local/go/bin:${PATH}
export GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto

DATA=${DATA:-docs/reports/e2e-parallel-experiment.data.tsv}
LOGDIR=${LOGDIR:-.e2e-bin/experiment-logs}
RUNS=${RUNS:-3}
NPROC=$(nproc)
mkdir -p "$LOGDIR" "$(dirname "$DATA")"

# 表头（只写一次）：列顺序是契约，报告里的表由它生成。
if [ ! -f "$DATA" ]; then
	printf '%s\n' \
		'config	run	utc	wall_s	inner_s	rc	failed_n	failed_suites	reverify	load1_before	nproc' >"$DATA"
fi

config_env() {
	case "$1" in
	A) CFG_PARALLEL=0; CFG_TAIL="" ;;
	B) CFG_PARALLEL=$((NPROC / 2)); CFG_TAIL="" ;;
	C) CFG_PARALLEL=4; CFG_TAIL="" ;;
	D) CFG_PARALLEL=0; CFG_TAIL="capacity,routing,vars_ui" ;;
	*) echo "未知配置 $1（只认 A/B/C/D）" >&2; return 2 ;;
	esac
	return 0
}

# 自己撞自己是最贵的错误：另有一份矩阵在跑时，本次的墙钟和假红全部不可信。
# 所以开跑前**硬等**到本机没有别的矩阵（chrome/headless_shell 归零 + 没有别的 parallel_runner）。
wait_idle() {
	local deadline=$((SECONDS + ${IDLE_WAIT:-600})) chromes runners
	while :; do
		# pgrep -c 在「0 个匹配」时仍然打印 0、但退出码 1：不能 `|| echo 0`（那会得到两行）。
		chromes=$(pgrep -fc 'chrome|headless_shell' 2>/dev/null || true)
		runners=$(pgrep -fc 'parallel_runner.mjs' 2>/dev/null || true)
		chromes=${chromes:-0}
		runners=${runners:-0}
		if [ "$chromes" = "0" ] && [ "$runners" = "0" ]; then return 0; fi
		if [ "$SECONDS" -ge "$deadline" ]; then
			echo "❌ 等了 ${IDLE_WAIT:-600}s 本机仍有别的矩阵在跑（chrome=$chromes runners=$runners）—— 拒绝在叠加负载下取证" >&2
			return 1
		fi
		sleep 10
	done
}

configs=("$@")
if [ ${#configs[@]} -eq 0 ]; then configs=(A B C D); fi

for round in $(seq 1 "$RUNS"); do
	for cfg in "${configs[@]}"; do
		if [ -n "${FORCE:-}" ]; then :; elif awk -F'\t' -v c="$cfg" -v r="$round" '$1==c && $2==r {found=1} END{exit !found}' "$DATA" 2>/dev/null; then
			echo "· 跳过 $cfg 第 $round 次（已有数据）"
			continue
		fi
		config_env "$cfg" || exit 2
		E2E_PARALLEL=$CFG_PARALLEL
		E2E_SERIAL_TAIL=$CFG_TAIL

		wait_idle || exit 1

		load1=$(cut -d' ' -f1 /proc/loadavg)
		log="$LOGDIR/${cfg}-run${round}.log"
		echo "▶ 配置 $cfg 第 $round/$RUNS 次：E2E_PARALLEL=${E2E_PARALLEL:-0} E2E_SERIAL_TAIL=${E2E_SERIAL_TAIL:-（无）} load1=$load1"
		t0=$(date +%s%3N)
		E2E_PARALLEL="$E2E_PARALLEL" E2E_SERIAL_TAIL="$E2E_SERIAL_TAIL" \
			timeout 2400 ./scripts/e2e/run_all.sh --parallel >"$log" 2>&1
		rc=$?
		t1=$(date +%s%3N)
		wall=$(awk -v a="$t0" -v b="$t1" 'BEGIN{printf "%.1f", (b-a)/1000}')
		inner=$(sed -n 's/^  总耗时: \([0-9.]*\)s.*/\1/p' "$log" 2>/dev/null | head -1)
		: "${inner:=0}"
		failed=$(grep -m1 '^  失败套件:' "$log" 2>/dev/null | sed 's/^  失败套件: //')
		if [ -z "$failed" ]; then fn=0; else fn=$(printf '%s' "$failed" | tr ',' '\n' | grep -c .); fi

		# 失败套件单跑复验：这是「负载型假红」与真回归的唯一区分手段。
		reverify=""
		if [ "$fn" -gt 0 ]; then
			for s in $(printf '%s' "$failed" | tr ',' ' '); do
				s=$(printf '%s' "$s" | tr -d ' ')
				[ -n "$s" ] || continue
				timeout 900 ./scripts/e2e/run_all.sh "$s" >"$log.reverify.$s" 2>&1
				if [ $? -eq 0 ]; then r=green; else r=RED; fi
				reverify="${reverify}${s}:${r} "
			done
			reverify=$(printf '%s' "$reverify" | sed 's/ $//')
		fi

		printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' \
			"$cfg" "$round" "$(date -u +%FT%TZ)" "$wall" "$inner" "$rc" "$fn" \
			"${failed:-—}" "${reverify:-—}" "$load1" "$NPROC" >>"$DATA"
		echo "  → 墙钟 ${wall}s（矩阵内 ${inner}s）rc=$rc 失败 ${fn} 个：${failed:-无}${reverify:+ ｜ 单跑复验: $reverify}"
	done
done
echo "✅ 数据：$DATA"
