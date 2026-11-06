# 存量 issue 处置批次（11 个 open，基线 HEAD 7cb2e88）

> 以**当前代码**为准。结论来自 8 路 scout 侦察（两轮：时效裁决 + 逐行改动清单）。
> 每条裁决附 file:line，worker 照抄即可，不需要重新侦察。

## 0. 裁决总表

| # | 主题 | 代码现状 | 处置 |
|---|---|---|---|
| 270 | pending 无 abandon | 已修 `5ba1c81`（orphan.go:161 + /v1/orphans + e2e） | **close** |
| 249 | 微秒写进 latency_ms | 已修 `5ba1c81`（middleware.go:99 + 护栏测试） | **close** |
| 254 | ValidateKey 不查 Disabled | 已修 `5ba1c81`（apikey.go:231-235 + gen bump） | **close** |
| 268 | sw.js 未指纹 icon | 已修 `5ba1c81`（hash.mjs:196-233 改写+硬校验） | **close** |
| 219 | pending 只靠 5s 巡检 | 已修 `5ba1c81`（Nudge），但有两个缺口（见 W3） | **close** + 开 N2 |
| 220 | run.concurrency 不参与调度 | v2 已删该字段（config.go:295 恒 1） | **close** + 开 N3 |
| 265 | 统计漂移不自动刷新 | 实现了 6h 维护（stats.go:34），但**机制可能无效** | **保持 open**，W4 取证后定 |
| 252 | 单连接注释 + idx_edges_lookup | 注释仍在；索引冗余已证实未删；**挖出真竞态** | **body 重写**，W2 修 + N1 |
| 274 | 移除演示模式 | 功能已修 `fea34b1`，残留死代码 + e2e 空转 | **body 重写**，W1 收尾 |
| 273 | 测试通知面板 | 面板已删；两条待定，其中「播种源」被 PUSH_UI 卡死 | **body 重写**，不做代码 |
| 264 | 索引写放大 | 仍有效，刻意搁置（先干净复测） | **不动** |

新开：N1（auth 竞态，critical）/ N2（#219 缺口）/ N3（perf 脚本 v1）/ N4（docs/perf 漂移）。

---

## 1. GitHub 记账（可直接执行）

```bash
cd /root/.herdr/worktrees/anotify/power

# ① close 6 个已修未关的（附修复提交）
for n in 270 249 254 268 219 220; do
  gh issue close $n --reason completed --comment "..."
done
```

关闭评语（逐条，别写模板话）：

- **#270**：已由 PR #277（`5ba1c81`）落地：`AbandonPendingTask`（CAS `pending→abandoned`，orphan.go:161）、`GET /v1/orphans`、`POST /v1/tasks/{id}/abandon`、单测 `orphan_test.go:163` 覆盖「abandon 后 `MIN(created_at)` 解开」、e2e `suites/orphans.mjs`。刻意不做自动 abandon（orphan.go:26-27）。残留：无 UI 入口（issue 允许只读端点），`docs/perf/capacity-and-backpressure.md:257,381` 文案过期 → 由 N4 收。
- **#249**：已由 `5ba1c81` 修（middleware.go:99 `µs/1000`，字段名保持 `latency_ms`；护栏 `middleware_latency_test.go:23`，25ms handler 断言 20..1000）。
- **#254**：已由 `5ba1c81` 修（apikey.go:231-235 fail-closed；admin 禁用 → `BumpGeneration`（admin_handlers.go:296）；负结果不缓存；`TestAPIKeyRejectedWhenUserDisabled`）。注意 issue 里的 sqlite3 直改库复现路径仍最长 60s 生效（key 缓存 TTL，`apikey_cache.go:44`）——那是刻意的有界设计，不是本 issue。
- **#268**：已由 `5ba1c81` 修，且采用的正是 issue 推荐的方案 1：`hash.mjs:196-233` 改写 `sw.js` 内的资源引用 + **构建期硬校验**（引用不存在即 `throw`）。
- **#219**：核心问题已由 `5ba1c81` 修（`Sweeper.spur` + `Nudge()`，mux.go:364 接线，实测 p99 89.3s→0.33s，CONTRACT §3.3 已写）。剩余两个缺口另开 N2：① `CancelTask` 释放槽位不 Nudge（task.go:1441-1445，与 CONTRACT.md:654 的措辞不一致）；② `ErrNoCapacity` 走 mux.go:339-347 写 `not_before=now+RetryBackoff(attempts)`，对 `attempts≥1` 的任务 = 10s，即时重派退化。
- **#220**：v2 已把 `capabilities[]`（含 `run.concurrency`）从 node.yaml 删除，`config.go:295` `slotConcurrency()==1`，老配置 `KnownFields(true)` 启动即拒。1×8 配置已造不出来，issue 描述的「静默失效」面消失。残留（脚本仍是 v1 yaml、契约 min() 公式、devseed 发 concurrency 4）另开 N3。

