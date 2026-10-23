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
| `manifest.commands[]` | `manifest.capabilities[]` |
| `rule_id` | 能力名 |
| `rule_versions` / `rty_` 前缀 | `graph_versions` / `grf_` 前缀 |
| `unknown_action` | `unknown_capability` |

**例外**：`04-edges.html` 和 `07-safety.html` 里有几处刻意保留的「旧模型 vs 新模型」对照说明，那些是对的，别删。

## 6. 现行模型速记

写任何页面时的事实依据：

**三个要素**——节点 `node`（参与流转的实体）、能力 `capability`（我能做什么，路由单位）、出口 `output`（命名出口，跑完才选）。

**三种节点，按端口形状分**——`source`（只有出，触发器）、`worker`（有进有出）、`sink`（只有入，终端）。没有第四种。

**路由**——

```sql
SELECT to_capability FROM edges
 WHERE from_capability = :cap AND from_port = :port;
```

返回 N 行派 N 个任务，0 行就到头。没有条件求值、没有优先级、没有 DSL。

**出口选择**——节点在 `result` 里回报 `{status, out}`。`out` 与 `status` 正交：失败也能继续，成功也能到头。

**收敛**——三个条件缺一不可：① 无在跑任务 ② 无未到期任务 ③ 每个终态任务都已回报 `out`。且不能首次扫描成立就发通知（要 `pending_settle` 复核），否则 trigger 事务还没提交手机就响了。

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

# 4. 侧栏 active 每页恰好 1
for f in design/wiki/*.html; do echo "$f $(grep -c 'class="active"' $f)"; done
```

再跑一遍七断点无溢出 + 0 console error（用 playwright，页面需在仓库根目录跑 node，否则 `playwright-core` 解析不到）。

## 8. 已知陷阱

- `.defs` 的 `dd` 很窄，任何 `white-space: nowrap` 的行内 `code` 会撑破布局。长片段用 `pre.code`，不要塞进 `.defs`。
- 合并分支前若工作区有同名未跟踪文件，`git merge` 会 Aborting，需先 `git clean -fd design/wiki`（先备份）。
- `sed` 连续替换勿用 `&&` 串联——`grep -c` 返回 0 会短路整条命令。
- 侧栏是十个页面各存一份的，改导航要**十处同改**。
