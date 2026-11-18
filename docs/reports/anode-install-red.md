# `cmd/anode` 安装脚本 4 条用例：在干净 `origin/main` 上就红

snapshot-doc: 2026-11-19 体检快照（下文每个数字都是这天本机实测的定格；事实源是各处给出的命令，复跑以新输出为准）

- 基线：`origin/main` = `2c115020`（Merge PR #433），分支 `fix/anode-install-red`，独立 worktree
- 结论：**类别 (a) 真 bug**（安装脚本越界读宿主安装），不是测试过度假设，也不是需要特权的用例
- 断言：**一条都没改、一条都没放宽**，没加 `|| true`，没加 `t.Skip`

## 1. 复现

```sh
go test ./cmd/anode/... -run 'Install' -v
```

红的是这 4 条（其余 Install 用例全绿，`cmd/anode/internal/install` 包也全绿）：

| 用例 | 断言报的错 |
| --- | --- |
| `TestInstallScriptRequiresToken` | `报错没点名 --token/--token-file：` |
| `TestInstallScriptPreflightRejected` | `预检失败却调用了 systemctl 4 次：` |
| `TestInstallScriptPreflightFailureNoPhantomStart` | `首次安装预检失败却动了 systemctl 4 次（没停过的东西不该 start）：` |
| `TestInstallScriptAutoUserWithPrefixDowngradesToNone` | `SERVICE 实际不是 none（摘要没说未安装服务）：` |

原文（`-v` 输出，调用日志按 `…` 截断；完整日志 `/tmp/anode-install-baseline.log`）：

```
--- FAIL: TestInstallScriptRequiresToken (0.12s)
    bootstrap_test.go:782: 报错没点名 --token/--token-file：注意：--prefix + 系统服务：unit 落在
        /tmp/TestInstallScriptRequiresToken1113680901/001/stage/etc/systemd/system/anode.service，
        systemd 搜索路径里没有它；systemctl enable 可能失败（建议 --service none，或去掉 --prefix）

        预检失败（退出码 2）。anode 的输出（末 20 行）：
          | anode check: unauthorized: hub: 节点身份被拒绝: 中枢在 welcome 之前以 close 1008 断开
          多半是令牌不对：节点可能被删了，或它属于另一个中枢/命名空间。
        注意：已恢复升级前的服务（anode.service，系统）—— 节点没被这次失败留在停止态
          正式配置未改动；旧服务已恢复（预检前停掉的实例已重新启动）。

--- FAIL: TestInstallScriptPreflightRejected (0.13s)
    bootstrap_test.go:841: 预检失败却调用了 systemctl 4 次：[sudo -n true curl … /anode --version
        … getent passwd zzjcool sudo -n systemctl stop anode.service systemctl stop anode.service
        … serve --check … sudo -n systemctl start anode.service systemctl start anode.service …]

--- FAIL: TestInstallScriptPreflightFailureNoPhantomStart (0.11s)
    bootstrap_test.go:1506: 首次安装预检失败却动了 systemctl 4 次（没停过的东西不该 start）：[…同上…]

--- FAIL: TestInstallScriptAutoUserWithPrefixDowngradesToNone (0.16s)
    bootstrap_test.go:1568: SERVICE 实际不是 none（摘要没说未安装服务）：下载 fixture.test/anode …
        预检通过
        == 落盘（anode install）
        Explicit token update requested; local vars/namespaces/limits/envPassthrough preserved.
        == 结论
        anode 已更新（升级完成；没有安装服务）
```

三条线索一眼可见，而且**都不是**这 4 条用例自己要测的东西：

1. 首装用例却报「**已恢复升级前的服务**（anode.service，**系统**）」「升级完成」「Explicit token
   update requested」——脚本认为这是一次**升级**，而且有一个**正在跑的系统服务**要先停。
2. 缺令牌用例却真的跑了一遍预检，还用了一把**别人**的令牌（1008 被拒）。
3. 全都只发生在带 `--prefix <tmp>/stage` 的 staging 安装上。

## 2. 三类判定与依据

实测环境（`id -u` = 1000、`uname -a` = Linux 7.2.5 omarchy x86_64、`PATH` 含 `/usr/local/bin`）：

