# Anotify UI 审计 v3（2026-11-13）

> 范围：`web-src/` 下全部 19 页 × 亮/暗 × 桌面 1440 / 移动 390 + 4 个弹层 + 4 个详情页（带参）。
> 方法：`make sitegen` 后用 `fe-preview`（mock 数据，`http://localhost:7788`）逐页跑无头 Chrome，
> 全页截图 + DOM 机械审计（溢出/裁切/异常文本/破图/字号圆角统计/动画覆盖）+ 看图人工判断。
> 证据：截图 `/tmp/ui-scan/shots/`（90 张，命名 `<page>--<theme>--<viewport>.png`）、
> DOM 报告 `/tmp/ui-scan/report.json`、局部放大 `/tmp/ui-scan/crops/`。
>
> 设计基线是 `design/ui-v2/REVIEW.md` §4 的 D1–D20。本清单**不改契约**，只收「实现没跟上基线」
> 与「基线没覆盖到的新页面/新状态」。

## 0. 一页结论

骨架（侧栏/顶栏/卡片/按钮/弹层动画）已经在 v2 立住了，现在剩下的问题集中在四类：

1. **会让人以为坏了的可见缺陷**：流程图右侧节点被裁且无可视滚动、KPI 迷你折线贴边裁切、
   弹层内容超长但没有任何滚动提示、`undefined` 出现在 KPI 副标题、`default ·` 这种悬挂分隔符、
   错误态只有一行裸灰字。
2. **同一件事在不同页面长三个样**：状态（badge / 纯文本 / 彩色小字）、弹层头部与底部、
   空态、原生 `<input type=file>`、裸 `<pre>` vs `.code-block`。
3. **动效基建齐了但几乎没用**：`ui.css` 里有 `stagger-in / stat-value-in / skeleton-shimmer`，
   而 6 个页面 animation 元素数 = 0，列表/表格/KPI 更新全是硬跳。
4. **开发者旁白直接上屏**：`nodes/sources/docs/playbook` 里成段的 12px 灰字说明、
   `retention.resultTtlSec` 这种 JSON key 当 label、`fixer@1 · tsk_mock_1` 混排。

## 1. 机械审计事实（先看这些定量结论，避免逐条主观争论）

| 指标 | 实测 | 说明 |
|---|---|---|
| 无任何 animation 的页面 | `events / playbooks / nodes / docs / trace / message`（6/16） | 其余页最多 1–4 个（多来自 toast/主题） |
| `.anim-stagger` 引用次数 | **0**（ui.css 定义了 `stagger-in` 与 8 档 delay） | 列表入场动画从一开始就没接上 |
| `.stat-value` 数值动画引用 | **0** | KPI 从骨架数字直接跳到新值 |
| 有 `.skeleton` 的页面 | `admin`、`capabilities` | 其余页首屏是白/空 |
| 原生 `<input type=file>` | `playbooks`×2、`playbook`×1 | 英文 "Choose Files / No file chosen" |
| `.flograph-canvas` 内容宽 vs 容器 | 1916px / 1068–1110px | 图被裁，容器有 `overflow-x:auto` 但无任何可见提示 |
| `.flograph-legend` 尺寸 | 1×1px（内容 232px，`overflow:hidden`） | 图例容器塌陷 |
| 裸 `<pre>`（无 `.code-block` 外壳） | `trace`×3、`playbook`×1、`node`×1 | 与 keys/docs 的代码块两套语言 |
| `code-body` 横向溢出 | `docs` 48px 裁切 | 代码块没有横向滚动样式 |
| 交互元素无 transition | `docs` 145 个、`keys/admin` 各 8 个、`index` 7 个 | 主要指表格行/文字链 |
| 裸状态文本（ok/true/disabled/succeeded…） | `sources` 8、`events` 4、`index` 4 | 该用 `.badge` 的地方是纯 mono 文本 |
| 页面 H1 与顶栏上下文标题同文 | 16 页里 15 页（除首页） | 视觉冗余（D2 允许，但同文=噪声） |
| `--dur-*` token 一致性 | 已统一 | 无需改 |

## 2. 问题清单

优先级：**P0** 可见破相/读不了；**P1** 一致性（同一件事多种样子）；**P2** 打磨。

### 2.1 外壳（侧栏 / 顶栏 / 品牌 / 导航）

