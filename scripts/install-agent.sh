#!/bin/sh
# Anotify Agent 原生安装脚本 —— 一条命令，装完即在线。
#
#   curl -fsSL https://raw.githubusercontent.com/zzjcool/anotify/main/scripts/install-agent.sh \
#     | sh -s -- --token=nt_xxx --server=https://anotify.example.com
#
# 一条命令做完七件事，中间不问任何问题，结尾给确定结论：
#   1. 下对平台的 anotify-agent，核对同名 .sha256（不匹配就中止，不降级）
#   2. 装到 ~/.anotify/bin（可写 → 以后 `anotify-agent upgrade` 免 sudo）；
#      `--prefix=/usr/local` 可装回系统目录（那样每次升级要 sudo）
#   3. 写 ~/.anotify/node.yaml（600）。重跑时沿用里面的令牌/中枢/名字
#   4. **预检**：先以前台方式连一次中枢，握手成功才继续。令牌错/中枢不通在这里就停，
#      不会留下一个开机就崩溃重启的服务（--no-preflight 关掉）
#   5. 装成 **系统级** systemd 服务、以你的身份跑：开机自启、没登录也活着、挂了会退避重启（3s 起，最多 30s）
#      —— 就是 tailscaled 那种存在方式。没有 sudo 的机器退化成用户服务（会尝试 linger）
#   6. 起来后核对 is-active，并打印节点/中枢/日志/升级/卸载
#   7. 顺带清理上一版装法留下的东西（用户服务单元、~/.local/bin 里的旧副本），
#      否则同名的两份 agent 会抢同一个令牌
#
# 重跑同一条命令 = 升级：只换二进制并重启服务，配置不动。
#   sh install-agent.sh --uninstall            卸载（保留 ~/.anotify 数据目录）
#   sh install-agent.sh --uninstall --purge    连数据目录一起删
#
# 为什么给原生装：docker 那条路门槛最低，但容器里**看不到宿主机文件**——
# 想在自己的项目目录里跑能力（make test、git status、就地改文件）就只能在宿主机上跑。
#
# 安全（必读）：agent 的执行权限 = 运行它的用户权限。
#   - 服务用 User=<安装者> 跑，不是 root；脚本本身也拒绝以 root 身份运行（--allow-root 例外）
#   - **节点上没有"工作目录"配置**：能力跑在哪个目录由能力定义（run.workDir）决定，
#     空 = 每次任务一个独立临时目录。所以这台机器上不存在"节点级白名单"这种别扭东西，
#     但代价是「能力说在哪跑就在哪跑」—— 这也正是 agent 不该用 root 跑的原因
#
# 退出码：0 成功（含 --uninstall）；非 0 失败并给出可执行的下一步。
set -eu

OWNER="zzjcool"
REPO="anotify"

# ---- 参数默认值（解析见 parse_args） ----
PREFIX=""
PREFIX_EXPLICIT="no"
TOKEN=""
SERVER=""
NAME=""
INSTANCES=""
SERVICE="auto" # auto | system | user | none
UNINSTALL="no"
PURGE="no"
KEEP_CONFIG="no"
ALLOW_ROOT="no"
NO_SUDO="no"
NO_PREFLIGHT="no"
PREFLIGHT_SEC="${ANOTIFY_PREFLIGHT_SEC:-20}"
DRY_RUN="no"

# 这三个环境变量是测试钩子（自测脚本用它把 sudo/systemctl/unit 路径换掉），
# 正常情况下不要设：默认值就是真实系统里的东西。
SUDO="${ANOTIFY_SUDO:-sudo}"
SYSTEMCTL="${ANOTIFY_SYSTEMCTL:-systemctl}"
UNIT_PATH="${ANOTIFY_UNIT_PATH:-/etc/systemd/system/anotify-agent.service}"

tmp=""
UNIT_NAME="anotify-agent"
USER_UNIT_PATH="${HOME}/.config/systemd/user/${UNIT_NAME}.service"
DATA_DIR="${HOME}/.anotify"
CFG="${DATA_DIR}/node.yaml"

die() {
	printf 'install-agent: %s\n' "$1" >&2
	exit 1
}

need() {
	command -v "$1" >/dev/null 2>&1 || die "缺少 $1，请先安装它"
}

warn() {
	printf '注意：%s\n' "$1" >&2
}

usage() {
	cat <<'EOF'
用法：install-agent.sh [选项]

必填（首次安装；重跑时从 ~/.anotify/node.yaml 读，可省略）：
  --token=nt_xxx          节点令牌（控制台「节点」页注册后只显示一次）
  --server=https://...    中枢地址（原生装猜不到，猜错会表现成"连不上"）

节点配置：
  --name=box-1            节点名（缺省用主机名）
  --instances=2           并发槽位数（缺省沿用已有配置，否则 1）

服务怎么装：
  --system                强制系统级 systemd 服务（默认：能 sudo 就用它）
  --user                  强制 systemd 用户服务（不碰 sudo；未登录时靠 linger）
  --no-systemd            只装二进制+配置，不装服务（前台自己跑）
  --no-sudo               不尝试 sudo（等价于 --user 的保守档：需要 root 的步骤直接失败）
  --prefix=/opt           安装前缀（二进制落到 <prefix>/bin，默认 ~/.anotify）

行为开关：
  --no-preflight          跳过"先连一次中枢"的预检（不推荐）
  --preflight-timeout=20  预检最长等待秒数
  --dry-run               只打印将要做什么，不动系统（含服务单元内容）
  --allow-root            允许以 root 运行（不推荐：agent 的权限 = 它的用户权限）
  -h, --help              显示这段帮助

卸载：
  --uninstall             停服务、删单元与二进制、删 node.yaml（保留数据目录）
  --purge                 配合 --uninstall：连 ~/.anotify（产物缓存/工作目录）一起删
  --keep-config           配合 --uninstall：保留 node.yaml（令牌还在，可先备份）
EOF
}