| 观察 | 命令 | 结果 |
| --- | --- | --- |
| 本机**真的装过一份系统服务**，而且在跑 | `systemctl is-active anode.service` | `active` |
| 宿主的 unit 存在 | `ls -l /etc/systemd/system/anode.service` | `-rw-r--r-- root root 580` （`User=zzjcool`，`ExecStart=/usr/local/bin/anode …`） |
| 宿主的配置存在，**里面有令牌** | `ls -l /etc/anode/anode.yaml` | `-rw------- zzjcool root 208` |
| 宿主的二进制存在 | `ls -l /usr/local/bin/anode` | `-rwxr-xr-x root root 8139034` |

装上这份节点（Nov 13）之前这 4 条是绿的；干净 runner（没有宿主 anode 安装）上也一直是绿的。
**这就是「干净 origin/main 也红」的全部原因**：不是代码回归，是测试机上有宿主安装。

判定：

- **(a) 真 bug**：脚本的「这台机器上已经装过一份吗」探测，在 `--prefix` 之外还兜底看宿主默认
  落点。这直接违反 `docs/anode-install.md` §8 已写明的契约 ——
  「`--prefix /absolute/staging` 把默认布局置于 staging 根内 …… **staging 不启动/改动宿主服务**」。
  两个具体后果（都是这台机器上实测到的，不只是测试里）：
  1. 宿主 `/etc/anode/anode.yaml` 被当成「本次安装的已有配置」→ `UPGRADE=yes`，令牌/中枢/名字
     从它读回：缺 `--token` 时不再报「缺少 `--token`」，而是拿宿主的令牌去预检（1008）；摘要报
     「升级完成」，还给 `anode install` 带上 `--update-token`（把宿主的令牌当成「旧令牌」）；
  2. 宿主 `/etc/systemd/system/anode.service` 被当成「本次安装的旧实例」→ 先 `sudo systemctl
     stop anode.service`（失败时再 `start` 回来）。也就是说：**在一台跑着生产节点的机器上执行
     一次 `--prefix` staging 安装，会把生产节点停一次**。
- **(b) 测试过度假设**：不成立。测试没有硬编码 `/usr/local/bin`、没假设 root（夹具自带假 `id`/
  `sudo`/`systemctl`/`curl`），也没假设某个 shell（`ANODE_LOGIN_SHELL` 是显式钩子）。它们断言的是
  **脚本的对外行为**（缺令牌就退 2、预检失败不动 systemctl、`--prefix`+auto 降级成 none），
  这些行为本该与「宿主装没装 anode」无关。
- **(c) 需要特权**：不成立。4 条用例全程 temp/offline，连 root 都不需要（本机就是非 root 跑的）。

归因实验（临时在脚本里插两行 `DEBUG`，随后 `git checkout` 还原；`stderr` 由用例捕获后打印）：

```
DEBUG read_existing_config: PREFIX=[/tmp/…/stage] CONFIG=[/tmp/…/stage/etc/anode/anode.yaml]
                           EXIST_CONFIG=[/etc/anode/anode.yaml] UPGRADE=[yes]
DEBUG stop_old_service:     UNIT_FILE=[/tmp/…/stage/etc/systemd/system/anode.service]
                            host_unit_exists=[YES]
```

`--prefix` 明明给了，`EXIST_CONFIG` 仍然指向**宿主**的 `/etc/anode/anode.yaml`。

## 3. 根因（两处，同一族）

`scripts/install-anode.sh`：

1. `read_existing_config()` —— 候选列表写死为 `"$CONFIG" "${RUN_HOME}/.anode/anode.yaml"
   /etc/anode/anode.yaml`，第三项无视 `--prefix`。
2. `stop_old_service()` —— 判「有没有旧实例」写死 `[ -f "$UNIT_FILE" ] || [ -f
   "/etc/systemd/system/anode.service" ]`，第二项无视 `--prefix`。

带 `--prefix` 时 `CONFIG`/`UNIT_FILE` 本来就已经在 prefix 下（`compute_paths`），所以这两处兜底
**只有副作用、没有收益**。

## 4. 修法

### 4.1 脚本（真 bug 的修）

- `read_existing_config()`：`--prefix` 下候选只有 `$CONFIG`；不带 prefix 时保留原有兜底
  （老机器的用户服务布局 / v3 遗留），且第三项改走 `host_default`。
