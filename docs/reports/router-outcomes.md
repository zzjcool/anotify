<!-- snapshot-doc: 2026-11-22 #522 A3 交付时的定格数字（make ci / make e2e-affected 的真实输出），复算命令见 §7 -->
# router 能力 + outcomes 表（#522 A3「判断用能力，执行留 hub」）交付报告

> 分支 `pi-subagent/hw-nj/anotify-522-router-abc7f88b` · base `main` @ `b2096466`（已 rebase 到含 #523 / #526 / #527 / #530 的 main；冲突按并集解）
> 设计记录：#512 · 本阶段范围：#522 · PR 正文 `Fixes #522`
>
> 下面每个数字都是**这台机器（hw-nj 迁移后）重新跑出来的**：`make ci`、`make e2e-affected`、
> 两条变异的红/绿都重测过一遍；前一版报告里的数字来自旧机，不复用。

一句话：**能力只选结局，平台负责执行**。source 的 target 多出一张 `outcomes`（结局名 →
1..N 个动作），router 任务跑完只留下**一个结局名 + 判决参数**，中介按建任务那一刻钉住的
表把动作派给下游。判断结果**不走 stdout**，走一颗任务级一次性凭据 + 一个显式端点。

---

## 1. 冻结语义（实现里定死）

### 1.1 决策通道的最终形状

```
lease.grant ──decisionToken──▶ 节点 ──execenv──▶ 脚本
                                   ANOTIFY_DECISION_TOKEN=tok_<base62(24B)>
                                   ANOTIFY_DECISION_URL=http://<hub>/v1/tasks/<taskId>/decision

脚本： POST <url>                                  # Authorization: Bearer <token>
       {"outcome":"close","params":{"reason":"广告"}}
hub：  200 {"taskId":"tsk_…","outcome":"close","actions":2}
```

- **凭据与 attempt token 是两颗独立的凭据**：`attemptToken` 授权「这次执行」（emit / 结果 /
  产物），`decisionToken` 只授权「给自己这个 task 选一个已声明的结局」。两者都由
  `NewTaskToken` 铸成 `tok_` 前缀的高熵串（**靠用途区分，不靠前缀**）、库里只存 sha256、
  明文只在 grant 时出现一次。
- **只发给 router 任务**：任务行 `outcomes` 为空 → grant 里没有 `decisionToken`（wire 上不出现
  这个键），节点也就不注入那两个 env（`execenv` 以「token 与 URL 都非空」为注入条件，
  不给半套端点）。
- **URL 指向中枢，不是本地 emit 网关**：结局白名单在库里，只有中枢能判定；emit 那条路是
  「先落本地网关再转发」，语义不同。
- **允许在成功之前提交**（先写判决再退出是推荐姿势）：判决落库与任务终态无关，执行动作
  只在**成功终态之后**发生。
- TTL 10 分钟（`protocol.DecisionTokenTTL`）、body 上限 1 MiB、`DisallowUnknownFields`。

### 1.2 三条不变量

1. **判决只有一次**：`UPDATE … WHERE decision = ''` 做 CAS。第二次同凭据提交 →
   `409 decision_already_taken`（**不清凭据摘要**：否则第二次会拿到 401，「你重复提交了」
   这个最该被看懂的现象反而看起来像伪造）。凭据仍受 TTL 约束；重排（retry）会清掉判决与
   凭据，下一次 attempt 重新铸。**凭据 TTL 随租约续期**（`RenewLeaseAuthorized` 在同一事务里推
   `decision_expires_at`）——只续租约会让跑超过 10 分钟的 router 在成功前一刻拿到 401 →
   `no_decision`；已判决的不再续。
2. **凭据只能给自己选一个已声明的结局**：结局名不在该任务的表里 → `403 outcome_not_declared`
   **+ 一条 `outcome_not_declared` 审计行**（同一事务提交，回滚它等于把越界吞了）+ 0 任务。
   任务行不存在与凭据不匹配都是 401（不把 404 变成探测面）。
3. **不静默**：任务非成功终态 → `router_failed`；成功但没判决 → `no_decision`；判决选中空
   动作结局 → `outcome_dropped`。三者都是 **0 任务 + 一条 `source='route'` 的观察行**（与剪枝
   同表同来源名，按 trace 可见）。

### 1.3 两段参数求值

动作参数可以混写两个根名，求值因此是两段的：

| 根名 | 求值时机 | 取不到时 |
| --- | --- | --- |
| `$(payload.路径)` | 建 router 任务时（那一刻载荷在手上） | 整条 target 按 `param_missing` 剪枝 |
| `$(decision.路径)` | 任务成功终态、按判决执行时 | 该动作 `param_missing` 剪枝（其它动作照派） |