# ---- 参数解析 ----
parse_args() {
	for arg in "$@"; do
		case "$arg" in
		--token=*) TOKEN="${arg#*=}" ;;
		--server=*) SERVER="${arg#*=}" ;;
		--name=*) NAME="${arg#*=}" ;;
		--instances=*) INSTANCES="${arg#*=}" ;;
		--prefix=*)
			# ${arg#*=/opt} 是错的写法（模式里带 /opt 永远匹配不上，结果把整串当路径）；
			# 这里显式去掉前缀，并容忍结尾多余的斜杠。
			p="${arg#--prefix=}"
			PREFIX="${p%/}"
			PREFIX_EXPLICIT="yes"
			;;
		--service=*)
			SERVICE="${arg#*=}"
			case "$SERVICE" in
			auto | system | user | none) ;;
			*) die "未知的服务模式：$SERVICE（auto|system|user|none）" ;;
			esac
			;;
		--system) SERVICE="system" ;;
		--user) SERVICE="user" ;;
		--no-systemd) SERVICE="none" ;;
		--no-sudo) NO_SUDO="yes" ;;
		--no-preflight) NO_PREFLIGHT="yes" ;;
		--preflight-timeout=*) PREFLIGHT_SEC="${arg#*=}" ;;
		--dry-run) DRY_RUN="yes" ;;
		--uninstall) UNINSTALL="yes" ;;
		--purge) PURGE="yes" ;;
		--keep-config) KEEP_CONFIG="yes" ;;
		--allow-root) ALLOW_ROOT="yes" ;;
		-h | --help)
			usage
			exit 0
			;;
		*) die "未知参数：$arg（--help 看全部）" ;;
		esac
	done
}

# ---- 特权操作：root 直接跑、否则走 sudo、dry-run 只打印 ----
#
# 为什么不整段 `sudo sh` 重跑自己：那样脚本体就成了 root 的，容易在 $HOME、
# PATH、缓存目录上做出"root 的文件"。这里只让**真正需要特权的那几条命令**提权，
# 其余（下二进制、写 node.yaml、预检）都在你身份下做完。
as_root() {
	if [ "$DRY_RUN" = "yes" ]; then
		printf '  [dry-run] sudo %s\n' "$*"
		return 0
	fi
	if [ "$(id -u)" = "0" ]; then
		"$@"
	else
		$SUDO "$@"
	fi
}

# as_root_tee <dest>：把 stdin 写进 root 拥有的文件（单元文件）。
# dry-run 下按缩进回显，好让运维先看清要装的是什么。
as_root_tee() {
	if [ "$DRY_RUN" = "yes" ]; then
		printf '  [dry-run] 将写入 %s：\n' "$1"
		sed 's/^/      /' >&2
		return 0
	fi
	if [ "$(id -u)" = "0" ]; then
		cat >"$1"
	else
		$SUDO tee "$1" >/dev/null
	fi
}

# as_user：用户级 systemctl（--user）。dry-run 只打印——
# 否则 `--dry-run` 会真的去 enable/restart 一个服务，那就不是预演了。
as_user() {
	if [ "$DRY_RUN" = "yes" ]; then
		printf '  [dry-run] systemctl --user %s\n' "$*"
		return 0
	fi
	$SYSTEMCTL --user "$@"
}

# rm_path：删一个**归你所有**的路径（配置、用户单元、数据目录）。
# root 拥有的东西走 as_root rm（它自带 dry-run 与 sudo 处理）。
rm_path() {
	if [ "$DRY_RUN" = "yes" ]; then
		printf '  [dry-run] 将删除 %s\n' "$1"
		return 0
	fi
	rm -rf "$1"
}

sudo_usable() {
	if [ "$(id -u)" = "0" ]; then
		return 0
	fi
	if [ "$NO_SUDO" = "yes" ]; then
		return 1
	fi
	# 已有缓存票据（或 NOPASSWD）→ 静默可用。
	if $SUDO -n true >/dev/null 2>&1; then
		return 0
	fi
	# 需要密码：只在有终端时试一次（`curl | sh` 时 stdin 是管道，sudo 走 /dev/tty）。
	# 无终端（CI/无人值守）就干脆不试，避免脚本卡在等密码上。
	if [ -r /dev/tty ] && [ -w /dev/tty ]; then
		if $SUDO -v </dev/tty >/dev/null 2>&1; then
			return 0
		fi
	fi
	return 1
}

# ---- 平台 ----
detect_platform() {
	need uname
	os=$(uname -s | tr '[:upper:]' '[:lower:]')
	case "$os" in
	linux | darwin) ;;
	*) die "不支持的平台：$os（可以自己下载 binary 或改用 docker）" ;;
	esac

	arch=$(uname -m)
	case "$arch" in
	x86_64 | amd64) arch="amd64" ;;
	aarch64 | arm64) arch="arm64" ;;
	*) die "不支持的架构：$arch" ;;
	esac
}