- `stop_old_service()`：`--prefix` 下只认 prefix 里的 unit，宿主 unit 一律不看。
- 新增 `host_default()` + 测试钩子 `ANODE_HOST_ROOT`（与既有 `ANODE_UNIT_PATH` /
  `ANODE_LOGIN_SHELL` 同类，默认空 = 真宿主 `/`）：把「宿主默认落点」整体搬进临时目录，
  用例才能在**任何机器**（包括没有宿主安装的 CI runner）上种诱饵验证上面两条。
- `web/install-anode.sh` 同步为字节相同（`TestInstallScriptCopyMatchesWeb` / `check_anode_skill.py` 都要求）。
- `docs/anode-install.md` §8 补一段把这个探测边界写成契约。

### 4.2 回归门（新用例，不碰老断言）

`TestInstallScriptPrefixIgnoresHostInstall`（两个子用例）：

| 子用例 | 诱饵 | 断言（= 修好的行为） |
| --- | --- | --- |
| `只落盘`（`--service none`） | 宿主 `etc/anode/anode.yaml`：`name: decoy-node`、中枢 `127.0.0.1:1`、令牌 = `fixtureWrong` | exit 0；摘要「未安装服务」；不出现「升级完成」/`decoy-node`；`anode install` 不带 `--update-token`；systemctl 0 次；预检 1 次（用 `--token` 那把） |
| `系统服务`（auto → system） | 同上 + 宿主 `etc/systemd/system/anode.service` | exit 0（没拿诱饵令牌）；**一次 `systemctl stop` 都没有**；不出现「已恢复升级前的服务」 |

诱饵令牌用 `fixtureWrong`（假中枢必拒）、诱饵中枢指向连不上的端口：**只要脚本读了宿主的配置，
这两条子用例就必然红**，与「机器上有没有真装过 anode」无关。

## 5. 验证

### 5.1 单元测试

```
$ go test ./cmd/anode/... -run 'Install' -v      # 修前
--- FAIL: TestInstallScriptRequiresToken (0.12s)
--- FAIL: TestInstallScriptPreflightRejected (0.13s)
--- FAIL: TestInstallScriptPreflightFailureNoPhantomStart (0.11s)
--- FAIL: TestInstallScriptAutoUserWithPrefixDowngradesToNone (0.16s)
FAIL	github.com/zzjcool/anotify/cmd/anode	83.807s

$ go test ./cmd/anode/...                        # 修后（全包，无 skip）
ok  	github.com/zzjcool/anotify/cmd/anode	87.986s
ok  	github.com/zzjcool/anotify/cmd/anode/internal/app	2.307s
ok  	github.com/zzjcool/anotify/cmd/anode/internal/applied	0.007s
ok  	github.com/zzjcool/anotify/cmd/anode/internal/capability	0.336s
ok  	github.com/zzjcool/anotify/cmd/anode/internal/clock	0.016s
ok  	github.com/zzjcool/anotify/cmd/anode/internal/config	0.034s
ok  	github.com/zzjcool/anotify/cmd/anode/internal/emitgw	0.202s
ok  	github.com/zzjcool/anotify/cmd/anode/internal/execenv	0.028s
ok  	github.com/zzjcool/anotify/cmd/anode/internal/hostenv	0.013s
ok  	github.com/zzjcool/anotify/cmd/anode/internal/hub	10.300s
ok  	github.com/zzjcool/anotify/cmd/anode/internal/install	0.195s
ok  	github.com/zzjcool/anotify/cmd/anode/internal/listen	0.017s
ok  	github.com/zzjcool/anotify/cmd/anode/internal/node	0.029s
ok  	github.com/zzjcool/anotify/cmd/anode/internal/ops	0.981s
ok  	github.com/zzjcool/anotify/cmd/anode/internal/resultstore	0.404s
ok  	github.com/zzjcool/anotify/cmd/anode/internal/runner	3.366s
ok  	github.com/zzjcool/anotify/cmd/anode/internal/upload	0.255s
```

`check-test-discipline` 的 `go/t.Skip` 仍为 `3 / 3`（**没有**新增静默跳过）。

### 5.2 变异验证（把修法改回去 ⇒ 必须红）

把脚本里两处 `--prefix` 分支改回「总是看宿主」（等价于修复前行为），再跑这 5 条：

