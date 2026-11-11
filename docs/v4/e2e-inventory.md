# v4 E2E inventory（#338 收尾台账）

> 逐套件的 **kept / migrated / retired** 台账 + 验收 authority 映射。
>
> 为什么要有这份文件：`#331/#332/#333/#334/#336/#337` 关闭审计里两条缺口都是它 ——
> ①「#336 的 e2e inventory 文档缺失」（迁移了 13 个套件却没有一张逐套件台账）；
> ②`scripts/e2e/suites/real_backend.mjs` 已具名退役但文件仍在（#336 的遗留）。
>
> **基线**：`origin/v4/integration @ 66d926a`（撰写时的 integration tip）。
> 本文件按 **#338 合并后终态**描述，并对**尚未合入本条基线的分支**逐条标注：
> `#354`（`issue/335-frontend`）与 `3651707`（`338/chrome-v4`）。
> 标注「66d926a 实测」的数字只代表基线版本；标「另一分支」的改动在本条基线上还看不到。
>
> **权威注册表**只有一处：`scripts/e2e/parallel_runner.mjs` 的 `HTTP_SUITES` /
> `CHROME_SUITES`（共 **28** 个套件 = 13 HTTP + 15 Chrome）。本文件不复制清单，
> 只解释每个套件为什么在、为什么走这个 authority。

## 0. 一页结论

| 类别 | 数量 | 分类 | 验收 authority |
|---|---|---|---|
| HTTP 套件（无 Chrome） | 13 | 11 migrated（#336）+ 1 新增（`node_v4`）+ 1 kept（`passkeys`） | real |
| Chrome 套件 | 15 | 5 migrated（#335/#354）+ 7 migrated（#338）+ 3 kept | 1 real 点名（`frontend`）+ 4 explicit-mock + 10 real |
| 已退役 | 1 | `real_backend`（#201 → #336 具名退役 → #338 删文件） | — |

三条要记住的事实：

1. **全部 28 个套件都真跑、真 exit**：`explicit-mock` 不是「跳过」，它只是把验收标的
   从「真实后端语义」换成「locale / DOM / 几何 / 交互契约」（裁决见 §3）。
2. `frontend` 是**唯一被点名**的 real 前端 authority；执行器对它**剥掉** `E2E_MOCK_ONLY`
   并告警，real 不允许静默降级成 mock。
3. 没有任何套件是靠「删断言」变绿的：v3 语义消失处一律换 v4 等价（通常更强）断言，
   或在套件头注释里写明「为什么它在 v4 里不存在了」（`contract.mjs` 是范式）。

## 1. 13 个 HTTP 套件（真后端进程，无 Chrome）

分类口径：**kept** = 文件与断言在 v4 迁移中未改；**migrated** = 从 v3 语义重写为 v4 语义。

