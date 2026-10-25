# 前端 mock UI 迭代清单

> 对应 epic **#87**。本阶段所有前端特性都先在 **mock 环境**（`make mock`）做出来、
> 看到效果，再决定哪些要落真后端。
>
> 开新会话时先读这个文件 + `gh issue list`。

## 环境

```bash
make mock        # 本地 + trycloudflare，:7788
make mock-local  # 只本机
```

改 `web/mock.js` 刷新浏览器即生效（不用重启）。
页面源文件在 `web-src/pages/*.html`，改完要 `make fe`（或重启 `make mock`）才进 `web/`。

## 状态

| # | 问题 | 状态 |
|---|---|---|
| [#88](https://github.com/zzjcool/anotify/issues/88) | 源的下游流向图要用成熟图库做完整工作流图 | 待做 |
| [#89](https://github.com/zzjcool/anotify/issues/89) | 事件流按源（触发器）分组展示最近事件 | 待做 |
| [#90](https://github.com/zzjcool/anotify/issues/90) | 任务链用图展示（当前是缩进树） | 待做 |
| [#91](https://github.com/zzjcool/anotify/issues/91) | 「通知接收」不该是独立一级页面——它只是 notify.phone 这个 end 能力的实现 | 待做 |
| [#92](https://github.com/zzjcool/anotify/issues/92) | 定义能力的 paramsSchema 支持可视化编辑（可与 JSON 模式切换） | 待做 |
| [#93](https://github.com/zzjcool/anotify/issues/93) | 「出口表」拆成独立页面（能力和边是两个不同的东西） | 待做 |
| [#94](https://github.com/zzjcool/anotify/issues/94) | 节点页显示能力详情 + 事件流按节点过滤 + 节点跳转到事件流 | 待做 |
| [#95](https://github.com/zzjcool/anotify/issues/95) | 事件流用 trace/span 瀑布图展示（时间维度） | 待做 |
| — | 等待用户逐条补充 | — |

### 已完成（本阶段）

| 问题 | 修复 |
|---|---|
| 新建源弹窗背景透明 | `.card` 的 `--surface-1` 是 `rgba(…,0.02)`，弹窗直接复用导致背后列表透出来。改用 `--panel-modal`（不透明 `#0b0b12`），同步修了 capabilities.html 两处 |
| 节点页能力标签全渲染成 `[object Object]` | `el("span","out-chip", c)` 里 c 是 `{id,name,version}` 对象，`el()` 内部 `String(c)`。改成拼 `name@version`，并兼容字符串形式 |
| 出口能力列表混入 `kind=origin` 的能力 | 过滤条件原本是 `kind==="origin" && source==="builtin"`，漏掉了「无节点声明 → 推导为 origin」的自定义能力。改成只看 kind |

## 已发现问题（细节）

### #88 流向图 → 用成熟库做完整工作流图

**需求已明确**：用户要完整的工作流图，**用成熟好看的库**，不自研布局。

引入方式受项目硬约束（纯静态、无构建框架）：
- 单文件放 `web/vendor/`，`<script src="/vendor/xxx.min.js" defer>`
- 先例：`web/vendor/marked.min.js`（35KB）
- 不引 npm 构建；版本 pin 死 + 记录来源与许可证
- **不能依赖 CDN**：e2e 离线门禁会拦所有外网请求

候选：Xyflow / Cytoscape.js+dagre / dagre+自绘 / Mermaid / D3。
倾向 Xyflow 或 Cytoscape.js，等确认图的真实形状后定。

#### 原始现状（待替换）

现状是缩进树：

```
analyze-pr@1
  ↓ approved  deploy-preview@1
      ↓ failed  apply-fix@1
          ↓ fixed  deploy-preview@1  ↩ 回到 deploy-preview（环路）
      ↓ preview.ready  （未接下游）
  ↓ needs-fix  apply-fix@1  （已在上方展开）
```

问题：
1. 不像图，只是一列缩进文本
2. 主干与旁支视觉权重一样
3. 环路只有一行灰字，扫一眼发现不了
4. 节点没有身份（kind / 是否有节点声明 / 是否在线）
5. 长链路越读越吃力

期望：方块节点 + 有向连线 + 出口标注 + 明显环路标识 + 缩放拖拽。

### #89 事件流按源分组 + 按源筛选 + 源页跳转（三条需求）

1. **分组**：每个源一个组，组头 = 源名 + 类型 + 最近触发 + 事件数
2. **筛选**：`events.html?source=<id>`，URL 可分享
3. **跳转**：源卡片上「查看事件」→ 带 source 参数跳过来

关联方式**已核实**：`Trigger.Fire` 把 `event.source` 写成源名
（`internal/source/source.go:99`），源名 org 内 UNIQUE，所以前端用名字匹配可行。
外部直接投 `/v1/events` 的会成为「未归类」组 —— 不能丢。

长期应让事件带 `source_id`（要改表，不在本 epic）。

mock 数据需要改：现在事件的 `source` 是 `github`/`cron` 这类原始值，不是源名，
要改成源名才能演示分组效果；另造几条外部事件当「未归类」。

### #90 任务链图化

与 #88 同类但不同数据：

| | #88 源流向 | #90 任务链 |
|---|---|---|
| 数据 | 能力定义+边（静态） | 实际任务（运行态） |
| 形状 | 可能含环 | 树（parentTaskId，无环） |
| 语义 | 可能怎么走 | 实际怎么走的 |

### #91 通知接收不该是独立页面

`receivers.html` 现在占一级导航（管理组，节点和 API Keys 之间），
内容是两个 tab：推送设备订阅 + WebSocket 在线连接。

问题：这是 `notify.phone`（内置 end 能力，中枢执行）的**实现细节**，
不是并列的一等概念。用户接了自己的 send-slack / send-email 后，
notify.phone 只是可选的一个。

期望：从一级导航移除，内容并入「能力与出口」页的 notify.phone 详情面板。
退一步方案：保留页面但去掉导航项，从能力详情链过去。

待定：receivers.html 的 URL 要不要保留兼容（有收藏/分享过）；
文案 key 怎么处理；e2e 的 PAGES 列表要同步。

### #92 paramsSchema 可视化编辑

`capabilities.html` 新建能力弹窗里 `paramsSchema` 是裸 textarea，必须手写 JSON Schema。

期望：可视化表单 ⇄ JSON 原文，可切换、双向同步。
可视化只覆盖后端**真正支持的子集**（`internal/route/validate.go:19-31`）：

```
type / properties / required / additionalProperties / items /
minItems / maxItems / enum / minLength / maxLength /
pattern / minimum / maximum / minProperties / maxProperties
```

**最重要的坑**：可视化绝不能生成后端不认识的关键字（`$ref`、`if-then-else`、`oneOf`）——
那会让用户以为设了约束、实际后端静默忽略。

同理，用户手写在 JSON 里的、可视化表达不了的内容（如 `patternProperties`），
切到可视化时**绝不能静默丢掉**，必须明确告知。

### #93 出口表拆成独立页面

`capabilities.html`（516 行）现在干两件事：能力定义 + 出口表（边）。

拆开的理由：
- **能力 vs 边是不同心智对象**：能力是节点（谁定义=节点代码声明，低频）；
  边是连线（用户在控制台配，高频试错）
- **出口表会长很大**：扇出时一条出口对多条边，40~50 条边时列表需要
  搜索/按能力过滤/按出口分组，能力页装不下
- **它天然是图**：出口表就是整张图的另一种表示，做完 #88 的图库后
  主视图应该是图，列表做侧栏，两者联动

期望新页面 `edges.html`：工具条（搜索/筛选/新建）+ 图（可选，依赖 #88）+ 列表。

导航要一起改名：「能力与出口」→「能力」（#91 也动导航，两件一起做别改两遍）。

待定：图视图现在就做还是先只做列表；能不能在图上手绘连线；
与 #88 共用同一个图库。

### #94 节点页能力详情 + 事件流按节点过滤 + 跳转

三条一起提的：节点页显示能力详情；事件流按节点过滤；节点页跳到事件流。

**已核实（不用新接口）**：
- `GET /v1/tasks?nodeId=<id>` **后端已支持**（`console_handlers.go:96`）
- 响应里 `nodeId` 字段完整（实测有）

所以前端流程：任务按 nodeId 查 → 取 traceId 去重 → 用 trace 过滤事件。
遗留风险：任务超 limit 会漏更早的事件（数据量小时可接受）。

与 #89（事件流按源分组）是同一页面的改动，**两件一起做**别改两遍筛选区。

### #95 trace/span 瀑布图（与 #90 高度重叠）

用户："我还希望能够使用 trace 和 span 的那种图展示一下事件，因为我们本身就是这种形态嘛。"

**数据完全够，不用改后端**（`internal/store/task.go:63-91`）：
traceId / parentTaskId / createdAt / startedAt / finishedAt / status / attempts / nodeId / error。
Event 也有 traceId + createdAt，可当根 span。

关键价值：**时序信息只有瀑布图能给**，节点图给不了 —— 谁先谁后、各跑多久、哪里并行。

**不需要图库**：纯 CSS 就够
```
span.left  = (startedAt - traceStart) / totalMs * 100%
span.width = duration / totalMs * 100%
```

与 #90 的关系：
| | #90 节点图 | #95 瀑布图 |
|---|---|---|
| 强项 | 派生关系、扇出汇聚 | 时序、耗时、并行 |

**建议 #95 优先**（数据现成、不用布局算法、时序信息不可替代），#90 后置。

## 待定问题（等用户逐条确认）

- 图放在哪：仍嵌在页面里，还是抽屉/全屏
- #88 与 #90 是否共用同一个图库（大概率是）
- 两者是否要联动
