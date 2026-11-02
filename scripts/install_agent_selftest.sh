#!/bin/sh
# install-agent.sh 的自测（`make selftest-install`，也在 `make ci` 里）。
#
# 为什么需要它：安装脚本是**唯一**一条把二进制放进用户机器的路径，而它的失败
# 模式全是"静默"的——装错目录、配置没写、校验和没验、JSON/YAML 少一行。
# 这些在浏览器里完全看不出来，只能靠一个真的跑一遍的测试钉住。
#
# 做法：本地起一个假 release（http.server + 真编译出来的二进制），把整条链路
# （下载 → 校验和 → 落盘 → 写配置 → 权限 → 服务单元 → 卸载）跑一遍，再故意弄坏
# 校验和 / 指向一个不通的中枢，验证它会中止。
#
# 服务那一段用**假的 sudo / systemctl**（测试钩子 ANOTIFY_SUDO / ANOTIFY_SYSTEMCTL /
# ANOTIFY_UNIT_PATH）真跑一遍：断言的是「装了什么、按什么顺序、单元内容对不对」，
# 而不是「真的动了我这台机器」——所以这个脚本可以在 CI 里裸跑。
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
	if [ -n "${broker_pid:-}" ]; then
		kill "$broker_pid" 2>/dev/null
		wait "$broker_pid" 2>/dev/null
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

# 恢复正确的校验和：上一段把 .sha256 改成了故意错的，后面还要从这台假 release 装机。
(
	cd "$work/release"
	sha256sum anotify-agent-linux-amd64 >anotify-agent-linux-amd64.sha256
)

# ---- 5. 系统服务安装（真跑一遍 sudo/systemctl 的调用顺序，用假的替身） ----
#
# 这一段锁的是「一条命令装完即上线」的全部承诺：单元是系统级（多用户目标、开机自启）、
# 以安装者身份运行（不是 root）、enable 之后真的要 restart 一次（升级时单元变了而进程
# 还跑着旧二进制，只有 restart 会换掉它），以及上一版装法的残留被清干净。

fakebin="$work/fakebin"
mkdir -p "$fakebin" "$work/systemd"
cat >"$fakebin/sudo" <<'SH'
#!/bin/sh
# 测试替身：-n / -v 这类探测直接成功，其余命令原样执行（测试里没有真 root，也不需要）。
case "$1" in
-n | -v) exit 0 ;;
esac
exec "$@"
SH
cat >"$fakebin/systemctl" <<SH
#!/bin/sh
# 测试替身：只记账，不碰真的 init 系统。
printf '%s\n' "\$*" >>"$work/systemctl.log"
exit 0
SH
chmod +x "$fakebin/sudo" "$fakebin/systemctl"
: >"$work/systemctl.log"

export ANOTIFY_SUDO="$fakebin/sudo"
export ANOTIFY_SYSTEMCTL="$fakebin/systemctl"
export ANOTIFY_UNIT_PATH="$work/systemd/anotify-agent.service"
PATH="$fakebin:$PATH"
export PATH

run_install() {
	HOME="$home" ANOTIFY_RELEASE_BASE="$ANOTIFY_RELEASE_BASE" \
		sh "$root/scripts/install-agent.sh" "$@"
}

# 先造出上一版装法的残留：用户服务单元 + ~/.local/bin 里的旧副本。
# 它们必须被清掉——同名的两份 agent 会抢同一个令牌，表现是两台机器互相踢线。
mkdir -p "$home/.config/systemd/user" "$home/.local/bin"
printf '# stale user unit\n' >"$home/.config/systemd/user/anotify-agent.service"
printf '#!/bin/sh\nexit 0\n' >"$home/.local/bin/anotify-agent"
chmod +x "$home/.local/bin/anotify-agent"

if run_install --token=nt_selftest --server=http://127.0.0.1:8080 --name=selftest \
	--prefix="$prefix" --no-preflight >"$work/install-system.log" 2>&1; then
	ok "系统服务安装退出码 0"
else
	no "系统服务安装退出码 0" "$(tail -5 "$work/install-system.log")"
fi

