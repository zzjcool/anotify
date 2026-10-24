# Anotify Wiki · 维护约定

> 这份文件是**维护 wiki 时**的约定，不是设计事实源。
> 设计事实源是 `design/wiki/` 下的十个页面本身。
> 本文件只回答两件事：**改页面时要守哪些硬约束**，以及**哪些说法已经废弃、不要再写回去**。

## 1. 文件结构

- 页面：`design/wiki/*.html`，十个页面，文件名即页面编号
- 共享样式：`design/wiki/assets/wiki.css`（**别改**，所有页面共用）
- 共享脚本：`design/wiki/assets/wiki.js`（**别改**，只做移动端侧栏开合）
- 色彩来源：`design/tokens.css`（唯一）
- 页面骨架模板：`design/wiki/_meta/_skeleton.html`

## 2. 硬性技术约束

1. **颜色只能走 `var(--token)`**。页面内禁止任何硬编码色值：`#hex`、`rgb()`、`rgba()`、`hsl()`、命名色都不行。需要透明度用 `color-mix(in srgb, var(--x) 50%, transparent)`。
2. **不用 Tailwind**。所有样式来自 `assets/wiki.css`；确需微调时只能内联 `style`，且值必须是 `var(--token)` 或纯几何（`width`/`gap`/`margin`/`grid-template-columns`）。
3. **禁止 `innerHTML` / `eval` / `document.write`**。动态内容用 `textContent` / `createElement` / `classList`。
4. **零横向溢出**：360 / 390 / 414 / 768 / 1024 / 1280 / 1600 七个宽度，`document.documentElement.scrollWidth <= innerWidth + 1`。表格必须包 `.tbl-wrap`；`pre.code` 自带横向滚动。
5. **禁止 `{{...}}`**（会被 Pi 模板替换）。占位符用 `{x}`。
6. HTML 标签必须成对闭合。
7. 侧栏每页只给当前页加 `active`。

## 3. 写作口吻

- 直抒胸臆。不要营销腔（赋能 / 闭环 / 抓手），不要堆形容词。
- 每页开头一句 `.lead` 说清这页解决什么问题，然后直接进内容。
- **一页彩色块 ≤ 3 处**（`.box.accent/.ok/.err/.warn`、`.note`）。不要每节都套卡片。
- 优先可视化：能用图表达的不要写三段话。图用 `div` + `.fig` / `.flow` / `.box` / `.bracket` / `.step` 组合，**不引入 mermaid、不内嵌大量 SVG 路径**。
- 表格用于并列对比，流程图用于顺序/分叉。

## 4. 页面清单

| 文件 | 标题 | 讲什么 |
| --- | --- | --- |
| `index.html` | 总览 | 产品是什么、为什么不是 workflow 引擎、五层结构、从哪读起 |
| `01-why.html` | 为什么 | 三个断裂点、我们不是什么 |
| `02-concepts.html` | 核心概念 | Node / Capability / Output / Task / Trace 五个名词、三类节点、一条链怎么连起来 |
| `03-flow.html` | 一条事件的旅程 | 十步时序，从 webhook 到手机震动 |
| `04-edges.html` | 能力与出口 | 三个要素、三种节点、edges 表、三方声明、出口选择、可解释性、环 |
| `05-runners.html` | 节点与执行 | 三类节点、能力注册表、凭据与标签、状态机与租约、节点池、粘性路由、下线分级、生命周期 |
| `06-trace.html` | 可观测与版本 | OTel 对齐、trace/span、为什么没触发、版本钉住 |
| `07-safety.html` | 安全边界 | 中枢永不下发命令、execve argv、六道闸、删掉规则引擎的安全收益 |
| `08-scope.html` | 本期范围 | 做什么、不做什么、预留的扩展点 |
| `09-glossary.html` | 术语表 | 名词 + 前缀速查 |

## 5. 已废弃的说法（不要再写回去）

模型在 2026-10 做过一次大改：**删掉规则引擎，改为显式出口表**。下面这些词属于旧模型，出现在新页面里就是 bug：