# ---- 已有配置（升级靠它：重跑时不用再给令牌） ----
cfg_get() {
	[ -f "$CFG" ] || return 0
	sed -n "s/^$1:[[:space:]]*//p" "$CFG" | head -n1 | sed -e 's/[[:space:]]*$//' -e 's/^"//' -e 's/"$//'
}

load_existing_config() {
	existing_token=$(cfg_get token)
	existing_server=$(cfg_get server)
	existing_name=$(cfg_get name)
	existing_instances=$(cfg_get instances)

	if [ -f "$CFG" ]; then
		UPGRADE="yes"
	else
		UPGRADE="no"
	fi

	# 命令行优先，其次沿用文件里的：一条 `curl|sh` 重跑就是升级。
	if [ -z "$TOKEN" ]; then TOKEN="$existing_token"; fi
	if [ -z "$SERVER" ]; then SERVER="$existing_server"; fi
	if [ -z "$NAME" ]; then NAME="$existing_name"; fi
	if [ -z "$INSTANCES" ]; then INSTANCES="$existing_instances"; fi
}

validate_config() {
	[ -n "$TOKEN" ] || die "缺少 --token（控制台「节点」页注册节点后可得，只显示一次）
  · 已经装过？那就在 $CFG；文件丢了只能用 --token 重新给"
	[ -n "$SERVER" ] || die "缺少 --server（原生装无法猜测中枢地址）"

	case "$SERVER" in
	http://* | https://*) ;;
	*) die "--server 必须是完整 URL（要带 http:// 或 https://）：$SERVER" ;;
	esac
	SERVER="${SERVER%/}"

	if [ -n "$INSTANCES" ]; then
		case "$INSTANCES" in
		*[!0-9]*) die "--instances 必须是正整数：$INSTANCES" ;;
		esac
		[ "$INSTANCES" -gt 0 ] || die "--instances 必须大于 0：$INSTANCES"
	fi
}

# ---- 下载 + 完整性校验 ----
download_binary() {
	# 版本：优先 $ANOTIFY_VERSION（可钉住），否则取 latest。
	#
	# ANOTIFY_RELEASE_BASE 是给**私有化 / 内网镜像**用的：把 release 资产放到自己的
	# 文件服务器上，安装脚本指向它就行（离线环境、或被墙的机器都靠这个）。
	# 它同时是测试入口——本地起个 http.server 就能把整条安装链路跑一遍。
	version="${ANOTIFY_VERSION:-latest}"
	if [ -n "${ANOTIFY_RELEASE_BASE:-}" ]; then
		asset_base="${ANOTIFY_RELEASE_BASE%/}"
	elif [ "$version" = "latest" ]; then
		asset_base="https://github.com/${OWNER}/${REPO}/releases/latest/download"
	else
		asset_base="https://github.com/${OWNER}/${REPO}/releases/download/${version}"
	fi

	asset="anotify-agent-${os}-${arch}"
	url="${asset_base}/${asset}"

	if [ "$DRY_RUN" = "yes" ]; then
		printf '  [dry-run] 将下载 %s\n' "$url"
		tmp="${TMPDIR:-/tmp}/${asset}"
		return 0
	fi

	printf '下载 %s …\n' "$url"
	tmp=$(mktemp)
	trap 'rm -f "${tmp:-}" "${tmp:-}.sha256"' EXIT INT TERM
	if ! curl -fsSL "$url" -o "$tmp"; then
		die "下载失败：$url
  · 检查这个平台/架构是否已发布（github.com/${OWNER}/${REPO}/releases）
  · 想钉版本：ANOTIFY_VERSION=v1.2.3 再跑一次
  · 用代理：export https_proxy=...
  · 内网/镜像：export ANOTIFY_RELEASE_BASE=https://your-mirror/anotify
  · 也可以改用 docker 一条命令（控制台里那份命令）"
	fi

	# 完整性校验：release 里每个资产都带 <asset>.sha256（sha256sum 的两空格格式）。
	#
	# 校验失败就**中止**，不降级：这个二进制会以你的用户身份执行任意脚本，
	# 下载链路被投毒（DNS/代理/镜像）而没有校验 = 直接把机器交出去。
	# 没有 sha256sum/shasum 时不静默通过，而是明确打印"未校验"——让用户自己决定。
	sumfile="$tmp.sha256"
	if ! curl -fsSL "${url}.sha256" -o "$sumfile" 2>/dev/null; then
		warn "release 里没有 ${asset}.sha256，跳过完整性校验"
	else
		want=$(awk '{print $1}' "$sumfile" | head -n1)
		if [ -z "$want" ]; then
			warn "校验和文件是空的，跳过完整性校验"
		else
			if command -v sha256sum >/dev/null 2>&1; then
				got=$(sha256sum "$tmp" | awk '{print $1}')
			elif command -v shasum >/dev/null 2>&1; then
				got=$(shasum -a 256 "$tmp" | awk '{print $1}')
			elif command -v openssl >/dev/null 2>&1; then
				got=$(openssl dgst -sha256 "$tmp" | awk '{print $NF}')
			else
				warn "本机没有 sha256sum/shasum/openssl，跳过完整性校验"
				got="$want"
			fi
			if [ "$got" != "$want" ]; then
				die "校验和不匹配（下载可能被篡改或截断）
  want: $want
  got:  $got
  · 重试一次：网络中断也会导致不匹配
  · 仍不一致就别装了，去 github.com/${OWNER}/${REPO}/releases 手工核对"
			fi
			printf '校验和 OK（%s）\n' "$got"
		fi
	fi

	chmod 0755 "$tmp"
}

