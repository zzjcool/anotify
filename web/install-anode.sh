#!/bin/sh
# Anotify v4 节点（anode）安装器 —— 一条命令：下载 → 校验 → 停旧服务 → 预检 → 落盘 → 服务 → 结论。
#
#   curl -fsSL https://<中枢>/install-anode.sh | sh -s -- \
#     --name node-a --server https://<中枢> --token nt_xxx
#
# 分工（契约 D1）：**本脚本是唯一安装入口** —— 下载/校验/预检/服务生命周期/卸载/升级；
# 落盘（二进制 + anode.yaml + unit）交给下载到的 `anode install`，脚本不写第二套 YAML/unit
# 源（D1）。`anode install` 自己不 sudo、不 systemctl，这两件事只能由本脚本做。
#
# 一条命令做完七件事，中间不提问，结尾给确定结论：
#   1. 取二进制（D4）：--download-url > --version（GitHub Release）> 中枢 GET /v1/node/binary
#      （Bearer 节点令牌，读 X-Agent-Sha256 / X-Agent-Version）
#   2. 校验：sha256 必须能校验（缺校验和/缺 sha256sum 一律失败，不做 v3 的"未校验假通过"）；
#      再核对 `anode --version` 的发布版本（**前缀**匹配，允许 `+<commit>` 后缀）
#   3. 停旧服务（同一个令牌只能有一条连接，否则预检会被旧实例踢掉）→ 用**临时** config 跑
#      `anode serve --check` 预检（D5）。令牌错/中枢不通在这里就停，不留一个开机就崩的服务
#      （`--service none` 也预检：只落盘同样要先证明这把令牌能用）
#   4. 落盘：`anode install … --token-file <临时文件>`（D9：令牌不进 argv 之外的任何地方；
#      从中枢下载时令牌走 curl --config 文件，也不进 curl 的 argv）
#   5. 服务生命周期（D6）：systemd system 或 user；**先**把 config/data 交给服务用户、**再**
#      restart，然后轮询 is-active 最多 10s；失败时把停机前的旧服务恢复回来，不把节点留在停止态
#   6. 结尾打印节点/中枢/二进制/配置/服务/日志/升级/卸载
#
# 重跑同一条命令 = 升级（D8）：沿用已有 anode.yaml（令牌/中枢/名字从文件读回），
# 只换二进制 + restart。
#   sh install-anode.sh --uninstall                停/禁用/删 unit + 删二进制 + 删配置
#   sh install-anode.sh --uninstall --keep-config  保留 anode.yaml（令牌还在里面，先备份）
#   sh install-anode.sh --uninstall --purge        连数据目录一起删
#
# 安全（D9）：令牌绝不打印（不 set -x、不进摘要、不进日志）；传参用临时 --token-file
# （目录 0700/文件 0600，退出即删）。sudo 只用在真正需要特权的命令上，密码由 sudo 自己
# 从 /dev/tty 读（`curl|sh` 时 stdin 是管道，不能拿来读密码；脚本本身不读 stdin）。
#
# 退出码：0 成功（含 --help / --uninstall）；2 用法/下载/校验/预检/服务失败，并打印下一步。
set -eu
umask 077

OWNER="zzjcool"
REPO="anotify"
DEFAULT_BASE="https://github.com/${OWNER}/${REPO}/releases/download"
UNIT_NAME="anode"
UNIT_BASENAME="anode.service"

# ---- 参数默认值（解析见 parse_args） ----
NAME=""
SERVER=""
TOKEN=""
TOKEN_FILE_SRC=""
TOKEN_FROM_CLI="no"
VERSION="${ANODE_VERSION:-}"
ARCH="${ANODE_ARCH:-}"
DL_URL="${ANODE_DOWNLOAD_URL:-}"
CHECKSUM_URL="${ANODE_CHECKSUM_URL:-}"
BASE="${ANODE_DOWNLOAD_BASE:-$DEFAULT_BASE}"
PREFIX=""
OPT_CONFIG=""
OPT_BIN=""
OPT_UNIT=""
OPT_DATA=""
SERVICE="auto" # auto | system | user | none
SERVICE_EXPLICIT="no" # 用户是否**显式**指定了服务模式（--service/--system/--user/--no-systemd）
UNINSTALL="no"
PURGE="no"
KEEP_CONFIG="no"
NO_SUDO="no"
NO_PREFLIGHT="no"
PREFLIGHT_SEC="${ANODE_PREFLIGHT_SEC:-20}"
DRY_RUN="no"

# 这四个是**测试钩子**（自测脚本用它把 sudo/systemctl/loginctl/unit 落点换掉）；
# 正常环境不要设，默认值就是真实系统里的东西。
SUDO="${ANODE_SUDO:-sudo}"
SYSTEMCTL="${ANODE_SYSTEMCTL:-systemctl}"
LOGINCTL="${ANODE_LOGINCTL:-loginctl}"
UNIT_OVERRIDE="${ANODE_UNIT_PATH:-}"

# ---- 运行期状态 ----
TMP=""
DL_BIN=""
SERVER_WS=""
SERVER_HTTP=""
CONFIG=""
BINP=""
UNIT_FILE=""
DATA=""
RUN_USER=""
RUN_HOME=""
RUN_USER_KNOWN="no"
SUDO_MODE="" # root | sudo-n | sudo-tty | none
UPGRADE="no"
EXIST_CONFIG=""
EXISTING_TOKEN=""
UPDATE_TOKEN="no"
UNIT_PATH_VALUE=""
TOKFILE=""
SERVICE_ASSUMED="no"
# 升级前停掉的旧实例（system|user）：install/restart 失败时要把它恢复回去，
# 否则一次失败的升级 = 节点掉线（D8“重跑=升级”不该有这个副作用）。
STOPPED_UNIT=""

# ---------- 输出 / 退出 ----------
info() { printf '%s\n' "$*"; }
warn() { printf '注意：%s\n' "$*" >&2; }
step() { printf '\n== %s\n' "$*"; }

# 统一失败出口：一律非 0（2），并把「下一步能做什么」写在同一段里。
die() {
	printf 'install-anode: %s\n' "$*" >&2
	exit 2
}

need() { # need <命令> <装什么/干什么的说明>
	command -v "$1" >/dev/null 2>&1 || die "缺少 $1（$2）"
}

cleanup() {
	if [ -n "$TMP" ] && [ -d "$TMP" ]; then
		rm -rf "$TMP"
	fi
}
trap cleanup EXIT
trap 'exit 2' HUP INT TERM

ensure_tmp() {
	if [ -z "$TMP" ]; then
		TMP=$(mktemp -d) || die "无法创建临时目录（TMPDIR=${TMPDIR:-/tmp}）"
	fi
}

usage() {
	cat <<'EOF'
用法：sh install-anode.sh [选项]

必填（首次安装；重跑时从已有 anode.yaml 读回，可省略）：
  --name NAME              节点名（缺省用主机名；已有配置时沿用）
  --server URL             中枢地址（http(s):// 或 ws(s)://，脚本自行归一化）
  --token TOKEN            节点令牌（控制台注册节点后只显示一次）
  --token-file PATH        从 0600 文件读令牌（推荐：不进 shell 历史）
                           轮换同样生效：与已保存的不同就自动带上 --update-token

取二进制（三选一，优先级从上到下）：
  --download-url URL       直接指定二进制 URL（内网/镜像/测试；校验和取 <URL>.sha256）
  --version V              指定 GitHub Release 版本（资产 anode_linux_<arch>）
  （都不给）               从中枢 GET <server>/v1/node/binary 下载（Bearer 令牌鉴权）

安装位置：
  --prefix DIR             传给 anode install：所有默认路径落在 DIR 之下
                           （<DIR>/usr/local/bin/anode、<DIR>/etc/anode/anode.yaml、
                             <DIR>/etc/systemd/system/anode.service、<DIR>/var/lib/anode）
                           这是 staging 布局：显式配 --service system 会被拒绝（unit 不在
                           systemd 搜索路径，enable 必失败），请用 --service none / user

服务怎么装（默认 auto）：
  --service auto|system|user|none
                            auto：有 systemctl 且能提权 → system；否则 user；没有 systemctl → none
  --system                 系统服务（开机自启、没登录也活着；unit 里 User=<安装者>）
  --user                   仅当前用户的 systemd 服务（不碰 sudo；尽力开 linger）
  --no-systemd             只落盘，不装服务（等价 --service none；打印手工命令）
  --no-sudo                禁止提权（需要 root 的步骤直接失败）

行为开关：
  --no-preflight           跳过「用临时配置连一次中枢」的预检（不推荐；这是预检的**唯一**例外，
                           --service none 也预检）
  --preflight-timeout SEC  预检最长等待秒数（默认 20）
  --dry-run                只打印将要执行的命令，不碰网络、不写盘、不提权（也不调
                           systemctl/loginctl）：只是一份计划
  -h, --help               显示这段帮助

卸载：
  --uninstall              停服务、删 unit 与二进制、删 anode.yaml
  --keep-config            配合 --uninstall：保留 anode.yaml（令牌还在，可先备份）
  --purge                  配合 --uninstall：连数据目录（产物缓存/工作区）一起删

离线镜像 / 测试钩子：
  --arch amd64|arm64       覆盖平台探测（默认 uname -m）
  --checksum-url URL       覆盖校验和来源（默认 <二进制 URL>.sha256 或中枢头）

环境变量：ANODE_VERSION、ANODE_DOWNLOAD_URL、ANODE_DOWNLOAD_BASE、ANODE_CHECKSUM_URL、
ANODE_SUDO、ANODE_SYSTEMCTL、ANODE_LOGINCTL、ANODE_UNIT_PATH、ANODE_PREFLIGHT_SEC、ANODE_ARCH。

重跑同一条命令 = 升级；退出码 0 成功，2 为用法/下载/校验/预检/服务失败。
EOF
}