unit="$ANOTIFY_UNIT_PATH"
if [ -f "$unit" ]; then
	ok "写了系统服务单元"
	grep -qx "User=$(id -un)" "$unit" && ok "单元以安装者身份运行（不是 root）" ||
		no "单元以安装者身份运行（不是 root）" "$(grep -n '^User=' "$unit" || echo '没有 User= 行')"
	grep -qx 'WantedBy=multi-user.target' "$unit" && ok "单元是系统级（开机自启，不依赖登录）" ||
		no "单元是系统级（开机自启，不依赖登录）" "$(grep -n '^WantedBy=' "$unit" || echo 缺失)"
	grep -q "^ExecStart=$prefix/bin/anotify-agent -f $home/.anotify/node.yaml -data-dir $home/.anotify$" "$unit" &&
		ok "ExecStart 钉死二进制/配置/数据目录的绝对路径" ||
		no "ExecStart 钉死二进制/配置/数据目录的绝对路径" "$(grep -n '^ExecStart=' "$unit")"
	grep -qx 'Restart=always' "$unit" && ok "单元会自愈（Restart=always）" ||
		no "单元会自愈（Restart=always）" "$(grep -n '^Restart=' "$unit" || echo 缺失)"
else
	no "写了系统服务单元" "缺失 $unit（--system 没生效？）"
fi

for call in 'daemon-reload' 'enable anotify-agent' 'restart anotify-agent'; do
	grep -qx "$call" "$work/systemctl.log" 2>/dev/null && ok "systemctl $call" ||
		no "systemctl $call" "实际调用：$(tr '\n' '|' <"$work/systemctl.log" 2>/dev/null)"

done

if [ -f "$home/.config/systemd/user/anotify-agent.service" ]; then
	no "旧的 systemd 用户服务被清理" "还在（会和系统服务抢令牌）"
else
	ok "旧的 systemd 用户服务被清理"
fi
if [ -e "$home/.local/bin/anotify-agent" ]; then
	no "旧的 ~/.local/bin 副本被清理" "还在（PATH 里可能排在前面）"
else
	ok "旧的 ~/.local/bin 副本被清理"
fi
grep -q 'active (running)' "$work/install-system.log" && ok "结尾回报服务已 active" ||
	no "结尾回报服务已 active" "$(tail -6 "$work/install-system.log")"
grep -q 'WantedBy=multi-user.target' "$work/install-system.log" && no "非 dry-run 不该把单元内容打到日志里" "居然打出来了" ||
	ok "非 dry-run 不刷屏（单元内容只在 --dry-run 里回显）"

# ---- 5b. 预检成功必须放行（真握手，不是文本断言） ----
#
# 这一段把「装完即上线」的核心逻辑跑真：假中枢握手 → agent 打出 agent.handshake.ok
# → 安装脚本放行并继续装服务。为什么不做文本断言：万一日志事件名/级别变了，
# 文本断言仍会"通过"，而每个用户的安装都会卡在最后一步。
broker_bin="$work/fakebroker"
if (cd "$root" && go build -o "$broker_bin" ./scripts/testdata/fakebroker) >"$work/broker-build.log" 2>&1; then
	ok "编译假中枢"
else
	no "编译假中枢" "$(tail -3 "$work/broker-build.log")"
fi

# broker_start [extra args]：起一个假中枢，成功后设 $broker_host。
broker_start() {
	"$broker_bin" -addr 127.0.0.1:0 "$@" >"$work/broker.log" 2>&1 &
	broker_pid=$!
	broker_host=""
	i=0
	while [ "$i" -lt 100 ]; do
		broker_host=$(sed -n 's/^listening on //p' "$work/broker.log" | head -n1)
		[ -n "$broker_host" ] && break
		i=$((i + 1))
		sleep 0.1
	done
	[ -n "$broker_host" ]
}

broker_stop() {
	kill "$broker_pid" 2>/dev/null || true
	wait "$broker_pid" 2>/dev/null || true
}