# ---- 配置落盘 ----
#
# write_config_file <dest>：只负责把四个键写进指定文件（预检用临时文件、成功后写正式的）。
# 先写临时文件再落正式的：预检失败时正式配置一个字没动，旧服务拉起来还能读到原来的值。
write_config_file() {
	dest="$1"
	# 用 if 而不是 `[ -n "$X" ] && printf`：最后一条语句返回 1 会被 set -e 当失败，
	# 表现成"没给 --name 时脚本写完配置就神秘退出"。
	{
		printf '# 由 install-agent.sh 生成（%s）\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
		printf 'server: %s\n' "$SERVER"
		printf 'token: %s\n' "$TOKEN"
		if [ -n "$NAME" ]; then printf 'name: %s\n' "$NAME"; fi
		if [ -n "$INSTANCES" ]; then printf 'instances: %s\n' "$INSTANCES"; fi
	} >"$dest"
	chmod 600 "$dest"
}

write_config() {
	if [ "$DRY_RUN" = "yes" ]; then
		printf '  [dry-run] 将写入 %s\n' "$CFG"
		return 0
	fi
	mkdir -p "$DATA_DIR" || die "无法创建 $DATA_DIR"
	write_config_file "$CFG"
	if [ "$UPGRADE" = "yes" ]; then
		printf '已更新配置：%s（沿用已有令牌/中枢，权限 600）\n' "$CFG"
	else
		printf '已写配置：%s（权限 600：里面有令牌）\n' "$CFG"
	fi
}

# ---- 预检：先连一次中枢 ----
#
# 为什么值得多花这几秒：服务一旦 enable，令牌错的表现是"每 3 秒崩一次、没人看得见"。
# 在这里以前台方式跑一次，握手成功（agent.handshake.ok）才算通过——令牌对不对、
# 中枢通不通、TLS 认不认，全在这一个动作里验完了。
# preflight_once：跑一次前台握手，成功返回 0；失败时把 agent 输出留在 $probe_log 里。
probe_log=""
probe_cfg=""
node_id=""
preflight_once() {
	probe_log=$(mktemp)
	probe_cfg=$(mktemp)
	write_config_file "$probe_cfg"

	# 预检要的是「现在能不能连上」这个立刻的答案，所以用 -no-retry 只试一次：
	# 常驻服务里 agent 自己会进程内退避重连，而这里必须马上失败好让安装中止。
	"$tmp" -no-retry -f "$probe_cfg" >"$probe_log" 2>&1 &
	probe_pid=$!
	i=0
	limit=$((PREFLIGHT_SEC * 4))
	while [ "$i" -lt "$limit" ]; do
		if grep -q 'agent.handshake.ok' "$probe_log" 2>/dev/null; then
			node_id=$(sed -n 's/.*node_id=\([^ ]*\).*/\1/p' "$probe_log" | head -n1)
			kill "$probe_pid" 2>/dev/null || true
			wait "$probe_pid" 2>/dev/null || true
			return 0
		fi
		# 进程已经退了（配置错、连接被拒、401）：不用再等满超时。
		if ! kill -0 "$probe_pid" 2>/dev/null; then
			break
		fi
		i=$((i + 1))
		sleep 0.25
	done
	kill "$probe_pid" 2>/dev/null || true
	wait "$probe_pid" 2>/dev/null || true
	return 1
}

# drop_stale_index：把 ~/.anotify/index 挪开（保留备份），返回 0 表示真挪了。
#
# 为什么需要：hello 帧会申报"本机缓存了哪些产物"，而中枢对不认识的能力是**整条连接
# 拒绝**（unknown_capability）。于是中枢上删掉或改名一个能力，曾缓存过它的节点就再也
# 连不上，报错还发生在安装的最后一步，看起来像"装不上"。产物是内容寻址的缓存、中枢
# 会重新部署，所以挪走索引是安全的；备份留着，排查时还能看。
drop_stale_index() {
	[ -d "$DATA_DIR/index" ] || return 1
	if [ "$DRY_RUN" = "yes" ]; then
		return 1
	fi
	backup="${DATA_DIR}/index.stale-$(date -u +%Y%m%dT%H%M%SZ)"
	mv "$DATA_DIR/index" "$backup" 2>/dev/null || return 1
	printf '本地缓存里有中枢已不认识的产物，已挪开：%s\n' "$backup"
	printf '（产物只是缓存，中枢会重新部署；确认没用后可删掉这个备份）\n'
	return 0
}