# ---------- 参数解析 ----------
take_value() { # take_value <flag> <是否还有下一个参数> <下一个参数>
	[ "$2" = "yes" ] || die "$1 缺少取值（--help 看全部）"
	VALUE=$3
}

parse_args() {
	while [ "$#" -gt 0 ]; do
		# 兼容 --flag=value：拆成两个参数回到同一个解析器（引号/空格都被保留）。
		case "$1" in
		--*=*)
			_pairs_rest=${1#*=}
			_pairs_head=${1%%=*}
			shift
			set -- "$_pairs_head" "$_pairs_rest" "$@"
			continue
			;;
		esac
		# `--` 之后的参数仍按同一张表解析（历史用法：
		#   sh install-anode.sh --version v4.0.0 -- --name node-a --server … --token-file …）。
		# 它们最终都由本脚本翻译成 `anode install` 的参数，不做二次拼接。
		[ "$1" != "--" ] || shift
		[ "$#" -gt 0 ] || break
		_hasnext="no"
		[ "$#" -ge 2 ] && _hasnext="yes"
		case "$1" in
		--name) take_value "$1" "$_hasnext" "${2:-}"; NAME=$VALUE; shift 2 ;;
		--server) take_value "$1" "$_hasnext" "${2:-}"; SERVER=$VALUE; shift 2 ;;
		--token) take_value "$1" "$_hasnext" "${2:-}"; TOKEN=$VALUE; TOKEN_FROM_CLI="yes"; shift 2 ;;
		--token-file) take_value "$1" "$_hasnext" "${2:-}"; TOKEN_FILE_SRC=$VALUE; TOKEN_FROM_CLI="yes"; shift 2 ;;
		--version) take_value "$1" "$_hasnext" "${2:-}"; VERSION=$VALUE; shift 2 ;;
		--download-url) take_value "$1" "$_hasnext" "${2:-}"; DL_URL=$VALUE; shift 2 ;;
		--checksum-url) take_value "$1" "$_hasnext" "${2:-}"; CHECKSUM_URL=$VALUE; shift 2 ;;
		--arch) take_value "$1" "$_hasnext" "${2:-}"; ARCH=$VALUE; shift 2 ;;
		--prefix)
			take_value "$1" "$_hasnext" "${2:-}"
			PREFIX=${VALUE%/}
			[ -n "$PREFIX" ] || die "--prefix 不能是 /（要落到根下就直接用系统默认）"
			shift 2
			;;
		--config) take_value "$1" "$_hasnext" "${2:-}"; OPT_CONFIG=$VALUE; shift 2 ;;
		--bin) take_value "$1" "$_hasnext" "${2:-}"; OPT_BIN=$VALUE; shift 2 ;;
		--unit) take_value "$1" "$_hasnext" "${2:-}"; OPT_UNIT=$VALUE; shift 2 ;;
		--data) take_value "$1" "$_hasnext" "${2:-}"; OPT_DATA=$VALUE; shift 2 ;;
		--service)
			take_value "$1" "$_hasnext" "${2:-}"
			case "$VALUE" in
			auto | system | user | none) SERVICE=$VALUE; SERVICE_EXPLICIT="yes" ;;
			*) die "未知的服务模式：$VALUE（auto|system|user|none）" ;;
			esac
			shift 2
			;;
		--system) SERVICE="system"; SERVICE_EXPLICIT="yes"; shift ;;
		--user) SERVICE="user"; SERVICE_EXPLICIT="yes"; shift ;;
		--no-systemd) SERVICE="none"; SERVICE_EXPLICIT="yes"; shift ;;
		--no-sudo) NO_SUDO="yes"; shift ;;
		--no-preflight) NO_PREFLIGHT="yes"; shift ;;
		--preflight-timeout)
			take_value "$1" "$_hasnext" "${2:-}"
			case "$VALUE" in '' | *[!0-9]*) die "--preflight-timeout 必须是正整数：$VALUE" ;; esac
			[ "$VALUE" -gt 0 ] || die "--preflight-timeout 必须大于 0：$VALUE"
			PREFLIGHT_SEC=$VALUE
			shift 2
			;;
		--dry-run) DRY_RUN="yes"; shift ;;
		--uninstall) UNINSTALL="yes"; shift ;;
		--purge) PURGE="yes"; shift ;;
		--keep-config) KEEP_CONFIG="yes"; shift ;;
		-h | --help) usage; exit 0 ;;
		*) die "未知参数：$1（--help 看全部）" ;;
		esac
	done

	if [ "$UNINSTALL" != "yes" ]; then
		[ "$PURGE" != "yes" ] || die "--purge 只能配合 --uninstall 使用"
		[ "$KEEP_CONFIG" != "yes" ] || die "--keep-config 只能配合 --uninstall 使用"
	fi
	# 二选一：两个都给就是"到底信哪个"的歧义，早点拦。
	[ -z "$TOKEN" ] || [ -z "$TOKEN_FILE_SRC" ] || die "--token 与 --token-file 只能给一个"
}

# ---------- 平台 ----------
detect_platform() {
	need uname "coreutils"
	os=$(uname -s)
	[ "$os" = "Linux" ] || die "只支持 Linux（当前 $os）：macOS/Windows 请自己 go build 或用容器"
	if [ -z "$ARCH" ]; then
		case "$(uname -m)" in
		x86_64 | amd64) ARCH="amd64" ;;
		aarch64 | arm64) ARCH="arm64" ;;
		*) die "不支持的架构：$(uname -m)（可用 --arch amd64|arm64 显式指定）" ;;
		esac
	fi
	case "$ARCH" in
	amd64 | arm64) ;;
	*) die "不支持的架构：$ARCH（只发布 amd64/arm64）" ;;
	esac
}

# ---------- 服务运行身份 ----------
user_home() { # user_home <user> → 家目录（查不到就输出空）
	_h=""
	if command -v getent >/dev/null 2>&1; then
		_h=$(getent passwd "$1" 2>/dev/null | cut -d: -f6) || _h=""
	fi
	if [ -z "$_h" ] && [ -r /etc/passwd ]; then
		_h=$(awk -F: -v u="$1" '$1 == u { print $6 }' /etc/passwd | head -n 1)
	fi
	printf '%s' "$_h"
}

# 服务以**安装者**身份跑（不是 root）：能力的权限 = 服务进程的权限。
# root 安装时从 SUDO_USER 还原安装者；拿不到不算错（只落盘 / 用户服务的场景不需要它），
# 由 require_runner 在真正需要"安装者"那一刻报错——不猜，猜错就是"以 root 跑一切"。
resolve_runner() {
	if [ "$(id -u)" = "0" ]; then
		_owner="${SUDO_USER:-}"
		if [ -n "$_owner" ] && [ "$_owner" != "root" ]; then
			RUN_USER=$_owner
			RUN_HOME=$(user_home "$RUN_USER")
			[ -n "$RUN_HOME" ] || die "查不到 $RUN_USER 的家目录（unit 的 Environment=HOME 要用它）"
			RUN_USER_KNOWN="yes"
		else
			RUN_USER=$(id -un)
			RUN_HOME=${HOME:-/root}
		fi
	else
		RUN_USER=$(id -un)
		RUN_HOME=${HOME:-}
		RUN_USER_KNOWN="yes"
	fi
	# 家目录是 /（系统用户常见，如 nobody:/）时按"没有家目录"处理：anode install 的
	# safePath 明确拒绝 /，而 <home>/.local/bin 这种拼接也没有意义。
	case "$RUN_HOME" in
	'' | /)
		RUN_HOME=""
		warn "拿不到可用的家目录（HOME=${HOME:-空}）：unit 里不写 Environment=HOME=/PATH 的 home 段"
		;;
	esac
}