| ID | P | 现象 | 证据 |
|---|---|---|---|
| SH-1 | P0 | 品牌标：黑底圆角块里的 cursive "An" 用**暗灰**（暗色下几乎看不见），右上角一颗**无解释的红点**贴边，且旁边再写一遍 cursive "Anotify" → 三个元素互相打架 | `crops/brand.png` |
| SH-2 | P1 | 亮色主题下品牌块仍是近黑实心方块，在浅色页头里像一块墨渍 | `nodes--light--desktop.png` |
| SH-3 | P1 | 侧栏图标语义不匹配且笔画不统一：能力=六边形+对勾（像认证徽章）、预案=难以辨识的笔画、节点=像信封的方块、源=闪电；描边粗细与视觉重心不一致 | `crops/nav.png` |
| SH-4 | P1 | 顶栏上下文标题与页面 H1 **完全同文**（安全与登录 / API Keys / 能力 / 事件流…），一眼看是重复标题 | 各页 `--desktop.png` 顶部 |
| SH-5 | P2 | 移动端顶栏：汉堡 + 标题 + 主题 + 铃 + 头像 5 个元素挤在 390 宽 | `capabilities--dark--mobile.png` |
| SH-6 | P2 | 侧栏底部用户区（`.app-sidebar-foot`，高 102px，bottom=890）在 900 高视口里只剩 10px，任何底部浮层都会压住它 | 实测 |

### 2.2 组件语言（同一件事多种样子）

| ID | P | 现象 | 证据 |
|---|---|---|---|
| CP-1 | P0 | **弹层三套头尾**：capabilities（标题+×，底部 取消/创建）、sources（标题+"关闭"文字按钮，底部**没有取消**只有 草稿试运行/保存）、keys（标题+×，取消/生成）、security（图标+标题+副标题+×） | `capabilities-modal--dark.png`、`sources-modal--dark.png`、`keys-modal--dark.png`、`security-enroll--dark.png` |
| CP-2 | P0 | 弹层内容超长无提示：capabilities 弹层 `.modal-body` scrollHeight **1327** / clientHeight **641**；sources **1057/637**。没有滚动条样式、没有底部渐隐，"目标能力 targets" 直接被切在可视区外 | 实测 |
| CP-3 | P1 | 状态词汇三套：`.badge`（能力页）、纯 mono 文本 `ok/true/disabled`（sources/node）、彩色小字（trace 任务行） | `sources--dark--desktop.png`、`trace-t1--dark.png` |
| CP-4 | P1 | 空态三套：`.empty`（capabilities/security）、一行居中灰字（playbooks/source/message/trace 的错误态）、空卡片（receivers） | 各页 |
| CP-5 | P1 | 代码块两套：`.code-block`（有头有复制）vs 裸 `<pre class="code-body">`（trace×3、playbook、node 安装命令） | `trace-t1--dark.png` |
| CP-6 | P1 | 原生 `<input type=file>` 3 处未样式化，英文 "Choose Files / No file chosen" | `playbooks--dark--desktop.png`、`playbook-detail--dark.png` |
| CP-7 | P1 | 表格 `/admin`、`/keys` 无粘顶表头、行无 hover 过渡；docs 端点表 145 个可交互元素无 transition | 实测 |
| CP-8 | P2 | 卡片层级：`.stat` 迷你折线贴到卡片圆角处被裁（见 HM-1） | `crops/statcard.png` |

### 2.3 首页 `/index`

| ID | P | 现象 |
|---|---|---|
| IDX-1 | P0 | KPI 卡 1/2 的 sparkline 画到卡片底边、被圆角裁掉；卡 3/4 没有图只有一个彩色圆点 → 四张卡两种形态（`crops/statcard.png`） |
| IDX-2 | P1 | 热力图：行标签只有「一/三/五」，空格用 `--surface-2` 在暗色下几乎不可辨；图例「少→多」9px |
| IDX-3 | P0 | 「最新事件链路」图内容 1916px 放进 1068px 卡片：右侧节点被裁、**没有滚动条/渐隐/拖拽提示**；`.flograph-legend` 塌陷成 1×1px |
| IDX-4 | P1 | 图里「未到达」节点靠 opacity 压到不可读（暗色对比≈1.5:1），应改虚线/降饱和但保文字可读 |
| IDX-5 | P1 | 事件流行：时间右对齐、trace id 裸 mono、类型 chip 与 severity chip 风格不同 |
| IDX-6 | P2 | 「事件活动」卡内网格上下留白不均，标题区与网格间距 28px/网格下方 20px |

