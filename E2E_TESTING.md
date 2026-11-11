# Anotify 端到端测试体系（固化方案）

> **规则：每次开发完成后，必须跑 `make e2e` 全绿才算完成。** 这是固化的质量门禁。
> 除「iOS 真机 APNs 推送」外，**所有环节全自动，无需人工**。

## 技术选型

| 层 | 技术 | 覆盖 |
| --- | --- | --- |
| 单元 | `go test ./...` | store/auth/api/push/ws/server/lease/source + 节点 runner/ops/resultstore 包级逻辑 |
| API 契约 | Node fetch（`scripts/e2e/suites/api_contract.mjs`） | 全端点的状态码/鉴权/参数校验 |
| **Passkey 认证** | **Playwright CDP 虚拟认证器** | 真实 WebAuthn 注册/登录/登出/会话（无头，无需真人） |
| WS 协议 | Node WebSocket 客户端 | v4 帧：hello/welcome、config.get/applied/changed、task.dispatch/accept/reject、lease.grant/renew/ack、task.result/result.ack、task.emit/ack、task.cancel/cancel.ack、upgrade/logs |
| 路由过滤 | Node fetch | 标签路由 + event_scope 过滤全矩阵（notify.matched 断言） |
| 桌面推送 | Playwright 持久化 Chrome | 真实 FCM 订阅 → 投递记录 |
| 持久化 | 重启服务验证 | 消息/设备/Key 重启后仍在 |
| 安全 | Node fetch + DB 检查 | scope 越权/Key 篡改/哈希格式/会话属性 |
| 前端 | Playwright | 6 页渲染 + 未登录跳登录 + 已登录真实数据 |
| 边界 | Node fetch | 畸形输入/超大体/并发/Unicode |
| Docker | docker build/run | 单二进制镜像端到端（中枢 + 节点镜像） |

## 唯一人工环节

- **iOS Safari 真机 APNs 推送**：需真实 iPhone，无法自动化（见 `IOS_TESTING.md`）。
  其余全部自动。

## 目录结构

```
scripts/e2e/
  run_all.sh            # 总编排：构建→逐套件运行→汇总报告
  lib/harness.mjs       # 服务生命周期 / HTTP 客户端 / 断言 / 播种
  suites/
    api_contract.mjs    # API 契约矩阵
    auth_flow.mjs       # Passkey 全流程（虚拟认证器）
    ws_protocol.mjs     # 节点协议（v4 帧集：config / dispatch / lease / result / emit / cancel）
    routing.mjs         # 派发四步 + 六种剪枝
    persistence.mjs     # 重启后 events/tasks 存活
    security.mjs        # 安全矩阵
    edge_cases.mjs      # 边界/并发/Unicode
    frontend.mjs        # 前端渲染 + 路由守卫
    ui_shift.mjs        # 开页抖动回归（几何采样去重）
    p2_guards.mjs       # P2 工程守卫：字体自托管 / 抽屉 a11y / reduce 语义 / View Transitions
```

新增套件要在下面几处登记，否则会被静默丢弃 / 抢端口 / 跑错 authority：

1. `scripts/e2e/parallel_runner.mjs` 的 `CHROME_SUITES`（或 `HTTP_SUITES`）
2. `scripts/e2e/lib/harness.mjs` 的 `SUITE_PORTS`（每套件独占 50 个端口段）
3. 只有需要跑在 **explicit-mock** 验收下的套件才进 `EXPLICIT_MOCK_SUITES`（默认 real）。
   `E2E_MOCK_ONLY` 是执行器**注入**的，不从外部环境继承：清单外的套件（含 `frontend`）
   会被剥掉。authority 裁决与分工见 `docs/v4/e2e-inventory.md`，离线核对用
   `node scripts/e2e/parallel_runner.mjs --authority-plan`。

另外：**临时目录一律用 `H.mkdtempTmp()` 创建、`finally` 里 `H.removeTmpDir()` 删除**，
不要裸写 `mkdtempSync(tmpdir(), ...)`（见下节；`make ci` 的静态守卫会拦）。

## 临时目录与 /tmp 卫生（#326）