# 系统服务要写 User=<安装者> 并把 config/data chown 给它——root 直跑又没有 SUDO_USER 时
# 这个身份根本不存在，只能报错（猜一个就等于把整台机器交给那个用户）。
require_runner() {
	if [ "$SERVICE" = "system" ] && [ "$RUN_USER_KNOWN" != "yes" ]; then
		# 预演不该因为"真跑会拒"而直接退出：先把整份计划打出来，并说清真跑会怎样。
		if [ "$DRY_RUN" = "yes" ]; then
			warn "以 root 预演且没有 SUDO_USER：预演里 unit 的 User= 暂用 root 占位；真跑时这里会拒绝（unit 必须写 User=<安装者>）"
			return 0
		fi
		die "以 root 直接运行无法确定服务运行身份（anode.service 需要 User=<安装者>）
  · 用普通用户重跑，由脚本自己做 sudo：sh install-anode.sh --name … --server … --token-file …
  · 或者只落盘：加 --prefix /opt/anode-stage --no-systemd
  · 容器里请用 --no-systemd（容器内 root 就是该服务的身份）"
	fi
}

# unit 里的 PATH 是**稳定集合**（R1/M4）：`$HOME/.local/bin` 打头（压过同名旧副本）+ 固定系统
# 目录。**不照抄调用者 `$PATH`**：安装时 PATH 嵌进 unit，换一个终端（direnv/nvm/不同 ssh 会话）
# 重跑就变成了“另一个 unit”，`anode install` 会以 `existing unit is unknown/modified` 拒绝覆盖，
# 而那时旧服务已经被 stop —— 节点停在停止态。“重跑=升级”（D8）要求同一串参数在任何 shell 下
# 都是同一个 unit。要自定义路径时用 `anode install --unit-path`（脚本会把它放在本行的 home 段之后）。
UNIT_PATH_STABLE="/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
unit_path_value() {
	if [ -n "$RUN_HOME" ]; then
		printf '%s:%s' "${RUN_HOME}/.local/bin" "$UNIT_PATH_STABLE"
	else
		printf '%s' "$UNIT_PATH_STABLE"
	fi
}

# ---------- sudo ----------
#
# 只让**真正需要特权的那几条命令**提权（不整段 `sudo sh` 重跑自己：那样脚本体就成了
# root 的，容易在 $HOME/PATH/缓存目录上做出"root 的文件"）。
resolve_sudo() {
	if [ "$(id -u)" = "0" ]; then
		SUDO_MODE="root"
		return 0
	fi
	if [ "$NO_SUDO" = "yes" ]; then
		SUDO_MODE="none"
		return 0
	fi
	if [ "$DRY_RUN" = "yes" ]; then
		# 预演不实测（不执行任何提权动作），只按"假设能提权"打印命令。
		SUDO_MODE="sudo-n"
		return 0
	fi
	if $SUDO -n true >/dev/null 2>&1; then
		SUDO_MODE="sudo-n"
		return 0
	fi
	# 需要密码：只在**真的能打开终端**时试一次（`curl|sh` 下 stdin 是管道，sudo 走 /dev/tty）。
	# 无终端（CI/无人值守）就不试，避免脚本卡在等密码上（也避免打印一条 /dev/tty 的报错）。
	if ( : </dev/tty ) 2>/dev/null; then
		if $SUDO -v </dev/tty >/dev/null 2>&1; then
			SUDO_MODE="sudo-tty"
			return 0
		fi
	fi
	SUDO_MODE="none"
}

# run_maybe_root：root 直跑 / sudo -n / 单次 sudo -v 之后用 sudo / dry-run 只打印。
run_maybe_root() {
	# 预演一律只打印，**哪怕当前就是 root**（预演不能真执行）。
	if [ "$DRY_RUN" = "yes" ]; then
		case "$SUDO_MODE" in
		root | none) printf '  [dry-run] %s\n' "$*" ;;
		sudo-n) printf '  [dry-run] %s -n %s\n' "$SUDO" "$*" ;;
		*) printf '  [dry-run] %s %s\n' "$SUDO" "$*" ;;
		esac
		return 0
	fi
	case "$SUDO_MODE" in
	root)
		"$@"
		;;
	sudo-n)
		$SUDO -n "$@"
		;;
	sudo-tty)
		$SUDO "$@"
		;;
	*)
		die "这一步需要 root 权限：$*
  · 用有 sudo 的用户重跑（脚本自己会单次 sudo -v）
  · 或加 --no-sudo / --user 只装用户服务
  · 或加 --prefix DIR + --no-systemd 只落到你有写权限的目录"
		;;
	esac
}

# systemctl（系统级）：dry-run 只打印，绝不真的动服务。
systemctl_system() { run_maybe_root "$SYSTEMCTL" "$@"; }

# try_root：与 run_maybe_root 一样，但**没权限时返回 1 而不是退出**——卸载路径用
# （非 root 且没 sudo 时也应该把能删的删掉、能报的报出来，而不是一句话不说就退出）。
try_root() {
	if [ "$SUDO_MODE" = "none" ]; then
		return 1
	fi
	run_maybe_root "$@"
}

# _target_writable <path>：这个落点现在能写吗（沿最近的已存在祖先目录判断）。
_target_writable() {
	_p=$1
	while [ -n "$_p" ] && [ "$_p" != "/" ] && [ ! -e "$_p" ]; do
		_p=$(dirname "$_p")
	done
	[ -d "$_p" ] || _p=$(dirname "$_p")
	[ -w "$_p" ]
}

# install_needs_root：**落盘本身**要不要提权，只由目标目录可不可写决定——
# 用户服务（$HOME/.anode）与 --prefix 到自己的目录都不需要 root，`--no-sudo` 也照样能装。
# 注册系统 unit / chown 是另一件事，各自在 start_service 里走 systemctl_system / run_maybe_root。
install_needs_root() {
	if [ "$(id -u)" = "0" ]; then
		return 1
	fi
	if [ "$SERVICE" = "system" ]; then
		return 0
	fi
	_target_writable "$CONFIG" || return 0
	_target_writable "$BINP" || return 0
	_target_writable "$UNIT_FILE" || return 0
	_target_writable "$DATA" || return 0
	return 1
}

# anode_install：把安装参数**原样**交给下载到的 `anode install`（D1：脚本不重写 flags、
# 不做第二套 YAML/unit 源）。只有目标目录不可写时才提权；密码由 sudo 自己从 /dev/tty 读，
# 令牌已经在调用方写进临时文件，这里只传 --token-file 的路径。
anode_install() {
	if install_needs_root; then
		run_maybe_root "$DL_BIN" install "$@" # dry-run 下 run_maybe_root 只打印
		return 0
	fi
	if [ "$DRY_RUN" = "yes" ]; then
		printf '  [dry-run] %s install %s\n' "$DL_BIN" "$*"
		return 0
	fi
	"$DL_BIN" install "$@"
}

# systemctl --user：非交互 shell 里常常没有 XDG_RUNTIME_DIR，补标准路径再试。
systemctl_user() {
	if [ "$DRY_RUN" = "yes" ]; then
		printf '  [dry-run] %s --user %s\n' "$SYSTEMCTL" "$*"
		return 0
	fi
	if [ -z "${XDG_RUNTIME_DIR:-}" ] && [ -d "/run/user/$(id -u)" ]; then
		XDG_RUNTIME_DIR="/run/user/$(id -u)"
		export XDG_RUNTIME_DIR
	fi
	"$SYSTEMCTL" --user "$@"
}

# rm_path：删一个**归你所有**的路径（配置、用户 unit、数据目录）。root 的走 run_maybe_root rm。
rm_path() {
	if [ "$DRY_RUN" = "yes" ]; then
		printf '  [dry-run] 将删除 %s\n' "$1"
		return 0
	fi
	rm -rf "$1"
}

has_systemctl() { command -v "$SYSTEMCTL" >/dev/null 2>&1; }

# ---------- 服务模式 ----------
resolve_service() {
	case "$SERVICE" in
	none) return 0 ;;
	system)
		if [ "$NO_SUDO" = "yes" ] && [ "$(id -u)" != "0" ]; then
			die "--no-sudo 与系统服务冲突：注册系统 unit 必须提权。去掉 --no-sudo，或改用 --user/--no-systemd"
		fi
		has_systemctl || die "--system 需要 systemctl（容器里常见缺失）：改用 --no-systemd 只落盘，或 --prefix 到可写目录"
		return 0
		;;
	user)
		has_systemctl || die "--user 需要 systemctl（容器里常见缺失）：改用 --no-systemd 只落盘"
		return 0
		;;
	auto) ;;
	esac
	if ! has_systemctl; then
		SERVICE="none"
		warn "没有 systemctl（容器里常见）——只落盘，不装服务；手工命令在结尾"
		return 0
	fi
	if [ "$DRY_RUN" = "yes" ]; then
		# 预演不实测 sudo（不执行任何提权动作），按"能提权"推进；摘要里会标明这一点。
		SERVICE="system"
		SERVICE_ASSUMED="yes"
		return 0
	fi
	if [ "$SUDO_MODE" != "none" ]; then
		SERVICE="system"
	else
		SERVICE="user"
		info "没有可用的 sudo —— 退化成 systemd 用户服务（只在你登录时活着，脚本会尽力开 linger）"
	fi
}

