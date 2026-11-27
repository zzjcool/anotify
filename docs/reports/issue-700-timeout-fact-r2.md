# issue #700 · 超时/无进展的终态事实与失败兜底 —— review r3 处置报告

- PR：#703（分支 `pi-subagent/issue-700-r1`）
- review 原文：`/tmp/rev700.txt`（第 3 轮，1 条阻断 + 5 条）
- 本轮 commit：`18446caf`（上一版 `d8e9794b`）
- 范围：只处理 review 的 6 条；功能主线（r2 已定）不动
- snapshot-doc: 2026-11-27 的取证快照（下文数字/命令输出都是这次本机实测的定格；复跑以新输出为准）

> 本文件此前不存在（r2 没有落报告），所以这里从 r3 开始记。

## 1. 做了什么

### 1.1（阻断）重复 `cancel.ack` 会重复落 `task_failed` + 重复派兜底

现象（review 实测，本轮先复现再修）：同一帧发两次 → `facts=2 tasks=3`。

根因链没有变，只是把「终态事实已落」从**调用点约定**变成**事务内事实**：

- store 对「已终态 + `cancel_requested_at` 有值」的 `cancel.ack` 重放走 reap-only 分支返回 `nil`
  （`reapAttemptOn` 是 `reaped_at IS NULL` 的幂等 CAS）；
- `handlers.go` 于是又把同一个 abandoned 任务送进 `dispatchTerminalOutcome`；
- `route.DispatchFailure` 原先没有任何判据，`events` 也没有唯一键 → 第二条 `task_failed` + 第二个兜底任务。

**选型**：review 给了两条，选「`DispatchFailure` 在同一个事务里先查本任务是否已有失败事实」。
理由（三条，都不是风格问题）：

1. 部分唯一索引只能挂在 `task_failed` 上，**漏掉 router 任务的 `router_failed`**（同一条重放路径、
   同一种双派发）。要覆盖它就得把 `router_failed` 也做成唯一键，而 `router_failed` 从 #522 起
   就允许出现在 `pruned[]`（`api/events.yaml:509`），等于顺手改既有对外口径。
2. `events` 是只增表（`schema.sql:318`）：历史库里已有重复行时 `CREATE UNIQUE INDEX` 会在
   启动期失败——而「历史重复行」正是这条 bug 的产物。
3. 判据与写入同事务（DSN 已是 `_txlock=immediate`），没有「先查后写」的窗口；
   `INSERT OR IGNORE` 还会顺带吞掉别的约束 violation。

改动：

- `internal/store/decision.go`
  - 新增 `DB.RecordFailureFact(ctx, FailureFact) (bool, error)`：一个事务里
    「查重（`type IN (task_failed, router_failed)` 且 `payload.taskId` 命中）→ 写事实行 →
    `createOutcomeDispatchTx` 派兜底」。已有事实 → 一行不写、返回 `false`。
  - `CreateOutcomeDispatch` 的 tx 体抽成 `createOutcomeDispatchTx`，校验抽成 `prepareOutcomeDispatch`，
    行为不变（`hasActions()` 取代原来的空动作判断）。
- `internal/route/failure.go`：`DispatchFailure` 先组 `FailureFact`（含兜底 plan），再交给 store 一次落账。
  兜底读不出来时仍**落事实**、把读错误交回调用方记日志（原来「事实先落」的语义没有退化）。
- 四个调用点（`task.result` / `cancel.ack` / 用户 abandon / 租约回收）不改一行就继承这条判据。

### 1.2 `api/resource.yaml` 与实现/`api/events.yaml` 自相矛盾

以实现为准（`eventIsFailureFact` 只过滤 `downstream_missing` / `task_failed`）改文档：
从「失败事实行」列表里删掉 `router_failed`，并单开一段说明它从 #522 起就按剪枝投影
（仍在 `pruned[]`，`observed.pruned[]` 也照收），与 `api/events.yaml:509`、
`internal/server/failure_fact_test.go:82-88` 三处口径一致。

### 1.3 无进展默认值三处口径 + UI 说假话

r2 已经定了「默认关闭」，所以这里把 UI/文案/死常量收到同一个口径（不是把节点默认改成 600）：