preflight() {
	if [ "$DRY_RUN" = "yes" ]; then
		printf '  [dry-run] 将以前台方式连一次中枢做预检\n'
		return 0
	fi
	printf '预检：以前台方式连一次中枢 …\n'

	if preflight_once; then
		rm -f "$probe_log" "$probe_cfg"
		printf '预检通过：握手成功（node_id=%s）\n' "${node_id:-?}"
		return 0
	fi

	# 自愈一次：unknown_capability 是"历史缓存 vs 中枢现状"的冲突，不是配置错。
	# 挪掉索引重试——用户不该为了一个过期的缓存去读懂 handshake rejected。
	if grep -q 'unknown_capability' "$probe_log" 2>/dev/null; then
		if drop_stale_index; then
			rm -f "$probe_log" "$probe_cfg"
			if preflight_once; then
				rm -f "$probe_log" "$probe_cfg"
				printf '预检通过：握手成功（node_id=%s）\n' "${node_id:-?}"
				return 0
			fi
		fi
	fi

	printf '\n预检失败：%s 秒内没能和中枢握手。agent 的输出（末 20 行）：\n' "$PREFLIGHT_SEC" >&2
	sed 's/^/  | /' "$probe_log" >&2 || true

	# 先按日志给原因，再删临时文件（删早了就永远只能落到兜底分支）。
	if grep -qi '401\|unauthorized\|token' "$probe_log" 2>/dev/null; then
		printf '  多半是令牌不对：节点可能被删了，或它属于另一个中枢/命名空间。\n' >&2
		printf '  去控制台「节点」页重新注册，把新的 --token 带上来。\n' >&2
	elif grep -q 'superseded' "$probe_log" 2>/dev/null; then
		printf '  这个令牌正被另一条连接用着（新连接会把旧连接顶掉，两边对跳）。\n' >&2
		printf '  停掉那一个再重跑：systemctl --user stop %s / sudo systemctl stop %s\n' "$UNIT_NAME" "$UNIT_NAME" >&2
		printf '  或者同一份 node.yaml 被两台机器同时用了。\n' >&2
	elif grep -qi 'unknown_capability' "$probe_log" 2>/dev/null; then
		printf '  本地缓存里有一个中枢已不认识的产物。删掉缓存再跑：\n' >&2
		printf '  rm -rf %s/index\n' "$DATA_DIR" >&2
	elif grep -qi 'no such host\|connection refused\|i/o timeout\|unreachable' "$probe_log" 2>/dev/null; then
		printf '  多半是网络：核对 --server（%s）能否从这台机器访问。\n' "$SERVER" >&2
	elif grep -qi 'x509\|certificate' "$probe_log" 2>/dev/null; then
		printf '  多半是 TLS：中枢证书不被信任（自签？公司代理？），先修证书链。\n' >&2
	elif grep -qi 'unknown field\|not found' "$probe_log" 2>/dev/null; then
		printf '  本机 node.yaml 是旧版格式（含 agent 已不认的字段，如旧的 workDirs）。\n' >&2
		printf '  删掉它重装（令牌要重新给）：rm -f %s\n' "$CFG" >&2
	else
		printf '  原因见上面的 agent 输出；修好后重跑同一条命令即可。\n' >&2
	fi
	printf '  想跳过预检硬装（不推荐）：加 --no-preflight\n' >&2
	rm -f "$probe_log" "$probe_cfg"
	restore_previous_service
	exit 1
}

# ---- 二进制 ----
install_bin_as_user() {
	dir="$1"
	if [ "$DRY_RUN" = "yes" ]; then
		printf '  [dry-run] 将安装 %s/anotify-agent\n' "$dir"
		return 0
	fi
	if mkdir -p "$dir" 2>/dev/null && install -m 0755 "$tmp" "$dir/anotify-agent" 2>/dev/null; then
		printf '已安装：%s\n' "$dir/anotify-agent"
		return 0
	fi
	return 1
}

# 上一版装法可能把二进制留在 ~/.local/bin：PATH 里它常排在 /usr/local/bin 前面，
# 留着会让人以为"升级没生效"（实际跑的是旧的那份）。
cleanup_stale_binary() {
	old="$HOME/.local/bin/anotify-agent"
	if [ -f "$old" ] && [ "$old" != "$BIN_DIR/anotify-agent" ]; then
		if [ "$DRY_RUN" = "yes" ]; then
			printf '  [dry-run] 将删除旧副本 %s\n' "$old"
		else
			rm_path "$old"
			printf '已删除旧副本：%s\n' "$old"
		fi
	fi
}

# 上一版装的是 systemd **用户**服务：同名的两份 agent 会抢同一个令牌，
# 必须停掉并删掉，否则"升级"后看到的是两台机器在互相踢线。
cleanup_user_unit() {
	[ -f "$USER_UNIT_PATH" ] || return 0
	if [ "$DRY_RUN" = "yes" ]; then
		printf '  [dry-run] 将停止并删除旧用户服务 %s\n' "$USER_UNIT_PATH"
		return 0
	fi
	as_user stop "$UNIT_NAME" 2>/dev/null || true
	as_user disable "$UNIT_NAME" 2>/dev/null || true
	rm_path "$USER_UNIT_PATH"
	as_user daemon-reload 2>/dev/null || true
	printf '已移除旧的 systemd 用户服务（改为系统服务，开机自启不再依赖登录）\n'
}