# ---------- 路径推导 ----------
#
# 与 `anode install` 的默认布局同一套规则（D1：脚本只做路径推导，不写第二套落盘逻辑）：
#   系统默认 /etc/anode/anode.yaml、/usr/local/bin/anode、/etc/systemd/system/anode.service、/var/lib/anode
#   --prefix DIR → 上面每条都落在 DIR 之下
#   用户服务（无 prefix）→ $HOME/.anode/{anode.yaml,bin/anode,data} + $HOME/.config/systemd/user/anode.service
compute_paths() {
	case "$PREFIX" in
	'') ;;
	/*) ;;
	*) die "--prefix 必须是绝对路径：$PREFIX" ;;
	esac
	if [ -n "$PREFIX" ]; then
		CONFIG="${PREFIX}/etc/anode/anode.yaml"
		BINP="${PREFIX}/usr/local/bin/anode"
		UNIT_FILE="${PREFIX}/etc/systemd/system/${UNIT_BASENAME}"
		DATA="${PREFIX}/var/lib/anode"
	elif [ "$SERVICE" = "user" ]; then
		[ -n "$RUN_HOME" ] || die "用户服务没有可落的位置（HOME 为空或是 /）：用 --prefix DIR 指定目录，或改用 --system"
		CONFIG="${RUN_HOME}/.anode/anode.yaml"
		BINP="${RUN_HOME}/.anode/bin/anode"
		UNIT_FILE="${RUN_HOME}/.config/systemd/user/${UNIT_BASENAME}"
		DATA="${RUN_HOME}/.anode/data"
	else
		CONFIG="/etc/anode/anode.yaml"
		BINP="/usr/local/bin/anode"
		UNIT_FILE="/etc/systemd/system/${UNIT_BASENAME}"
		DATA="/var/lib/anode"
	fi
	# 显式覆盖（不在 --help 主表，对应 anode install 的 --config/--bin/--unit/--data）：
	# 给测试与 staging 用；落盘仍然由 anode install 做。
	[ -z "$OPT_CONFIG" ] || CONFIG=$OPT_CONFIG
	[ -z "$OPT_BIN" ] || BINP=$OPT_BIN
	[ -z "$OPT_UNIT" ] || UNIT_FILE=$OPT_UNIT
	[ -z "$OPT_DATA" ] || DATA=$OPT_DATA
	# ANODE_UNIT_PATH 是测试钩子：覆盖 unit 落点（其余路径不变）。
	[ -z "$UNIT_OVERRIDE" ] || UNIT_FILE=$UNIT_OVERRIDE
	UNIT_PATH_VALUE=$(unit_path_value)
}

# --prefix + 系统服务：`anode install` 把 unit 写进 <prefix>/etc/systemd/system/，那个目录**不在**
# systemd 的搜索路径里，`systemctl enable` 必然失败（还会在用户目录里造 root 的文件）。
# 显式要了 --service system 就直接拦；auto 推导出来的只 warn —— 容器/托管环境里 systemctl
# 可能是调用方提供的（ANODE_SYSTEMCTL 覆盖即“自带 systemd 语义”，unit 落点由那一侧决定）。
check_prefix_service_combo() {
	[ -n "$PREFIX" ] || return 0
	[ "$SERVICE" = "system" ] || return 0
	if [ "$SERVICE_EXPLICIT" = "yes" ] && [ -z "${ANODE_SYSTEMCTL:-}" ]; then
		die "--prefix 与 --service system 不能一起用：unit 会落到 $UNIT_FILE，systemd 搜索路径里没有它，systemctl enable 必然失败。
  · 只落盘（容器/staging）：--prefix DIR --service none（等价 --no-systemd）
  · 只装当前用户的服：--prefix DIR --service user
  · 真装系统服务：去掉 --prefix，用系统默认落点（/etc/systemd/system）"
	fi
	warn "--prefix + 系统服务：unit 落在 $UNIT_FILE，systemd 搜索路径里没有它；systemctl enable 可能失败（建议 --service none / --service user，或去掉 --prefix）"
}

# ---------- 已有配置（重跑 = 升级靠它） ----------
cfg_get() { # cfg_get <file> <key>
	[ -f "$1" ] || return 0
	sed -n "s/^$2:[[:space:]]*//p" "$1" | head -n 1 |
		sed -e 's/[[:space:]]*$//' -e 's/^"//' -e 's/"$//' -e "s/^'//" -e "s/'\$//"
}

read_existing_config() {
	EXIST_CONFIG=""
	for _f in "$CONFIG" "${RUN_HOME}/.anode/anode.yaml" /etc/anode/anode.yaml; do
		[ -n "$_f" ] || continue
		if [ -f "$_f" ]; then
			EXIST_CONFIG=$_f
			break
		fi
	done
	if [ -z "$EXIST_CONFIG" ]; then
		UPGRADE="no"
	else
		UPGRADE="yes"
	fi
	EXISTING_TOKEN=""
	[ -z "$EXIST_CONFIG" ] || EXISTING_TOKEN=$(cfg_get "$EXIST_CONFIG" token)
	# 命令行优先，其次沿用文件里的：一条 `curl|sh` 重跑就是升级。
	if [ -n "$EXIST_CONFIG" ]; then
		[ -n "$NAME" ] || NAME=$(cfg_get "$EXIST_CONFIG" name)
		[ -n "$SERVER" ] || SERVER=$(cfg_get "$EXIST_CONFIG" server)
		# 令牌：命令行给了（--token / --token-file）就不从文件读。
		if [ -z "$TOKEN" ] && [ -z "$TOKEN_FILE_SRC" ]; then
			TOKEN=$(cfg_get "$EXIST_CONFIG" token)
		fi
	fi
}

# 令牌只从文件读到内存，再落回我们自己 0600 的临时文件（不 echo、不进 argv、不进日志）。
load_token_file() {
	[ -n "$TOKEN_FILE_SRC" ] || return 0
	# 预演不该因为"目标机器上的文件不在这台机器上"而中止：打一条警告，用占位符继续。
	if [ ! -r "$TOKEN_FILE_SRC" ]; then
		if [ "$DRY_RUN" = "yes" ]; then
			warn "--token-file 读不到：$TOKEN_FILE_SRC（预演里用占位符，不读文件）"
			return 0
		fi
		die "读不到 --token-file：$TOKEN_FILE_SRC
  · 令牌文件必须是运行安装的用户可读的 0600 文件
  · 只想试运行：加 --dry-run"
	fi
	TOKEN=$(cat "$TOKEN_FILE_SRC") || die "读取 --token-file 失败：$TOKEN_FILE_SRC"
	[ -n "$TOKEN" ] || die "--token-file 是空的：$TOKEN_FILE_SRC"
}

# ---------- 身份校验 ----------
default_name() {
	_n=$(hostname 2>/dev/null || echo anode)
	_n=$(printf '%s' "$_n" | tr 'A-Z' 'a-z' | sed -e 's/[^a-z0-9._-]/-/g' -e 's/^[^a-z0-9]*//' | cut -c1-63)
	[ -n "$_n" ] || _n="anode"
	NAME=$_n
}

normalize_server() {
	case "$1" in
	http://*) SERVER_HTTP=$1; SERVER_WS="ws://${1#http://}" ;;
	https://*) SERVER_HTTP=$1; SERVER_WS="wss://${1#https://}" ;;
	ws://*) SERVER_WS=$1; SERVER_HTTP="http://${1#ws://}" ;;
	wss://*) SERVER_WS=$1; SERVER_HTTP="https://${1#wss://}" ;;
	*) die "--server 必须是完整 URL（http(s):// 或 ws(s)://）：$1" ;;
	esac
	SERVER_WS=${SERVER_WS%/}
	SERVER_HTTP=${SERVER_HTTP%/}
	# 这些字符会让 anode.yaml / curl 出现歧义（空白、引号、注释符、反斜杠）。
	if ! printf '%s' "$SERVER_WS" | grep -qE '^[A-Za-z0-9._~:/?#@!$&()*+,;=%~-]+$'; then
		die "--server 含 URL 里不该出现的字符：$SERVER_WS"
	fi
}

