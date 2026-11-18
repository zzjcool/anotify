> 总任务：https://github.com/zzjcool/anotify/issues/330
> 任务标识：V4_09；建议优先级：P0（安全）；一个工作树一个源码 writer；base = 最新 `origin/v4/integration`。
> 发现于 #335 real 模式收口（不是 #335 范围内可修：internal/ 只读）。

# 目标

InstallReceipt 的 `unapplied[]`（`playbook.md §4.2` 第 12 行，源 targets 差异）在 `declared` / `actual` 里**原样**带出渲染后的 `params`。这些 params 可能包含 playbook `spec.values` 的 `writeOnly` 值（字符串 / 数字 / 布尔 / 对象 / 数组成员、以及混合插值字符串）。

回执会落库 `playbook_installs.receipt` 并经 `GET …/installs/{installId}`、折叠列表、`status` 回读 —— 等同把 writeOnly 凭证回显给任意有命名空间读权限的调用方。同一份回执自己的 `values` 字段已经打码（D16），但 `unapplied[].declared/actual` 没有，属于真实泄漏。

## 已确认事实（不要重新侦察）

- 构造点：`internal/playbook/plan.go:402-410`（`declared := append(...s.Spec.Targets...)` / `actual := ...snap.Targets`）→ `UnappliedEntry.Declared/Actual`，raw 渲染值直接进回执。
- `BuildReceipt`（`internal/playbook/plan.go:582`）只对 `Values` 调 `MaskValues`，不动 `Unapplied`。
- `notes[].message` 用的是 `formatTargets`（`plan.go:778-791`），只打印 `capability{params}`，**不**泄漏值；本次不需要改 notes。
- 契约依据：`docs/design/v4/playbook.md §4.5`（D16：writeOnly 值进回执打码；"凭证不该进回执"）、`decisions.md:108`、`.pi-orchestrator/v4/STATUS.md:186/192`（父已裁决：敏感性沿 values 引用/渲染路径传播至回执差异，raw 执行 render/digest 不动，非敏感同值保留，禁止全局替值/删除 unapplied 假绿）。
- 真实失败断言：`scripts/e2e/fixtures/f3/f3-extra.mjs:147`（#335 real 模式 `playbook-secret-history`），期望 `declared/actual[0].params.{blob,list,pin,flag,mixed} === '***'`，`fixedNumber:77` / `fixedBoolean:true` 保持原值。mock（`web/mock.js`·`pbMask`/`pbDisplaySecrets`）已按同一语义实现，真实服务端是缺口。

# 实施范围 / 所有权

- `internal/playbook/**`（plan/render/values/receipt 及对应测试）。
- `internal/server/resource_playbook.go`（若安装/回读路径需要传敏感性事实）。
- 若必须持久化"敏感性策略"才可能安全：`internal/store/schema.sql`（v4 未发布可重建 baseline）与该 store 测试；**必须**在报告里证明为什么按当前位置/模板无法表达，并附迁移幂等 + 往返测试。
- 只读：`internal/protocol`、`internal/node*`、`web/**`、`scripts/e2e/**`（f3 断言是验收物，不能改）、五 spec。

## 不做

- 不做全局按值替换（相同字面量 77/true 不得被误伤），不引入通用脱敏框架/新 DSL。
- 不改变 `/render` 返回的原始渲染结果、`valuesDigest` 计算、已生效资源 config/执行事实（D16：打码只影响回执展示副本）。
- 不删/弱化 `unapplied` 结构或既有测试；不以“整包拒绝安装”绕开。
- 不加 v3 兼容层，不碰前端/Node/wire。

# 验收标准

- [ ] 连续两次 install（第二次改 writeOnly 值，触发 targets 差异）：`unapplied[].declared` 与 `actual` 中所有由 writeOnly values 派生的 params（whole interpolation 的对象/数组/整数/布尔、混合字符串）都是 `***`；非敏感同值字面量（`fixedNumber:77`、`fixedBoolean:true`）保持原值。
- [ ] `GET …/installs/{installId}`、折叠列表、`Playbook.status`、错误体（`playbook_conflict` 等）、notes 的完整 JSON 不含上述明文；落库的 `receipt` JSON 同样已打码（读回即证）。
- [ ] `/render` 响应与 `valuesDigest` 与改前逐字节/逐值一致；`values` 打码、digest 对未打码值算的既有测试不变绿转红。
- [ ] 新回归具名：两安装 + 旧 actual/新 declared、混合字符串、非敏感同值、对象/数组/整数/布尔叶。
- [ ] `go test ./internal/playbook/... ./internal/store/... ./internal/server/... -count=1`、`gofmt -l` 空、`go vet ./internal/...`、`make contract` 真实 exit 0（贴 log/exit）。

# 完成定义

真实 API 回执不再泄漏 writeOnly 值，且不是靠改测试/全局替换达成；独立 review 无 critical/major。合并后 #335 的 real `playbook-secret-history` 应转绿（由 #335 收口方复跑，不在本 issue 里改 f3 断言）。