- `web-src/pages/capability.html` 与 4 份 locale：`节点默认（600）` → `跟随节点（缺省关闭）`
  （en `Node setting (off by default)` / ja `ノード設定（既定は無効）` /
  es `Configuración del nodo (desactivado por defecto)`）；
- 删 `protocol.DefaultIdleTimeout`（零引用的死常量，`grep -rn DefaultIdleTimeout` 现为空）；
- PR 描述里「省略 = 节点默认 **600s**」已改（`gh pr edit`）。

### 1.4 节点配置键 `idleTimeoutSec` 没有文档

- `docs/anode-install.md` §10 的完整 `anode.yaml` 样例补 `idleTimeoutSec: 0`；
- 样例后补一段语义：节点级默认、能力 `lifecycle.idleTimeoutSec` 可覆盖、缺省 0 = **关闭**、
  显式 0 也是关闭、**strict YAML 写错键名是启动失败而不是静默忽略**（`TestStrictUnknownFields` 的坑）；
- `docs/anode-cli.md` 的 strict-config 那条指到这份样例，运维在 CLI 手册里也能找到键清单。

### 1.5 注释棘轮：还原被删的无关注释，额度从新注释里省

还原 review 点名的 3 处 + **review 漏掉的第 4 处**（同一批、同一手法，diff 里同样是删既有解释性注释）：

| 文件 | 行 |
| --- | --- |
| `internal/route/route.go` | `// Ready 返回可以建任务的 target。` |
| `internal/artifact/artifact_test.go` | `// 合法的 requires 能过。`、`// 信封错误。` |
| `internal/artifact/artifact.go` | `// Value 是字面量值。`（review 未列，属同批删除） |
| `web-src/pages/capability.html` | `// 正文` |

额度账（BASELINE 逐目录）：

| 目录 | 上一版 | 现在 | 差 | 构成 |
| --- | --- | --- | --- | --- |
| `internal/route` | 293 | 297 | +4 | +1 还原；+3 幂等判据的 why |
| `internal/artifact` | 423 | 426 | +3 | +3 全是被误删注释的还原 |
| `internal/store` | 6760 | 6764 | +4 | +4 幂等判据的 why |
| `internal/ws` | 668 | 670 | +2 | +2 重放路径这条外部约束 |
| `internal/server` | 7731 | **7727** | −4 | +1 用例夹具说明；**−5 从本 PR 自己的新注释里去重省出** |
| `web-src/pages` | 2282 | 2283 | +1 | +1 还原 |
| **总量** | 42870 | **42880** | **+10** | +5 还原 + 10 本次修复 − 5 去重 |

- 对 main（42873）是 **+7 行（+0.016%）**；没有再从任何无关文件搬额度；
- `--update` 只降不升（抬高直接拒），所以这 10 行是**手写**进 `BASELINE` 的，diff 可逐行核；
- 我先写了 16 行新注释，压到 10 行（删掉 6 行自己写的用例注释，只留「代码无法自述」的两处）。

### 1.6 兜底对用户主动 abandon 也触发

选了**在契约里点名**而不是「默认排除用户主动终止」：`reasons` 省略 = 任何失败是 #700 已定的口径，
改成默认排除会让「用户 abandon 也想让 record 记一笔」没有表达方式（想排除的人本来就能写窄）。
`api/resource.yaml · CapabilityFailureFallback.reasons` 现在明写 `cancelled_by_user` /
`abandoned_by_user` / `node_lost` / `lease_expired` 都会触发（四个码都在 `protocol.taskFailureReasons` 闭集里）。

## 2. 测试覆盖

| 用例 | 断言 | 变异下 |
| --- | --- | --- |
| `internal/ws` `TestDuplicateCancelAckDoesNotDispatchTwice` | 同一 `cancel.ack` 发两次，**每次之后**都 `facts=1 tasks=2` | 红（`facts=2 tasks=3`） |
| `internal/route` `TestDispatchFailureIsIdempotentPerTask` | 同一条失败任务连打 3 次 → 1 条事实、1 个兜底 | 红（`task_failed:3`） |
| `internal/route` `TestDispatchFailureIsIdempotentForRouterTask` | router 任务的 `router_failed` 也只落一条 | 红（`router_failed:2`） |
| `internal/server` `TestTaskResourceAbandonLeavesTerminalFact` | abandon 后恰 1 条 `/abandon`，重复 POST 只 409、不再落事实 | **绿**（见下） |