```bash
# ② 三条 body 重写（narrow 到当前真实剩余）
gh issue edit 252 --body-file .pi-orchestrator/issue-252-body.md
gh issue edit 274 --body-file .pi-orchestrator/issue-274-body.md
gh issue edit 273 --body-file .pi-orchestrator/issue-273-body.md
```

```bash
# ③ 新开 4 条
gh issue create --title 'bug(auth): 并发首注册两个都成 admin —— UserCount 与 RegisterUser 是两次独立池操作（#240 连接池放开后）' --body-file ...
gh issue create --title 'perf(dispatch): #219 的两个缺口 —— cancel 释放槽位不 Nudge / ErrNoCapacity 被写成 10s 退避' --body-file ...
gh issue create --title 'chore(perf): scripts/perf/b 仍生成 v1 node.yaml —— #220 的复现脚本已跑不起来（含 demo/chain.sh、devseed）' --body-file ...
gh issue create --title 'docs(perf): docs/perf 一批结论已随 #240/#242/#249/#270 过期' --body-file ...
```

---

## 2. 代码批次（7 个 bundle，文件所有权互斥）

### W1 · #274 残留清理（web，无行为变更）

**所有权**：`web/partials.js`、`web-src/pages/{trace,capabilities,node,sources,source,security}.html`、`web-src/locales/{zh-CN,en,ja,es}.yaml`、`scripts/e2e/suites/i18n_coverage.mjs`

1. `web/partials.js`：`:1569` 注释去掉「失败时回退到 demo」；`:1664` `api(path, opts, demo)` → `api(path, opts)`；**删 `:1731-1732` 两行回退**（`if (demo && …) return demo;` / `if (demo !== undefined) return demo;`）。
   > 这是唯一真正能复活假数据的通道：传 `null` 当第 3 参会吞掉所有失败。全仓 0 个真实 3 实参调用点。
2. 删 10 处 `{ demo: null }`（落在 opts 里、对 fetch 无效，纯安慰剂）：
   `trace.html:697`、`capabilities.html:1411`、`node.html:473`、`sources.html:822,883,884`、`source.html:382,383,384`、`source.html:759-762`（删 `demo: null,` 一行）。
3. 同步两条撒谎的注释：`sources.html:2596-2597`（改为「api() 失败即抛，错误必须在本页显式处理」）、`capabilities.html:1898-1899`（签名去掉 demo）。
4. `security.html` 删三段**不可达**分支：`:1474-1481`、`:1527-1532`、`:1766-1771`（gate `loadFailed.passkeys` 时列表已被错误卡替换并早退，按钮不存在）；`:1582`、`:2047` 注释去掉「demo 模式」措辞。
5. locales 四语删 56 条/语言（行号四语一致，**按行号从大到小删**）：
   `1080-1081`、`638-655`、`635`、`630`、`627`、`543-547`、`533`、`522`、`451-456`、`434`、`348`、`341`、`325`、`190-206`、`145`、`133`。
   **必须保留**：`sources.form.demo*`（5 条，「填入示例」按钮在用）、`admin.users.demote*`。
6. `i18n_coverage.mjs`：删 `:223`、`:257`、`:424` 的 `?demo=1`（断言只读渲染文本与 pageerror，与 query 无关）。

**不做的**（写进 issue body，防止下一轮重开）：活文案仍在说「演示模式」（`keys.error.create_failed`、`security.alert.backend_down`、`passkeyenroll.panel.error_backend_desc`）—— 那是内容修复，另开条；`i18n_coverage` A 组**没有 `injectSession`**，受保护页会 401 跳 login，实际只扫了 login/docs —— 这是测试空转，需单独修（见 #274 新 body 的「遗留」）。

**verify**：`make fe && ./scripts/e2e/run_all.sh --serial frontend i18n i18n_coverage p2_guards`

---

### W2 · #252 索引 DROP + 过时注释（store/schema）

**所有权**：`internal/store/{schema.sql,migrate.go,task.go,node.go,authz_test.go,schema_baseline_test.go,seq_in_trace_test.go}`、`internal/lease/{sweeper.go,sweeper_test.go}`、`internal/server/admin_handlers.go`
（**不碰** `internal/store/store.go`、`internal/lease/stats.go`（归 W4）、`internal/auth/webauthn.go`（归 W7））

