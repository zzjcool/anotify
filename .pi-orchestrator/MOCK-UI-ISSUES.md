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
| [#88](https://github.com/zzjcool/anotify/issues/88) | 源的下游流向图需要真正的图形化（当前缩进文本太简陋） | 待做 |
| — | 等待用户逐条补充 | — |

## 已发现问题（细节）

### #88 流向图太简陋

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

## 待定问题（等用户逐条确认）

（逐个记录）