# ---- 服务单元 ----
#
# write_unit_file <dest> <run-as-user 空=不写 User=> <WantedBy>
write_unit_file() {
	dest="$1"
	run_as="$2"
	wanted_by="$3"
	{
		printf '[Unit]\n'
		printf 'Description=Anotify Agent（节点：执行控制台部署下来的能力产物）\n'
		printf 'After=network-online.target\n'
		printf 'Wants=network-online.target\n'
		printf '# 令牌/中枢写错时不要被 systemd 的重启限速永久放弃——\n'
		printf '# 安装时的预检已经拦过一次，这里宁可一直重试（agent 自己会打日志说明原因）。\n'
		printf 'StartLimitIntervalSec=0\n'
		printf '\n[Service]\n'
		printf 'Type=simple\n'
		if [ -n "$run_as" ]; then printf 'User=%s\n' "$run_as"; fi
		printf 'Environment=HOME=%s\n' "$HOME"
		printf 'WorkingDirectory=%s\n' "$HOME"
		printf 'ExecStart=%s -f %s -data-dir %s\n' "$BIN_DIR/anotify-agent" "$CFG" "$DATA_DIR"
		printf 'Restart=always\n'
		printf 'RestartSec=3\n'
		printf '# systemd 254+ 的重启收敛：连续失败时把间隔从 RestartSec 按 RestartSteps 拉到 RestartMaxDelaySec。\n'
		printf '# 旧版本 systemd 只把它当未知键（warn），不影响启动；进程内退避重连才是主路径。\n'
		printf 'RestartSteps=5\n'
		printf 'RestartMaxDelaySec=30\n'
		printf '# 关机/登出时给在飞任务留出回报时间（agent 自己的 -grace 默认 20s，比这里小）\n'
		printf 'KillSignal=SIGTERM\n'
		printf 'TimeoutStopSec=30\n'
		printf '\n[Install]\n'
		printf 'WantedBy=%s\n' "$wanted_by"
	} | as_root_tee "$dest"
}

install_service_system() {
	run_as="$(id -un)"
	if [ "$(id -u)" = "0" ]; then run_as=""; fi

	printf '\n安装系统级服务（以 %s 身份运行）…\n' "${run_as:-root}"
	cleanup_user_unit
	write_unit_file "$UNIT_PATH" "$run_as" "multi-user.target"
	as_root "$SYSTEMCTL" daemon-reload
	# enable 负责开机自启，restart 负责"现在就是最新的那版"——
	# 升级时单元文件变了而进程还在跑旧二进制，只有 restart 会换掉它。
	as_root "$SYSTEMCTL" enable "$UNIT_NAME"
	as_root "$SYSTEMCTL" restart "$UNIT_NAME"
	SERVICE_DESC="systemd 系统服务（开机自启；没登录也活着；重启带退避：3s 起，最多 30s）"
	LOG_HINT="journalctl -u ${UNIT_NAME} -f"
}

install_service_user() {
	printf '\n没有可用的 sudo —— 退化成 systemd 用户服务（只在你登录时活着）。\n'
	unit_dir=$(dirname "$USER_UNIT_PATH")
	if [ "$DRY_RUN" = "yes" ]; then
		printf '  [dry-run] 将创建目录 %s\n' "$unit_dir"
	else
		mkdir -p "$unit_dir"
	fi
	write_unit_file "$USER_UNIT_PATH" "" "default.target"

	# 非交互 shell（ssh 命令、`curl|sh`）里常常没有 XDG_RUNTIME_DIR，
	# systemctl --user 会报 "Failed to connect to bus"。补上标准路径再试。
	if [ -z "${XDG_RUNTIME_DIR:-}" ] && [ -d "/run/user/$(id -u)" ]; then
		XDG_RUNTIME_DIR="/run/user/$(id -u)"
		export XDG_RUNTIME_DIR
	fi

	if ! as_user daemon-reload 2>/dev/null; then
		warn "连不上 systemd 用户实例（没有登录会话？）。单元文件已写好：
  $USER_UNIT_PATH
  · 登录后执行：systemctl --user enable --now ${UNIT_NAME}
  · 或在有桌面的会话里重跑本脚本"
	fi
	as_user enable "$UNIT_NAME" 2>/dev/null || true
	as_user restart "$UNIT_NAME" 2>/dev/null || true

	# 想在没登录时也活着，只有 linger 一条路（这就是用户服务不如系统服务的地方）。
	if [ "$DRY_RUN" = "no" ] && command -v loginctl >/dev/null 2>&1; then
		if loginctl enable-linger "$(id -un)" 2>/dev/null; then
			printf '已开启 linger：没登录时服务也在跑\n'
		elif sudo_usable && $SUDO loginctl enable-linger "$(id -un)" 2>/dev/null; then
			printf '已开启 linger（经 sudo）：没登录时服务也在跑\n'
		else
			warn "没开启 linger，登录态结束后服务会停。手动执行：
  sudo loginctl enable-linger $(id -un)"
		fi
	fi

	SERVICE_DESC="systemd 用户服务（仅登录时活着；sudo loginctl enable-linger 可常驻）"
	LOG_HINT="journalctl --user -u ${UNIT_NAME} -f"
}

install_service_none() {
	SERVICE_DESC="未安装（仅二进制 + 配置）"
	LOG_HINT="（前台跑：${BIN_DIR}/anotify-agent -f ${CFG}）"
}

# 服务是否真的起来了：系统服务与用户服务的 systemctl 作用域不同，必须分开查。
svc_active() {
	case "$SERVICE" in
	system) $SYSTEMCTL is-active --quiet "$UNIT_NAME" 2>/dev/null ;;
	user) as_user is-active --quiet "$UNIT_NAME" 2>/dev/null ;;
	*) return 1 ;;
esac
}

