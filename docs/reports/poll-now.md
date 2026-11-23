> **历史快照**：本文件是本次交付的记录，不随代码演进；行为以代码、测试与 `api/` 为准。

<!-- snapshot-doc: 2026-11-23 #555 交付时的定格数字（make ci / make e2e-affected 的真实输出），复算命令见各节 -->
# poll 源的「立即拉取一次」（#555）交付报告

> 分支 `pi-subagent/host-workspace/anotify-555-pollnow-ff8935e1` · base `main` `11ac6ccc` · PR 见 §8
> issue 冻结的端点形状、六条语义与 9 条验收在本文件里逐条对上号（§4 / §5）。

一句话：**给「拉」加一个手动扳机，但不加第二条拉取路径** —— handler 只做投影，真正跑一轮的
是后台排期用的那个 `Poller` 实例上的同一个函数；差别只有一处写库参数（不写 `next_fire_at`）。

---

## 1. 落点

| 层 | 文件 | 落点 |
| --- | --- | --- |
| 执行 | `internal/source/poll.go:158` | `PollOnce(ctx, src)`：**先拿 `begin/done` 这把锁**（与 `Sweep` 同一把），再调 `roundOnce(ctx, src, manual=true)` |
| 执行 | `internal/source/poll.go:175` | `roundOnce` 是**一轮拉取的唯一实现**：抓取 → `envelope.items` 解包 → 逐条 `Ingest` → 游标前进 / 失败退避。定时轮询（`round`）与手动触发都走它 |
| 执行 | `internal/source/poll.go:142` | `ErrPollInFlight`；`PollResult{NotModified,Items,Created,Duplicates,DurationMs,Cursor,CursorAdvanced}` |
| 数据 | `internal/store/source.go:728` | `PollOutcome.KeepNextFire`：`next_fire_at = CASE WHEN ? THEN next_fire_at ELSE ? END` —— 手动触发只跳过排期这一列，游标 / `lastPollAt` / `lastFiredAt` / 失败计数与错误照写 |
| 数据 | `internal/store/source_audit.go` + `schema.sql`（migration 9） | `source_audit`（append-only）：一行 `source.polled_manually`（actor + 本轮计数摘要） |
| HTTP | `internal/server/resource_source.go:98` | `POST /v1/namespaces/{ns}/sources/{name}/poll`（`noStore(session(backpressure(...)))`，与 `fire` 同形态） |
| HTTP | `internal/server/resource_source.go:401` | handler `poll`：ns 授权 → 取源（404）→ `kind != poll` → 409 `not_poll_source`；`!enabled` → 409 `source_disabled`；然后 `PollOnce` |
| HTTP | `internal/server/resource_source.go:433` | `writePollError`：`ErrPollInFlight` → 409 `poll_in_progress`；其余 → 502 `poll_failed`（`message` = 本轮错误原文） |
| 契约 | `api/ingress.yaml`（新 operation `pollSourceNow`）· `api/resource.yaml`（新 schema `SourcePollResult` + `Status409` / `Status502` 的 description 补 reason） | 200 `{items,created,duplicates,durationMs,cursorAdvanced}` |
| UI | `web-src/pages/source-detail.html`（`pollNowRow`）· 四份 locale · `web/mock.js` | 「拉取」区块的「立即拉取」按钮（仅 `kind=poll` 且 `enabled`） |
| e2e | `scripts/e2e/suites/routing.mjs` §15（`15.1`–`15.6`）· `scripts/e2e/lib/v4.mjs` `P.sourcePoll` | 成功计数 + 事件页 / 并发 409 / 失败 502 且游标不动 / 非 poll 源 409 / 未登录 401 / 停用 409 |
| e2e(UI) | `scripts/e2e/fixtures/f3/f3-source-poll.mjs` | `POLL-10` / `10b` / `10c` |

`Poller` 曾只存在于 `mux.go` 的后台循环里（局部变量）。这次它被注入 `serverDeps.Poller`
（`internal/server/api_deps.go`）——**同一个实例**这一点是 409 语义的前提：两把锁就是两套并发。

## 2. 六条语义怎么落成代码

1. **同一把锁**：`PollOnce` 用的是 `Sweep` 的 `begin/done`（`poll.go:126/136`），在飞直接返回
   `ErrPollInFlight` → 409。**不排队**：没有队列、没有等待，第二次点击立刻拿到结论。
2. **同一份代码路径**：`roundOnce` 是唯一实现；`round`（定时）与 `PollOnce`（手动）只是两个薄壳。
   判定「成 / 败」的分支只有一处（`if err != nil {…} else {…}`），不可能两处走岔。