**本机 `/tmp` 是 tmpfs（7.5G）——测试漏下的临时目录吃的是内存，不是磁盘。**
2026-11 一次清理就回收了 4.5G：281 个 `anotify-agent-bin`（v3 时代 `go test ./cmd/agent`
每次编一份真二进制；v4 的等价物是 `cmd/anode` 的 `anode-n3-binaries-*`，已由 `TestMain` 自清）、
1424 个 `anotify-e2e-*`（端口重试失败的那些尝试从不删）、
214 个 `anotify-script-*`（cli_auth 的假 HOME）。泄漏还会放大并行 e2e 的
"环境型失败"：tmpfs 余量越小 → Chrome 越容易崩；端口越抢不到 → 漏得越多。

规矩只有一条：**测试创建的临时目录，测试自己必须删干净。**

### e2e 侧（`scripts/e2e/`）

统一用 harness 的 helper，不要裸写 `mkdtempSync(tmpdir(), ...)`：

```js
import * as H from "../lib/harness.mjs";

const tmp = H.mkdtempTmp("anotify-xxx-"); // 创建即登记
try {
	// ... 用例 ...
} finally {
	H.removeTmpDir(tmp); // 正常路径：立刻删并注销
}
```

- `mkdtempTmp(prefix, parentDir?)`：`parentDir` 只用于「在已登记的套件目录下再建子目录」。
- 进程退出（断言崩溃 / `process.exit` / 未捕获异常）时 harness 的 `process.on("exit")`
  兜底删除所有仍登记的目录；**但这只是保险丝**，正常路径仍应在 `finally` 里删。
- 端口重试循环（`startServer`）里每次尝试的临时目录必须在 `continue` 之前删掉 ——
  失败尝试最容易被忘掉，而它正是历史上泄漏的大头。

### Go 侧（`cmd/anode`）

跨用例共享、不属于任何单个用例的目录（例如 `TestMain` 编出来的两个真 anode 二进制，
`anode-n3-binaries-*`）在 `TestMain` 里用 `os.MkdirTemp` 创建、`m.Run()` 之后统一
`os.RemoveAll`（`cmd/anode/main_test.go`）。单个用例自己的目录继续用 `t.TempDir()`
（testing 会删，且 `-run` 子集下也能工作）。

覆盖「没有 `--data-dir` → 工作目录/数据目录兜底到系统临时目录」的用例（standalone
的 `run`、真进程 `emit` 等）用 `isolateSystemTemp(t)`：`t.Setenv("TMPDIR", t.TempDir())`
把兜底行为指到 testing 自己会删的目录里。兜底逻辑仍被真实验证，但超时/失败路径
（故意保留现场）不会再往真实 `/tmp` 里写。

### 守卫（防回归）

```bash
# 包住一条命令：给被包命令一个私有 TMPDIR，跑完只断言「本次运行自己的新增 = 0」
node scripts/check-tmp-leaks.mjs -- go test ./cmd/anode -count=3

node scripts/check-tmp-leaks.mjs --lint        # 静态：e2e 里不许有裸 mkdtempSync
node scripts/check-tmp-leaks.mjs --self-test   # 守卫自己的红/绿/不误报/透传自测
```

- **为什么是「私有 TMPDIR + 新增比对」而不是直接比对 `/tmp` 总量或集合差**：
  本机经常**同时**有别的 e2e / 测试在跑（并行子 agent、另一个 worktree 的
  `make ci`）。它们用的也是 `/tmp/anotify-*`，直接看总量必然误报；只看
  "运行期间新增"也不行 —— 实测就撞过：本分支最后一次全量 e2e 时另一个
  worktree 正在跑旧代码的 `make e2e`，它漏的 4 个目录全记到了本次头上。
  所以守卫给被包命令一个私有的 `TMPDIR`（`/tmp/.tXXXXXX`）。
  Go 的 `os.MkdirTemp` / `t.TempDir` 与 Node 的 `os.tmpdir()` 都跟随
  `TMPDIR`，因此「我们的泄漏」全部落在这个私有目录里、与别的进程天然分开；
  跑完只断言私有目录里 `anotify-*` 为 0（有就逐个列出路径并 `exit 1`，
  同时把私有目录连同残留清掉）。`/tmp` 顶层的新增只作为 ⚠️ 提示打印
  （按定义不归因于本次运行），不算失败。
