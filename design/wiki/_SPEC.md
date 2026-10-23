# Anotify Wiki · 写作与结构约定（所有页面必须遵守）

> 本文件是本次重写的事实源。**不要参考 `design/hub-blueprint.md` / `design/hub-architecture.html` / `design/adr-001-event-center-pivot.md` / `design/positioning.md`**，那些是历史讨论稿，结构混乱。只依据本文档 + 下面的「设计结论清单」。

## 0. 产品一句话

Anotify 从一个「Agent 完成通知终端」升级为**事件中心 + 执行节点**：触发源（cron / webhook / watch / 外部 API）产生事件 → 事件日志 → 可解释的规则 → 带租约的任务 → 机器上的 node agent 执行 → 结果回流成新事件（可接力）→ 该通知人的推手机。

## 1. 文件

- 输出目录：`design/wiki/`
- 共享样式：`design/wiki/assets/wiki.css`（**已写好，不要改**）
- 共享脚本：`design/wiki/assets/wiki.js`（**已写好，不要改**）
- 每页一个 `.html`，文件名见下方页面清单
- 共享片段模板（**每个 HTML 都从这里抄**）：`design/wiki/_skeleton.html`

## 2. 硬性技术约束

1. **颜色只能用 `tokens.css` 的 CSS 变量**。禁止任何硬编码色值（`#fff`、`rgba(...)`、`rgb(...)`、命名色）。需要透明度时用 `color-mix(in srgb, var(--x) 50%, transparent)`。
2. **Tailwind 一律不用**（本次不用 CDN）。所有样式用 `assets/wiki.css` 里的类；确需微调时只能加内联 `style` 且值必须是 `var(--token)` 或纯几何（`width`/`gap`/`grid-template-columns`/`margin` 等）。
3. **禁止 `innerHTML`**（全仓 lint 规则）、禁止 `eval`、禁止 `document.write`。需要动态内容用 `textContent` / `createElement` / `classList`。
4. **零横向溢出**：360 / 390 / 414 / 768 / 1024 / 1280 / 1600 七个宽度 `document.documentElement.scrollWidth <= innerWidth`。表格必须包在 `.tbl-wrap` 里；`pre.code` 已自带横向滚动。
5. **禁止 `{{...}}` 双花括号**（会被 Pi 模板替换）。要写占位符用 `{x}` 或直接写具体值。
6. HTML 语法必须干净（标签成对闭合）。

## 3. 写作口吻（重要）

- **直抒胸臆**。不要营销腔，不要「赋能」「闭环」「抓手」。不要堆形容词。
- 每页开头一句 `.lead` 说清**这页解决什么问题**，然后直接进内容。
- **不要过度渲染**：不要给每段都加 emoji、不要每节都套彩色卡片、不要满屏高亮。**一页里彩色块 ≤ 3 处**。
- **优先可视化**：能用图表达的不要写三段话。图用 `div` + `.fig` / `.flow` / `.box` / `.bracket` / `.step` 组合画，**不要用 mermaid，不要用 SVG 内嵌大量路径**。
- 表格用于「并列对比」，流程图用于「顺序/分叉」。
- 中文为主；术语首次出现时给英文/代码原名。

## 4. 页面骨架（从 `_skeleton.html` 复制，只改标题、面包屑、正文、上下页、侧栏 active）

结构固定为：

```html
<div class="shell">
  <aside class="sidebar" id="sidebar"> ... 侧栏（所有页面共享同一份导航）... </aside>
  <div class="scrim" id="scrim"></div>
  <div class="main">
    <div class="topbar"><button id="menuBtn">…</button><span>标题</span></div>
    <main class="content [wide]">
      <div class="crumb">第 0X 页 · 分组名</div>
      <h1>标题</h1>
      <p class="lead">…</p>
      …正文…
      <nav class="pager">…上一页 / 下一页…</nav>
    </main>
  </div>
</div>
<script src="assets/wiki.js"></script>
```

侧栏清单见 `_skeleton.html`，**除 `active` 类之外不要修改侧栏**。

## 5. 页面清单与分工

