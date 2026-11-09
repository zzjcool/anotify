package artifact

// 本文件是内置模板 agent.tui 的正文：把一个任务交给 **hc 托管**的 TUI agent。
// 它把脚本本体从 templates.go 的模板表里挪出来，因为这一段比其它模板长一个量级。
//
// # 为什么是「一条参数化模板」而不是每家一个模板
//
// 对 hc 来说 agent 就是一个参数（它认 argv，`hc get` 的 foreground.argv）。所以
// `agent` 是一个 select（枚举，不是自由字符串）：填一个 hc 不认识的程序名，
// status/wait 立刻退化，编排层就只剩「瞎等超时」这一条路。
//
// # 三条纪律（改脚本前先读）
//
//  1. **不解析屏幕**：结果只走 emit（agent 自己调 `anode emit`），屏幕只进档案
//     （`hc read --lines 400`）。脚本不做任何结果抽取。
//  2. **token 不进 argv、不进提示词正文**：v4 的任务身份只有三个环境变量
//     （ANOTIFY_EMIT_ENDPOINT / ANOTIFY_TASK_TOKEN / ANOTIFY_TASK_ID），脚本把它们
//     写进 `<工作目录>/.anotify/task.env`（0600）——提示词里只出现这个**路径**，
//     agent 用 `. .anotify/task.env` 把身份读进自己的 shell。hc 的终端屏幕是人能
//     随便读的档案，token 一旦进了正文，归档里就有了明文。
//  3. **模板层零插值**：脚本正文是固定的，用户填的值全在 ANOTIFY_PARAM_* 里；
//     `{{task8}}` / `{{seq}}` 这类占位符由**这个脚本**（节点任务期）展开——
//     产物 digest 因此与「谁在哪天派了什么」无关。
//
// # 已定的取舍
//
//   - 一任务一会话（不做 reuse）：会话名 `anotify-<task8>-<seq>` 天然唯一，
//     两条任务的屏幕不会混在同一个终端里。
//   - `wait` 的 timeout **不是失败**（hc 手册原话：a check-in, not failure），
//     循环接着等；`timeout_sec` 才是整个任务的上限。
//   - `blocked`（等人/等权限）→ 失败 + summary 前缀 `blocked:`；失败/blocked
//     无论 `worktree_cleanup` 取值都保留现场。
//   - hc core 不可达 → summary 前缀 `hc_core_unreachable:`（不改协议；`requires`
//     只负责「没装」）。
const agentTUIScript = `#!/bin/bash
# anode 能力模板 agent.tui：把一个任务交给 hc 托管的 TUI agent。
#
# 三条纪律（改这段脚本前先读）：
#   1. 不解析屏幕：结果只走 emit（agent 自己调 anode emit），屏幕只 hc read 归档。
#   2. token 不进 argv / 不进提示词正文：只写进 <工作目录>/.anotify/task.env（0600），
#      提示词里只出现这个路径；agent 用 . .anotify/task.env 把身份读进环境。
#   3. 模板层零插值：用户填的值都从参数环境变量来，{{task8}} / {{seq}}
#      由这里（节点任务期）展开。
set -uo pipefail

# ---------- 输出纪律 ----------
# stdout 的**第一行**是本次任务的 summary，后面才是屏幕归档；
# 所有进度/诊断一律走 stderr，不许抢 summary。
SUMMARY_PRINTED=0
DETAIL=""
SESSION=""
WT_CREATED=0
wait_out=""

log() { printf '[agent.tui] %s\n' "$*" >&2; }

finish() { # $1 退出码  $2 summary（单行）
  local code="$1" sum="$2"
  if [ "$SUMMARY_PRINTED" -eq 0 ]; then
    SUMMARY_PRINTED=1
    printf '%s\n' "$sum"
    [ -z "$DETAIL" ] || printf '%s\n' "$DETAIL"
  fi
  if [ -n "$SESSION" ]; then
    printf -- '--- hc read %s --lines 400 ---\n' "$SESSION"
    hc read "$SESSION" --lines 400 2>/dev/null || printf '(读不到屏幕：会话可能已被移除)\n'
  fi
  exit "$code"
}

die() { # $1 summary  $2 退出码  $3 详细原因（stderr）
  local sum="$1" code="${2:-1}" detail="${3:-}"
  printf '[agent.tui] %s\n' "$sum" >&2
  [ -z "$detail" ] || printf '[agent.tui] %s\n' "$detail" >&2
  finish "$code" "$sum"
}

# 节点 cancel 时 runner 先 TERM、5s 后 KILL 整个进程组；能到 TERM 就把 agent 的
# 当前 turn 停掉——别把一个「还在 working」的终端留在 hc 里。
on_signal() {
  if [ -n "$SESSION" ]; then hc interrupt "$SESSION" >/dev/null 2>&1; fi
  log "收到终止信号：已 interrupt 会话 ${SESSION:-none}（现场保留）"
  finish 143 "canceled: agent.tui 被终止（会话 ${SESSION:-none} 已 interrupt）"
}
trap on_signal TERM INT

first_line() { printf '%s' "$1" | head -n 1; }
json_field() { printf '%s' "$1" | grep -o "\"$2\":[^,}]*" | head -n 1 | cut -d: -f2- | tr -d '"'; }
count_char() { printf '%s' "$1" | tr -cd "$2" | wc -c | tr -d ' '; }
shell_quote() { local s="$1"; s=${s//\'/\'\\\'\'}; printf "'%s'" "$s"; }

# ---------- 身份（v4 契约变量只有三个，capability.md §4.0） ----------
TASK_ID="${ANOTIFY_TASK_ID:-local}"
# 会话名 / 分支名只收 [a-z0-9-_]：任务 id 可能是 "cron:nightly:1730000000"，
# 直接截 8 位会带冒号 → hc --name 与 git ref 都不收。
TASK8=$(printf '%s' "$TASK_ID" | tr '[:upper:]' '[:lower:]' | tr -c 'a-z0-9-' '-')
TASK8="${TASK8#-}"
TASK8="${TASK8%-}"
TASK8="${TASK8:0:8}"
[ -n "$TASK8" ] || TASK8="notask"
# 工作目录就是本次任务目录（exec 的 cwd）；workDir 声明了就用那个目录。
REPO="$PWD"

# ---------- 参数（值全部来自参数环境变量，这里只做校验） ----------
PROMPT="${ANOTIFY_PARAM_PROMPT:-}"
[ -n "$PROMPT" ] || die "invalid_param: prompt 为空（派发时没给任务正文）" 2

# agent 必须是 hc 认得的那批（它靠命令行认 agent）。
AGENT_OPTIONS="pi claude codex gemini cursor opencode"
AGENT="${ANOTIFY_PARAM_AGENT:-pi}"
case " $AGENT_OPTIONS " in
  *" $AGENT "*) ;;
  *) die "invalid_param: agent=$AGENT 不是 hc 认得的 agent（可选：$AGENT_OPTIONS）" 2 ;;
esac

# agent_args 原样进 hc 模板的 command: 行（单行），所以只挡**格式破坏**：
# 换行会把 frontmatter 拆坏；引号不配对会让命令行被拆错。不做安全过滤——
# 模板作者本来就在写一段跑在节点上的脚本，这里没有新增权限面。
AGENT_ARGS="${ANOTIFY_PARAM_AGENT_ARGS:-}"
case "$AGENT_ARGS" in
  *$'\n'*) die "invalid_param: agent_args 不能含换行（hc 模板的 command 是单行）" 2 ;;
  *$'\r'*) die "invalid_param: agent_args 不能含回车（hc 模板的 command 是单行）" 2 ;;
esac
sq=$(count_char "$AGENT_ARGS" "'")
dq=$(count_char "$AGENT_ARGS" '"')
if [ $(( sq % 2 )) -ne 0 ] || [ $(( dq % 2 )) -ne 0 ]; then
  die "invalid_param: agent_args 的引号不配对（单引号 $sq 个、双引号 $dq 个）" 2
fi

EMIT_NOTES="${ANOTIFY_PARAM_EMIT_NOTES:-}"
SPACE="${ANOTIFY_PARAM_SPACE:-default}"
[ -n "$SPACE" ] || SPACE="default"
BASE="${ANOTIFY_PARAM_BASE:-HEAD}"
[ -n "$BASE" ] || BASE="HEAD"
WORKTREE="${ANOTIFY_PARAM_WORKTREE:-off}"
CLEANUP="${ANOTIFY_PARAM_WORKTREE_CLEANUP:-remove}"
WT_PATH="${ANOTIFY_PARAM_WORKTREE_PATH:-}"
TIMEOUT_SEC="${ANOTIFY_PARAM_TIMEOUT_SEC:-3600}"
case "$TIMEOUT_SEC" in
  ''|*[!0-9]*) die "invalid_param: timeout_sec 必须是正整数，实际 $TIMEOUT_SEC" 2 ;;
esac
[ "$TIMEOUT_SEC" -gt 0 ] || die "invalid_param: timeout_sec 必须大于 0" 2
case "$WORKTREE" in
  off|new|existing) ;;
  *) die "invalid_param: worktree=$WORKTREE（可选 off/new/existing）" 2 ;;
esac
case "$CLEANUP" in
  remove|keep) ;;
  *) die "invalid_param: worktree_cleanup=$CLEANUP（可选 remove/keep）" 2 ;;
esac

# ---------- hc 可用性 ----------
command -v hc >/dev/null 2>&1 || die "hc_missing: 本机没有 hc（herdr-core）；部署期的 requires 应当已经拦下" 1
if ! core_out=$(hc server status --json 2>&1); then
  die "hc_core_unreachable: hc server status 不可用：$(first_line "$core_out")" 1
fi

# ---------- 名字（hc 的 --name 是 1-32 个 a-z0-9-_） ----------
TPL_NAME="anotify-$TASK8"
if [ "${#TPL_NAME}" -gt 32 ]; then TPL_NAME="${TPL_NAME:0:32}"; TPL_NAME="${TPL_NAME%-}"; fi
# hc 按 **OS 用户的家目录** 找模板（不认 $HOME / $XDG_CONFIG_HOME：实测把 HOME 指到
# 别处，hc templates / hc run 照样读 passwd 里那个），所以这里也按 passwd 解析。
HC_HOME=$(getent passwd "$(id -u)" 2>/dev/null | cut -d: -f6)
[ -n "$HC_HOME" ] || HC_HOME=$(eval echo ~$(id -un))
[ -n "$HC_HOME" ] || HC_HOME="$HOME"
TPL_DIR="$HC_HOME/.config/herdr-core/templates"

# 同一 task 的第几条会话：数一数已经带 anotify.task=<id> 标签的会话。
# 带 seq 是为了重试不撞名（hc 只要求「存活终端里唯一」，但我们不想靠撞了再重试）。
SEQ=1
existing=$(hc ls --all --label "anotify.task=$TASK_ID" --json 2>/dev/null)
n=$(printf '%s' "$existing" | grep -o '"name":"' | wc -l | tr -d ' ')
[ -n "$n" ] || n=0
SEQ=$(( n + 1 ))

SESSION="$TPL_NAME-$SEQ"
if [ "${#SESSION}" -gt 32 ]; then SESSION="${SESSION:0:32}"; SESSION="${SESSION%-}"; fi
if [ "${#SESSION}" -gt 32 ]; then SESSION="${SESSION:0:32}"; fi

# branch 里的 {{task8}} / {{seq}} 由**这里**展开（模板层零插值）。
# 注意：显式给空串 = 不要分支（--detach），没给才用默认模板——
# 两者都走 ${X+x} 判断，否则「填空退化成 detached」这条语义会被默认值吞掉。
if [ "${ANOTIFY_PARAM_BRANCH+x}" = "x" ]; then
  BRANCH_TPL="$ANOTIFY_PARAM_BRANCH"
else
  BRANCH_TPL='anotify/{{task8}}-{{seq}}'
fi
BRANCH="${BRANCH_TPL//\{\{task8\}\}/$TASK8}"
BRANCH="${BRANCH//\{\{seq\}\}/$SEQ}"

# ---------- worktree（hc 没有这个 API，节点自己做） ----------
CWD="$REPO"
if [ "$WORKTREE" != off ]; then
  [ -d "$REPO" ] || die "invalid_param: 工作目录 $REPO 不是目录（开 worktree 要在仓库里）" 2
fi
if [ "$WORKTREE" = new ]; then
  if ! git -C "$REPO" rev-parse --git-dir >/dev/null 2>&1; then
    die "worktree_failed: worktree=new 需要工作目录本身是 git 仓库（现在是 $REPO）；先在能力定义里把 run.workDir 指到仓库" 1
  fi
  [ -n "$WT_PATH" ] || WT_PATH="$REPO/wt"
  if [ -e "$WT_PATH" ]; then
    die "worktree_failed: 目标 $WT_PATH 已存在（上一次任务的现场？先清理或换个 worktree_path）" 1
  fi
  if [ -n "$BRANCH" ]; then
    # 开分支而不是 detached：否则 agent 的提交会变成悬挂 commit。
    if ! wt_out=$(git -C "$REPO" worktree add -b "$BRANCH" "$WT_PATH" "$BASE" 2>&1); then
      die "worktree_failed: git worktree add -b $BRANCH 失败（base=$BASE）" 1 "$wt_out"
    fi
  else
    if ! wt_out=$(git -C "$REPO" worktree add --detach "$WT_PATH" "$BASE" 2>&1); then
      die "worktree_failed: git worktree add --detach 失败（base=$BASE）" 1 "$wt_out"
    fi
  fi
  WT_CREATED=1
  CWD="$WT_PATH"
  log "worktree: $WT_PATH（branch=${BRANCH:-<detached>} base=$BASE）"
elif [ "$WORKTREE" = existing ]; then
  [ -n "$WT_PATH" ] || die "invalid_param: worktree=existing 必须给 worktree_path" 2
  [ -d "$WT_PATH" ] || die "worktree_failed: worktree_path=$WT_PATH 不存在（existing 只用现成目录，不替你建）" 1
  CWD="$WT_PATH"
  log "worktree: 复用现有目录 $CWD"
fi

# ---------- emit 身份落盘（env 进不去 hc，所以给一个 0600 的文件） ----------
TASK_DIR="$CWD/.anotify"
mkdir -p "$TASK_DIR" || die "emit_contract_failed: 建不出 $TASK_DIR" 1
# 让 .anotify/ 整个目录对 git 隐身：agent 一句 git add -A 就不会把 token 追进提交
# （分支是保留的，提交会活下去）。自带的 .gitignore 比改仓库的 info/exclude 干净。
printf '*\n' > "$TASK_DIR/.gitignore" || die "emit_contract_failed: 写不出 $TASK_DIR/.gitignore" 1
TASK_FILE="$TASK_DIR/task.env"
if ! ( umask 077; {
    printf 'ANOTIFY_EMIT_ENDPOINT=%s\n' "$(shell_quote "${ANOTIFY_EMIT_ENDPOINT:-}")"
    printf 'ANOTIFY_TASK_TOKEN=%s\n' "$(shell_quote "${ANOTIFY_TASK_TOKEN:-}")"
    printf 'ANOTIFY_TASK_ID=%s\n' "$(shell_quote "$TASK_ID")"
  } > "$TASK_FILE" ); then
  die "emit_contract_failed: 写不出 $TASK_FILE" 1
fi
chmod 600 "$TASK_FILE" || die "emit_contract_failed: chmod 600 $TASK_FILE 失败" 1
[ -n "${ANOTIFY_EMIT_ENDPOINT:-}" ] || log "没有 ANOTIFY_EMIT_ENDPOINT：节点没给 emit 端点，agent 的 emit 会失败"

# ---------- hc 模板（一个任务一份，任务期只读） ----------
COMMAND_LINE="$AGENT"
[ -z "$AGENT_ARGS" ] || COMMAND_LINE="$AGENT $AGENT_ARGS"
NOTES_TEXT="$EMIT_NOTES"
[ -n "$NOTES_TEXT" ] || NOTES_TEXT="（能力作者没填：只在真的做完时 emit 一次 summary）"
TPL_FILE="$TPL_DIR/$TPL_NAME.md"
mkdir -p "$TPL_DIR" || die "hc_template_failed: 建不出模板目录 $TPL_DIR" 1
TPL_TMP="$TPL_DIR/.$TPL_NAME.md.tmp.$$"
# 先写临时文件再 mv：同一任务的两个会话并发跑时，另一个不会读到半份 frontmatter。
if ! {
  printf -- '---\n'
  printf 'name: %s\n' "$TPL_NAME"
  # hc 的 frontmatter 是**逐行 key: value 的原文**（不剥引号、不认注释），所以这里
  # 不引号、不转义：值里的引号/井号就是命令本身的一部分。
  printf 'description: anode agent.tui：任务 %s 的一次执行（hc 托管）。看历史用 hc read，--keep 之后 attach 上去只是个 shell，不能继续对话。\n' "$TASK_ID"
  printf 'command: %s\n' "$COMMAND_LINE"
  printf -- '---\n'
  cat <<BODY
产出只通过 emit 上报，stdout 不算数（中枢不读屏幕，也不解析你的界面）。

第 0 步：把本次任务的身份读进环境（每个新 shell 都要做一次）：
  . .anotify/task.env

它给出 emit 端点、本次任务的一次性令牌与任务 id 三个变量。
不要 cat 这个文件、不要把里面的值复制到别处、不要打进任何输出——凭据只留在本机。

emit 这样调（目标必须是本能力 spec.next 里声明过的下游）：
  anode emit <下游能力> --data '{"k":"v"}'
  anode emit <下游能力> --id review-a.go --data '{"k":"v"}'   # 可选幂等键：同 task 同 id 只派发一次

退出码：0 已派发/剪枝 · 2 目标没声明或没给目标 · 3 令牌无效 · 4 任务已结束 ·
        5 端点不可达/结果未知（可能已派发，别裸重试）· 6 同 id 不同请求

产出说明（能力作者写的，照做）：
$(printf '%s\n' "$NOTES_TEXT" | sed 's/^/    /')

全部做完后，最后**单独**打印一行 ANOTIFY-STOP-<taskId>：把 <taskId> 原样换成
.anotify/task.env 里的 taskId 值（形如 ANOTIFY-STOP-tsk_abc123）。这一行是给
编排层看的结束信号，不是结论；结论只走 emit。
BODY
} > "$TPL_TMP"; then
  rm -f "$TPL_TMP"
  die "hc_template_failed: 写不出 hc 模板 $TPL_FILE" 1
fi
if ! mv -f "$TPL_TMP" "$TPL_FILE"; then
  rm -f "$TPL_TMP"
  die "hc_template_failed: 装不上 hc 模板 $TPL_FILE" 1
fi

# ---------- 起 TUI agent（首条消息就是任务正文，避免发给还没起来的 agent 丢掉） ----------
LABELS=(--label "anotify.space=$SPACE" --label "anotify.task=$TASK_ID")
if [ "$WORKTREE" != off ]; then LABELS+=(--label "anotify.worktree=${BRANCH:-$CWD}"); fi

run_out=$(hc run --bg --json --outlive-parent --keep --no-notify \
  --name "$SESSION" "${LABELS[@]}" --cwd "$CWD" -- "$TPL_NAME" "$PROMPT" 2>&1)
run_code=$?
if [ "$run_code" -ne 0 ]; then
  if ! hc server status --json >/dev/null 2>&1; then
    die "hc_core_unreachable: hc run 失败，且 hc server status 也不可用：$(first_line "$run_out")" 1
  fi
  die "hc_run_failed: hc run 起不来会话（模板 $TPL_NAME）：$(first_line "$run_out")" 1
fi
case "$run_out" in
  *'"sent":false'*)
    die "hc_run_failed: 首条消息没送进会话（hc 说 agent 还没 ready）：$(first_line "$run_out")" 1 ;;
esac
log "会话: $SESSION（模板 $TPL_NAME，模板文件 $TPL_FILE）"

# ---------- 等结束：哨兵 + 状态。wait 的 timeout 只是 check-in，不是失败 ----------
#
# hc wait 走**后台 + wait 内建**：bash 只在「当前命令结束」或「内建 wait 被信号打断」
# 时执行 trap。写成 wait_out=$(hc wait …) 的话，节点 cancel 发的 TERM 要等整段
# hc wait 返回才轮得到 on_signal——而 runner 5s 后就会 KILL 进程组，等于「取消时
# 从不 interrupt 会话」。后台 + wait 让信号当场生效。
SENTINEL="ANOTIFY-STOP-$TASK_ID"
DEADLINE=$(( $(date +%s) + TIMEOUT_SEC ))
WAIT_FILE="$TASK_DIR/.wait.out"
WHY=""
while :; do
  now=$(date +%s)
  remain=$(( DEADLINE - now ))
  if [ "$remain" -le 0 ]; then WHY="deadline"; break; fi
  chunk=1800
  if [ "$remain" -lt "$chunk" ]; then chunk=$remain; fi
  hc wait "$SESSION" --output "$SENTINEL" --timeout "$chunk" --json > "$WAIT_FILE" 2>&1 &
  wait "$!"
  wait_out=$(cat "$WAIT_FILE" 2>/dev/null)
  rm -f "$WAIT_FILE"
  case "$wait_out" in
    *'"reason":"timeout"'*)
      # 正常时 hc wait 会阻塞到 --timeout；这里再睡 1 秒是防「hc 立刻返回 timeout」
      # （坏掉的/fake 的 hc）把循环变成热转圈。
      log "hc wait 到点（check-in，不是失败），继续等"
      sleep 1 ;;
    *'"reason":"output"'*) WHY="sentinel"; break ;;
    *'"reason":"exit"'*) WHY="exit"; break ;;
    *'"reason":"blocked"'*) WHY="blocked"; break ;;
    *'"reason":"error"'*) WHY="error"; break ;;
    *) WHY="wait"; break ;;
  esac
done

get_out=$(hc get "$SESSION" --json 2>/dev/null)
exit_code=$(json_field "$get_out" exit_code)
status_word=$(json_field "$get_out" status)

case "$WHY" in
  sentinel) ;;
  exit)
    if [ "$exit_code" != "0" ]; then
      die "agent_exit: 会话 $SESSION 退出码 $exit_code，且没看到结束哨兵（现场保留）" 1
    fi
    ;;
  blocked)
    die "blocked: 会话 $SESSION 在等人（权限/确认）。看：hc read $SESSION；接管：hc attach $SESSION" 1 ;;
  error)
    die "hc_agent_error: 会话 $SESSION 报了 error，状态=$status_word。看：hc read $SESSION" 1 ;;
  deadline)
    hc interrupt "$SESSION" >/dev/null 2>&1
    die "timeout: $TIMEOUT_SEC 秒内没等到结束哨兵（会话 $SESSION 已 interrupt，现场保留）" 1 ;;
  *)
    die "hc_wait_failed: 等会话结束失败（会话 $SESSION）：$(first_line "$wait_out")" 1 ;;
esac

# ---------- 成功：按 worktree_cleanup 收现场（只删目录，不删分支） ----------
if [ "$WT_CREATED" = "1" ] && [ "$CLEANUP" = "remove" ]; then
  if wt_out=$(git -C "$REPO" worktree remove --force "$WT_PATH" 2>&1); then
    DETAIL="worktree: $WT_PATH 已删除（分支 ${BRANCH:-<detached>} 保留）"
  else
    DETAIL="worktree: $WT_PATH 清理失败，现场保留：$(first_line "$wt_out")"
  fi
elif [ "$WORKTREE" = "existing" ]; then
  DETAIL="worktree: $CWD（existing：别人的目录，不自动删除）"
elif [ "$WT_CREATED" = "1" ]; then
  DETAIL="worktree: $CWD（cleanup=$CLEANUP，现场保留）"
fi

finish 0 "agent.tui: 任务 $TASK_ID 会话 $SESSION 结束"
`