`targets[].params` 里写 `$(decision.…)` 是**非法**的（判决那时还不存在）→ 写入期 400
`invalid_source_config`，message 指出参数路径（`specs` 级的 `causes[]` 只用于带位置的字段级判据；
这条走既有的模板错误形状，不进 `causes`）。

### 1.4 写入期校验（400，`details.causes[].field`）

| 判据 | `field` |
| --- | --- |
| 结局名 `^[a-z0-9][a-z0-9_-]{0,31}$` | `spec.targets[0].outcomes`（名字进不了路径，写进 message） |
| 结局名 ≤ 32 个 | `spec.targets[0].outcomes` |
| 单结局动作 ≤ 16 | `spec.targets[0].outcomes.close` |
| 动作缺 `capability` | `spec.targets[0].outcomes.close[0].capability` |
| 动作能力 ∉ 该能力的 `spec.next` | `spec.targets[0].outcomes.close[0].capability` |
| 动作参数模板写坏 | `spec.targets[0].outcomes.close[0].params` |

`drop: []` 合法（留痕不派发）；同一 target 同时有 `params` 与 `outcomes` 合法（params 是
router 的输入）。派发期下游解析失败沿用 `downstream_missing` 的**失败 Task**语义（不是静默跳过），
一个动作失败不拖垮同结局的其它动作。

---

## 2. 落地形状（文件 → 职责）

| 文件 | 职责 |
| --- | --- |
| `internal/sourcespec/outcomes.go` | 结局表的纯判据（名字/上限/模板语法）+ `DeclaredRef`/`SplitRef`（白名单**唯一一份**语义，emit 与 outcome 共用） |
| `internal/sourcespec/template.go` | 两根名判据：target 的 `params` 写 `$(decision.…)` 写入期就拒（它只属于 outcomes 动作） |
| `internal/sourcespec/template.go` | 两根名模板系统：`RenderTarget`（payload 段）/ `RenderDecisionParams`（decision 段） |
| `internal/store/decision.go` | 判决落库（凭据↔任务、白名单、CAS）、结局派发事务、route 观察行 |
| `internal/store/execution_auth.go` | `GrantLeaseAuthorized` 多返回 `decisionToken`：只有 `outcomes` 非空才铸；续租同事务推凭据 TTL（已判决不续） |
| `internal/store/source.go` | 写入期 `spec.next` 归属校验（要读能力行，所以只能在这里判） |
| `internal/route/outcome.go` | `PlanFromOutcome`（动作 → 下游计划）+ `DispatchOutcome`（终态之后执行/留痕） |
| `internal/ws/handlers.go` | grant 带 `decisionToken`；`task.result` 的 `Recorded` 分支在 ack **之前**执行结局 |
| `internal/server/decision.go` | `POST /v1/tasks/{taskId}/decision`（不在资源域内：能力没有会话） |
| `internal/protocol/decision.go` + `env.go` + `exec_frames.go` | 端点/头的唯一常量源、两个 env 名、`lease.grant.decisionToken` |
| `cmd/anode` | 凭据从 grant 传到 `execenv`，按中枢根拼 URL，成对注入 |

数据库：`-- +migration 5 router_outcomes`（`headVersion` 4 → 5）给 `tasks` 加五列
`outcomes` / `decision` / `decision_at` / `decision_token_hash` / `decision_expires_at`。
`outcomes` 是**声明快照**（建任务那一刻钉住，不回读源的 targets）；`decision` 是判决事实；
后两列是一次性凭据的摘要与 TTL。

---

## 3. 五件套（#522 验收 10）

- **`api/resource.yaml`**：`SourceTarget.outcomes`（`additionalProperties: array<SourceOutcomeAction>`）
  + 新 schema `SourceOutcomeAction` + `POST /v1/tasks/{taskId}/decision`（operationId
  `submitTaskDecision`，`security: [{taskToken: []}]`）+ `TaskDecisionRequest` /
  `TaskDecisionResponse` + 新安全方案 `taskToken`。