validate_identity() {
	[ -n "$SERVER" ] || die "缺少 --server（中枢地址猜不到；重跑旧安装时可从 $CONFIG 读回，文件在才能省）
  · 控制台「节点」页注册节点后会给一条完整命令
  · 只想试运行：加 --dry-run"
	normalize_server "$SERVER"
	[ -n "$NAME" ] || default_name
	if ! printf '%s' "$NAME" | grep -qE '^[a-z0-9][a-z0-9._-]{0,62}$'; then
		die "节点名不合法：$NAME（只允许小写字母/数字/._-，最长 63，首字符必须是字母或数字）"
	fi
	[ -n "$TOKEN" ] || die "缺少 --token 或 --token-file（节点令牌只在注册时显示一次）
  · 已经装过？那就在 $CONFIG；文件丢了只能用新的 --token 重新给
  · 只想试运行：加 --dry-run"
	if ! printf '%s' "$TOKEN" | grep -qE '^[A-Za-z0-9._~+/=-]+$'; then
		die "令牌里出现了非常规字符（多半是复制时带进了空白/引号/换行），请重新复制"
	fi
	# 换令牌是显式动作（anode install 默认拒绝覆盖不同 token）：命令行给了新令牌才带上。
	if [ "$UPGRADE" = "yes" ] && [ "$TOKEN_FROM_CLI" = "yes" ] && [ -n "$EXISTING_TOKEN" ] && [ "$TOKEN" != "$EXISTING_TOKEN" ]; then
		UPDATE_TOKEN="yes"
	fi
}

# ---------- 下载 ----------
write_probe_config() { # 预检专用临时 anode.yaml（0600）：只含身份三键，不落正式路径
	{
		printf 'name: %s\n' "$NAME"
		printf 'server: %s\n' "$SERVER_WS"
		printf 'token: %s\n' "$TOKEN"
	} >"$1"
	chmod 600 "$1"
}

# 校验和只接受「恰好一条 digest」：绝不使用远端 checksum 里的文件名/路径（不 eval、不拼路径）。
read_digest() { # read_digest <checksum 文件>
	[ -f "$1" ] || die "缺校验和文件：$1（D4：没有 sha256 一律不装）"
	_d=$(awk 'NF { n++; s = $1 } END { if (n != 1) exit 1; print s }' "$1") ||
		die "校验和文件的形状不对（必须恰好一行摘要，不能有多行/空行+多摘要）：$1"
	case "$_d" in
	'' | *[!0-9a-fA-F]*) die "校验和不是十六进制：$1" ;;
	esac
	[ "${#_d}" -eq 64 ] || die "校验和长度不是 64：$1"
	printf '%s' "$_d"
}

verify_checksum() {
	printf '%s  %s\n' "$1" "$DL_BIN" | sha256sum --check --status ||
		die "sha256 不匹配：下载可能被截断或篡改（want=$1）
  · 网络中断也会导致不匹配：重试一次
  · 仍不一致就别装了，去 release 页手工核对（脚本不做'未校验也放行'的降级）"
}

download_via_url() { # download_via_url <bin-url> <checksum-url>
	_u=$1
	_c=${CHECKSUM_URL:-$2}
	if [ "$DRY_RUN" = "yes" ]; then
		printf '  [dry-run] 将下载 %s 与 %s\n' "$_u" "$_c"
		return 0
	fi
	printf '下载 %s …\n' "$_u"
	curl --fail --location --connect-timeout 10 --max-time 300 --output "$DL_BIN" "$_u" ||
		die "下载失败：$_u
  · 核对 URL 是否可达（内网/镜像请用 --download-url + --checksum-url）
  · 想钉版本：--version <tag>（走 GitHub Release）
  · 走代理：export https_proxy=…"
	curl --fail --location --connect-timeout 10 --max-time 300 --output "$TMP/checksum" "$_c" ||
		die "校验和下载失败：$_c（D4：拿不到 sha256 就不装）
  · 用 --checksum-url 指定摘要地址；摘要必须是 <sha256sum 两空格格式> 的单行"
	_digest=$(read_digest "$TMP/checksum")
	verify_checksum "$_digest"
	chmod 0700 "$DL_BIN"
	info "校验和 OK（sha256 ${_digest}）"
}

download_via_hub() {
	[ -n "$SERVER_HTTP" ] || die "缺少 --server：脚本猜不到中枢，无法用 /v1/node/binary 下载"
	[ -n "$TOKEN" ] || die "从中枢下载需要节点令牌：--token nt_xxx 或 --token-file PATH
  · 或者绕开中枢：--version <tag> / --download-url <URL> + --checksum-url <URL>（D4 不允许无校验下载）"
	if [ "$DRY_RUN" = "yes" ]; then
		printf '  [dry-run] 将请求 %s/v1/node/binary（Authorization: Bearer <token>），从响应头取 X-Agent-Sha256/X-Agent-Version\n' "$SERVER_HTTP"
		return 0
	fi
	printf '从中枢下载节点二进制：%s/v1/node/binary …\n' "$SERVER_HTTP"
	ensure_tmp
	# D9：令牌不进 argv（同机用户可以从 /proc/<pid>/cmdline 读到 curl 的参数）。
	# curl 用 --config 从 0600 文件读 header；文件在 0700 的 $TMP 里，随临时目录一起删除。
	_hdr="$TMP/curl-auth.conf"
	printf 'header = "Authorization: Bearer %s"\n' "$TOKEN" >"$_hdr"
	chmod 600 "$_hdr"
	# 不用 -f：501/401 要靠状态码区分并给不同的下一步。
	_code=$(curl --silent --show-error --location --connect-timeout 10 --max-time 300 \
		--config "$_hdr" \
		--dump-header "$TMP/headers" --output "$DL_BIN" \
		--write-out '%{http_code}' "$SERVER_HTTP/v1/node/binary") ||
		die "无法访问中枢：$SERVER_HTTP/v1/node/binary
  · 核对 --server 能否从这台机器访问（curl -I $SERVER_HTTP/healthz）
  · TLS 自签？先让系统信任证书链
  · 想跳过中枢：--version <tag> 或 --download-url <URL>"
	case "$_code" in
	200) ;;
	401)
		die "令牌无效（HTTP 401）
  · 节点可能在控制台被删了，或令牌属于另一台中枢/另一个命名空间
  · 去控制台「节点」页重新注册，把新的 --token 带上来"
		;;
	501)
		die "这台中枢没带节点二进制（HTTP 501：ANOTIFY_AGENT_BIN 没配置）
  · 改用发布版：--version <tag>（GitHub Release anode_linux_${ARCH}）
  · 或直接给 URL：--download-url <URL> [--checksum-url <URL>]"
		;;
	*)
		die "中枢返回 HTTP $_code（期望 200）——/v1/node/binary 不可用
  · 看中枢日志；或改用 --version/--download-url"
		;;
	esac
	_digest=$(awk 'tolower($1) == "x-agent-sha256:" { gsub(/\r/, "", $2); print $2 }' "$TMP/headers" | tail -n 1)
	_ver=$(awk 'tolower($1) == "x-agent-version:" { gsub(/\r/, "", $2); print $2 }' "$TMP/headers" | tail -n 1)
	[ -n "$_digest" ] || die "中枢响应缺 X-Agent-Sha256 头（D4：无法校验就不装）
  · 这一版中枢的升级源不完整（见 internal/server/agent_binary.go 的头）
  · 改用 --version <tag> 或 --download-url <URL>+--checksum-url"
	case "$_digest" in
	*[!0-9a-fA-F]*) die "中枢给的 X-Agent-Sha256 不是十六进制" ;;
	esac
	[ "${#_digest}" -eq 64 ] || die "中枢给的 X-Agent-Sha256 长度不是 64"
	verify_checksum "$_digest"
	chmod 0700 "$DL_BIN"
	[ -z "$_ver" ] || [ -n "$VERSION" ] || VERSION=$_ver
	info "校验和 OK（sha256 ${_digest}${_ver:+，中枢版本 $_ver}）"
}

download_binary() {
	if [ "$DRY_RUN" != "yes" ]; then
		need curl "apt install curl / apk add curl——下载节点二进制要用它"
		need sha256sum "coreutils——校验二进制要用它（没有它就不装，不做未校验降级）"
		ensure_tmp
		DL_BIN="$TMP/anode"
	fi
	if [ -n "$DL_URL" ]; then
		download_via_url "$DL_URL" "$DL_URL.sha256"
	elif [ -n "$VERSION" ]; then
		# 版本号进 URL：只接受安全字符（不允许 ../、空白、?、& 之类）。
		case "$VERSION" in
		'' | *[!A-Za-z0-9._-]*) die "--version / ANODE_VERSION 只能含字母数字和 . _ -：$VERSION" ;;
		esac
		_asset="anode_linux_${ARCH}"
		download_via_url "${BASE%/}/${VERSION}/${_asset}" "${BASE%/}/${VERSION}/${_asset}.sha256"
	else
		download_via_hub
	fi
	# 预演不跑下载来的二进制，也不核对版本（没东西可核对）。
	if [ "$DRY_RUN" = "yes" ]; then
		return 0
	fi
	check_binary_version
}