| 套件 | 分类 | 覆盖什么 / 迁移要点 |
|---|---|---|
| `api_contract` | migrated（#336 / PR #353） | v4 资源面 API 契约矩阵：状态码 / 信封 / CAS / camelCase / 空列表 `[]` |
| `contract` | migrated（#336） | v4 冻结契约（`decisions` V4-D1…D6）的运行时断言；v3 的 A1–A12 逐条要么迁到 v4 等价物，要么写明「为什么它在 v4 里不存在了」 |
| `ws_protocol` | migrated（#336） | v4 节点协议帧级：真后端 + 真 WS 会话，**执行由 mock 节点驱动**（帧契约归它，真执行归 `node_v4`） |
| `routing` | migrated（#336） | 派发四步 + 未触发原因：源 `targets` → 校参 → 候选节点 → 派发（v3 的「边表」换成资源本身） |
| `persistence` | migrated（#336） | 重启后 v4 资源文档（Capability / Source / Namespace + CAS 令牌）与执行事实仍存活 |
| `security` | migrated（#336） | v4 安全矩阵：三类主体（用户会话 / API Key / 节点令牌）互不串用、跨 ns 隔离、凭据卫生、限流与背压 |
| `edge_cases` | migrated（#336） | 边界 / 并发 / 编码 / CAS / 分页（事件中心模型） |
| `passkeys` | **kept** | Passkey 管理端点（增删改查 / 越权 / 最后一个不能删）。认证面在 v4 未被重写，`#336` 的 diff 里没有它（`git diff --stat 0b0501b^1 0b0501b -- scripts/e2e/suites/passkeys.mjs` 为空） |
| `capacity` | migrated（#336） | 保留期清理（挂在 30s 租约巡检 tick 上，所以超时预算单独放宽到 300s）+ 派发背压阈值 |
| `orphans` | migrated（#336） | 孤儿 pending 任务的诊断（`/v1/orphans`）与处置（abandon）。v4 删了「节点申报能力表」：判据改成与调度器**共用同一份实现**的 `EligibleNodesForTask`（`no_capability` / `no_online_node` + 7 种 `nodeReason`） |
| `deploy` | migrated（#336） | v4 部署模型：能力产物 / 人工部署动作 / 内容通道 / 部署事实读模型 |
| `live_agent` | migrated（#336） | 真 `anode` 的**CLI 面 + 安装/启动/退出生命周期**（`--version` 形状、`emit` 退出码映射、install 落盘幂等、SIGTERM 干净退出）。数据面（部署/执行/日志/产物/取消）刻意不重复，归 `node_v4` |
| `node_v4` | **新增**（#336） | 真节点核心链，零 mock：真 `cmd/anode` 二进制 + 真后端 → v4 hello/配置对齐 → 人工部署 + 节点拉产物 → 事件 → task → `lease.grant` → **真执行** → `task.result` → 日志 / 产物上传与私有下载 / 两种取消 |

要点：v3 的节点侧 `cmd/agent` 与 `AGENT_BIN` 已随协议 v4 退役（#331/#332/#333），
`run_all.sh` 现在只构建 `anotify`（`cmd/server`）/ `devseed` / `anode`（`cmd/anode`）。

## 2. 15 个 Chrome 套件（真浏览器）

| 套件 | 分类 | authority | 覆盖什么 / 迁移要点 |
|---|---|---|---|
| `frontend` | migrated（#335 / PR #354） | **real**（点名） | 前端渲染 + 路由守卫 + 真实数据：真 server / anode / devseed，35 条契约用例。f3 驱动，`mode` 由 `E2E_MOCK_ONLY` 决定，默认 real |
| `i18n` | migrated（#335 / PR #354） | explicit-mock | 多语言 AC：页面 × 4 语言可达、hreflang、切换器、跨语言导航、query 保留。f3 版覆盖 19 页；**基线 66d926a 的 7 页版实测 299 断言 0 失败** |
| `i18n_coverage` | migrated（#335 / PR #354） | explicit-mock | 渲染 DOM 的中文残留门禁：en/es 页可见文本 ZERO CJK |
| `ui_shift` | migrated（#335 / PR #354） | explicit-mock | 开页抖动（layout shift）回归：语言条 / 命名空间槽位 / 折叠态 / 预排版，实测几何去重。#354 起超时预算 300s（单跑 ~148s，默认 120s 在并发下必被 SIGKILL） |
| `ux_regression_19_23` | migrated（#335 / PR #354） | explicit-mock | #19–#23 控制台 UX 回归（无权限页 / KPI 外壳 / 设备测试通知面板等）。**基线 66d926a 实测 13 断言 0 失败** |
| `cli_auth` | migrated（#338 @ `3651707`） | real | CLI 设备授权全链路（AC-01~36 可端到端部分）。v4：事件投递改 `POST /v1/namespaces/{ns}/events`（Bearer Key）+ 开局建真实源 |
| `passkey_enroll` | migrated（#338 @ `3651707`） | real | Passkey 新设备授权（D-B 端点契约 + D-C 安全不变量）；节点/身份断言改走 v4 真 WS 握手 |
| `admin_flow` | migrated（#338 @ `3651707`） | real | 管理后台全流程（首用户自动 admin / 管理端点 / 权限 / 前端渲染）；任务读改 `/v1/namespaces/{ns}/tasks` + `TaskList` 信封 |
| `deeplink` | migrated（#338 @ `3651707`） | real | 通知深链 → `message.html?id=<taskId>` 任务详情；断言改盯 v4 DOM 锚点（`#msg-title` / `#msg-sub` / `#task-facts` / `#msg-trace`） |
| `auth_flow` | migrated（#338 @ `3651707`） | real | Passkey 认证全流程（CDP 虚拟认证器）；能力/边/源改 v4 信封（`capabilityBody` / `sourceBody`，边读派生投影 `source.spec.targets`） |
| `events_paging` | migrated（#338 @ `3651707`） | real | events 游标分页（#234/#235/#236）。v4：`before` → `continue`，游标在 `metadata.continue`；UI 由「?before= 翻页」改「加载更多」累积 |
| `password_login` | migrated（#338 @ `3651707`） | real | 密码登录（第二套登录模型）：设置/修改/移除密码、与 Passkey 并存、最后一个 Passkey 删除护栏 |
| `lang_hint` | **kept** | real | 语言提示条（渐进增强，纯客户端 `navigator.language` 判定）。v4 未改；**基线 66d926a 实测 86 断言 0 失败** |
| `recovery_next` | **kept** | real | #32 恢复码登录跳转（`?next=` 白名单页）。v4 未改；**基线 66d926a 实测 5 断言 0 失败** |
| `p2_guards` | **kept** | real | P2 工程守卫：字体自托管（0 外部请求 + `document.fonts.check`）、390 视口抽屉 a11y（role/aria-modal/Esc/焦点回归）、reduce 下 `--dur` 归零、View Transitions opt-in。v4 未改；**基线 66d926a 实测 30 断言 0 失败** |