- **`api/agent.yaml`**：`LeaseGrantFrame.decisionToken`（`writeOnly`，省略 = 本任务没有结局表）。
- **parity**（`internal/server/openapi_parity_test.go`）：三对新的 DTO 双向字段
  （`sourcespec.OutcomeAction` ↔ `SourceOutcomeAction`、`protocol.DecisionRequest` ↔
  `TaskDecisionRequest`、`protocol.DecisionResponse` ↔ `TaskDecisionResponse`）+ 新方案
  `taskToken` 的注册语句与源码证人（`taskTokenRegistration`：注册语句必须指向
  `newTaskDecisionHandler`，且 `internal/server/decision.go` 里真的有 `BearerToken` 与
  `RecordDecision`）+ 门禁下限：`muxPatternCount` 129→130、ops 140→141、
  `specSchemaFloor` 246→249、`parityPairsFloor` 206→209、`protocolPathConsts` 增
  `DecisionPatternPath`。
- **DTO**：`sourcespec.Target.Outcomes` / `OutcomeAction`；`store.Task` 五列
  （内部事实，`json:"-"`）；`protocol.LeaseGrantFrame.DecisionToken`。
- **e2e**：`scripts/e2e/suites/routing.mjs` §13（77 条断言，**9 组**：13.0 读投影回显 + 13.1–13.8）。
- **单测**：sourcespec（6 个）、store（8 个，含 `TestDecisionTokenExpiryFollowsLeaseRenew`）、
  route（11 个）、server（3 个）、protocol/execenv/runner（4 个，含
  `TestDecisionChannelInjectedIntoScriptEnv`：grant → attempt → 脚本 env 的成对注入）。

---

## 4. 验收（逐条贴实测）

### 4.1 十条验收 ↔ 证据

| # | 验收 | 实测证据 |
| --- | --- | --- |
| 1 | 单结局：`close` → 只跑 `issue-close`；`triage` → 只跑 `issue-triage` | e2e `13.1 结局 close → 恰好 2 个子任务`、`13.1 没有派 issue-triage`、`13.2 结局 triage → 恰好 1 个子任务`；单测 `TestDispatchOutcomeRunsOnlyChosenOutcome` |
| 2 | 一结局多动作：`close` 挂 2 个动作 → 两个任务都建、回执可见 | e2e `13.1 回执 actions=2`、`13.1 子任务能力集合 = {issue-close, issue-notify}`、`13.1 每个子任务的 parentTaskId 都指向 router 任务` |
| 3 | `drop: []` → 0 任务 + 留痕（reason 可读） | e2e `13.3 空动作结局的 actions=0`、`13.3 drop → 0 子任务`、`13.3 trace 里有 outcome_dropped 观察行`；单测 `TestDispatchOutcomeDropRecordsTrail`（断言观察行 payload 里 `outcome=drop`） |
| 4 | 无决策 → 0 任务 + `no_decision` | e2e `13.4 没判决 → 0 子任务`、`13.4 trace 里有 no_decision 观察行`；单测 `TestDispatchOutcomeNoDecisionRecordsTrail` |
| 5 | 越界 → 403 + 审计行 + 0 任务 | e2e `13.5 … → 403`、`reason=outcome_not_declared`、`trace 里有 outcome_not_declared 审计行`、`13.5 越界判决不派任何任务`；单测 `TestRecordDecisionOutcomeNotDeclaredWritesAuditRow`（断言审计行带 `declared` 列表）、`TestDecisionEndpointOverBoundThenIdempotent` |
| 6 | 幂等：第二次 POST → 409，0 额外任务 | e2e `13.6 第二次 POST close → 409`、`reason=decision_already_taken`、`13.6 判决只执行一次：仍然恰好 2 个子任务（不是 4）`；单测 `TestRecordDecisionHappyThenIdempotent` |
| 7 | 写入期：`spec.next` 之外 → 400；结局名非法 → 400 | e2e `13.7 ① … → 400`、`① reason = invalid_source_config`、`① causes[0].field 精确到 outcomes.close[0].capability`、`② "Close" / "has space" 非法 → 400`、`③ target 参数写 $(decision.…) → 400`；单测 `TestSourceTargetsRejectOutcomesOutsideNext`、`TestValidateOutcomesCauses`、`TestValidateTargetsMergesOutcomeCauses`、`TestValidateParamsTemplate`（decision 根名只属于动作） |
| 8 | 变异 ①② | 见 §5 |
| 9 | `make ci` exit 0；`make e2e-affected` 绿；known-env 空 | 见 §6（`entries: []` 未变） |
| 10 | 五件套 | 见 §3 |

### 4.2 额外钉住的行为（issue 没点名但会咬人）

- **失败终态**：e2e `13.8 失败终态 → 0 子任务` + `13.8 trace 里有 router_failed 观察行`；单测
  `TestDispatchOutcomeFailedRecordsRouterFailed`。失败路由器**即使已经提交了判决**也不执行动作
  （否则「判断完就崩」的任务会派出半截链）。