# 预检前必须先让开令牌。
#
# 中枢对同一个节点的策略是「新连接顶掉旧连接」（internal/ws/node_hub.go 里的
# superseded by a newer connection）。所以如果这台机器上已经有一个 agent 在跑，
# 预检会把它踢下线，它的 Restart=always 三秒后又蹬回来把预检踢掉 —— 两边对跳，
# 最后表现成「预检超时」，而真实原因只是「同一个令牌被用了两次」。
# 顺序改成：先停旧服务 → 再预检 → 预检失败就把旧服务拉起来（不让你变更差）。
PREV_SERVICE=""
stop_running_service() {
	if [ -f "$UNIT_PATH" ]; then
		if as_root "$SYSTEMCTL" stop "$UNIT_NAME" 2>/dev/null; then
			PREV_SERVICE="system"
			printf '已停下旧的服务实例（预检要用同一个令牌，不能两条连接抢）\n'
		fi
	fi
	if [ -f "$USER_UNIT_PATH" ]; then
		if as_user stop "$UNIT_NAME" 2>/dev/null; then
			if [ -z "$PREV_SERVICE" ]; then PREV_SERVICE="user"; fi
			printf '已停下旧的用户服务实例\n'
		fi
	fi
}

restore_previous_service() {
	case "$PREV_SERVICE" in
	system)
		as_root "$SYSTEMCTL" start "$UNIT_NAME" 2>/dev/null || true
		warn "已把原来的服务重新拉起来（这次安装没改动任何东西）"
		;;
	user)
		as_user start "$UNIT_NAME" 2>/dev/null || true
		warn "已把原来的用户服务重新拉起来（这次安装没改动任何东西）"
		;;
	esac
}

# ---- 状态回报 ----
report() {
	node_label="${NAME:-$(hostname 2>/dev/null || echo '?')}"
	printf '\n'
	if [ "$DRY_RUN" = "yes" ]; then
		printf '安装预演（dry-run，系统没动）——真跑起来会是这个样子：\n'
	fi
	case "$SERVICE" in
	system | user)
		state="未检查（dry-run）"
		if [ "$DRY_RUN" = "no" ]; then
			if svc_active; then
				state="active (running)"
			else
				state="未运行 —— 查：$LOG_HINT"
			fi
		fi
		printf 'anotify-agent 已装好%s\n' "$([ "$UPGRADE" = "yes" ] && echo '（升级完成）' || echo '并已启动')"
		printf '  节点      %s\n' "$node_label"
		printf '  中枢      %s\n' "$SERVER"
		printf '  运行身份  %s（agent 以这个用户身份执行能力）\n' "$(id -un)"
		printf '  二进制    %s\n' "$BIN_DIR/anotify-agent"
		printf '  配置      %s\n' "$CFG"
		printf '  服务      %s：%s\n' "$UNIT_NAME" "$SERVICE_DESC"
		printf '  状态      %s\n' "$state"
		printf '  日志      %s\n' "$LOG_HINT"
		printf '  升级      重跑同一条安装命令（沿用现有 node.yaml）\n'
		printf '  卸载      sh install-agent.sh --uninstall\n'
		;;
	*)
		printf 'anotify-agent 已装好（未安装服务）\n'
		printf '  节点      %s\n' "$node_label"
		printf '  中枢      %s\n' "$SERVER"
		printf '  二进制    %s\n' "$BIN_DIR/anotify-agent"
		printf '  配置      %s\n' "$CFG"
		printf '  跑起来    %s -f %s\n' "$BIN_DIR/anotify-agent" "$CFG"
		;;
	esac
	printf '\n控制台「节点」页应该看到 %s 在线；再到「能力」页点部署，它就有活了。\n' "$node_label"
	printf '没上线？先看日志： %s\n' "$LOG_HINT"
}