变异方法：把 `if seen > 0` 改成 `if false && seen > 0`，其余不动；验证后已还原（工作树干净）。

`TestTaskResourceAbandonLeavesTerminalFact` 在变异下仍绿是**预期**：它走的是 abandon 端点的
409 分支，第二次请求在到达 `DispatchFailure` 之前就被 `ErrInvalidTransition` 挡住。它是
`resource_task.go:252-258` 新分支的端到端断言（此前只有 lease 层回调单测），**不是**幂等判据的
证明——这里写清楚，避免被当成假绿。

## 3. 验证输出（本机真跑，原样）

```
$ go build ./... && gofmt -l . && go vet ./internal/...
(no output above = 全部通过)

$ go test ./internal/ws ./internal/store ./internal/route ./internal/server -count=1
ok  	github.com/zzjcool/anotify/internal/ws	4.998s
ok  	github.com/zzjcool/anotify/internal/store	9.568s
ok  	github.com/zzjcool/anotify/internal/route	1.946s
ok  	github.com/zzjcool/anotify/internal/server	57.757s

$ node scripts/check-comment-density.mjs
   扫描 788 个文件（下限 600）· 注释行 42880 / 基线 42880
   目录：80 个有基线，0 个与基线不一致
✅ 与基线完全一致（超与降都算不一致）

$ node scripts/check-annotations.mjs
✅ 注解即外键守卫通过：全部引用可解析、锚点可解析、棘轮未超基线。

$ node scripts/check-test-discipline.mjs
   e2e/waitForTimeout    :  19 /  19   ✅
   e2e/swallow-catch     :  34 /  34   ✅
   go/t.Skip             :   3 /   3   ✅
   go/time.Sleep         :  37 /  37   ✅
   e2e/bare-tag-first-nth:   3 /   3   ✅
✅ 五项全部与基线一致（t.Skip 登记 3/3 命中）

$ python3 scripts/check_openapi.py
  OpenAPI exact-five 通过（5 份文件 / 146 个 operation / 146 个唯一 operationId / 跨文件 $ref 与 pathItems 可解析）
```

顺带（本轮碰了前端与契约，跑对应的窄门）：`check-classes` 835 类全绿、`check-ui-copy` 绿、
i18n 键守卫（4 份 locale 键集一致 / 2178 个引用点都有词条）、`check-contrast` 48/48、
`check-yaml-doc` 绿、`go test ./internal/server -run TestOpenAPIParity` 绿、
`go test ./internal/artifact ./internal/protocol ./internal/playbook ./internal/schema ./internal/lease` 全绿。

## 4. 未决问题

1. **`mux.go:246-255` 的 `OnAbandoned` 接线仍只有 lease 层回调单测。** `NewMux` 只回
   `http.Handler`，拿不到里面的 `Sweeper`；要端到端跑 `RunOnce` 得把它从装配函数里吐出来或加
   test seam。这是接线层重构，超出本轮 review 范围，没有擅自扩。行为面（abandoned → 一条事实 +
   兜底）已由 ws 用例与 abandon 端点用例覆盖，缺的只是那一行闭包。
2. **`internal/artifact` 的 `TestAgentTUI*` 三条在本机红**（macOS `/var` → `/private/var` 路径
   规范化）。在 `origin/pi-subagent/issue-700-r1`（`d8e9794b`）的干净 worktree 复跑同样三条，结果
   完全相同；`go test ./internal/artifact -skip TestAgentTUI` → ok。与 #700 无关。
3. **注释总量比 main 多 7 行**（42873 → 42880）。构成见 §1.5 的表：5 行是还原被误删的注释，
   10 行是本次幂等判据/用例的 why，−5 行从本 PR 自己的新注释里去重省出。若 owner 要求严格
   与 main 持平，唯一还能砍的是新用例的重放路径说明（2 行）与 store 的返回语义（3 行）——
   代价是把「这条用例为什么存在」从代码里删掉，不建议，但这是取舍点，留给 owner 裁。