1. `schema.sql` 末尾追加：

```sql
-- +migration 6 drop_edges_lookup

-- idx_edges_lookup 的四列是 edges 主键的**严格左前缀**（PRIMARY KEY 前四列逐列相同），
-- 给不了 sqlite_autoindex_edges_1 给不了的东西：派发 4 列等值实测一直选 autoindex，
-- 3 列前缀查询删掉它也仍由 autoindex 以同一 search shape 承担（无全表扫）。
-- 删掉只少一份等价的 B-tree 维护成本。IF EXISTS 保证重放旧块时不报错。
DROP INDEX IF EXISTS idx_edges_lookup;
```

2. `migrate.go:38` `headVersion = 5` → `6`；`schema.sql:8-9` 的头注释「五块」→「六块 + `6 drop_edges_lookup`」。
3. `schema_baseline_test.go`：删 `:205-207`（注释 + 表项）；`:190` 「27 条」→「26 条」。
4. **地雷**：`seq_in_trace_test.go:415` 用的是 `headVersion` 当「migration 5」——改成字面量 `5` + 注释「迁移 5 = seq_in_trace；别用 headVersion（随新增块漂移）」，否则 `go test ./internal/store/` 会以 `no such column: seq_in_trace` 报错，极易误判成 DROP 引起。
5. 过时注释（统一原则：**保留风险提醒**，把「只有一根连接/会抢连接」换成真理由）：
   - `task.go:866` / `:1083-1084` / `:1187-1188` → 「本函数已在事务里，读必须走同一事务句柄；走池既读不到未提交写，又多占一条连接」
   - `node.go:1319-1320` → 「经 BEGIN IMMEDIATE 串行化后仍不可达；连接数已放开到 8，这是并发/跨进程的最后一道闸」
   - `node.go:1246-1247` → **不是措辞问题**：`LeaseInstance` 的查空槽与 CAS 占位都走池、无事务，安全来自 CAS 重试（:1248/:1273），不来自池大小。按此改写。
   - `authz_test.go:54-55` → 「开着写事务（BEGIN IMMEDIATE 持写锁），断言必须同 block 做完并立刻回滚」
   - `admin_handlers.go:294` → key 缓存默认 **60s**（`apikey_cache.go:44`），不是 10s（10s 是会话缓存）。`:298` 的「10s」是对的，别改。
   - `sweeper.go:58` + `sweeper_test.go:274`（同一句的测试副本）→「把 SQLite 写队列打满」
6. `node.go:1238` 的 min() 注释改成「实际并发由 instances 决定，每槽一个任务；`node_instances.concurrency` 与 `nodes.max_concurrency` 目前只记录、不参与调度」。CONTRACT.md:254 的同一句交 W5。

**verify**：`go test ./internal/store/... ./internal/ws/... ./internal/lease/...` + `make contract`

---

### W3 · #219 两个缺口（server/dispatch）

**所有权**：`internal/server/{mux.go,console_handlers.go}`、`internal/route/dispatch.go`（+ 各自测试）

1. **cancel 释放槽位要 Nudge**：`consoleHandler` 加一个窄依赖字段（照 `taskLogs` 的写法）：`nudge func()`；`mux.go:209` 构造处注入 `consoleH.nudge = sweeper.Nudge`（sweeper 在 mux:319 已创建，注意构造顺序）；`console_handlers.go:688` cancel 成功后调用（nil-safe）。
   理由：CONTRACT.md:654 明说「任务终态落库（槽位释放）→ 立即重派」，而 cancel 是唯一没接的终态路径（task.go:1441-1445 释放槽位后无人知晓），最坏 5s 空槽。
2. **ErrNoCapacity 不该吃坏节点退避**：`RedispatchTask` 现在两处 `return false, nil`（dispatch.go:1589-1593 无节点声明 / :1623-1626 无容量），mux.go:339-347 一律 `RetryBackoff(attempts)` → 对 `attempts≥1` 写 `now+10s`，把即时重派退化成定时。
   改法：让 `RedispatchTask` 在 `ErrNoCapacity` 时返回 `store.ErrNoCapacity`（`(false, err)`），mux 里 `errors.Is` 分支**不写退避**（容量不是节点故障）；「无节点声明」维持原退避。
   风险：容量不足的 pending 在每次 Nudge 都会被重试一次 → Nudge 已 20ms 限频 + 单轮 BatchLimit 100，可接受；但需在契约里写明。
3. 测试：① fake `nudge` 被 cancel 调用一次（`console_handlers_test.go`）；② `attempts=1` 的任务在无容量后 `not_before` 仍为 NULL，且下一次 Nudge 能把它捞出来（`sweeper_test.go` 或 `mux` 级测试）。