3. **失败不前进游标**：失败分支不设 `out.Success`，SQL 的 `COALESCE(?, poll_cursor)` 保住旧值
   ——与定时轮询同一条不变量，没有新增第二个判定点。
4. **SSRF 三道闸照常生效**：手动路径没有自己的 `http.Client`，走的是 `fetch` → 写入期
   `sourcespec.CheckPollURL` / 解析预检 `checkResolvedHost` / dial 闸 `blockPrivateDial` 全在同一处。
5. **审计一行**：`AppendSourceAudit`（`source_audit`，action 在封闭集合内）。审计写失败只记
   日志、**不**把响应改成 500——那一轮已经真的跑完了，报错会让调用方误以为没拉。
6. **不改排期**：`KeepNextFire` 让 `next_fire_at` 原样保留（成功与失败都是），`failureNextFire`
   算出来的值被丢弃。手动触发不是「重排」。

> **一个范围内的决定**（issue 没写，写在这里以免被当成缺失）：已停用的源 → **409
> `source_disabled`**。`fire` 对停用源回 404 是为了防 token 探测（webhook 入口），而这里路径里
> 的源名本来就要先过会话鉴权，回一个能读懂的原因比「假装不存在」更有用。已写进
> `api/resource.yaml` 的 `Status409` description，单测与 e2e 各一条断言。

## 3. 五件套

| 面 | 位置 | 状态 |
| --- | --- | --- |
| spec | `api/ingress.yaml` `pollSourceNow`；`api/resource.yaml` `SourcePollResult` | ✅ `check_openapi.py`：5 份文件 / **144** operation / 144 个唯一 operationId |
| parity | `internal/server/openapi_parity_test.go`：`parityPairs` 加 `srcPollResultDoc ↔ SourcePollResult`；快照同步 `muxPatternCount 132→133`、ops `143→144`、`parityPairsFloor 217→218`、`specSchemaFloor 257→258` | ✅ `TestOpenAPIParity*` 全绿（含 security 探针：新路由无凭据必须 401） |
| DTO | Go：`srcPollResultDoc` / `srcPollAuditDetail`（`resource_source.go`）；前端：`web/partials.js` 的 `modelSource` **未动**（按钮只读已建模字段，不新增 `status.X`） | ✅ `check-wire-fields` 绿 |
| e2e | `routing.mjs` §15（六段）+ `f3-source-poll.mjs` POLL-10/10b/10c | ✅ 见 §6 |
| 单测 | `internal/source/poll_now_test.go`（5 条）· `internal/server/resource_source_poll_test.go`（4 条）· `internal/store/source_audit_test.go`（2 条） | ✅ 见 §5 |

## 4. 9 条验收 → 证据

| # | 验收 | 证据 |
| --- | --- | --- |
| 1 | 手动拉一次 → 计数正确；事件页出现这批事件 | e2e `15.1`（`items=2 / created=2 / duplicates=0 / cursorAdvanced=true`，事件页 2 条 + `deliveryId=a,b` + `eventType=advisory`）· 单测 `TestPollOnceReportsCountsAndDedupes` |
| 2 | 立刻再点一次（慢 URL）→ 409 `poll_in_progress`；放行后再点成功 | e2e `15.2`（feed 挂住不 `end()`，第一次在飞时第二次 409，放行后 200，锁释放后再点 200）· 单测 `TestPollOnceAndSweepShareOneLock` · handler `TestSourcePollNowInFlightConflict` |
| 3 | 写错 `envelope.items` → 502 `poll_failed`，之后 `status.cursor` 没变 | e2e `15.3`（先成功拿到 `cursor="e1"`，再喂新 ETag + 结构里没有 items → 502 + `cursor` 逐字未变 + `consecutiveFailures=1`）· 单测 `TestPollOnceFailureKeepsCursorAndSchedule` · handler `TestSourcePollNowFailureKeepsCursor` |
| 4 | 非 poll 源 → 409 `not_poll_source` | e2e `15.4` · handler `TestSourcePollNowRejectsNonPollAndDisabled` |
| 5 | SSRF：运行期私网同样被拦（复用 #533 的夹具） | 单测 `TestPollOnceRefusesPrivateHostAtRuntime`（`WithResolver` 返回 `127.0.0.1` + 非 allowInsecure → 一轮失败、游标不动、0 事件）；写入期拒绝仍由 e2e `14.1` 六种形状覆盖 |
| 6 | UI：按钮出现 / 禁用 / 结果回显 / 409 提示各一条断言 | `f3-source-poll.mjs` `POLL-10`（出现 + 点一次只发一次无 body 的 POST + 在飞 disabled + 四项计数回显）、`POLL-10b`（409 `poll_in_progress` → 「正在轮询中，稍等」）、`POLL-10c`（非 poll 无按钮、停用无按钮） |
| 7 | 变异 ①/② 必红 | §7 |
| 8 | `make ci` exit 0；`make e2e-affected` 绿；known-env 空；注释密度不上调 | §6（`make ci` **2m44.5s / EXIT=0**；e2e 见同节）；`scripts/e2e/known-env-failures.json` 的 `entries: []` 保持为空（这次没有新增任何豁免）；注释行棘轮实测**与基线完全一致** |
| 9 | 五件套 | §3 |