if [ -x "$broker_bin" ]; then
	if broker_start; then
		ok "假中枢就绪（$broker_host）"
		ok_prefix="$work/prefix-preflight-ok"
		mkdir -p "$work/home-preflight-ok"
		if HOME="$work/home-preflight-ok" ANOTIFY_RELEASE_BASE="$ANOTIFY_RELEASE_BASE" \
			ANOTIFY_UNIT_PATH="$work/systemd/preflight-ok.service" \
			sh "$root/scripts/install-agent.sh" --token=nt_selftest \
			--server="http://$broker_host" --name=preflight-ok \
			--prefix="$ok_prefix" --preflight-timeout=10 >"$work/preflight-ok.log" 2>&1; then
			ok "预检通过后安装继续（退出码 0）"
		else
			no "预检通过后安装继续（退出码 0）" "$(tail -8 "$work/preflight-ok.log")"
		fi
		grep -q '预检通过' "$work/preflight-ok.log" && ok "预检通过有明确措辞" ||
			no "预检通过有明确措辞" "$(tail -6 "$work/preflight-ok.log")"
		grep -q 'node_selftest' "$work/preflight-ok.log" && ok "预检从 welcome 里拿到了 node_id" ||
			no "预检从 welcome 里拿到了 node_id" "$(tail -6 "$work/preflight-ok.log")"
		[ -x "$ok_prefix/bin/anotify-agent" ] && ok "预检通过后二进制落了盘" ||
			no "预检通过后二进制落了盘" "缺失 $ok_prefix/bin/anotify-agent"
	else
		no "假中枢就绪" "$(cat "$work/broker.log")"
	fi
	broker_stop
fi

# ---- 5c. 过期缓存自愈：中枢拒绝 unknown_capability → 挪走 index → 重试成功 ----
#
# 真实场景：中枢上删掉/改名了一个能力，而节点的 index 里还留着它。hello 会申报它，
# 中枢**整条连接**拒绝，用户看到的是“装不上”。产物只是缓存（中枢会重新部署），
# 所以安装脚本应当自己挪开过期索引再试一次——这一段钉的就是这条自愈路径。
if [ -x "$broker_bin" ]; then
	if broker_start -fail-first 1; then
		ok "假中枢就绪（拒绝首连：$broker_host）"
		stale_home="$work/home-stale"
		mkdir -p "$stale_home/.anotify/index"
		printf 'stale\n' >"$stale_home/.anotify/index/stale.cap@1"
		if HOME="$stale_home" ANOTIFY_RELEASE_BASE="$ANOTIFY_RELEASE_BASE" \
			ANOTIFY_UNIT_PATH="$work/systemd/stale.service" \
			sh "$root/scripts/install-agent.sh" --token=nt_selftest \
			--server="http://$broker_host" --name=stale \
			--prefix="$work/prefix-stale" --preflight-timeout=10 >"$work/preflight-stale.log" 2>&1; then
			ok "预检被拒后自愈并继续安装（退出码 0）"
		else
			no "预检被拒后自愈并继续安装（退出码 0）" "$(tail -10 "$work/preflight-stale.log")"
		fi
		grep -q '已挪开' "$work/preflight-stale.log" && ok "自愈时明确告知挪走了过期索引" ||
			no "自愈时明确告知挪走了过期索引" "$(tail -8 "$work/preflight-stale.log")"
		grep -q '预检通过' "$work/preflight-stale.log" && ok "重试后预检通过" ||
			no "重试后预检通过" "$(tail -8 "$work/preflight-stale.log")"
		[ -e "$stale_home/.anotify/index" ] && no "过期索引被挪走" "index 还在" || ok "过期索引被挪走"
		ls "$stale_home/.anotify" | grep -q '^index\.stale-' && ok "过期索引留了备份（不是静默删除）" ||
			no "过期索引留了备份（不是静默删除）" "$(ls "$stale_home/.anotify" 2>/dev/null)"
	else
		no "假中枢就绪（拒绝首连）" "$(cat "$work/broker.log")"
	fi
	broker_stop
fi

# ---- 6. 重跑同一条命令 = 升级（令牌/中枢从 node.yaml 读，不用再给） ----
: >"$work/systemctl.log"
if run_install --prefix="$prefix" --no-preflight >"$work/upgrade.log" 2>&1; then
	ok "重跑（不给 --token/--server）退出码 0 —— 升级可用"
else
	no "重跑（不给 --token/--server）退出码 0" "$(tail -5 "$work/upgrade.log")"
fi
grep -qx 'token: nt_selftest' "$cfg" 2>/dev/null && ok "升级后令牌还在 node.yaml 里" ||
	no "升级后令牌还在 node.yaml 里" "$(cat "$cfg" 2>/dev/null)"
grep -qx 'restart anotify-agent' "$work/systemctl.log" 2>/dev/null && ok "升级会 restart（换掉在跑旧二进制的进程）" ||
	no "升级会 restart" "实际调用：$(tr '\n' '|' <"$work/systemctl.log" 2>/dev/null)"