「kept」= 在 66d926a 与 `338/chrome-v4`（`3651707`）之间**逐字节未改**
（`git diff --stat 66d926a 3651707 -- scripts/e2e/suites/{lang_hint,recovery_next,p2_guards}.mjs` 为空），
且在本基线实跑全绿 —— 它们不碰 v3 删除掉的 API 形状，所以不需要迁移。

## 3. 五个 FE 套件的验收 authority（#338 冻结裁决）

原文：`.pi-orchestrator/v4/issues/dispatch/TRACKER.md`「2026-11-11 13:5x 裁决：五个 FE suite 的验收模式」。

| 套件 | authority | 理由 |
|---|---|---|
| `frontend` | **real**（真 server / anode / devseed） | 它是前端唯一的真后端体检：页面级 `/v1/*` 无 4xx/5xx、真部署链、真数据 |
| `i18n` | explicit-mock | 验 locale / hreflang / 切换器 / 语言保持；不含真实 API 语义 |
| `i18n_coverage` | explicit-mock | 验渲染 DOM 无 CJK 残留（locale 契约） |
| `ui_shift` | explicit-mock | 验首帧后的几何稳定性（DOM 几何契约） |
| `ux_regression_19_23` | explicit-mock | 验控制台 UX 回归点（DOM 交互契约） |

这四个套件的 real 模式缺 session/seed bootstrap（症状是 `guest index redirected to password login`
这类**系统性**失败），补那套底座属新底座工作，不在 #335/#338 范围。
真实 API / 节点覆盖由 `frontend`(real) + `node_v4` + 13 个 HTTP 套件 + 7 个迁移后的
legacy Chrome 套件承担 —— **不是**由 mock 承担。

机器可读的两个入口（都在执行器里，属实现而非文档承诺）：

```bash
# 1. 离线打印每个套件的 mode/authority（不跑任何套件）
node scripts/e2e/parallel_runner.mjs --authority-plan

# 2. 每次运行落盘本轮 authority 清单（与 results/<suite>.json 同级）
cat .e2e-bin/results/_authority-manifest.json
```

实现纪律（`scripts/e2e/parallel_runner.mjs` 的 `EXPLICIT_MOCK_SUITES` 注释里逐条写明）：

- 只有**具名**清单里的四个套件被注入 `E2E_MOCK_ONLY=1`；其余套件 env 一个都不动；
- `frontend` 属 real authority：父环境若带 `E2E_MOCK_ONLY` 会被**剥掉**并告警；
- 清单内套件若从父环境继承了别值（如 `E2E_MOCK_ONLY=0`），按裁决**覆盖成 1** 并告警；
- 清单自检（`assertAuthorityContract`）：`frontend` 一旦混进 mock 清单、或清单里出现未注册
  套件名，执行器直接 `exit 2` —— 门禁对着 mock 判「前端 real 绿」比红更危险。