## 5. 单测（新增 11 条）

- `internal/source/poll_now_test.go`：计数与去重、失败保游标且 `nextFireAt` 逐字不变、
  手动与 `Sweep` 共用一把锁（在飞时 `Sweep` 只记 `skippedOverlaps`）、304 不动游标、运行期私网拒绝。
- `internal/server/resource_source_poll_test.go`：200 五字段 + 真的入库 + 一行审计（action/actor/detail 计数）、
  409 `not_poll_source` / 409 `source_disabled`、在飞 409 且放行后可再触发、502 保游标。
- `internal/store/source_audit_test.go`：`KeepNextFire` 只跳过排期那列（成功与失败都验）、
  审计行的封闭动作集合 / 非法 detail / 缺命名空间。

## 6. 门禁实测

```
$ PATH=/usr/local/go/bin:$PATH timeout 1500 make ci
✅ 契约静态断言（A2/A4/A5/A6/A7 + openapi 一致性 + 字段级核对 + anode 可编译）通过
共 48 项判定：PASS 48 / FAIL 0
real	2m44.516s
EXIT=0
```

（同时有另一个 worktree 在跑 `make ci`，本数字含 CPU 争用。）

```
$ PATH=/usr/local/go/bin:$PATH timeout 2400 make e2e-affected
  改动文件: 28 个（api/ingress.yaml 落在共享面 → 全量）
  ⚠️ 全量 e2e（30/30 套）
  总耗时: 441.8s
  断言通过: 3229
  断言失败: 0
  case 级重试: 首次 incomplete 0 条 · 首次 timeout 0 条 · 重试通过 0
  套件级重试: 0 个套件
✅ E2E 全绿：30/30 个请求套件均达标（断言 3229 通过 / 0 失败，authority 清单覆盖 30 个）

  ✅ routing            459通过/0失败  9.4s  mode=real      ← 含新增 §15 的 34 条（原 425）
  ✅ frontend            46通过/0失败  218.8s mode=real     ← 含新增 POLL-10/10b/10c
real	7m28.802s
EXIT=0
```

`known-env-failures.json` 的 `entries` 仍为 `[]`（这次没有新增任何环境性豁免）。

## 7. 变异自证（两条必红）

**变异 ①：手动路径绕过 in-flight 锁** —— 把 `PollOnce` 里的 `begin/done` 四行删掉：

```
--- FAIL: TestPollOnceAndSweepShareOneLock (0.16s)
    poll_now_test.go:142: 在飞时的第二次手动拉取 = <nil>，want ErrPollInFlight
--- FAIL: TestSourcePollNowInFlightConflict (0.14s)
    resource_source_poll_test.go:176: 在飞时的第二次触发期望 409，得到 200：{"items":2,…,"cursorAdvanced":true}
```

**变异 ②：手动失败也前进游标** —— 在 `roundOnce` 的失败分支里补 `out.Success = true; out.Cursor = fetched.cursor`：

```
--- FAIL: TestPollOnceFailureKeepsCursorAndSchedule (0.13s)
    poll_now_test.go:100: 失败轮把游标推进了："\"e1\"" → "\"e2\""（失败不前进游标）
--- FAIL: TestSourcePollNowFailureKeepsCursor (0.14s)
    resource_source_poll_test.go:219: 失败轮推进了游标："e2"
```

两处变异都当场变红（不是挂死）：并发测试的 feed 改成**只挂第一轮**，后续请求立刻返回，
所以「绕过锁」会得到 200 / `nil` 而不是等一个永远不来的信号；放行的 defer 排在
`httptest.Close` 之后注册（LIFO 先跑它），断言失败也不会把 `Close` 挂住。

## 8. 交付

- 分支：`pi-subagent/host-workspace/anotify-555-pollnow-ff8935e1`（base `main` `11ac6ccc`）
- PR：正文 `Fixes #555`（未自行合并）
- 注释行棘轮：新增的说明按仓规在**同目录**用既有冗余叙述换掉（`internal/server` / `internal/store` /
  `internal/source` / `api` 四个目录实测与基线逐字一致），基线未上调。

## 9. 不做（与 issue 一致）

- 手动拉取的历史记录（只有审计行，没有读端点）
- 批量触发多个源
