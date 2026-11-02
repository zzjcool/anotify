#!/bin/sh
# Anotify Agent 原生安装脚本。
#
#   curl -fsSL https://raw.githubusercontent.com/zzjcool/anotify/main/scripts/install-agent.sh \
#     | sh -s -- --token=nt_xxx --server=https://anotify.example.com
#
# 做什么：
#   1. 从 GitHub Release 下对应平台的 anotify-agent 二进制
#   2. 装到 /usr/local/bin（可用 --prefix 改）
#   3. 写 ~/.anotify/node.yaml（四件 bootstrap + workDirs）
#   4. 生成 systemd 用户服务（Linux）——**可选**，--no-systemd 关掉
#
# 为什么给原生装：docker 那条路门槛最低，但容器里**看不到宿主机文件**——
# 想在
# 自己的项目目录里跑能力（make test、git status、就地改文件）就只能在宿主机上跑。
#
# 安全（必读）：agent 的执行权限 = 运行它的用户权限。
#   - 用普通用户跑，不要 root（脚本会拒绝 root，除非显式 --allow-root）
#   - workDirs 是「能力工作目录」的白名单：控制台只能从这里面选目录，
#     选到范围外任务会失败。不配置 = 一个目录都不许（能力只能跑在临时目录里）
#
# 退出码：0 成功；非 0 失败并给出可执行的下一步。
set -eu

OWNER="zzjcool"
REPO="anotify"
BIN_DIR="${PREFIX:-/usr/local}/bin"
DATA_DIR="${HOME}/.anotify"
SERVER=""
TOKEN=""
NAME=""
INSTANCES=""
WORK_DIRS=""
WITH_SYSTEMD="yes"
ALLOW_ROOT="no"

die() {
	printf 'install-agent: %s\n' "$1" >&2
	exit 1
}

need() {
	command -v "$1" >/dev/null 2>&1 || die "缺少 $1，请先安装它"
}

usage() {
	cat <<'EOF'
用法：install-agent.sh [选项]

  --token=nt_xxx          节点令牌（必填；在控制台「节点」页注册后显示一次）
  --server=https://...    中枢地址（必填；原生装猜不到，猜错会表现成"连不上"）
  --name=box-1            节点名（缺省用主机名）
  --instances=2           并发槽位数（缺省 1）
  --work-dirs=/a:/b       允许能力使用的工作目录白名单（缺省空 = 一个都不许）
  --prefix=/opt           安装前缀（二进制落到 <prefix>/bin）
  --no-systemd            不生成 systemd 用户服务，只装二进制与配置
  --allow-root            允许以 root 运行（不推荐：agent 的权限 = 它的用户权限）
  -h, --help              显示这段帮助
EOF
}

for arg in "$@"; do
	case "$arg" in
	--token=*) TOKEN="${arg#*=}" ;;
	--server=*) SERVER="${arg#*=}" ;;
	--name=*) NAME="${arg#*=}" ;;
	--instances=*) INSTANCES="${arg#*=}" ;;
	--work-dirs=*) WORK_DIRS="${arg#*=}" ;;
	--prefix=*)
		# ${arg#*=/opt} 是错的写法（模式里带 /opt 永远匹配不上，结果把整串当路径）；
		# 这里显式去掉前缀，并容忍结尾多余的斜杠。
		p="${arg#--prefix=}"
		BIN_DIR="${p%/}/bin"
		;;
	--no-systemd) WITH_SYSTEMD="no" ;;
	--allow-root) ALLOW_ROOT="yes" ;;
	-h | --help)
		usage
		exit 0
		;;
	*) die "未知参数：$arg（--help 看全部）" ;;
	esac
done

[ -n "$TOKEN" ] || die "缺少 --token（在控制台「节点」页注册节点后可得，只显示一次）"
[ -n "$SERVER" ] || die "缺少 --server（原生装无法猜测中枢地址）"

# agent 的执行权限就是它所在进程的权限：以 root 跑等于把整台机器交出去。
if [ "$(id -u)" = "0" ] && [ "$ALLOW_ROOT" != "yes" ]; then
	die "不要用 root 安装/运行 agent（它的权限 = 你的权限）。请用普通用户，或显式加 --allow-root 承担后果"
fi

need curl
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