### 2.4 事件流 `/events`

| ID | P | 现象 |
|---|---|---|
| EV-1 | P1 | 每条事件一张 `.card`（5 张卡=4 条事件），卡内 2 列字段 + 1 个右对齐数字，页面极长、信息密度低 |
| EV-2 | P1 | 卡内 label 11px 灰 / value 15px mono；「已派发任务数 0」用不同字重显得错位 |
| EV-3 | P2 | 筛选栏 4 个控件固定 `w-48`，1440 下右侧大片空白；「应用筛选」是唯一主按钮但离控件很远 |

### 2.5 能力 `/capabilities`

| ID | P | 现象 |
|---|---|---|
| CAP-1 | P1 | 每行右侧 4 个 badge 宽度随文案变化 → **各行不对齐**，形成锯齿；badge 内 `✓/✗` 增加噪声（`crops/cap-row.png`） |
| CAP-2 | P1 | 搜索框只有 placeholder 无 label；「8 个能力」孤零零挂在筛选行最右、与 select 基线不齐 |
| CAP-3 | P1 | 移动端：badge 行换行后「详情」按钮掉到下一行右侧，卡片被拉高到 200px+ |
| CAP-4 | P2 | 详情/编辑抽屉（`#cap-modal`）超长无提示（同 CP-2） |

### 2.6 预案 `/playbooks` `/playbook`

| ID | P | 现象 |
|---|---|---|
| PB-1 | P0 | 空态只有一行居中灰字「这个命名空间里还没有预案。」，没有图标、没有 CTA；上方还留着 200px 空白 |
| PB-2 | P1 | 有卡片时 `.pb-grid` 最小 320px → 单张卡只占 1/3 宽，右侧整片空白 |
| PB-3 | P1 | 卡片正文把 4 个 condition 平铺成 4 行 `Implemented · False · NotInstalled`（英文 key + 重复信息），没有 badge/分组 |
| PB-4 | P1 | 详情页：9 个 `.card`，每张卡前都有 1–2 行灰字说明；中英混排（conditions/items/blocker/PUT/dryRun） |
| PB-5 | P1 | 详情页裸 `<pre>` JSON（体检统计）+ 原生 file input + 一行 3 个宽度不齐的按钮 |
| PB-6 | P2 | 详情页按钮文案过长：「渲染（纯预览）/ 试装（冲突检查，零写入）/ 安装（只新增）」 |

### 2.7 节点 `/nodes` `/node`

| ID | P | 现象 |
|---|---|---|
| ND-1 | P1 | 节点行状态「在线」是纯灰字（不是 badge）；`configRevision=7 · appliedRevision=7` 连写；未知值以 `?` 结尾看起来像 bug |
| ND-2 | P1 | 每行右侧都挂一个「轮换身份令牌」主按钮（5 行 5 个），主次不分 |
| ND-3 | P1 | 列表下方一整段 12px 灰字开发者旁白（「有效 slot 上的 busy=false 是空闲…」）占据整行 |
| ND-4 | P1 | 「系统保留配置」用裸 JSON key 当 label（`retention.resultTtlSec`）、值是裸数字无单位 |
| ND-5 | P2 | 详情页顶部把「在线 · anode-1 · linux/amd64 生效授权…」挤成一行；左右两栏高度差极大；安装命令代码行被裁 |

### 2.8 安全与登录 `/security`

| ID | P | 现象 |
|---|---|---|
| SEC-1 | P1 | Passkey 空态：160px 高的空卡片 + 一行字 + 居中按钮，没有图标（`.empty` 的图标位没用上） |
| SEC-2 | P1 | 会话行「Chrome on Linux / 当前会话」+ 右侧 chip「当前会话」**同文重复**；行尾一个孤立的「—」 |
| SEC-3 | P1 | 恢复码卡用 ⚠️ emoji 当图标，其他页全是 SVG 图标 |
| SEC-4 | P2 | 「密码登录 / 已设置」卡与「登录会话」卡行高、字号、动作位置不一致 |

### 2.9 源 `/sources` `/source`