# 版本核对用**前缀**匹配：构建标签的形状是 `<release>+<commit>`（与中枢
# internal/version.String() 同一个串，节点 hello 上报的也是它）。所以接受
# `"version":"<release>"` 与 `"version":"<release>+…"` 两种；不相关的版本
# （坏二进制/被换过的包）仍然拒装。
check_binary_version() {
	_info=$("$DL_BIN" --version) || die "下载的二进制跑不起来（--version 失败）：架构不对或文件损坏"
	case "$_info" in
	*'"version":"'*) ;;
	*) die "下载的二进制 --version 输出形状不对（期望单行 JSON）：$_info" ;;
	esac
	if [ -n "$VERSION" ]; then
		case "$_info" in
		*'"version":"'"$VERSION"'"'* | *'"version":"'"$VERSION"'+'*) ;;
		*) die "下载的二进制版本不匹配：want $VERSION，got $_info
  · 换一台镜像/清代理缓存后重试
  · 或钉一个明确的 --version" ;;
		esac
		info "版本核对 OK：✓ $_info"
	else
		info "版本：$_info（未指定 --version，跳过发布版本比对）"
	fi
}

# ---------- 停旧服务 / 预检 / 服务启停 ----------
stop_old_service() {
	if [ "$DRY_RUN" = "yes" ]; then
		printf '  [dry-run] 若已有 %s 在跑：先 systemctl stop（预检要用同一个令牌，不能两条连接抢）\n' "$UNIT_BASENAME"
		return 0
	fi
	_stopped="no"
	if [ "$SUDO_MODE" != "none" ] && has_systemctl; then
		if [ -f "$UNIT_FILE" ] || [ -f "/etc/systemd/system/${UNIT_BASENAME}" ]; then
			if systemctl_system stop "$UNIT_BASENAME" 2>/dev/null; then
				_stopped="yes"
				STOPPED_UNIT="system"
			fi
		fi
	fi
	if has_systemctl && [ -f "${RUN_HOME}/.config/systemd/user/${UNIT_BASENAME}" ]; then
		if systemctl_user stop "$UNIT_BASENAME" 2>/dev/null; then
			_stopped="yes"
			[ -n "$STOPPED_UNIT" ] || STOPPED_UNIT="user"
		fi
	fi
	[ "$_stopped" = "no" ] || info "已停下旧的服务实例（同一个令牌不能有两条连接）"
}

# 失败回滚：升级前把旧实例 stop 了，如果后面的 install/restart 失败，必须把它重新拉起来。
# 否则一次失败的升级 = 节点掉线（D8 的“重跑=升级”不能有这个副作用）。
restore_old_service() {
	[ -n "$STOPPED_UNIT" ] || return 0
	[ "$DRY_RUN" = "yes" ] && return 0
	_was=$STOPPED_UNIT
	STOPPED_UNIT="" # 只补偿一次：后面的 die 不该再 start 一次
	if [ "$_was" = "system" ]; then
		if systemctl_system start "$UNIT_BASENAME" >/dev/null 2>&1; then
			warn "已恢复升级前的服务（$UNIT_BASENAME，system）—— 节点没被这次失败留在停止态"
			return 0
		fi
	elif systemctl_user start "$UNIT_BASENAME" >/dev/null 2>&1; then
		warn "已恢复升级前的服务（$UNIT_BASENAME，user）—— 节点没被这次失败留在停止态"
		return 0
	fi
	warn "恢复升级前的服务失败：$UNIT_BASENAME 现在是停止态，手动启动：systemctl start $UNIT_BASENAME（或 $(service_hint "$_was"))"
}

preflight() {
	if [ "$NO_PREFLIGHT" = "yes" ]; then
		info "跳过预检（--no-preflight）"
		return 0
	fi
	if [ "$DRY_RUN" = "yes" ]; then
		printf '  [dry-run] 将用临时配置预检：%s serve --check --config <tmp>/preflight.yaml --data <tmp>/data（最多 %s 秒）\n' "$DL_BIN" "$PREFLIGHT_SEC"
		return 0
	fi
	printf '预检：用临时配置连一次中枢（anode serve --check，最多 %s 秒）…\n' "$PREFLIGHT_SEC"
	ensure_tmp
	_pf_cfg="$TMP/preflight.yaml"
	_pf_log="$TMP/preflight.log"
	# 临时配置只含身份三键、只在 $TMP（0700）里，预检完立刻删（D5/D9）。
	write_probe_config "$_pf_cfg"
	# 预检**不碰正式数据目录**：这一步只验证身份与中枢可达，而 /var/lib/anode 往往
	# 非 root 建不了（那会变成一条假的"预检失败"）。
	_pf_rc=0
	if command -v timeout >/dev/null 2>&1; then
		timeout "$PREFLIGHT_SEC" "$DL_BIN" serve --check --config "$_pf_cfg" --data "$TMP/data" >"$_pf_log" 2>&1 || _pf_rc=$?
	else
		# 没有 timeout(1)：退化成直接跑（anode 的 --check 自带 10s 超时）。
		"$DL_BIN" serve --check --config "$_pf_cfg" --data "$TMP/data" >"$_pf_log" 2>&1 || _pf_rc=$?
	fi
	rm -f "$_pf_cfg"
	if [ "$_pf_rc" -eq 0 ]; then
		sed 's/^/  /' "$_pf_log" 2>/dev/null || true
		info "预检通过"
		return 0
	fi
	printf '\n预检失败（退出码 %s）。anode 的输出（末 20 行）：\n' "$_pf_rc" >&2
	tail -n 20 "$_pf_log" 2>/dev/null | sed 's/^/  | /' >&2 || true
	if [ "$_pf_rc" -eq 124 ]; then
		printf '  %s 秒内没握手成功（外层 timeout 收掉了进程）。\n' "$PREFLIGHT_SEC" >&2
	fi
	if grep -qi 'unauthorized\|1008\|401' "$_pf_log" 2>/dev/null; then
		printf '  多半是令牌不对：节点可能被删了，或它属于另一个中枢/命名空间。\n' >&2
		printf '  去控制台「节点」页重新注册，把新的 --token 带上来。\n' >&2
	elif grep -qi 'no such host\|connection refused\|i/o timeout\|unreachable\|x509\|certificate' "$_pf_log" 2>/dev/null; then
		printf '  多半是网络/TLS：核对 --server（%s）能否从这台机器访问、证书链是否受信。\n' "$SERVER_WS" >&2
	elif grep -qi 'config' "$_pf_log" 2>/dev/null; then
		printf '  多半是配置：节点名/中枢地址格式不对（name 只允许小写字母数字/._-）。\n' >&2
	else
		printf '  原因见上面的 anode 输出；修好后重跑同一条命令即可。\n' >&2
	fi
	printf '  想跳过预检硬装（不推荐）：加 --no-preflight\n' >&2
	printf '  正式配置与服务都没有被改动。\n' >&2
	exit 2
}

# 轮询 is-active（最多 10s，每秒一次）：学前端的"立即判定"会误报失败。
wait_active() { # wait_active system|user
	# 预演下不能真的去问 systemd（那就不叫预演了）。
	if [ "$DRY_RUN" = "yes" ]; then
		return 0
	fi
	_i=0
	while [ "$_i" -lt 10 ]; do
		if [ "$1" = "system" ]; then
			"$SYSTEMCTL" is-active --quiet "$UNIT_BASENAME" 2>/dev/null && return 0
		else
			systemctl_user is-active --quiet "$UNIT_BASENAME" 2>/dev/null && return 0
		fi
		_i=$((_i + 1))
		[ "$_i" -lt 10 ] || break
		sleep 1
	done
	return 1
}

service_hint() {
	case "$1" in
	system) printf 'journalctl -u %s -f' "$UNIT_NAME" ;;
	user) printf 'journalctl --user -u %s -f' "$UNIT_NAME" ;;
	*) printf '%s serve --config %s --data %s' "$BINP" "$CONFIG" "$DATA" ;;
	esac
}

# 升级到**系统**服务时，旧的用户 unit 必须退位：同名的两份 anode 会抢同一个令牌，
# 表现成两条连接互相对跳（两边都"在线"又都没用）。这是 v3 踩过的坑，照旧清理。
retire_user_unit() {
	_old="${RUN_HOME}/.config/systemd/user/${UNIT_BASENAME}"
	[ -f "$_old" ] || return 0
	if [ "$DRY_RUN" = "yes" ]; then
		printf '  [dry-run] 将停用并删除旧的用户服务 %s（否则两份实例抢同一个令牌）\n' "$_old"
		return 0
	fi
	systemctl_user disable --now "$UNIT_BASENAME" 2>/dev/null || true
	rm_path "$_old"
	systemctl_user daemon-reload 2>/dev/null || true
	info "已移除旧的 systemd 用户服务（改为系统服务后不再依赖登录）"
}