- 隔离目录名故意很短（`/tmp/.tXXXXXX`）：unix domain socket 的 `sun_path` 上限是
  108 字节，而测试的 socket 路径常拼在 `t.TempDir()` 下；长名字会把「本来刚好
  放得下」的用例挤爆（实测 `TestEmitJSONSuccessAndTextMode` 的 fake.sock
  因此 `bind: invalid argument`）。
- 前提：测试临时目录必须走 `os.tmpdir()` / `tmpdir()`（Go `os.MkdirTemp("", …)`、
  Node `H.mkdtempTmp()` 都满足）。**不要在测试里硬编码 `/tmp`** —— 那会绕过归因，
  而且不可移植。
- 已接线：`make ci` 跑 `--lint` + `--self-test`，并用它包住 `make test`（含
  `go test ./cmd/anode`）；`./scripts/e2e/run_all.sh` 在收尾前 `--lint` 并包住
  并行执行器。因此 `go test ./cmd/anode` 与全部 e2e 都受守卫覆盖，新增套件不用单独登记。

### 主机兜底（可选，不在仓库里跑）

`systemd-tmpfiles` 默认对 `/tmp` 是 10 天保留，对高频测试太慢。想加一道主机层兜底：

```ini
# /etc/tmpfiles.d/anotify-tests.conf
q /tmp/anotify-agent-bin* - - - 12h
q /tmp/anotify-e2e-*      - - - 12h
q /tmp/anotify-script-*   - - - 12h
```

`q`（cleanup only if exists）比默认 10d 激进得多，只清这些前缀，不影响别的程序。
这是**兜底**不是修复：守卫红的时候请修测试，不要靠它。

## 运行

```bash
make e2e          # 全量端到端（构建 + 所有套件）
make e2e-one S=auth_flow   # 只跑某个套件
```

## 测试矩阵（全 case 清单）

见各 suite 文件顶部注释。当前注册 **28 套件**（13 HTTP + 15 Chrome），断言总数以执行器
汇总行为准（每次运行现算；逐套件 kept/migrated/retired 台账与历史口径见
`docs/v4/e2e-inventory.md`），覆盖：

- 每个 API 端点的 成功/401/403/400/404 路径
- Passkey 注册→登录→登出→会话失效 全生命周期
- WS 全部帧类型 + 断线续传
- 投递规则全部边界（无tag消息/无tag设备/交集/ALL拒绝/status 5 种 × filter 3 种）
- 并发 seq 单调性、畸形输入、SQL 注入安全性、Key 哈希不可逆
- **P2 工程守卫**（`p2_guards`，30 断言）：字体 0 外部请求 + `document.fonts.check`
  证明 Inter/Fraunces/JetBrains Mono 真的生效；390 视口抽屉的 role=dialog /
  aria-modal / Esc 关闭 / 焦点回归；reduce 下 `--dur`、`--dur-modal-out` 归零；
  跨文档 View Transitions 的 head 内联 opt-in + pagereveal 触发 + 导航 0 pageerror
- **真节点**：两个套件刻意分工，不重复 ——
  - `node_v4`（数据面）：真 `anode` 二进制 + 真后端，真 hello/配置对齐 → 真部署拉取 →
    真跑任务（事件 → task → `lease.grant` → 真执行 → `task.result`）→ 日志 / 产物上传与
    私有下载 / 两种取消。
  - `live_agent`（机器面 + 生命周期）：`anode` 的 CLI 契约（`--version` 形状 /
    `emit` 退出码映射）、`anode install --prefix` 的落盘权限与幂等、安装产物 → `serve`
    往返、SIGTERM 干净退出后中枢承认离线。验收读**进程退出码 / 文件权限 / 文件内容**，
    专抓「每层单独看都对、连起来才错」的一类 bug。
  > 构建事实（#336/#338）：`run_all.sh` 只构建三个真实产物 —— `anotify`（`cmd/server`，带
  > `embeddist`）、`devseed`、`anode`（`cmd/anode`）；v3 的 `cmd/agent` / `AGENT_BIN` 已随协议 v4
  > 退役，已不存在「迁移中」的构建路径。
  > `real_backend` 已具名退役并删文件（理由见 `docs/v4/e2e-inventory.md` §4）。