| ID | P | 现象 |
|---|---|---|
| SRC-1 | P0 | 每条源 8 个字段 + 5 个按钮全塞在一个无分组的块里：按钮行**夹在两组字段之间**，破坏 label/value 对齐（`sources--dark--desktop.png`） |
| SRC-2 | P1 | `webhook · 已启用`、`ok`、`true`、`disabled` 全是 mono 纯文本；下面「已停用的入口」区块又把状态重复一遍 |
| SRC-3 | P1 | 顶部一条通栏灰字说明（deliveryId 去重规则）读起来像遗留调试文案 |
| SRC-4 | P1 | 移动端单条源 ~700px 高（8 个字段一列堆叠 + 按钮换两行） |
| SRC-5 | P1 | 详情页无参打开时是裸居中灰字 + 裸「重试」 |
| SRC-6 | P1 | 新建源弹层：没有取消按钮、字段 14 个不分段、`targets` 被切（CP-1/CP-2） |

### 2.10 接收 `/receivers`

| ID | P | 现象 |
|---|---|---|
| RCV-1 | P1 | page-head 与顶栏标题同文重复（同 SH-4），且页头下方立刻是 tab 条，视觉上像没有页头（v2 REVIEW 的「唯一没有页头」已不再成立，已核实 `.page-head` 存在） |
| RCV-2 | P0 | 空态卡只有一行文字；「新增设备」按钮真实存在但被 `.hidden` 祖先包住（rect 0×0）→ 空态没有任何下一步动作（违反 D16） |
| RCV-3 | P2 | tab 条「推送订阅 0 / WebSocket 长连接 0」用大号 pill 占满整行，与 admin 的 `.tabs` 风格不同 |

### 2.11 管理后台 `/admin`

| ID | P | 现象 |
|---|---|---|
| ADM-1 | P0 | KPI 副标题渲染出字面 **`undefined`**：「1 超管 · undefined 7天活跃」（`web/mock.js` 的 `/v1/admin/stats` 缺 `activeUserCount`，而 openapi 契约有该字段） |
| ADM-2 | P1 | 4 张卡右上角都有同样的「全部署」灰 pill，像 badge 但其实像筛选器，无 chevron、无点击反馈 |
| ADM-3 | P1 | 表格下方整页空白（1440×900 里 300px 什么也没有） |
| ADM-4 | P1 | 角色列 `member` 未本地化；行操作「禁用/提为超管」是文字链，与其他页按钮不一致 |

### 2.12 文档 `/docs`

| ID | P | 现象 |
|---|---|---|
| DOC-1 | P0 | 代码块横向溢出被裁：`pre.code-body` scrollWidth 930 / clientWidth 882（1440 宽下），移动端多个块 817→348；没有横向滚动样式，看不到被裁的是哪一半 |
| DOC-2 | P1 | 端点表大量 10px/11px 字号（全页最密处），密到不可读；145 个可交互元素 0 过渡 |
| DOC-3 | P2 | 页头首屏只有「接入 Agent + 一次 curl」，右侧目录条与正文层级接近，长页里没有「回到顶部」 |

### 2.13 链路 `/trace` / 任务 `/message`

| ID | P | 现象 |
|---|---|---|
| TR-1 | P0 | 顶部图同样被裁（1916/1110），未到达节点不可读（同 IDX-3/IDX-4） |
| TR-2 | P1 | 任务列表 7 行：状态是彩色小字（成功/失败/等待）无 badge；`fixer@1 · tsk_mock_1` 把 @序号与 id 混排 |
| TR-3 | P1 | 事件事实的 JSON 是裸 `<pre class="code-body">`，无折叠/复制/语法色 |
| TR-4 | P1 | 无参打开时错误态是裸灰字 + 裸「重试」；正常态标题 `default · trace_mock_t1` 正常，但空参时出现 `default ·` 悬挂分隔符 |
| MSG-1 | P1 | 字段网格 2 列且一半是 `—`（waitingReason/nextAttemptAt/leaseExpiresAt…），像未填完的表单 |
| MSG-2 | P2 | 无参打开时 `<h1>` 位置出现 `tsk_mock_1` 之类的兜底标题 |

### 2.14 认证 `/login` `/cli-auth` `/passkey-enroll`