// agentTUITemplate 返回内置的 agent.tui 模板。
//
// 字段名是 snake_case（会变成 ANOTIFY_PARAM_<UPPER>）；参数表用的是 snake_case 的
// 逻辑名，落到产物里必须保持 snake_case，否则环境变量名会变成 AGENTARGS 这种谁也
// 猜不到的写法（capability.md §2.3）。
func agentTUITemplate() Template {
	return Template{
		ID:          "agent.tui",
		DisplayName: "hc 托管 TUI Agent",
		Category:    "agent",
		Description: "把一个任务交给 hc 托管的 TUI agent（pi / claude / codex …）：" +
			"agent 自己按提示词里的 emit 契约上报产出，人随时能 hc read / hc attach。" +
			"任务期在 ~/.config/herdr-core/templates/ 下生成同名 hc 模板。" +
			"注意：herdr 与 hc 是两套会话，看不见彼此。",
		Fields: []Field{
			{Key: "agent", Label: "Agent", Type: "select", Default: "pi",
				Options: []string{"pi", "claude", "codex", "gemini", "cursor", "opencode"},
				Help: "必须是 hc 认得的 agent：它靠命令行认 agent（status/wait 才准）。" +
					"填不认识的程序名，编排层就只剩瞎等超时这一条路。"},
			{Key: "agent_args", Label: "Agent 参数", Type: "string",
				Help: "原样拼到命令行：--model gpt-6.1-sol --thinking medium。" +
					"不做各家 flag 映射；只挡换行与引号不配对（hc 模板的 command 是单行）。"},
			{Key: "emit_notes", Label: "产出说明", Type: "text",
				Help: "每行一条：先写「什么时候 emit」，再写内容期望，原样进提示词。" +
					"留空则只在真的做完时 emit 一次 summary。"},
			{Key: "space", Label: "空间（标签）", Type: "string", Default: "default",
				Help: "label anotify.space=<值>：hc ls --label anotify.space=<值> 只看这一组。" +
					"hc 没有选 space 的参数，标签就是命名空间。"},
			{Key: "worktree", Label: "Worktree", Type: "select", Default: "off",
				Options: []string{"off", "new", "existing"},
				Help: "off = 直接用工作目录；new = 节点自己 git worktree add 再交给 agent；" +
					"existing = 用 worktree_path 指的那个现成目录。"},
			{Key: "branch", Label: "分支", Type: "string", Default: "anotify/{{task8}}-{{seq}}",
				Help: "仅 worktree=new。{{task8}} / {{seq}} 由节点在任务期展开；" +
					"留空 = --detach（agent 的提交会变悬挂 commit）。"},
			{Key: "base", Label: "起点", Type: "string", Default: "HEAD",
				Help: "仅 worktree=new：git worktree add 的起点 ref。"},
			{Key: "worktree_path", Label: "Worktree 路径", Type: "string",
				Help: "existing 必填；new 缺省 <工作目录>/wt。"},
			{Key: "worktree_cleanup", Label: "结束后清理", Type: "select", Default: "remove",
				Options: []string{"remove", "keep"},
				Help: "仅 worktree=new 且任务成功时：remove = git worktree remove --force" +
					"（只删目录，分支与 agent 的提交都保留）；失败/blocked 永远保留现场。"},
			{Key: "prompt", Label: "任务正文", Type: "text", Required: true,
				Help: "交给 agent 的提示词（中枢每次派发时给的）。"},
			{Key: "timeout_sec", Label: "任务上限（秒）", Type: "number", Default: "3600",
				Help: "整个任务的上限，不是单次 hc wait 的上限（wait 到点是 check-in，会接着等）。"},
		},
		Script:     agentTUIScript,
		Requires:   []string{"hc", "git"},
		TimeoutSec: 3600,
	}
}
