# 源详情页链路图（source-detail.html）

> 状态：进行中。本文随实现同步更新，最终版含验收数字与变异证据。

## 为什么图丢了（考古）

一行结论：链路图原本挂在 `web-src/pages/source.html` 上（`.source-graph` 容器 +
`render()` 里 `await A.graphLib()` + `{{define "graph-libs"}}`），该页在 **`85de629c`**
（"批量清审计债… + 下线重复源页"，B-4 项）被整页删除、playbook 的引用入口改指
`source-detail.html`；而 `source-detail.html` 历史上从未有过图，于是用户看到的就是
「链路图没了」。

## 待填

- 容器 / id / class
- 数据来源与降级
- e2e 断言与变异
- ci / e2e 数字