| ID | P | 现象 |
|---|---|---|
| AU-1 | P1 | 登录卡里 5 个动作同权（登录/注册 tab、Passkey、密码、恢复码），主次只靠 Violet 区分 |
| AU-2 | P1 | `cli-auth` / `passkey-enroll` 输入框 **双描边**（border + focus ring 同时画），看起来脏 |
| AU-3 | P2 | 卡片下方说明行尾悬挂「·」（`● 授权在你的服务器上生效 · 短码 10 分钟过期 ·`） |
| AU-4 | P2 | 两页是同一模板，卡片内边距/圆角略有差异 |

### 2.15 动效（用户明确要求）

| ID | P | 现象 |
|---|---|---|
| AN-1 | P1 | 列表/表格渲染无 stagger（`.anim-stagger` 零引用）；`events`/`capabilities`/`keys`/`sources`/`nodes`/`playbooks` 都是整体硬出现 |
| AN-2 | P1 | KPI 数值更新无过渡（`.stat-value` 动画零引用） |
| AN-3 | P1 | 首屏骨架只有 `admin`/`capabilities` 有；`keys`/`events`/`sources`/`nodes`/`playbooks` 首屏是空白（`.skeleton` 未用） |
| AN-4 | P1 | 表格行/列表行无 hover 背景过渡（`transition-duration:0s`） |
| AN-5 | P2 | 图「回放」时节点从 pending→done 是硬切（opacity 瞬变），没有 160ms 过渡 |
| AN-6 | P2 | 弹层进出已有动画 ✅（不列为缺陷，改造时不要回退） |

## 3. 分派（4 个并行集群，文件域互不重叠）

| 集群 | 负责文件 | 负责 ID |
|---|---|---|
| **A 壳与组件基建** | `web/ui.css`、`web/tokens.css`、`web/partials.js`、`web-src/layouts/*`、`web/flograph.js`、`web/graphs.js`、`web/traceview.js` | SH-1…SH-6、CP-2(样式部分)、CP-7(样式部分)、IDX-3/IDX-4、AN-1(工具类)、AN-2、AN-4、AN-5 |
| **C 工作台页** | `web-src/pages/{index,events,capabilities,playbooks,playbook}.html` | IDX-1/2/5/6、EV-1…3、CAP-1…4、PB-1…6、AN-1(应用)、AN-2(应用)、AN-3 |
| **D 资源与身份页** | `web-src/pages/{nodes,node,keys,sources,source,receivers,security,admin}.html`、`web/mock.js`(仅 admin stats) | ND-1…5、SEC-1…4、SRC-1…6、RCV-1…3、ADM-1…4、CP-1/CP-3/CP-4/CP-5/CP-6(本人页面内落地) |
| **E 文档与认证页** | `web-src/pages/{docs,trace,message,login,cli-auth,passkey-enroll}.html` | DOC-1…3、TR-1(应用 A 的图修复)、TR-2…4、MSG-1/2、AU-1…4、CP-1/CP-4/CP-5(本人页面内落地) |

约束（所有集群）：

1. **不改契约**：`id=`、`class=` 名、`data-*`、DOM 结构里被 e2e 依赖的部分只增不改；新增 id 在报告里列出。
2. **不改 Go 代码**；`web/mock.js` 只允许 D 集群补 `activeUserCount` 一行。
3. **i18n**：能复用现有 key 就复用。必须新增文案 key 时，四份 `web-src/locales/*.yaml` 都要加，统一追加在文件末尾的 `uxaudit.<cluster>.` 命名空间下，并在报告里列出（合并冲突由协调者解决）。
4. **类的来源纪律**：新 class 必须落在 `ui.css` / 页面内联 `<style>` / Tailwind 工具类三者之内，`make check-classes` 必须绿。
5. **动效**：只用 `--dur-fast/mid/slow` + `--ease/--ease-out`；只动 `opacity/transform/box-shadow/background-color/border-color`；`prefers-reduced-motion` 下必须归零（ui.css 已有全局兜底，不要绕开）。
6. **对比度**：正文 ≥ 4.5:1，次要信息 ≥ 3:1，亮/暗两套都要看。

验证配方（每个集群自己在 worktree 里跑）：

```bash
make sitegen && make check-classes          # 死类守卫
node scripts/check-tailwind-css.mjs         # 工具类覆盖
go run ./cmd/fe-preview -dir web -addr 127.0.0.1:<PORT> &   # <PORT> 见任务书
# 然后用 playwright-core 截图（chromium: ~/.cache/ms-playwright/chromium-1187/chrome-linux/chrome）
# 亮/暗 × 1440/390，逐张看图，改前改后都要看
```