**verify**：`go test ./internal/server/... ./internal/route/... ./internal/lease/...`

---

### W4 · #265 先证伪再改（store stats）

**所有权**：`internal/store/store.go`（stats 段 120-280）、`internal/lease/stats.go`、`internal/store/stats_test.go`

**关键怀疑**：`MaintainStats` 的实现跑的仍是 `PRAGMA optimize=0x2`（store.go:230），而同一文件的注释（:136-141）明说 `0x2` **够不到**运行期漂移（需要 `0x10000` 且本会话查过该表）。⇒ 6h 维护可能是**安慰剂**，issue 只在字面上被修。

1. 先写证伪测试（文件库）：建库 → 灌 100 行 → `ANALYZE "tasks"` → 断言 stat1=100 → 再灌 2400 行（21×）→ `db.MaintainStats(ctx)` → 读 stat1。
   - 若 stat1 **没变** → 实现无效，改口径：漂移由显式 `ANALYZE "tasks"` 触发（当建议为空但写入量/距上次分析超过阈值时），保持可关 + 打日志；
   - 若 stat1 **变了** → 保留实现，把这条测试留下当证据，关掉注释里的矛盾叙述。
   可复用夹具：`newTestDBFile`（store_testutil_test.go:16）、`newTestNS`（:33）、`mkCap`（capability_test.go:11）、`mkSeqTaskBatch`（seq_in_trace_test.go:30，一次事务批量插）、`pendingStat1Entries`（pending_backlog_test.go:293）。2400 行 ≈ 0.3-0.6s，可当单测。
2. 顺手修 4 处注释矛盾：`store.go:127`（「缺失或过期」→ 只承诺缺失）、`:150`（「保留全部判定规则」与 `:132` 互斥）、`:157`（说走 `db.Exec`，实际 `QueryContext`）、`:136-141`（「那是另一个 issue」→ 指向 #265 实现）。
3. **不要**为了刷新统计去掉 `0x2`：`0x10`（降级成 2000 行采样）的坑注释里写清了。

**verify**：`go test ./internal/store/ -run 'Stats|Maintain|PendingBacklog' -v` + `go test ./internal/store/ ./internal/lease/`

---

### W5 · 文档与契约收口（**最后跑**，唯一 owner of CONTRACT.md）

**所有权**：`docs/contracts/CONTRACT.md`、`docs/perf/*`、`DEVELOPMENT.md`

1. `CONTRACT.md:337-338`：删 `idx_edges_lookup` DDL，改一句「不额外建索引：四列是主键严格左前缀，`sqlite_autoindex_edges_1` 已覆盖（#252）」。
2. `CONTRACT.md:254`：min() 公式改写（见 W2-6）。
3. `CONTRACT.md §3.3`（:654-660）：补「cancel 也走即时重派」与「容量不足不退避（退避只用于节点故障）」两句。
4. `docs/perf/capacity-and-backpressure.md:257,381-382`：「全仓没有任何 abandon 路径」→ 改为「无**自动** abandon（刻意），但有 `POST /v1/tasks/{id}/abandon` + 巡检侧自动 abandon」；`:185-186` 的「迁移 17」→ baseline `schema.sql:415`。
5. `docs/perf/path-a-http-api-store.md`：`:13-15`（连接池钉 1 → 现 8）、`:21`（SQL 每请求重 parse → #242 语句缓存）、`:94-102/:171-185`（「单连接串行瓶颈」前提已消失，保留数字但标「#240 已修复」）、`:212`、`:215-216`（latency_ms 已修 #249）。

**verify**：`make contract`

---

### W6 · perf / demo 脚本回到 v2（低风险，可独立）

**所有权**：`scripts/perf/b/*`、`scripts/perf/3b/nodes.sh`、`scripts/demo/chain.sh`、`cmd/devseed/demo.go`

1. `scripts/perf/b/lib.sh:178-194`（`pb_node`）与 `:212-219`（`pb_node_create`）：删 `capabilities:` 整段（含 `run.concurrency`），只留 `name/server/token/instances`；形参 `concurrency` 标「v2 每槽恒 1，已无意义」（或删形参并同步 6 个调用点）。
2. `s2-roundtrip.sh:2,7-10,22-28`：b/d 两臂（concurrency 维度）作废，降为 a（1 槽）/ c（8 槽），并注明 #220 结论。
3. `scripts/perf/3b/nodes.sh:74`、`scripts/demo/chain.sh:177` 同改（后者是活目标 `make demo-chain`，优先级高）。
4. `cmd/devseed/demo.go:238,245`：hello 的 `capabilities` 键与 `concurrency: 4` 一起改成 v2 形状（`concurrency: 1`）；顺手核对 `"protocol": 1` 与 v2 的兼容（`node_handshake.go:250` 对 protocol≠2 拒收 → 疑似 devseed 已不可用，需实跑确认）。