printf '下载 %s …\n' "$url"
tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT INT TERM
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
verify_checksum() {
	cd_ok="$(dirname "$tmp")"
	sumfile="$tmp.sha256"
	if ! curl -fsSL "${url}.sha256" -o "$sumfile" 2>/dev/null; then
		printf '注意：release 里没有 %s.sha256，跳过完整性校验\n' "$asset" >&2
		return 0
	fi
	want=$(awk '{print $1}' "$sumfile" | head -n1)
	if [ -z "$want" ]; then
		printf '注意：校验和文件是空的，跳过完整性校验\n' >&2
		return 0
	fi
	if command -v sha256sum >/dev/null 2>&1; then
		got=$(sha256sum "$tmp" | awk '{print $1}')
	elif command -v shasum >/dev/null 2>&1; then
		got=$(shasum -a 256 "$tmp" | awk '{print $1}')
	elif command -v openssl >/dev/null 2>&1; then
		got=$(openssl dgst -sha256 "$tmp" | awk '{print $NF}')
	else
		printf '注意：本机没有 sha256sum/shasum/openssl，跳过完整性校验\n' >&2
		return 0
	fi
	if [ "$got" != "$want" ]; then
		die "校验和不匹配（下载可能被篡改或截断）
  want: $want
  got:  $got
  · 重试一次：网络中断也会导致不匹配
  · 仍不一致就别装了，去 github.com/${OWNER}/${REPO}/releases 手工核对"
	fi
	printf '校验和 OK（%s）\n' "$got"
}

verify_checksum

# 装二进制。普通用户写 /usr/local/bin 会失败 → 退化到 ~/.local/bin 并提示 PATH。
mkdir -p "$DATA_DIR" || die "无法创建 $DATA_DIR"
install_bin() {
	target="$1/anotify-agent"
	if mkdir -p "$1" 2>/dev/null && install -m 0755 "$tmp" "$target" 2>/dev/null; then
		printf '已安装：%s\n' "$target"
		return 0
	fi
	return 1
}

if ! install_bin "$BIN_DIR"; then
	fallback="${HOME}/.local/bin"
	printf '/usr/local/bin 不可写（需要 sudo）——改装到 %s\n' "$fallback"
	install_bin "$fallback" || die "安装失败：$fallback 也不可写"
	BIN_DIR="$fallback"
	case ":${PATH}:" in
	*":${fallback}:"*) ;;
	*) printf '注意：%s 不在 PATH 里，把它加进 shell 的启动文件\n' "$fallback" ;;
	esac
fi

# 配置。workDirs 只在给了的时候才写：不写 = 一个目录都不许（能力只能跑在临时目录）。
cfg="${DATA_DIR}/node.yaml"
{
	printf '# 由 install-agent.sh 生成（%s）\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
	printf 'server: %s\n' "$SERVER"
	printf 'token: %s\n' "$TOKEN"
	[ -n "$NAME" ] && printf 'name: %s\n' "$NAME"
	[ -n "$INSTANCES" ] && printf 'instances: %s\n' "$INSTANCES"
	if [ -n "$WORK_DIRS" ]; then
		printf 'workDirs:\n'
		# 必须给**结尾换行**：`while read` 在"没有换行的最后一行"上返回非 0，
		# 循环体不会执行 —— 只加 `tr ':' '\n'` 会静默丢掉最后一个目录，
		# 而这个白名单少一条的后果是「那个目录下的能力任务全部失败」。
		printf '%s\n' "$WORK_DIRS" | tr ':' '\n' | while IFS= read -r d; do
			[ -n "$d" ] && printf '  - %s\n' "$d"
		done
	fi
} >"$cfg"
chmod 600 "$cfg"
printf '已写配置：%s（权限 600：里面有令牌）\n' "$cfg"

if [ "$WITH_SYSTEMD" = "yes" ] && [ "$os" = "linux" ] && command -v systemctl >/dev/null 2>&1; then
	unit_dir="${HOME}/.config/systemd/user"
	mkdir -p "$unit_dir"
	cat >"${unit_dir}/anotify-agent.service" <<EOF
[Unit]
Description=Anotify Agent（节点：执行控制台部署下来的能力产物）
After=network-online.target

[Service]
Type=simple
ExecStart=${BIN_DIR}/anotify-agent -f ${cfg}
Restart=always
RestartSec=3
# 关机/登出时给在飞任务留出回报时间（agent 自己也有 grace，这里只是别太快 SIGKILL）
KillSignal=SIGTERM
TimeoutStopSec=30

[Install]
WantedBy=default.target
EOF
	systemctl --user daemon-reload || true
	printf '\n下一步（systemd 用户服务）：\n'
	printf '  systemctl --user enable --now anotify-agent\n'
	printf '  systemctl --user status anotify-agent\n'
	printf '  journalctl --user -u anotify-agent -f\n'
	printf '\n让服务在你没登录时也活着（可选）：sudo loginctl enable-linger %s\n' "$(id -un)"
else
	printf '\n下一步（前台跑起来看看）：\n'
	printf '  %s/anotify-agent -f %s\n' "$BIN_DIR" "$cfg"
fi

printf '\n跑起来之后：控制台「节点」页应该看到它上线；再到「能力」页点部署，它就有活了。\n'