- **只有 router 任务有凭据**：单测 `TestDecisionTokenOnlyForRouterTasks`。
- **凭据绑定 taskId**：单测 `TestRecordDecisionRejectsBadCredential`（别任务 id / 错凭据 / 过期 / 空）。
- **`lease.grant` 的 wire 形状**：`protocol_test.go` 的 `wantKeys` 增 `decisionToken`
  （只有 router 任务出现）。
- **node 注入**：`TestResolveDecisionChannel`（成对注入，只给一半两个都不注入）、
  `TestDecisionURL`（无凭据 → 空串，base 尾斜杠归一）、
  `TestDecisionChannelInjectedIntoScriptEnv`（真跑一次脚本，断言 `ANOTIFY_DECISION_*` 真的进了
  子进程环境；普通任务两个都不出现）——这是「grant → attempt → env」整条线上唯一的端到端单测。
- **凭据 TTL 跟租约**：`TestDecisionTokenExpiryFollowsLeaseRenew`（续租推有效期、已判决不再续）。
- **`task.result` 的 ack 纪律不变**：只在 `Recorded` 执行结局（`Duplicate` 重放不二次派发）。
- **fail closed**：任务行上的判决与表不一致（手改库/回滚）→ `ErrOutcomeNotDeclaredExec`，不猜
  一个结局去派发（`TestDispatchOutcomeUndeclaredOutcomeFailsClosed`）。
- **普通任务零成本**：`TestDispatchOutcomeIgnoresNonRouterTask`（`outcomes` 为空立刻返回）。

---

## 5. 变异（红后复绿）

### ① 去掉 outcomes 成员校验（允许任意结局名）

改动：`store.RecordDecision` 删掉 `if !declared { … 403 + 审计行 … }` 分支。

```
--- FAIL: TestRecordDecisionOutcomeNotDeclaredWritesAuditRow
    decision_test.go:189: 越界结局应 ErrOutcomeNotDeclared，得到 <nil>

❌ routing 374通过/3失败
   - 13.5 结局名不在声明表里 → 403 outcome_not_declared → 403 | 期望 403 实际 200
   - 13.5 … → reason=outcome_not_declared | 期望 outcome_not_declared 实际 undefined
   - 13.5 trace 里有 outcome_not_declared 审计行 | []
断言通过: 374 / 断言失败: 3
```

复绿：恢复分支 → `TestRecordDecision…` ok；`make e2e-one S=routing` → **375 通过 / 0 失败**。

### ② 「只选一个结局」改成「全部结局都跑」

改动：`route.PlanFromOutcome` 把 `outcomes[decision.Outcome]` 换成所有结局动作的并集。

```
--- FAIL: TestDispatchOutcomeRunsOnlyChosenOutcome
    outcome_test.go:200: trace 任务数 = 4，want 3（router + 2 个动作，triage 不许派）

❌ routing 372通过/5失败
   - 13.1 结局 close → 恰好 2 个子任务 | 期望 2 实际 3
   - 13.1 子任务能力集合 = {issue-close, issue-notify} | 期望 [issue-close,issue-notify] 实际 [issue-close,issue-notify,issue-triage]
   - 13.1 没有派 issue-triage（未选中的结局不执行）| ["issue-notify","issue-triage","issue-close"]
   - 13.2 结局 triage → 恰好 1 个子任务 | 期望 1 实际 2
   - 13.6 判决只执行一次：仍然恰好 2 个子任务（不是 4）| 期望 2 实际 3
断言通过: 372 / 断言失败: 5
```

复绿：恢复 → **375 通过 / 0 失败**。

绿侧：两次恢复后 `make e2e-one S=routing` 都是 **377 通过 / 0 失败**；最终整轮
`make e2e-affected` 也是 routing 377/0（见 §6）。

### 附：第三条（本轮补的守卫，不在 issue 要求内）

决策凭据的 TTL 原来是「grant 时定 10 分钟，租约续期不管」——跑超过 10 分钟的 router 会在成功前
一刻被判 401 → `no_decision`。修法是续租同事务推 `decision_expires_at`；去掉这个 `CASE`
（只续 `lease_until`）→ 新守卫当场红：

```
--- FAIL: TestDecisionTokenExpiryFollowsLeaseRenew
    decision_test.go:362: 续租后凭据有效期 = 1795290617，want 1795290647（grant 时 1795290617）
```

另有两条更早的守卫记在这里备查（本轮未重跑，属旧机证据）：去掉**写入期** `spec.next` 归属校验
→ `TestSourceTargetsRejectOutcomesOutsideNext` 红 + e2e `13.7 ①` 期望 400 实际 201。