start_service() {
	case "$SERVICE" in
	system)
		step "安装系统级服务（以 $RUN_USER 身份运行）"
		retire_user_unit
		systemctl_system daemon-reload ||
			die "systemctl daemon-reload 失败（见上面原文）"
		# enable 负责开机自启，restart 负责“现在跑的就是刚装的那版”（升级路径同样如此）。
		systemctl_system enable "$UNIT_BASENAME" ||
			die "systemctl enable $UNIT_BASENAME 失败（unit 是否真的写进了 $UNIT_FILE？）"
		# 服务以安装者身份跑：config（0600）与 data 必须在**restart 之前**归它（R1/C1）。
		# 顺序反了就是“先证明服务能跑、再让它能跑”：新进程起来第一件事就是读不到配置而退出，
		# wait_active 十秒后把安装判失败（unit 已 enable，节点停在停止态）。
		run_maybe_root chown -R "$RUN_USER" "$CONFIG" "$DATA" ||
			warn "chown $RUN_USER $CONFIG $DATA 失败：服务可能读不到配置（看 $(service_hint system)）"
		systemctl_system restart "$UNIT_BASENAME" || {
			_rc=$?
			restore_old_service
			die "systemctl restart $UNIT_BASENAME 失败（退出码 $_rc；见上面原文；日志：$(service_hint system)）"
		}
		if ! wait_active system; then
			restore_old_service
			die "$UNIT_BASENAME 10 秒内没有 active（systemctl is-active 一直是非 active）
  · 看日志：$(service_hint system)
  · 常见原因：令牌/中枢不对（预检已拦一遍）、数据目录不可写、unit 里的地址写错"
		fi
		;;
	user)
		step "安装 systemd 用户服务"
		systemctl_user daemon-reload 2>/dev/null || warn "连不上 systemd 用户实例（没有登录会话？）：$UNIT_FILE 已写好，登录后 systemctl --user enable --now $UNIT_BASENAME"
		systemctl_user enable "$UNIT_BASENAME" 2>/dev/null || true
		systemctl_user restart "$UNIT_BASENAME" 2>/dev/null ||
			warn "systemctl --user restart $UNIT_BASENAME 失败（见上面原文）"
		if ! wait_active user; then
			restore_old_service
			die "$UNIT_BASENAME 10 秒内没有 active（systemctl --user is-active 一直是非 active）
  · 看日志：$(service_hint user)
  · 没有登录会话/没有 dbus 的机器上，用户服务起不来：改用 --system（需要 sudo）或 --no-systemd"
		fi
		# 想在没登录时也活着只有 linger 一条路（这就是用户服务不如系统服务的地方）。
		# 失败只 warn：不因为 linger 没开成而否掉一次成功的安装。
		# 预演只打印：enable-linger 会写 /var/lib/systemd/linger/<user>，是真的改系统状态（R1/M1）。
		if [ "$DRY_RUN" = "yes" ]; then
			printf '  [dry-run] 若还没开：%s enable-linger %s（失败只 warn）\n' "$LOGINCTL" "$RUN_USER"
		elif command -v "$LOGINCTL" >/dev/null 2>&1; then
			if "$LOGINCTL" enable-linger "$RUN_USER" 2>/dev/null; then
				info "已开启 linger：没登录时服务也在跑"
			elif [ "$SUDO_MODE" != "none" ] && run_maybe_root "$LOGINCTL" enable-linger "$RUN_USER" 2>/dev/null; then
				info "已开启 linger（经 sudo）：没登录时服务也在跑"
			else
				warn "没开启 linger，登录态结束后服务会停。手动执行：
  sudo $LOGINCTL enable-linger $RUN_USER"
			fi
		fi
		;;
	none)
		step "只落盘（不装服务）"
		info "手工启动：$BINP serve --config $CONFIG --data $DATA"
		info "装成服务时把 $UNIT_BASENAME 放到 /etc/systemd/system/（或 \$HOME/.config/systemd/user/）后："
		info "  systemctl daemon-reload && systemctl enable --now $UNIT_BASENAME"
		;;
	esac
}

# ---------- 摘要 ----------
report() {
	state="未检查（dry-run）"
	if [ "$DRY_RUN" != "yes" ]; then
		case "$SERVICE" in
		system) if "$SYSTEMCTL" is-active --quiet "$UNIT_BASENAME" 2>/dev/null; then state="active (running)"; else state="未运行 —— 查：$(service_hint system)"; fi ;;
		user) if systemctl_user is-active --quiet "$UNIT_BASENAME" 2>/dev/null; then state="active (running)"; else state="未运行 —— 查：$(service_hint user)"; fi ;;
		*) state="未安装服务（仅二进制 + 配置）" ;;
		esac
	fi
	step "结论"
	if [ "$DRY_RUN" = "yes" ]; then
		info "安装预演（dry-run，系统没动）——真跑起来会是这个样子："
	fi
	case "$SERVICE" in
	system | user)
		if [ "$UPGRADE" = "yes" ]; then
			info "anode 已装好（升级完成）"
		else
			info "anode 已装好并已启动"
		fi
		info "  节点      $NAME"
		info "  中枢      $SERVER_WS"
		info "  运行身份  $RUN_USER（能力的权限 = 这个用户）"
		info "  二进制    $BINP"
		info "  配置      $CONFIG（0600：里面有令牌）"
		info "  数据      $DATA"
		info "  服务      $UNIT_BASENAME（$SERVICE）"
		info "  状态      $state"
		info "  日志      $(service_hint "$SERVICE")"
		;;
	*)
		if [ "$UPGRADE" = "yes" ]; then
			info "anode 已更新（升级完成；没有安装服务）"
		else
			info "anode 已装好（未安装服务）"
		fi
		info "  节点      $NAME"
		info "  中枢      $SERVER_WS"
		info "  二进制    $BINP"
		info "  配置      $CONFIG（0600：里面有令牌）"
		info "  数据      $DATA"
		info "  跑起来    $BINP serve --config $CONFIG --data $DATA"
		;;
	esac
	info "  升级      重跑同一条安装命令（沿用现有 anode.yaml，只换二进制 + restart）"
	info "  卸载      sh install-anode.sh --uninstall"
	info ""
	case "$SERVICE" in
	system | user) info "控制台「节点」页应该看到 $NAME 在线；没上线先看日志：$(service_hint "$SERVICE")" ;;
	*) info "控制台「节点」页到这个进程跑起来后才会看到 $NAME 在线：$(service_hint none)" ;;
	esac
}

# ---------- 卸载 ----------
#
# 卸载**不需要** token/server（D8 明确要求的）：它按已知落点逐个清，清不到就跳过。
# 所有提权动作走 try_root：没 sudo 时把能删的删掉并如实报告，而不是直接退出。
uninstall_one_unit_system() {
	_path=$1
	[ -f "$_path" ] || return 0
	try_root "$SYSTEMCTL" disable --now "$UNIT_BASENAME" 2>/dev/null || true
	try_root "$SYSTEMCTL" stop "$UNIT_BASENAME" 2>/dev/null || true
	if try_root rm -f "$_path"; then
		REMOVED="${REMOVED}  服务单元 $_path
"
	else
		warn "删不掉 $_path（权限？加 sudo 重试）"
	fi
}

uninstall_one_unit_user() {
	_path=$1
	[ -f "$_path" ] || return 0
	systemctl_user disable --now "$UNIT_BASENAME" 2>/dev/null || true
	systemctl_user stop "$UNIT_BASENAME" 2>/dev/null || true
	rm_path "$_path"
	systemctl_user daemon-reload 2>/dev/null || true
	REMOVED="${REMOVED}  用户服务单元 $_path
"
}