grep -q '升级完成' "$work/upgrade.log" && ok "升级路径有明确措辞" ||
	no "升级路径有明确措辞" "$(tail -4 "$work/upgrade.log")"

# ---- 7. 预检失败必须中止（令牌错/中枢不通时不留崩溃重启的服务） ----
#
# 指向一个必然拒连的端口：agent 立刻报 connection refused 退出，用不着等满超时。
preflight_prefix="$work/prefix-preflight"
if HOME="$work/home-preflight" ANOTIFY_RELEASE_BASE="$ANOTIFY_RELEASE_BASE" \
	sh "$root/scripts/install-agent.sh" --token=nt_selftest --server=http://127.0.0.1:1 \
	--prefix="$preflight_prefix" --preflight-timeout=5 >"$work/preflight.log" 2>&1; then
	no "预检失败时中止安装" "居然成功了（退出码 0）"
else
	ok "预检失败时中止安装"
fi
grep -q '预检失败' "$work/preflight.log" && ok "预检失败有明确说明" ||
	no "预检失败有明确说明" "$(tail -6 "$work/preflight.log")"
# 失败原因要按日志重新给一次，而不是干巴巴一句“失败了”：拒连应被认成网络问题。
grep -q '多半是网络' "$work/preflight.log" && ok "预检失败会指出原因（网络）" ||
	no "预检失败会指出原因（网络）" "$(tail -8 "$work/preflight.log")"
if [ -e "$preflight_prefix/bin/anotify-agent" ]; then
	no "预检失败时什么都没装" "却装上了 $preflight_prefix/bin/anotify-agent"
else
	ok "预检失败时什么都没装"
fi

# ---- 8. 卸载 ----
if run_install --uninstall --keep-config --prefix="$prefix" >"$work/uninstall.log" 2>&1; then
	ok "卸载退出码 0"
else
	no "卸载退出码 0" "$(tail -5 "$work/uninstall.log")"
fi
[ -e "$unit" ] && no "卸载删了服务单元" "还在 $unit" || ok "卸载删了服务单元"
[ -e "$prefix/bin/anotify-agent" ] && no "卸载删了二进制" "还在" || ok "卸载删了二进制"
[ -f "$cfg" ] && ok "--keep-config 保留 node.yaml" || no "--keep-config 保留 node.yaml" "被删了"

if run_install --uninstall --purge --prefix="$prefix" >"$work/uninstall2.log" 2>&1; then
	ok "卸载 --purge 退出码 0"
else
	no "卸载 --purge 退出码 0" "$(tail -5 "$work/uninstall2.log")"
fi
[ -e "$cfg" ] && no "默认卸载删了 node.yaml" "还在（含令牌）" || ok "默认卸载删了 node.yaml"
[ -d "$home/.anotify" ] && no "--purge 删了数据目录" "还在" || ok "--purge 删了数据目录"

# ---- 9. --dry-run 必须真的是预演（什么都不删、什么都不启） ----
#
# 这一条是防回归的：卸载路径里最容易写错的就是“把 rm 忘了包进 dry-run 分支”，
# 那样 `--dry-run` 会直接把配置/数据目录删掉——用户以为自己在看预演。
dry_home="$work/home-dry"
mkdir -p "$dry_home/.anotify" "$dry_home/.config/systemd/user"
printf 'server: http://127.0.0.1:8080\ntoken: nt_dry\n' >"$dry_home/.anotify/node.yaml"
printf '# dry user unit\n' >"$dry_home/.config/systemd/user/anotify-agent.service"

if HOME="$dry_home" ANOTIFY_UNIT_PATH="$work/systemd/dry.service" \
	sh "$root/scripts/install-agent.sh" --uninstall --purge --dry-run >"$work/uninstall-dry.log" 2>&1; then
	ok "--uninstall --dry-run 退出码 0"
else
	no "--uninstall --dry-run 退出码 0" "$(tail -5 "$work/uninstall-dry.log")"
fi
[ -f "$dry_home/.anotify/node.yaml" ] && ok "--dry-run 没删 node.yaml" ||
	no "--dry-run 没删 node.yaml" "被删了"
[ -f "$dry_home/.config/systemd/user/anotify-agent.service" ] && ok "--dry-run 没删用户服务单元" ||
	no "--dry-run 没删用户服务单元" "被删了"

printf '\ninstall-agent 自测：%d 通过 / %d 失败\n' "$pass" "$fail"
[ "$fail" -eq 0 ] || exit 1