---

## 6. 门禁数字（本机实测）

```
$ make ci
共 48 项判定：PASS 48 / FAIL 0
ok github.com/zzjcool/anotify/…                      43 个包
✅ 契约静态断言（A2/A4/A5/A6/A7 + openapi 一致性 + 字段级核对 + anode 可编译）通过
   OpenAPI exact-five 通过（5 份文件 / 141 个 operation / 141 个唯一 operationId）
   契约字段守卫通过（243 个 schema、761 个已知字段）
✅ 注解即外键守卫通过（unbacked 298 / 基线 298，unmarked 209 / 基线 209）
✅ 注释密度棘轮与基线完全一致（注释行 43330 / 基线 43330，目录 75 个 0 不一致）
✅ 秘密扫描通过（869 文件 / 17 检测器 / 命中 0 / 放行 3）
exit=0

$ make e2e-affected          # rebase 到 b2096466 后重跑
✅ E2E 全绿：30/30 个请求套件均达标（断言 2891 通过 / 0 失败，authority 清单覆盖 30 个）
   routing 377通过/0失败 · security 100 · ws_protocol 134 · events_paging 179 · frontend 43 …
✅ 临时目录零残留（隔离 TMPDIR 内本次新增 = 0，耗时 433.8s）
known-env-failures.json: entries [] （未变）
```

注释棘轮：本 PR 的注释**净增 0**，`--update` 只做下调（`internal/store/v4test` 1197→1193、
`env.go` 密集预算 28→25、总量 43334→43330（rebase 到 `b2096466` 后重测）；本轮新增「续租推 TTL」
等解释时同批回收了 `store.go` / `orphan.go` 的复述行）。契约静态检查器的
自测 fixture 补了第五个方案 `taskToken`（白名单与 fixture 必须同步，否则自测当场红）。

---

## 7. 复算命令

```bash
make ci                                     # 静态守卫 + 全量单测 + 契约 + 秘密扫描
make e2e-affected                           # 受影响 e2e（30 套件）
make e2e-one S=routing                      # 本次功能的主证据（375 断言）
go test ./internal/sourcespec ./internal/store ./internal/route ./internal/server -count=1
node scripts/check-comment-density.mjs      # 注释棘轮（--update 只降）
python3 scripts/check_openapi.py            # 五份 spec 的 exact-five 门禁
```

---

## 8. 已知边界 / 遗留

- **超时不是终态**：`DispatchOutcome` 挂在 `task.result`（`Recorded`）上。节点卡死被 GC 或
  `cancel.ack` 收掉的路由任务不会走这条路径，因此没有 `router_failed` 行——它们的 trace 里
  仍是既有的事实（cancel/回收），不是「静默派发」。要覆盖这一档需要把钩子挂进重新派发/GC，
  属于 A4 的范围。
- **`outcomes` 是快照**：改源不影响已建任务；反过来，改完源之后同一条事件的新任务用新表。
  这是与 `params`/`digest` 同一条纪律，不是缺陷。
- **没有 `router → router`**：`outcomes` 的动作可以指向另一个声明了 `outcomes` 的能力，但
  该下游任务的 `decisionToken` 只有在它自己也声明时才铸，深度不受限——本 PR 不做深度限制
  （#512 的「深度 1」是 B 阶段的事），因为每一跳都必须显式声明白名单，越权面没有扩大。
- **playbook 安装路径不跑 targets 校验**：`createInstalledSourceOn` 有意「不校验 targets 可解析」
  （安装允许 pending，判定在 Plan），所以 `outcomes ⊆ spec.next` 这条在安装时也不判——装完改源
  才会 400。不是本 issue 的验收面，但要知道白名单在那条路上是「装进去、用之前由源写入兜住」。
- **判决参数不校验**：`params` 是任意 JSON（供动作模板消费）。下游能力自己的 `spec.params`
  会在派发期把它对齐/剪枝，所以坏参数不会变成坏任务。
- **决策端点的请求面比产物上行略松**：body 超 1 MiB 走 `Decode` 失败 → **400**（不是 413）；
  只 `Decode` 一次，尾部多余 JSON 不报错；没有独立限流（与同样持 Bearer 的产物上行端点同口径，
  属既有约定）。三条都不影响白名单语义，留作后续收紧项。
- **凭据与 attempt token 同一个前缀（`tok_`）**：靠用途区分。若将来想让日志/审计一眼分得出，
  换前缀是兼容的（库里只存 sha256），但不是本 issue 的验收面。