> 合入顺序说明：本条基线（66d926a）的四个套件**还不读** `E2E_MOCK_ONLY`；
> 读它的 f3 驱动随 `#354` / `3651707` 落地。因此在本条基线上，注入是**authority 声明**，
> 合入后才是**功能开关**（`f3-driver.mjs` 的 `isMock()`）。执行器的 mode 一律由**注入的 env**
> 推导，不从套件 stdout / 断言数反推 —— 反推会在「套件其实没读这个开关」时给出假 authority。

## 4. `real_backend` 具名退役（#201 → #336 → #338 删文件）

`scripts/e2e/suites/real_backend.mjs`（1765 行）**已删除**，不是「暂时下线」。
它从 #336 起就不在注册表里（`CHROME_SUITES` 只剩一条具名退役注释），本轮把文件也删掉，
避免下一个人误以为「跑一下 real_backend 就能体检真后端」。

退役理由（逐条对应当年它存在的三个目的）：

1. **真 `cmd/agent` v3 CLI 已随协议 v4 删除**（#331/#332/#333）：它的 B 部分靠
   `anotify-agent emit --port go` / `anotify-agent run` 起节点、验多跳产出，
   这些子命令与 `AGENT_BIN` 都不存在了。
2. **真节点链归 `node_v4`（#336）**：真 `cmd/anode` 二进制 + 真后端，覆盖
   hello/配置对齐 → 人工部署 → 拉产物 → 事件 → task → `lease.grant` → 真执行 →
   `task.result` → 日志/产物下载/两种取消。比 `real_backend` 当年那条链更全。
3. **真前端后端体检归 `frontend`（real authority）**：#338 裁决把「前端对着真后端」
   这件事明确成 `frontend` 一个套件负责（页面级 `/v1/*` 无 4xx/5xx、真数据渲染），
   不需要第二个套件重复同一件事。

残留（**未在本轮授权范围内修改**，记录在此以免被当成遗漏）：

- `scripts/e2e/lib/harness.mjs` 的 `SUITE_PORTS` 里仍有 `real_backend: 6900` 端口段
  （同样还有更早退役的 `reply_e2e: 6750`）。没有套件读它们，只是号段保留；
  要清理请单独开一条（改 harness 会影响所有套件）。
- `DEVELOPMENT.md` 与 `docs/design/v4/{capability,playbook}.md` 里把 `real_backend`
  当成「回归验收套件」举例的历史文字，属历史设计记录，不改。

## 5. 断言计数与出处（不猜数字）

| 口径 | 断言数（或已注明的用例数） | 出处 |
|---|---|---|
| 13 个非前端套件（含 `node_v4`）**两连绿** | **1321** | **PR #353**（`issue/336-real-node-e2e`，merge `0b0501b`）；记录在 `.pi-orchestrator/v4/issues/dispatch/TRACKER.md:58`：「13 个非前端套件 1321 断言两连绿；node_v4 真 anode 88」 |
| 其中 `node_v4`（真 `anode`） | 88 | 同上 |
| 7 个 legacy Chrome 迁移后 | 527 / 0 | `338/chrome-v4 @ 3651707`（7 个套件完整跑一遍） |
| `frontend`（real） | 35 条契约用例 35/35（用例数，非断言数） | #338 裁决记录（TRACKER「2026-11-11 13:5x」） |
| 66d926a 基线实测（本轮门禁预置） | `i18n` 299/0、`ux_regression_19_23` 13/0、`lang_hint` 86/0、`recovery_next` 5/0、`p2_guards` 30/0 | 本轮实跑，日志 `/root/tmp/338prep/{i18n,ux,kept3}.log` |
| `make e2e-quick`（4 套件快检） | 463 断言 0 失败 | TRACKER「2026-11-11 13:0x 快照」协调者基线 |