# _gone <path>：删掉它并回答"真的不在了吗"（卸载摘要只记成功的那几条，
# 否则权限不足时会打印一份骗人的"已移除"清单）。
_gone() {
	case "$1" in
	"$RUN_HOME"/*) rm_path "$1" ;;
	*) try_root rm -rf "$1" || true ;;
	esac
	if [ "$DRY_RUN" = "yes" ]; then
		return 0
	fi
	if [ -e "$1" ]; then
		FAILED=$((FAILED + 1))
		return 1
	fi
	return 0
}

do_uninstall() {
	REMOVED=""
	FAILED=0
	step "卸载 anode"
	# unit：system（含 ANODE_UNIT_PATH/prefix 覆盖）与 user 两处都清，不看 SERVICE 猜。
	for _u in "$UNIT_OVERRIDE" "$OPT_UNIT" "${PREFIX:+$PREFIX/etc/systemd/system/$UNIT_BASENAME}" "/etc/systemd/system/$UNIT_BASENAME"; do
		[ -n "$_u" ] || continue
		uninstall_one_unit_system "$_u"
	done
	uninstall_one_unit_user "${RUN_HOME}/.config/systemd/user/$UNIT_BASENAME"
	if [ -f "/etc/systemd/system/$UNIT_BASENAME" ] || [ -n "$UNIT_OVERRIDE" ]; then
		try_root "$SYSTEMCTL" daemon-reload 2>/dev/null || true
	fi

	# 二进制：脚本可能装在 prefix、系统路径或 ~/.anode/bin（v4.1 的用户服务布局）。
	for _b in "$OPT_BIN" "${PREFIX:+$PREFIX/usr/local/bin/anode}" "/usr/local/bin/anode" "${RUN_HOME}/.anode/bin/anode"; do
		[ -n "$_b" ] || continue
		[ -f "$_b" ] || continue
		if _gone "$_b"; then
			REMOVED="${REMOVED}  二进制 $_b
"
		else
			warn "删不掉 $_b（权限？加 sudo 重试）"
		fi
	done

	# 配置：默认删（令牌不该留在机器上）；--keep-config 保留。
	for _c in "$OPT_CONFIG" "${PREFIX:+$PREFIX/etc/anode/anode.yaml}" "/etc/anode/anode.yaml" "${RUN_HOME}/.anode/anode.yaml"; do
		[ -n "$_c" ] || continue
		[ -f "$_c" ] || continue
		if [ "$KEEP_CONFIG" = "yes" ]; then
			info "保留配置：$_c（令牌还在里面）"
			continue
		fi
		if _gone "$_c"; then
			REMOVED="${REMOVED}  配置 $_c
"
		else
			warn "删不掉 $_c（权限？加 sudo 重试）"
		fi
	done

	# 数据目录只在 --purge 时删（产物缓存/工作区可能还有用）。
	if [ "$PURGE" = "yes" ]; then
		for _d in "$OPT_DATA" "${PREFIX:+$PREFIX/var/lib/anode}" "/var/lib/anode" "${RUN_HOME}/.anode/data"; do
			[ -n "$_d" ] || continue
			[ -d "$_d" ] || continue
			if _gone "$_d"; then
				REMOVED="${REMOVED}  数据目录 $_d
"
			else
				warn "删不掉 $_d（权限？加 sudo 重试）"
			fi
		done
	fi

	if [ "$DRY_RUN" = "yes" ]; then
		info "  [dry-run] 以上只是预演，系统没动"
		return 0
	fi
	if [ -z "${REMOVED:-}" ] && [ "${FAILED:-0}" -eq 0 ]; then
		info "没找到装过的东西（服务单元/二进制/配置都不在）。"
	elif [ -z "${REMOVED:-}" ]; then
		warn "什么都没删掉（权限不足？用 sudo 重跑同一条命令）"
	else
		printf '已移除：\n%s' "$REMOVED"
		[ "${FAILED:-0}" -eq 0 ] || warn "另有 $FAILED 项没删掉（权限不足？用 sudo 重跑）"
	fi
	info ""
	info "节点在控制台里会显示离线；不需要它了就在「节点」页删掉这个节点。"
}

# ---------- 主流程 ----------
parse_args "$@"

if [ "$UNINSTALL" = "yes" ]; then
	detect_platform
	resolve_sudo
	RUN_USER=$(id -un)
	RUN_HOME=${HOME:-}
	[ -n "$RUN_HOME" ] || die "HOME 为空：无法确定用户级 unit 的落点"
	do_uninstall
	exit 0
fi

detect_platform
resolve_runner
resolve_sudo
resolve_service
require_runner
compute_paths
check_prefix_service_combo
read_existing_config
load_token_file

if [ "$DRY_RUN" = "yes" ]; then
	# 预演：不碰网络、不写盘、不提权。缺什么就用占位符顶上（--dry-run 不该因为缺 token 报错）。
	if [ -n "$SERVER" ]; then
		normalize_server "$SERVER"
	else
		SERVER_WS="<server>"
		SERVER_HTTP="<server>"
	fi
	[ -n "$NAME" ] || default_name
	[ -n "$TOKEN" ] || TOKEN="<token>"
	DL_BIN="<tmp>/anode"
	TOKFILE="<tmp>/token"
	step "安装预演（dry-run，系统没动）"
	[ "$SERVICE_ASSUMED" = "no" ] ||
		info "  注意      预演不实测 sudo：真跑时会先实测，不能提权就退化成用户服务"
	info "  节点      $NAME"
	info "  中枢      $SERVER_WS"
	info "  令牌      <token>（不打印）"
	info "  服务模式  $SERVICE"
	info "  二进制    $BINP"
	info "  配置      $CONFIG"
	info "  数据      $DATA"
	info "  unit      $UNIT_FILE"
else
	validate_identity
	ensure_tmp
	TOKFILE="$TMP/token"
	printf '%s\n' "$TOKEN" >"$TOKFILE"
	chmod 600 "$TOKFILE"
fi

download_binary

# 顺序很要紧：停旧服务（让出令牌）→ 预检（临时配置，正式配置一个字没动）→ 落盘 → 起服务。
#
# `--service none` 也预检（R1/M3）：契约 D5 只给了 `--no-preflight` 一个例外。"只落盘"同样要
# 先证明这把令牌能用，否则错令牌会以 exit 0 "装成功"却永远上不了线。这种模式**不停**旧服务
# （停了没人再拉起来），所以若真有一台旧实例在跑，预检会把它的连接顶掉一次，它 1s 内自愈
# （`--check` 的 hello 会整份覆盖 declared_capabilities，见 docs/anode-install.md §6）。
if [ "$SERVICE" != "none" ]; then
	stop_old_service
fi
preflight

step "落盘（anode install）"
# 安装参数**只有一个来源**（D1）：下面算出来的 flag 原样交给 anode install，脚本不重写
# flags、不写第二套 YAML/unit。令牌只能走 --token-file（不进程 argv、不进日志，D9）。
if [ -n "$PREFIX" ]; then
	set -- --name "$NAME" --server "$SERVER_WS" --token-file "$TOKFILE" --prefix "$PREFIX"
else
	set -- --name "$NAME" --server "$SERVER_WS" --token-file "$TOKFILE"
fi
case "$SERVICE" in
system)
	# 系统服务以安装者身份跑：模板要写 User=/Environment=HOME=/Environment=PATH=
	# （D7；不给 --unit-user 的话 unit 会以 root 跑，权限等于整台机器）。
	set -- "$@" --unit-user "$RUN_USER"
	if [ -n "$RUN_HOME" ]; then
		set -- "$@" --unit-home "$RUN_HOME"
	fi
	if [ -n "$UNIT_PATH_VALUE" ]; then
		set -- "$@" --unit-path "$UNIT_PATH_VALUE"
	fi
	;;
user)
	# 用户服务：unit 本来就是这个用户的，不写 User=；但要给它 HOME/PATH。
	set -- "$@" --bin "$BINP" --config "$CONFIG" --unit "$UNIT_FILE" --data "$DATA"
	if [ -n "$RUN_HOME" ]; then
		set -- "$@" --unit-home "$RUN_HOME"
	fi
	if [ -n "$UNIT_PATH_VALUE" ]; then
		set -- "$@" --unit-path "$UNIT_PATH_VALUE"
	fi
	;;
esac
# 显式覆盖（--config/--bin/--unit/--data/ANODE_UNIT_PATH）要传下去，否则落盘位置会和摘要不一致。
if [ "$SERVICE" != "user" ]; then
	[ -z "$OPT_CONFIG" ] || set -- "$@" --config "$CONFIG"
	[ -z "$OPT_BIN" ] || set -- "$@" --bin "$BINP"
	[ -z "$OPT_DATA" ] || set -- "$@" --data "$DATA"
fi
if [ -n "$UNIT_OVERRIDE" ] || [ -n "$OPT_UNIT" ]; then
	set -- "$@" --unit "$UNIT_FILE"
fi
[ "$UPDATE_TOKEN" = "no" ] || set -- "$@" --update-token

_install_rc=0
anode_install "$@" || _install_rc=$?
if [ "$_install_rc" -ne 0 ]; then
	# 落盘失败时先把刚 stop 的旧实例拉回来（否则一次失败的升级 = 节点掉线）。
	# 子进程的退出码写进诊断：不吞失败，也不假装成功（脚本自身按契约退 2）。
	restore_old_service
	die "anode install 失败（退出码 $_install_rc；原文见上）
  · 权限：root 拥有的 /etc、/usr/local/bin、/var/lib 需要提权 —— 让脚本自己 sudo，或用 sudo 重跑
  · 只想落到可写目录：--prefix \$HOME/.anode-stage
  · 已有配置/unit 与这次不一致时 anode install 会拒绝覆盖（保护用户数据）：确认后手工迁移或先 --uninstall
  · 换令牌要显式：--token / --token-file <新令牌>（脚本会自动带 --update-token）"
fi

if [ "$DRY_RUN" = "yes" ]; then
	start_service
	report
	info ""
	info "（预演结束：以上命令都没有真的执行）"
	exit 0
fi

rm -f "$TOKFILE"
start_service
report