# ---- 卸载 ----
do_uninstall() {
	printf '卸载 anotify-agent …\n'
	removed=""

	if [ -f "$UNIT_PATH" ]; then
		as_root "$SYSTEMCTL" disable --now "$UNIT_NAME" 2>/dev/null || true
		as_root rm -f "$UNIT_PATH" || warn "删不掉 $UNIT_PATH（权限？加 sudo 重试）"
		as_root "$SYSTEMCTL" daemon-reload 2>/dev/null || true
		removed="${removed}  服务单元 $UNIT_PATH\n"
	fi

	if [ -f "$USER_UNIT_PATH" ]; then
		as_user disable --now "$UNIT_NAME" 2>/dev/null || true
		rm_path "$USER_UNIT_PATH"
		as_user daemon-reload 2>/dev/null || true
		removed="${removed}  用户服务单元 $USER_UNIT_PATH\n"
	fi

	for bin in "${PREFIX:-$HOME/.anotify}/bin/anotify-agent" "/usr/local/bin/anotify-agent" "$HOME/.local/bin/anotify-agent"; do
		if [ -f "$bin" ]; then
			case "$bin" in
			"$HOME"/*) rm_path "$bin" ;;
			*) as_root rm -f "$bin" || warn "删不掉 $bin（权限？加 sudo 重试）" ;;
			esac
			removed="${removed}  二进制 $bin\n"
		fi
	done

	if [ -f "$CFG" ]; then
		if [ "$KEEP_CONFIG" = "yes" ]; then
			printf '保留配置：%s（令牌还在里面）\n' "$CFG"
		else
			rm_path "$CFG"
			removed="${removed}  配置 $CFG\n"
		fi
	fi

	if [ "$PURGE" = "yes" ] && [ -d "$DATA_DIR" ]; then
		rm_path "$DATA_DIR"
		removed="${removed}  数据目录 $DATA_DIR（产物缓存/工作目录）\n"
	fi

	if [ "$DRY_RUN" = "yes" ]; then
		printf '  [dry-run] 以上只是预演，系统没动\n'
		return 0
	fi

	if [ -z "$removed" ]; then
		printf '没找到装过的东西（服务单元/二进制/配置都不在）。\n'
	else
		printf '已移除：\n%b' "$removed"
	fi
	printf '\n节点在控制台里会显示离线；不需要它了就在「节点」页删掉。\n'
}

# ---- 主流程 ----
parse_args "$@"

# agent 的执行权限就是它所在进程的权限：以 root 跑等于把整台机器交出去。
if [ "$(id -u)" = "0" ] && [ "$ALLOW_ROOT" != "yes" ]; then
	die "不要用 root 安装/运行 agent（它的权限 = 你的权限）。请用普通用户，或显式加 --allow-root 承担后果"
fi

detect_platform
load_existing_config

if [ "$UNINSTALL" = "yes" ]; then
	do_uninstall
	exit 0
fi

validate_config

# 安装前缀：--prefix 显式给了就听它的。
#
# 缺省是 **$HOME/.anotify**（不是 /usr/local）：服务单元以安装者身份跑，二进制放在
# 自己家目录里才**可写** —— 这是「`anotify-agent upgrade` 与控制台下发升级」能真正
# 工作的前提（CONTRACT-agent-upgrade §4：写不进去就只能报 need_root 让人去 sudo）。
# 单用户机器上 /usr/local 的 root 所有权换不来任何隔离，只换来每次升级都要 sudo。
if [ -z "$PREFIX" ]; then
	PREFIX="$HOME/.anotify"
fi
BIN_DIR="${PREFIX}/bin"

# 服务模式：auto = 能 sudo 就用系统服务（tailscaled 那种），否则用户服务。
if [ "$SERVICE" = "auto" ]; then
	case "$os" in
	linux)
		if ! command -v "$SYSTEMCTL" >/dev/null 2>&1; then
			SERVICE="none"
			warn "没有 systemctl（容器里常见）——退回只装二进制+配置"
		elif [ "$DRY_RUN" = "yes" ] || sudo_usable; then
			SERVICE="system"
		else
			SERVICE="user"
		fi
		;;
	*)
		SERVICE="none"
		warn "只有 Linux 有 systemd；$os 上只装二进制+配置（用 launchd 请自行接）"
		;;
	esac
fi

download_binary

# 互相矛盾的组合早点拦住，别到最后一步才报 sudo 的错。
if [ "$SERVICE" = "system" ] && [ "$NO_SUDO" = "yes" ] && [ "$(id -u)" != "0" ]; then
	die "--no-sudo 与 --system 冲突：系统服务要靠 root 注册。去掉其中一个，或用 --user"
fi

# 顺序很要紧：停旧服务（让出令牌）→ 预检（用临时配置，正式配置还没动）→ 落配置 → 装服务。
if [ "$SERVICE" != "none" ]; then
	stop_running_service
	if [ "$NO_PREFLIGHT" = "no" ]; then
		preflight
	fi
fi

write_config

case "$SERVICE" in
system)
	# 单元文件由 root 装（要写 /etc/systemd/system），但**二进制先试着以当前用户装**：
	# 缺省前缀现在是 ~/.anotify（可写），装那里以后 `anotify-agent upgrade` 与控制台
	# 下发升级都不需要 sudo。装不进去（/usr/local 等）才退回 root。
	if ! install_bin_as_user "$BIN_DIR"; then
		as_root install -d -m 0755 "$BIN_DIR"
		as_root install -m 0755 "$tmp" "$BIN_DIR/anotify-agent"
		printf '已安装：%s（root 所有：升级时需要 sudo 或控制台会报 need_root）\n' "$BIN_DIR/anotify-agent"
	fi
	install_service_system
	;;
user)
	if [ "$PREFIX_EXPLICIT" = "yes" ]; then
		install_bin_as_user "$BIN_DIR" || die "安装失败：$BIN_DIR 不可写"
	else
		if ! install_bin_as_user "$BIN_DIR"; then
			BIN_DIR="$HOME/.local/bin"
			printf '%s 不可写 —— 改装到 %s\n' "${PREFIX:-$HOME/.anotify}/bin" "$BIN_DIR"
			install_bin_as_user "$BIN_DIR" || die "安装失败：$BIN_DIR 也不可写"
		fi
		case ":${PATH}:" in
		*":${BIN_DIR}:"*) ;;
		*) warn "$BIN_DIR 不在 PATH 里，把它加进 shell 的启动文件" ;;
		esac
	fi
	install_service_user
	;;
none)
	if [ "$PREFIX_EXPLICIT" = "yes" ]; then
		install_bin_as_user "$BIN_DIR" || die "安装失败：$BIN_DIR 不可写"
	else
		if ! install_bin_as_user "$BIN_DIR"; then
			BIN_DIR="$HOME/.local/bin"
			printf '%s 不可写 —— 改装到 %s\n' "${PREFIX:-$HOME/.anotify}/bin" "$BIN_DIR"
			install_bin_as_user "$BIN_DIR" || die "安装失败：$BIN_DIR 也不可写"
		fi
	fi
	install_service_none
	;;
esac

cleanup_stale_binary
report