```
--- FAIL: TestInstallScriptRequiresToken
    bootstrap_test.go:805: 报错没点名 --token/--token-file：注意：--prefix + 系统服务：…
--- FAIL: TestInstallScriptPreflightRejected
    bootstrap_test.go:864: 预检失败却调用了 systemctl 4 次：[… systemctl stop anode.service …
                           systemctl start anode.service …]
--- FAIL: TestInstallScriptPreflightFailureNoPhantomStart
    bootstrap_test.go:1529: 首次安装预检失败却动了 systemctl 4 次（没停过的东西不该 start）：…
--- FAIL: TestInstallScriptAutoUserWithPrefixDowngradesToNone
    bootstrap_test.go:1591: SERVICE 实际不是 none（摘要没说未安装服务）：…
--- FAIL: TestInstallScriptPrefixIgnoresHostInstall
    bootstrap_test.go:1631: 宿主配置被当成了本次的已有配置（摘要不是首装口气）：…
    bootstrap_test.go:1664: 停掉了宿主正在跑的 anode.service：[… systemctl stop anode.service …
                           --update-token …]
FAIL	github.com/zzjcool/anotify/cmd/anode	5.983s
```

注意最后一条：新用例的红**不依赖这台机器是否装过 anode**（诱饵在自己临时目录里），所以
干净 runner 上变异同样会红。改回修法后 5 条全绿。

## 5.3 `make ci`

```
修前（干净 worktree @ origin/main 2c115020）：
$ make ci
--- FAIL: TestInstallScriptAutoUserWithPrefixDowngradesToNone (0.50s)
--- FAIL: TestInstallScriptPreflightRejected (0.28s)
--- FAIL: TestInstallScriptPreflightFailureNoPhantomStart (0.10s)
--- FAIL: TestInstallScriptRequiresToken (0.12s)
FAIL	github.com/zzjcool/anotify/cmd/anode	87.176s
FAIL
make[1]: *** [Makefile:281: ci] Error 2      → EXIT=2（挂在 `go test ./... -count=1 -shuffle=on`）

修后（本 worktree）：
$ make ci
→ EXIT=0，守卫✅ 56 条全绿，`go test ./...` 40 个包 ok 0 FAIL，耗时 2m24s
```

对比：唯一的差就是这 4 条用例；其余 38 个包/守卫在修前也是绿的（所以它是「本机恒红」而不是「顺手带红了别的」）。

## 6. 遗留风险（同类但**未**在本 PR 改）

同一族（`--prefix` 不自包含）还有两处，属于设计变更，不在本次「修红」范围内：

1. `do_uninstall()` 有意按「所有已知落点」清理（含宿主 `/etc/anode/anode.yaml`、
   `/usr/local/bin/anode`、`/var/lib/anode`），用例 `TestInstallScriptUninstallModes` 也按这个语义写
   （夹具的假 `rm` 就是为了拦住它别真删）。`--prefix --uninstall` 会不会误删宿主安装，需要产品决定。
2. `--prefix` + `SERVICE=system` 时，`systemctl <verb> anode.service` 在真 systemd 下操作的是
   **宿主**同名 unit（prefix 里的 unit 不在搜索路径）。脚本对此只 warn，靠 `ANODE_SYSTEMCTL`
   「自带 systemd 语义」兜底；`retire_user_unit()` 也会去动宿主用户 unit。要真隔离得改服务生命周期语义。

另：本 worktree 首次 `make ci` 死在 `check-wire-fields`（`Cannot find package 'yaml'`）——新
worktree 没有 `node_modules`，`npm ci` 之后正常。这是 worktree 初始化步骤缺失，与本 PR 无关。

## 7. `make ci` 前后对比

| | 修前（worktree @ `2c115020`） | 修后 |
| --- | --- | --- |
| `make ci` | **EXIT=2** | **EXIT=0** |
| 挂在哪一步 | `go test ./... -count=1 -shuffle=on` | — |
| `go test ./...` | 38 个包 ok / 1 个包 FAIL（`cmd/anode`，4 条用例） | 40 个包 ok / 0 FAIL |
| 守卫 `✅` | 56（守卫全过） | 56（守卫全过） |
| 耗时 | 2m14s | 2m24s |

修前那轮的完整日志见 `/tmp/ci-before.log`（本机临时文件，不入库），要点就是 §1 的 4 条。
修后这轮的 `make ci` 里也含 `check-test-discipline`（`go/t.Skip 3/3`，无新增跳过）与
`check-anode-skill`（`web/install-anode.sh` 与 `scripts/install-anode.sh` 字节校验）。
