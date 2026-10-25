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
| — | 等待用户逐条补充 | — |

### 已完成（本阶段）

| 问题 | 修复 |
|---|---|
| 新建源弹窗背景透明 | `.card` 的 `--surface-1` 是 `rgba(…,0.02)`，弹窗直接复用导致背后列表透出来。改用 `--panel-modal`（不透明 `#0b0b12`），同步修了 capabilities.html 两处 |
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

## 待定问题（等用户逐条确认）

- 图放在哪：仍嵌在页面里，还是抽屉/全屏
- #88 与 #90 是否共用同一个图库（大概率是）
- 两者是否要联动