**verify**：`bash -n scripts/perf/b/*.sh scripts/demo/chain.sh` + `make build && timeout 180 bash scripts/perf/b/s0-smoke.sh`（若环境不许，至少 `go test ./cmd/devseed/... ./cmd/agent/...`）

---

### W7 · N1 并发首注册双 admin（**安全，独立 commit**）

**所有权**：`internal/auth/webauthn.go` + `internal/auth/*_test.go`

现状：`webauthn.go:224` `UserCount` 与 `:240` `RegisterUser` 是两次独立池操作，中间无事务；`:219-220` 的注释声称「单连接使两者串行化」——池放开到 8（#240）后该不变量已假 ⇒ 两个并发「首个注册」都可能读到 `count==0`，**双双拿到 `RoleAdmin`**。

- 修法：把 `SELECT count(*)` 挪进 `RegisterUser` 的同一事务（它本来就原子），据此决定角色；注释改为「同事务判定」。
- 测试：文件库 + 两个 goroutine 并发注册 → 断言恰好一个 admin（`:memory:` 会强制单连接，测不出来，必须文件库）；`-race` 跑。

**verify**：`go test ./internal/auth/... -race -run 'Register|Admin|First'` + `go test ./internal/auth/...`

---

## 3. 依赖与合并顺序

```
并行：W1 (web)  W2 (store/schema)  W3 (server/dispatch)  W4 (stats)  W6 (scripts)  W7 (auth)
  ↓ 全部合入
串行：W5 (CONTRACT.md + docs/perf，唯一 owner)
  ↓
全量门禁：make ci && make e2e
```

冲突面已按文件切开；唯一共享文件 `docs/contracts/CONTRACT.md` 只给 W5。
W3 需要 W2 的 `store.ErrNoCapacity` 语义——它已存在（不必等）；但 W3 的契约文字由 W5 落地。

## 4. 需拍板的三件事

| # | 决策 | 建议 |
|---|---|---|
| D1 | #273「给新命名空间播种手动通知源」做不做 | **不做**。播了也点不通：① `notify.phone` 被 `PUSH_UI_ENABLED=false` 隐藏（partials.js:2087），编辑源时目标候选列表已过滤 → **一保存就静默删掉该目标**；② fire 发 `{}`，`required:["to","title"]` 不满足 → 必被 `param_schema` 剪枝。先把「手机推送是否开放」定了再说，issue 里写明被它阻塞。 |
| D2 | `node_instances.concurrency` 是否封顶 1 | **不封顶**。agent 已恒发 1（config.go:295），封顶会红 2 个用例（store/node_test.go:284、ws/node_test.go:481）且与「节点申报」语义冲突。改为把契约的 min() 公式改对（W5）。 |
| D3 | W6 的 perf 脚本：修还是删 | **修**（`s0-smoke` 是回归入口），但 `scripts/perf/b/REPORT.md` 与 `docs/perf/*` 的历史数字**不改**，只加「#220 后 concurrency 已不存在」的注记。 |
| D4 | W4 若证伪（0x2 刷不到漂移）怎么办 | 按「显式 ANALYZE + 阈值 + 可关」实现，并把 #265 的验收（100→100 万行走错索引）落成 `scripts/perf` 的可复跑脚本，不进单测（100 万行 ≈ 2 分钟+）。 |

## 5. 风险

- **`seq_in_trace_test.go:415`** 是最容易踩的地雷（`headVersion` 被当 migration 5 用），报错形如 `no such column: seq_in_trace`。
- 迁移块名只能用 `\w+`（`migrate.go:41` 正则），写 `drop-edges-lookup` 会让解析把它当 body。
- baseline（migration 1）的 body 一个字都不能动；新库「先建后删」是正常路径。
- `migrate.go:116` 的 hooks 是 `nil` 字面量，DROP 不需要 hook，**不要动**。
- 前端 `web/*.html`、`internal/server/dist` 全是 gitignore 产物：改完必须 `make fe`，否则 e2e 吃陈旧 embed。
- `make fe` 的 `check-classes` 会拦自造类名；W1 只删不加类，风险低。
- #265 的语义未定前，不要写「漂移会被自动修复」的断言（会写出永远绿的用例）。
