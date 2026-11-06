## 现状（按当前代码重写）

「发送测试通知」面板与 `POST /v1/test-notify` 端点**已删**（`fea34b1`）：Go 侧只剩负断言 `internal/server/console_test.go:1066 TestLegacyNotificationEndpointsRemoved`（必须 404），e2e 有 `#test-notify` 不存在的断言（`ux_regression_19_23.mjs:406-420`）。平台模型（`kind=end`、`notify.phone` 由中枢执行）成立。

**本 issue 只剩下面两条产品决策，不含代码缺陷。**

## 决策 1：给新命名空间播种内置「手动通知」源 —— **已决定：不做**

> 产品决策（2026-11）：**不播种**。理由见下（即使做了也不通：UI 隐藏 + 编辑丢目标 + 必被 param_schema 剪枝）。
> 若将来要开，「手机推送是否开放」必须先定，且要一起改 `PUSH_UI_ENABLED`、源编辑的目标候选过滤、以及 fire 的 payload。

技术上播种很简单：`internal/store/namespace.go:199` 已有 `SeedBuiltinCapabilitiesTx` 同事务播种能力；加一条 `INSERT INTO sources`（参考 `playbook.go:1405`）即可。

但现在播了会得到「看得见、点得动、永远派不出去、一编辑就丢」的东西：

1. `notify.phone` 被 UI 隐藏：`web/partials.js:2087 PUSH_UI_ENABLED = false`、`:2089 HIDDEN_CAP_NAMES`；源编辑弹窗的目标候选来自 `filterCaps`（`sources.html:893,1150`）⇒ **用户看不见这个目标勾选，保存时 `collectTargets()` 只读 DOM 已勾选项（`sources.html:2011-2015`）→ 静默把该目标删掉**。
2. 触发必被剪枝：`notify.phone` 的 `ParamsSchema.required = ["to","title"]`（`internal/store/builtin.go:57-71`），而 fire 目前发 `{}`（见决策 2）⇒ `param_schema` 剪枝。
3. 执行还要 VAPID + 用户设备：`internal/server/mux.go:366-376` 里 `vapidSender` 为 nil 时 notifier 为 nil，`executor.runNotifyPhone` 直接 failed（`internal/executor/executor.go:164-166`）。

⇒ **被「手机推送是否开放（`PUSH_UI_ENABLED`）」阻塞**。要么先决定开放推送并一起处理 UI 可见性 + 编辑不丢目标 + 触发参数，要么就不播种。

## 决策 2：Sources 的「触发」支持可选 payload —— **已决定：做**（本 issue 的唯一剩余工作）

后端与契约**已就绪**：`internal/server/source_handlers.go:514-548` 已收 `{type, payload}`，`api/openapi.yaml:1083-1121` 同款；`payload` 作为任务 params 原样传下去。

只有前端写死空 body：`web-src/pages/sources.html:1072-1076` 与 `source.html:695-701` 都是 `JSON.stringify({})`。

落点：两处 `fireSource` 换成可选 payload 输入；列表页可复用已有的 `SAMPLE_PAYLOAD`（`sources.html:1461`，现在是 dry-run 用）。这是**通用能力**，不是通知专用。

## 验收（若做决策 2）

- Sources 列表页与详情页都能带 payload 触发；不带 payload 时行为与现在一致（`{}`）
- e2e：给一个 `required` 参数的源带 payload 触发 → 任务不被 `param_schema` 剪枝；不带 payload → 剪枝原因仍如实展示