| 废弃 | 现在应该写 |
| --- | --- |
| 规则 `rule`、`rules` 表 | 出口表 `edges` |
| `match` 条件表达式 | 无（路由靠查表，没有条件求值） |
| `priority` / 优先级择一 | 无（查到的 N 行全部下发） |
| `action` | 能力 `capability` + 出口 `output` |
| `terminal`（布尔字段） | `result.out`（出口名或 `null`） |
| `manifest.commands[]` | `manifest.implements[]` |
| `manifest.capabilities[].outputs` | 删掉：出口由接口定义，执行器无命名权 |
| `role` / 角色 | `node` / 节点（链上的位置；机器叫 runner） |
| `unknown_action` / `unknown_capability` | `unknown_interface` |
| （无接口层） | **接口 `interface` 先定义**，执行器只能 implements |
| `rule_id` | 接口名 `name@version` |
| `rule_versions` / `rty_` 前缀 | `graph_versions` / `grf_` 前缀 |
| `unknown_action` | `unknown_capability` |

**例外**：`04-edges.html` 和 `07-safety.html` 里有几处刻意保留的「旧模型 vs 新模型」对照说明，那些是对的，别删。

## 6. 现行模型速记

写任何页面时的事实依据：

**四层**——接口 `interface`（契约：name@version + kind + params_schema + outputs）→ 能力 `capability`（某台执行器对某接口的实现，= `(runner_id, interface_id)`）→ 执行器 `runner`（一台机器）→ 图 `edges`（只引用接口，不引用机器）。

**关键**：出口名只能来自接口定义，执行器没有命名权；不允许只实现一部分出口；`review@1` 与 `review@2` 是两个不同接口，并存。

**三种节点，按端口形状分**——`origin`（只有出，触发器）、`pipe`（有进有出）、`end`（只有入，终点）。没有第四种。注意「节点」是**链上的位置**，不是机器；机器叫执行器 `runner`。

**路由**——

派发四步：

```sql
-- ① 查表（两端都是接口）
SELECT to_interface FROM edges
 WHERE from_interface = :ifc AND from_port = :out;
-- 返回 N 行 → N 个下游；0 行 → 到头，记 pruned
-- ② 用目标接口的 params_schema 校验参数（不通过不建任务）
-- ③ SELECT runner_id FROM capabilities WHERE interface_id = :to_interface
-- ④ 按负载挑一台（或按 sticky 查 runner_pins）
```

没有条件求值、没有优先级、没有 DSL。

**出口选择**——节点在 `result` 里回报 `{status, out}`。`out` 与 `status` 正交：失败也能继续，成功也能到头。

**没有后台收敛判定**——中枢不维护「整条链是否结束」这个状态，它只在你查看事件流向时才有意义。通知不依赖收敛，它本身就是图里的一条边（→ `notify.phone`）。

**环**——允许。图显式声明，环在配置期可见；运行期靠 `path` 能力名去重剪枝，剪枝静默但必须在 trace 上可见。

**本期不做**——收集/join、`batching`、条件路由、动态边、拖拽画布、任意代码节点、中枢存密钥、多租户 UI。

## 7. 交付前自查

```bash
cd <repo>

# 1. 硬编码色值（应输出 0 行）
grep -nE '#[0-9a-fA-F]{3,8}|rgba?\(|hsla?\(' design/wiki/*.html

# 2. innerHTML（应输出 0 行）
grep -n 'innerHTML' design/wiki/*.html

# 3. 旧术语（应只剩 04/07 的对照片段）
grep -n '规则\|match\|priority\|rule_id\|unknown_action\|manifest\.commands' design/wiki/*.html

# 3b. 接口层的旧写法（应为 0）
grep -n 'from_capability\|to_capability\|manifest\.capabilities\[\]\.outputs' design/wiki/*.html

# 4. 侧栏 active 每页恰好 1
for f in design/wiki/*.html; do echo "$f $(grep -c 'class="active"' $f)"; done
```

再跑一遍七断点无溢出 + 0 console error（用 playwright，页面需在仓库根目录跑 node，否则 `playwright-core` 解析不到）。

## 8. 已知陷阱

- `.defs` 的 `dd` 很窄，任何 `white-space: nowrap` 的行内 `code` 会撑破布局。长片段用 `pre.code`，不要塞进 `.defs`。
- 合并分支前若工作区有同名未跟踪文件，`git merge` 会 Aborting，需先 `git clean -fd design/wiki`（先备份）。
- `sed` 连续替换勿用 `&&` 串联——`grep -c` 返回 0 会短路整条命令。
- 侧栏是十个页面各存一份的，改导航要**十处同改**。
