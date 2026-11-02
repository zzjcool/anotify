#!/bin/sh
# install-agent.sh 的自测（`make selftest-install`，也在 `make ci` 里）。
#
# 为什么需要它：安装脚本是**唯一**一条把二进制放进用户机器的路径，而它的失败
# 模式全是"静默"的——装错目录、配置没写、校验和没验、JSON/YAML 少一行。
# 这些在浏览器里完全看不出来，只能靠一个真的跑一遍的测试钉住。
#
# 做法：本地起一个假 release（http.server + 真编译出来的二进制），把整条链路
# （下载 → 校验和 → 落盘 → 写配置 → 权限）跑一遍，再故意弄坏校验和验证它会中止。
#
# 用法：sh scripts/install_agent_selftest.sh
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
pass=0
fail=0

ok() { printf '  ✅ %s\n' "$1"; pass=$((pass + 1)); }
no() {
	printf '  ❌ %s — %s\n' "$1" "$2"
	fail=$((fail + 1))
}

work=$(mktemp -d)
port="${ANOTIFY_SELFTEST_PORT:-18991}"
srv_pid=""
cleanup() {
	# 关掉 `set -e`：wait 一个被 kill 的子进程会返回 143，
	# 在 trap 里触发 set -e 会让整个脚本以 143 退出——测试全绿却报失败。
	set +e
	if [ -n "${srv_pid:-}" ]; then
		kill "$srv_pid" 2>/dev/null
		wait "$srv_pid" 2>/dev/null
	fi
	rm -rf "$work"
}
trap cleanup EXIT INT TERM

printf 'install-agent 自测\n'

# ---- 1. 造一个假 release：真二进制 + sha256 ----
mkdir -p "$work/release"
(
	cd "$root"
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" \
		-o "$work/release/anotify-agent-linux-amd64" ./cmd/agent
) || {
	no "编译假 release 二进制" "go build 失败"
	exit 1
}
(
	cd "$work/release"
	sha256sum anotify-agent-linux-amd64 >anotify-agent-linux-amd64.sha256
)

if ! command -v python3 >/dev/null 2>&1; then
	printf '  ⚠️  没有 python3，跳过（这是环境缺失，不是失败）\n'
	exit 0
fi

# 起假 release 服务器。两处细节是必需的：
#   · `exec` —— 否则 $! 是外层子 shell 的 pid，trap 里 kill 掉它并不会杀掉 python，
#     上一次的服务器会一直占着端口，下一次测试就悄悄用了旧目录（表现为莫名其妙的
#     校验和不匹配）。
#   · 端口占用就换一个 —— 本机可能已经有别的 http.server 在跑。
start_server() {
	p="$1"
	(cd "$work/release" && exec python3 -m http.server "$p" >"$work/http.log" 2>&1) &
	srv_pid=$!
	i=0
	while [ $i -lt 50 ]; do
		curl -fsS -o /dev/null "http://127.0.0.1:$p/anotify-agent-linux-amd64.sha256" 2>/dev/null && return 0
		i=$((i + 1))
		sleep 0.1
	done
	return 1
}
p="$port"
tries=0
while ! start_server "$p"; do
	kill "$srv_pid" 2>/dev/null
	wait "$srv_pid" 2>/dev/null
	p=$((p + 1))
	tries=$((tries + 1))
	[ "$tries" -lt 20 ] || {
		printf '  ❌ 起不来假 release 服务器（端口全占）\n'
		exit 1
	}
done
port="$p"

export ANOTIFY_RELEASE_BASE="http://127.0.0.1:$port"

# ---- 2. 正常安装 ----
home="$work/home"
mkdir -p "$home"
prefix="$work/prefix"
if HOME="$home" ANOTIFY_RELEASE_BASE="$ANOTIFY_RELEASE_BASE" \
	sh "$root/scripts/install-agent.sh" \
	--token=nt_selftest --server=http://127.0.0.1:8080 --name=selftest \
	--instances=3 \
	--prefix="$prefix" --no-systemd >"$work/install.log" 2>&1; then
	ok "安装退出码 0"
else
	no "安装退出码 0" "$(tail -3 "$work/install.log")"
fi

[ -x "$prefix/bin/anotify-agent" ] && ok "二进制落到 --prefix 指定的位置" ||
	no "二进制落到 --prefix 指定的位置" "缺失 $prefix/bin/anotify-agent（--prefix 被忽略了？）"

grep -q '校验和 OK' "$work/install.log" && ok "校验和已核对" ||
	no "校验和已核对" "日志里没有校验和结果"

cfg="$home/.anotify/node.yaml"
if [ -f "$cfg" ]; then
	ok "写了 node.yaml"
	perms=$(stat -c '%a' "$cfg" 2>/dev/null || stat -f '%Lp' "$cfg")
	[ "$perms" = "600" ] && ok "node.yaml 权限 600（里面有令牌）" ||
		no "node.yaml 权限 600（里面有令牌）" "实际 $perms"
	grep -qx 'server: http://127.0.0.1:8080' "$cfg" && ok "配置含 server" || no "配置含 server" "$(cat "$cfg")"
	grep -qx 'token: nt_selftest' "$cfg" && ok "配置含 token" || no "配置含 token" "$(cat "$cfg")"
	grep -qx 'instances: 3' "$cfg" && ok "配置含 instances" || no "配置含 instances" "$(cat "$cfg")"
	# 节点侧**不该**再有任何目录配置（#289：目录归能力定义）
	grep -q 'workDirs' "$cfg" && no "node.yaml 里没有 workDirs（目录归能力定义）" "$(cat "$cfg")" ||
		ok "node.yaml 里没有 workDirs（目录归能力定义）"
else
	no "写了 node.yaml" "缺失 $cfg"
fi

# ---- 3. 校验和不匹配必须中止（且什么都不装） ----
(
	cd "$work/release"
	printf '%s  %s\n' \
		"$(printf 'tampered' | sha256sum | awk '{print $1}')" anotify-agent-linux-amd64 \
		>anotify-agent-linux-amd64.sha256
)
home2="$work/home2"
prefix2="$work/prefix2"
mkdir -p "$home2"
if HOME="$home2" ANOTIFY_RELEASE_BASE="$ANOTIFY_RELEASE_BASE" \
	sh "$root/scripts/install-agent.sh" \
	--token=nt_selftest --server=http://127.0.0.1:8080 \
	--prefix="$prefix2" --no-systemd >"$work/tamper.log" 2>&1; then
	no "校验和不匹配时中止" "居然成功了（退出码 0）"
else
	ok "校验和不匹配时中止"
fi
[ -e "$prefix2/bin/anotify-agent" ] && no "篡改包没有落盘" "装上了 $prefix2/bin/anotify-agent" ||
	ok "篡改包没有落盘"
[ -e "$home2/.anotify/node.yaml" ] && no "篡改包没写配置" "写了 node.yaml" || ok "篡改包没写配置"

# ---- 4. 缺参数必须报错（不是静默用默认值） ----
if HOME="$work/home3" sh "$root/scripts/install-agent.sh" --server=http://x >/dev/null 2>&1; then
	no "缺 --token 时报错" "退出码 0"
else
	ok "缺 --token 时报错"
fi
if HOME="$work/home4" sh "$root/scripts/install-agent.sh" --token=x >/dev/null 2>&1; then
	no "缺 --server 时报错" "退出码 0"
else
	ok "缺 --server 时报错"
fi

printf '\ninstall-agent 自测：%d 通过 / %d 失败\n' "$pass" "$fail"
[ "$fail" -eq 0 ] || exit 1
