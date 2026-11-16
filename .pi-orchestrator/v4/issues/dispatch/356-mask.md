# #356 回执 writeOnly 泄漏修复（实现 centurion）

> 读 `dispatch/COMMON.md` 全文。issue 正文（权威）：`/root/code/anotify/.pi-orchestrator/v4/issues/V4-09-receipt-secret-leak.md`，GitHub https://github.com/zzjcool/anotify/issues/356

## 工作树（唯一）

`/root/code/wt-356`（已从 `origin/v4/integration` 66d926a 建好，detached）。不要写主 checkout，不要动 `wt-335b`/`wt-349`/`wt-chrome` 等其他人的 worktree。

```bash
cd /root/code/wt-356
git fetch origin
git checkout -B issue/356-receipt-secret-mask origin/v4/integration
git rev-parse HEAD          # = 66d926a
ln -s /root/code/anotify/node_modules node_modules   # 若 make contract 需要 node（gitignore，不提交）
```

## 目标（已确认事实，不要重新侦察）

`internal/playbook/plan.go:402-410` 把渲染后的 `s.Spec.Targets` / `snap.Targets` 原样塞进 `UnappliedEntry.Declared/Actual`；`BuildReceipt`（`plan.go:582`）只对 `Values` 打码。回执落库 `playbook_installs.receipt` 并经 `GET …/installs/{id}`/折叠列表/`Playbook.status` 回读 → **writeOnly values 明文泄漏**。mock（`web/mock.js`·`pbMask`/`pbDisplaySecrets`/`receiptPolicy`）已实现“沿 values 引用/渲染路径传播敏感性”的语义，真实 Go 是缺口。

父级已裁决（`.pi-orchestrator/v4/STATUS.md:186/192`）：敏感性沿 values 引用/渲染路径传播至**回执差异**；raw 执行 render/digest 不动；非敏感同值保留；**禁止全局替值、禁止删 unapplied 假绿**。

## 修复语义（冻结）

1. 打码对象：`UnappliedEntry.Declared` / `.Actual` 里由 writeOnly values 派生的 `params` 叶。
   - whole interpolation（`{{values.blob}}` 对象/数组/整数/布尔）→ 该 params 值整体 `"***"`。
   - 混合字符串（`secret={{values.pin}}`）→ 该 params 值整体 `"***"`（与 mock 一致；不是只替换子串）。
   - 非敏感同值字面量（例：`fixedNumber:77`、`fixedBoolean:true`）**保持原值**——不得按值相等做全局替换。
2. `actual` 侧没有旧 values 的 provenance：优先用**同一 target 位置 + 同名 param key** 继承 declared 侧的敏感性（这正是“同一逻辑字段”的语义）。若结构错位无法对齐，如实记录残余限制与例子，不要用全局值匹配兜底；也不要为此发明大框架。
3. 实现优先级（最低复杂度优先，报告写清选择理由）：
   a. 在 plan 内用**打码后的 values 对同一包再渲染一次**（纯函数），逐 target/param 比较 raw vs masked 推导敏感性——不新增 wire、不落库新字段；
   b. 若必须持久化敏感性策略：改 `internal/store/schema.sql`（v4 未发布可改 baseline），必须附迁移幂等 + 往返测试，并证明 a 不可行。
4. 打码必须在**落库前**完成（receipt JSON 本身即打码），不是 GET 时才遮。

## 不做

- 不改 `/render` 输出、不改 `valuesDigest`、不改已生效资源 config / 执行事实 / `values` 的既有打码与 digest 口径。
- 不改 `internal/protocol`、Node、`web/**`、`scripts/e2e/**`（f3 断言是验收物，禁止改）。
- 不删/弱化任何既有测试；不改契约来迁就实现。契约矛盾 → 上报协调者，不自行发明。

## 验收（真实跑，贴 exit + log）

- 新增 Go 回归（建议 `internal/playbook/receipt_mask_test.go`）精确复刻 f3 场景：
  - 同一 playbook 连续两次 install（第二次改 writeOnly 值使 targets 差异 → `targets_unapplied`）；
  - 断言旧 actual/新 declared 的 `blob`（object whole）/`list`（array whole）/`pin`（int whole）/`flag`（bool whole）/`mixed`（混合串）都是 `"***"`；
  - `fixedNumber==77`、`fixedBoolean==true` 保留；
  - 完整 receipt JSON 序列化后不含明文；落库回读（server/store 路径）同样无明文；
  - 同一 receipt 的 `/render` 结果与 `valuesDigest` 与未打码输入一致。
- 既有：`go test ./internal/playbook/... ./internal/store/... ./internal/server/... -count=1`、`gofmt -l`（空）、`go vet ./internal/...`、`make contract` 全 0。
- `make build`（若改动触及 server/store 编译面）。

## 交付

commit + `git push -u origin issue/356-receipt-secret-mask`。**禁止** PR create/merge、禁止 push 共享分支/force push。报告按 COMMON §5，另列：改动文件、actual 侧对齐策略与残余限制、测试映射、真实 exit/log 路径。