| 文件 | 标题 | 这一页要讲什么 |
| --- | --- | --- |
| `index.html` | 总览 | 产品是什么、为什么不是 workflow 引擎、五层结构图、从哪读起 |
| `01-why.html` | 为什么 | 现有 agent 通知的断裂点、事件中心要解决的三个问题、我们不是什么 |
| `02-concepts.html` | 核心概念 | Event / Rule / Task / Node / Trace 五个名词 + 关系图 + 一次旅程 |
| `03-flow.html` | 一条事件的旅程 | 从 webhook 进来到手机震动的完整时序（可视化步骤图） |
| `04-rules.html` | 规则与扇出 | 规则长什么样、多条命中全部下发、接力（派生）、成环与 path 剪枝 |
| `05-nodes.html` | 节点与执行 | 命名命令注册表、凭据与标签、租约与 ack 四态、节点池、粘性路由、下线分级 |
| `06-trace.html` | 可观测与版本 | OTel 模型对齐、trace/span、版本钉住三对象、叠加图、为什么能解释「没触发」 |
| `07-safety.html` | 安全边界 | 中枢永不下发命令、execve argv、六道闸（G1/G2/G3/G4/G6）、三层可配 |
| `08-scope.html` | 本期范围 | 做什么 / 不做什么（收集、canvas、任意代码节点…）、预留的扩展点 |
| `09-glossary.html` | 术语表 | 全部名词 + 字段名速查 |

## 6. 设计结论清单（唯一事实源，不要自己发明别的）

### 6.1 五层结构

触发源 → 事件日志 → 规则 → 任务 → 节点。旁挂：通知（human sink）、trace（可观测）。

### 6.2 Sink 两类，永不混用

- **human sink**：推手机。尽力而为、可丢、不回报结果。
- **machine sink**：node agent。至少一次、租约、重试、DLQ、**必须回报**。

### 6.3 事件与规则

- 事件有 `source`（来源）、`type`、`payload`、`trace_id`、`span_id`。
- 规则 = `match`（条件）+ `action`（做什么）+ 可配 `max_hops` / `max_tasks` / `max_duration` / `daily_budget`。
- **多条规则同时命中 → 全部下发**（一个 PR 既派 agent 审查、也推手机通知）。这不是互斥选择。
- 命中 0 条 → 不派任务，但 trace 上记 `pruned` + 原因（可解释性）。

### 6.4 扇出 vs 收集（关键区分）

| | 扇出 | 收集（join） |
| --- | --- | --- |
| 回答 | 这个事件要引起几件事 | 那几件事都回来了吗 |
| 本期 | ✅ 无条件支持，N 条规则 → N 个任务 | ❌ **不做** |
| 难度 | 极低（一次 INSERT 多行） | 高（批次/计数/菱形/部分失败） |

**没有收集怎么实现「等一组都完成」**：放在节点内部。A 在自己进程内并行、自己等待、自己汇总，回报中枢一个 result。对中枢来说这仍是一个任务。代价：A 内部并行在 trace 上不可见。

### 6.5 接力与成环

- 任务结果可以产生新事件 → 新事件再命中规则 → 新任务。这就是「接力」。
- **允许成环**（审查 → 修复 → 复审 是真实场景）。
- 防死循环靠 `path`：`path` = **从根到本任务这条具体路径**的 rule_id 序列（不是全 trace 集合），`rule_id ∈ path` → **静默剪枝**，不报错不暂停，但**必须在 trace 上可见**。
- 保证终止的不是 `max_hops`，而是 path 去重（路径最长 = 规则总数）。

### 6.6 节点协议

- 节点本地 `~/.anotifyd/config.yaml` 定义 `commands[].argv`，**永不上传**。
- 中枢只发 `{kind, name, params}`；节点用 `execve(argv[])`，**禁止 `/bin/sh -c`**。
- 未注册命令 → `unknown_action`。
- 标签绑 `node_id`（凭据身份），**中枢赋值，不接受自报**。
- 任务状态机：`pending → assigned → acked → running → succeeded | failed | canceled | abandoned`（另有 `lease_expired` 中间态）。
- **ack = 接单确认，不是完成**。
- 租约 TTL 30s、心跳 10s、`max_attempts` 3、退避 10s → 60s → 300s、assign→ack 20s。
- **节点内部重试不消耗中枢 attempts**（`manifest.commands[].retries` 节点自定义）。
- 派生用 JWT：claim 含 `{trace, span, hop, maxHop, branchBudget, exp, jti}`；`jti` 一次性消费（`branch_nonces` 表 + 建任务 + 扣额度同一事务）→ 结构上封死跳数篡改与重放。
- `branchBudget` 由节点自主分配；额度用尽必须可见（`409 budget_exhausted` + span event + 控制台标注），绝不静默失败。