汇总一句话：**1321** 是 #336 那批「非前端套件」的断言总量，不是全仓总量；
全仓总量随 FE 套件迁移在变（f3 重写后前端套件断言数会重排），所以本文件不再维护
「总计 N 断言 / M 套件」这种会立刻过期的数字 —— 现跑现看的权威是执行器汇总行与
`results/<suite>.json`。

## 6. 怎么核对这份台账

```bash
# 注册表 + authority（28 个套件，24 real / 4 explicit-mock）
node scripts/e2e/parallel_runner.mjs --authority-plan

# 某个套件真的跑、真的 exit（mode 打印在汇总每一行，也落盘在 manifest）
bash scripts/e2e/run_all.sh --serial i18n
cat .e2e-bin/results/_authority-manifest.json

# real_backend 确实不在了：文件不存在 + 注册表无该项
ls scripts/e2e/suites/real_backend.mjs   # No such file or directory
grep -n real_backend scripts/e2e/parallel_runner.mjs   # 1 行（实测，`rg -c` = 1）：只有 :85 那条具名退役注释，没有任何代码引用
```

## 7. 未决 / 已知限制

- 四个 explicit-mock 套件在 66d926a 上**不读** `E2E_MOCK_ONLY`（见 §3 合入顺序说明）；
  authority 由执行器声明并落盘，功能开关随 `#354`/`3651707` 生效。
- `frontend` 的 real bootstrap（prod ns / 双会话 / 真实资源）随 `#354` 落地；
  本条基线上的 `frontend` 仍是 v3-era 版本。
- harness 里 `real_backend: 6900` / `reply_e2e: 6750` 两个号段是死配置（见 §4 残留）。

## 8. 产物身份：二进制 sha256 不是发布身份

验收记录里经常出现「本轮构建 `anotify` = `149228ce…`」这种行。**它不能当发布身份用**：

`make build` / `make manifest` 每次都把**构建实例信息**编进二进制（`Makefile` 顶部）：

- `BUILT_AT := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)` → `internal/version.BuiltAt`，
  **两次构建之间必然不同**（同一棵树、同一命令，隔一分钟 sha256 就变）；
- VCS stamp：`GIT_SHA`（`git rev-parse --short HEAD`）→ `internal/version.Commit`，
  `anode` 还把它拼进 `main.version`（`$(VERSION)+$(GIT_SHA)`）；`VERSION` 缺省是
  `git describe --tags --always --dirty`，**工作区有未提交改动就带 `-dirty`**；
- 因此二进制 sha256 的变化只说明「构建事实变了」，不说明「代码变了」——反过来也不成立。

**稳定身份 = 版本串 + spec/fe 指纹**，也就是 `make manifest` 里除两行 sha256 之外的三行：

```bash
make manifest
# version:  <VERSION_LABEL>   ← 版本串（干净 tag 时形如 v0.2.0-336-gd5c6a5d+d5c6a5d）
# git:      <git short sha>
# built_at: <RFC3339 UTC>     ← 只用于「这是哪一次构建」
# <sha256>  anotify           ← 构建实例指纹：随 BuiltAt / VCS stamp 变，别当版本
# <sha256>  anode
# spec:     <api/*.yaml 拼接的 sha256>          ← 五份 spec 一改必变（exact-five）
# fe:       <internal/server/dist/manifest.json 的 sha256>  ← 前端资源集合/内容一改必变
```

判据（写验收记录时的正确姿势）：

| 想回答 | 看哪一行 | 不要看 |
|---|---|---|
| 这份产物是**哪个版本 / commit 的源码** | `version` / `git` | 二进制 sha256 |
| 五份 spec 是否与本轮一致 | `spec` | 二进制 sha256 |
| 前端产物是否与本轮一致 | `fe` | 二进制 sha256 |
| 这次构建本身是否一致（同一台机器重跑） | 两行二进制 sha256（**并且** `built_at` 相同才可能相等） | — |

所以验收文档里写二进制 sha256 时必须带限定语「本轮构建（非稳定身份）」，
并与同轮 `version` / `spec` / `fe` 一起记录才可复核。逐字段来源与「为什么 anode 的
`main.version` 要带 `+<commit>`」见 `docs/v4/g4a-build-manifest.md` §2–§3。