### 6.7 节点池与粘性路由

- 两级：`nodes`（身份/脑，`max_concurrency`、`max_instances`）与 `node_instances`（执行器/手，`concurrency`/`inflight`/`status`）。
- 三层并发上限取 min（实例申报 / 命令级 / 节点级）；`max_instances=20` 防 token 扩散。
- 派发按最少负载优先。
- **粘性路由**：`sticky_key = sha256(trace_id ∥ rule_id ∥ selector)`，首次查 `node_pins` 没有就按负载选并写入，之后一律查表。**rebalance 只处理失效 pin，不重排全局**。
- 节点下线分级（按 `manifest.commands[].idempotent`）：
  - `true`（只读）→ `abandoned`，不重派
  - `false`（**默认**，有副作用）→ `abandoned` + 告警 + 人工确认
  - `resumable: true` → 重派到新节点
- `abandoned` ≠ 成功，**默认通知**（和 `failed` 区别：不重试、必通知）。

### 6.8 任务生命周期属性（规则可声明）

`lifecycle.expiresInSec`（硬过期，到点 abandoned 绝不重派）/ `retryable`（false = 一次性，被 `idempotent:false` 强制为 false）/ `maxAttempts` / `resultTtlSec`（到期只清 result 不清行）。

### 6.9 版本钉住

- 三对象：`rule_versions`、`node_manifest_versions`、`registry_versions`（append-only，`content_hash UNIQUE`）。
- 标识 = `<prefix>_<UTC时间>_<content_hash前8>`，如 `reg_20261023T101500Z_a1b2c3d4`。
- **trace 钉住不可变版本 id（hash），不钉时间** —— 否则补录历史版本会让旧 trace 变形。
- 叠加图三层：亮实线 = 走过 / 灰虚线 = 配了没走 / 红虚框 = 剪枝。
- 节点不上报版本号，只上报 manifest 内容。

### 6.10 六道闸（G5 已废弃）

| 闸 | 内容 | 默认 | 硬顶 |
| --- | --- | --- | --- |
| G1 | `allow_chain`，不显式开启就不接力 | 关 | **不可配** |
| G2 | `max_hops` | 10 | 32 |
| G3 | 环检测 = `path` 里 rule_id 去重 | 隐含 | **不可配** |
| G4 | `max_tasks` | 100 | 1000 |
| G6 | `max_duration` | 24h | 7d |
| ~~G5~~ | ~~付费任务数~~ | 已废弃 | — |

三层可配：全局 env → 单规则 → 代码硬顶。硬顶只能改代码。

### 6.11 中枢永不下发命令（安全根本）

- DB 里**没有** argv/shell 字符串字段。
- 中枢不知道 node 上有什么命令，只知道「这个节点声明了哪些名字」。
- token 泄漏 = 继承该节点全部标签，这是必然代价（不做过度防御）。
- 只出站长连接（agent 出站），不开放入站端口。

### 6.12 产品定位（写文案时用）

**B「可解释的自动化：事件为什么触发、走了哪条路、卡在哪一步」**。执行能力是第二层卖点。

不能当卖点：①只出站长连接（行业默认）②开源自托管（入场券）。

5 条差异化（按强度排序）：①未触发的逐条件解释 ②长时任务无上限 ③凭据永不进中枢 ④成本可预测 ⑤内网/私有栈（弱）。

**不能变成 n8n 劣化版**：不做拖拽画布 / 任意代码节点 / 中枢存密钥 / 50 个集成 / DAG 校验。

### 6.13 本期不做

拖拽画布、任意代码节点、中枢存第三方密钥、大量第三方集成、DAG 校验、收集/join、`batching`、多租户 UI、人工审批节点、子流程复用。

## 7. 完成标准

每页交付前自查：

- 七个断点零横向溢出
- 无 Console 错误 / JS 报错
- 无硬编码色值（`grep -nE '#[0-9a-fA-F]{3,8}|rgba?\(|hsla?\(' ` 只应命中 `tokens.css`，页面内为零）
- 无 `innerHTML`
- 侧栏导航与 `_skeleton.html` 一致，仅当前页有 `active`
- 上下页链接正确
- 一页里彩色块（`.box.accent/.ok/.err/.warn`、`.note`）总数 ≤ 3
