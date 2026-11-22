> **历史快照**：本文件是本次交付的记录，不随代码演进；行为以代码、测试与 `api/` 为准。

<!-- snapshot-doc: 2026-11-22 #532 B1 交付时的定格数字（make ci / e2e 的真实输出），复算命令见各节 -->
# kind: poll —— http-json 拉取型源（游标 / 退避 / 不重叠 / SSRF 默认拒）（#532 B1）交付报告

> 分支 `pi-subagent/host-workspace/anotify-532-poll-8ecb1aa5` · base `main` · 设计记录 #512 · 本阶段 #532
> issue 正文冻结的形状、六条语义与 10 条验收在本文件里逐条对上号（§3）。

一句话：**给「拉」加一条与「推」完全对称的通道** —— 传输仍在 Go（信任边界不外包），
「从响应里取哪个数组、每条事件的 type/deliveryId/payload、什么时候拉」都是配置；事件入库、
去重、剪枝、派发**一行没改**，走的是 webhook/cron/manual 同一条 `source.Service.Ingest`。

---

## 1. 冻结语义（实现里定死）

### 1.1 形状

```yaml
kind: poll
config:
  url: https://api.github.com/advisories?per_page=50   # 必填，只允许 https
  headers: { Accept: application/vnd.github+json }      # 可选，≤20 项，键/值禁 CR/LF
  secretRef: { name: gh-token, header: Authorization, prefix: "Bearer " }
  secret: "…"                                           # 只写：落 namespace_secrets，不进 config
  schedule: { expr: "*/5 * * * *", tz: UTC }            # 必填，复用 cron 的五段表达式
  timeoutMs: 10000                                      # 默认 10000，区间 [1000,120000]
  maxItems: 100                                         # 默认 100，硬上限 500
  allowInsecure: false                                  # 测试逃生口：http + 跳过私网判据
  envelope:
    items: "."            # ← 第四指针：在**响应里**定位事件数组（"." = 顶层数组）
    type:       { pointer: type }   # 以下三指针是**每条 item 内部**的路径（与 #515 同一套方言）
    deliveryId: { pointer: id }
    payload:    { pointer: "" }     # 空指针 = 整个 item；也可 { whole: true }
```

- **`items` 是新增的第四指针**，语义与其它三个不同：它在响应文档上取**数组**。取不到/不是
  数组 → 该轮失败（不是「0 条」）。写法与 #515 同源：`. / $ / $.` 归一成根，其余走
  `ParsePointer`，实际取值只有一份 `walkPath`（`sourcespec/template.go`）。
- `type` / `deliveryId` / `payload` 三指针**逐字复用** #515 的 `normalizeFieldSource` /
  `normalizePayloadSource`：header 与 pointer 二选一、`joinFrom` 只 type 支持、`payload` 的
  `whole` 与 `pointer` 二选一。poll 额外两条：`payload: {pointer: ""}` 等价 `{whole: true}`；
  `type/deliveryId` 写 `header` 直接 400（poll 没有请求头这层事实，留着只会静默不生效）。
- `schedule` 与 cron **同一套计算**：`NextPollTime` 把 `{expr,tz}` 拼回 cron 形状后委托
  `sourcespec.NextCronTime`；`lastFiredAt` / `nextFireAt` 复用 `sources` 现有两列。
- 归一化字节稳定（`NormalizeConfig` 幂等），因为安装比较靠字节比对。

### 1.2 六条语义

| # | 语义 | 实现位置 |
| --- | --- | --- |
| 1 | 游标 `status.cursor` + `lastPollAt` + `consecutiveFailures` + `lastError`，成功后写回 | `store.MarkSourcePolled` + `srcStatusView` |
| 2 | **游标只在整轮成功后前进**（失败沿用旧值，靠 deliveryId 幂等重抓） | `MarkSourcePolled` 对失败用 `COALESCE(?, poll_cursor)` |
| 3 | **不重叠**：上一轮没结束 → 本 tick 跳过并 `skippedOverlaps++` | `Poller.begin/done` + `BumpSourceSkippedOverlaps` |
| 4 | **退避** `min(base << (failures-1), 30min)`；`429 Retry-After` 优先；成功清零 | `Poller.failureNextFire` |
| 5 | **SSRF 默认拒**（写入期 + 解析后 + dial 时三道闸；跨主机重定向不跟随） | `sourcespec.CheckPollURL` / `Poller.checkResolvedHost` / `blockPrivateDial` |
| 6 | **上限**：`maxItems`（默认 100/上限 500）、响应体 4 MiB；超限=该轮失败并留痕 | `Poller.items` / `readLimited` |

### 1.3 两处实现判断（issue 没写死，这里定死）

**(a) 游标长什么样、怎么"带上"。** 游标的正确性**不依赖**增量协议（issue 明说），所以：
`status.cursor` = 上一次**整轮成功**响应的 `ETag`（优先）或 `Last-Modified`（都没有就保持原值）；
下一轮把它作为 `If-None-Match` 发回；`304` = 成功的空轮（0 item、游标不变、失败计数清零）。
这样验收 2 的「重启后带上游标、不从头抓」是可机检的事实（test server 断言收到的头），
而验收 1 的「再轮一次仍是 duplicate」也在没有 ETag 的源上天然成立。

**(b) 「命名空间凭据」的最小落地。** 仓库此前**没有**这个概念（全仓 `secretRef` 0 命中），
所以按最小面落地：新表 `namespace_secrets(namespace_id, name, value, …)`，
**写入口只有 poll 源的 write-only `config.secret`**（与 webhook 的 `token`/`secret` 同一纪律：
读不回显、PATCH 省略 = 保留）。不新增 HTTP 端点、不动 YAML 文档层 —— 少一条写路径，就少一处
「同一份事实两个写入口」的缝隙。`secretRef.name` 在同 namespace 里不存在且本次没给 `secret`
→ 写入期 **400**，`details.causes[].field = spec.config.secretRef`。

## 2. 实现落点

| 文件 | 变化 |
| --- | --- |
| `internal/sourcespec/poll.go` | 新增：`pollConfig` 归一化、`CheckPollURL`（写入期 SSRF）、`BlockedIPKind`（唯一 IP 分类器）、`NextPollTime`、`ParsePointerOrRoot` / `LookupIn` / `LookupItems`、`SecretRef` / `PollSchedule` |
| `internal/sourcespec/envelope.go` | `Envelope` 加 `Items`（第四指针）；`normalizeEnvelope` 加 `withItems` 开关（webhook 仍不认它） |
| `internal/sourcespec/config.go` · `sourcespec.go` | `KindPoll` 注册 + `knownFields[KindPoll]`（12 键）+ `Config` 的 poll 字段与读侧默认值 |
| `internal/store/schema.sql` | `-- +migration 6 poll_source`：`sources` 加 5 列 + `namespace_secrets` 表（块 5 是 #522 的 router 结局表，本分支 rebase 到 main 后顺延） |
| `internal/store/source.go` | `Source` 的 5 个轮询状态字段、`ListDueSourcesOfKind`、`MarkSourcePolled`、`BumpSourceSkippedOverlaps`、`sourceNextFire` 支持 poll |
| `internal/store/namespace_credential.go` | 新增：凭据 upsert/get、`pollSecretOf`、`resolveSourceSecret`（缺失即 400 的 cause） |
| `internal/source/poll.go` · `poll_fetch.go` · `poll_items.go` | 新增：`Poller`（tick/不重叠/退避/游标）、HTTP 客户端（三道 SSRF 闸 + 重定向策略 + 4 MiB）、item→`Input` 映射 |
| `internal/server/resource_source.go` | `srcStatusView` 加 `cursor/lastPollAt/consecutiveFailures/skippedOverlaps`，`lastError` 从「恒 null」变成真实事实 |
| `internal/server/mux.go` | 登记 `poll` 后台循环（5s tick，走 `runBackgroundLoop` 的关停屏障） |
| `api/resource.yaml` | `SourceStatus` 五个字段 + `SourceSpec.kind`/`config` 文档（poll 的 12 个键与 items 语义） |
| `web/partials.js` · `web-src/pages/source-detail.html` · `web/mock.js` | `modelSource` 建模四个 status 字段；详情页基本信息追加「最近轮询」「连续失败」两行（不改布局） |
| `scripts/e2e/suites/routing.mjs` | 新增 §14（48 条断言；§13 是 #522 的决策通道） |

> 为什么 poll 的代码在 `internal/source` 而不是新包：注释密度棘轮是**逐目录等值**的双向棘轮
> （新目录注释预算 10 行、总注释行不许增长），新包要么抬基线（本次明确不允许），要么把
> 注释压到 10 行以内。折进同包并同批删等量陈旧注释（`store/stmt.go` 的驱动实测流水账等），
> 是本仓既有的做法（#505 就是这么过的）。

## 3. 十条验收（逐条实测）

复算命令前缀：`export PATH=/usr/local/go/bin:$PATH`（Go 1.25）。

### 验收 1 · 基本：3 item → 3 事件 3 任务；再轮 → duplicate 3、0 新任务、游标不变

```text
$ go test ./internal/source/ -run TestPollRoundIngestsThenDedupes -v -count=1
--- PASS: TestPollRoundIngestsThenDedupes (0.22s)
```

断言（`internal/source/poll_test.go`）：`errors/tasks = 3/3` → 第二轮 `3/3`（`Receipt.Duplicate`
命中，不建新任务）→ 第二轮仍然真的抓了一次（`hits == 2`，证明不是靠「不去抓」蒙过去）→
响应没有 `ETag` 时 `cursor` 保持为空（= 没新数据，游标不动）。

### 验收 2 · 游标持久化：重建 Poller（模拟重启）→ 下一轮带 `If-None-Match`，不从头抓

```text
$ go test ./internal/source/ -run TestPollCursorSurvivesRestartAndSends304 -v -count=1
--- PASS: TestPollCursorSurvivesRestartAndSends304 (0.08s)
```

test server 记录每次请求的 `If-None-Match`：第一轮为空、第二轮（**新 Poller 实例**，源行从库里
重读）为 `"v1"`（第一轮响应的 ETag）；服务端回 304 后 events/tasks 仍是 `1/1`（没重抓成新事件），
`consecutiveFailures = 0`、`cursor` 仍是 `"v1"`。

### 验收 3 · 失败不前进：500 → `consecutiveFailures=1`、游标不动、`lastError` 有内容；成功清零

```text
$ go test ./internal/source/ -run TestPollFailureKeepsCursorAndBacksOff -v -count=1
--- PASS: TestPollFailureKeepsCursorAndBacksOff (0.17s)
$ go test ./internal/store/ -run TestSourcePollStateRoundTrip -v -count=1
--- PASS: TestSourcePollStateRoundTrip (0.03s)
```

失败响应故意**带** `ETag: "v2"`：游标仍是 `"v1"`（这条断言就是变异 ① 的靶子）；`lastPollAt`
每轮推进、`lastFiredAt` 只在成功轮推进；恢复成功后 `consecutiveFailures = 0`、`lastError = ""`。

### 验收 4 · 退避：两次失败 → 第三次 ≥ base×2；`429 Retry-After: 7` → 下一次 ≥ 7s

同一条用例（注入时钟，直接断言落库的 `next_fire_at`）：
第一次失败后 `next_fire_at - now = base`（`*/5` → 300s），第二次失败后 `gap ≥ 2 × firstGap`；
`429` + `Retry-After: 7` 的源失败一次后 `gap == 7`（Retry-After 优先于 base）。

### 验收 5 · 不重叠：test server 挂住 + tick 20ms → `skippedOverlaps ≥ 1`，事件不翻倍

```text
$ go test ./internal/source/ -run TestPollOverlapSkippedNotQueued -v -count=1
--- PASS: TestPollOverlapSkippedNotQueued (0.12s)
```

服务端阻塞到显式 release；Run(tick=20ms) 期间每次 tick 都命中 in-flight → `skippedOverlaps ≥ 1`；
放行后 tasks 恰好 `1`（跳过的 tick 不排队、不重放）。

### 验收 6 · SSRF 两半

```text
$ go test ./internal/sourcespec/ -run TestPollURLRejectsNonPublicHosts -v -count=1
--- PASS: TestPollURLRejectsNonPublicHosts (0.00s)
$ go test ./internal/sourcespec/ -run TestPollURLAllowInsecure -v -count=1
--- PASS: TestPollURLAllowInsecure (0.00s)
$ go test ./internal/source/ -run TestPollRuntimeBlocksResolvedPrivateIP -v -count=1
--- PASS: TestPollRuntimeBlocksResolvedPrivateIP (0.12s)
$ go test ./internal/source/ -run 'TestBlockPrivateDialGuardsActualIP|TestSameHostRedirectPolicy' -v -count=1
--- PASS: TestBlockPrivateDialGuardsActualIP (0.00s)
--- PASS: TestSameHostRedirectPolicy (0.00s)
```

- **写入期**（`http://127.0.0.1:9/x`、`https://169.254.169.254/…`、`https://10.0.0.5/x`、
  `https://[::1]/x`、`https://[fd00::1]/x`、`https://localhost/x`、`https://api.internal/x`、
  `https://printer.local/x`、userinfo、非 https）：全部 `ErrInvalidConfig`，且私网那几条带
  `details.causes[].field = config.url`（信封投影成 `spec.config.url`）。
- **运行期**（DNS 指向 127.0.0.1）：注入 resolver 返回 `127.0.0.1` → 该轮失败，
  `status.lastError` 原文 `解析到私网地址: rebind.example.test → 127.0.0.1（环回）`，
  一次请求都没发出去。
- **dial 时**（TOCTOU 的第二道闸）：`blockPrivateDial` 对 `127.0.0.1 / 169.254.169.254 /
  10.1.2.3 / [fd00::1] / [::1]` 直接拒；公网地址放行。
- **重定向**：同 host 放行、跨 host 拒、>3 跳拒。

### 验收 7 · secret 不回显 + 缺失 400

```text
$ go test ./internal/source/ -run TestPollSecretRefResolvesCredential -v -count=1
--- PASS: TestPollSecretRefResolvesCredential (0.11s)
$ go test ./internal/store/ -run TestNamespaceCredentialResolvesPollSecretRef -v -count=1
--- PASS: TestNamespaceCredentialResolvesPollSecretRef (0.04s)
```

`source.Config` 里只有 `secretRef`（含 name/header/prefix），**不含** `secret`；运行期按
`(namespace, name)` 取值并注入 `Authorization: <prefix><value>`（test server 断言收到的头）；
同名凭据可被第二个源复用；引用不存在且没给值 → `*sourcespec.CausesError`，`Field = config.secretRef`，
`errors.Is(err, ErrInvalidSourceConfig)` 成立（HTTP 侧据此翻 400 + causes）。

### 验收 8 · 变异（两条，都必须红）

| 变异 | 改法 | 结果 |
| --- | --- | --- |
| ① 游标「失败也前进」 | `store.MarkSourcePolled` 无条件写 `poll_cursor`；`Poller.round` 无条件取 `res.cursor` | `TestPollFailureKeepsCursorAndBacksOff` **FAIL**（`失败必须留痕且不动游标`）、`TestSourcePollStateRoundTrip` **FAIL**（`失败不得动游标: ""`） |
| ② 去掉「解析后 IP 校验」 | 删掉 `Poller.checkResolvedHost` 的调用 | `TestPollRuntimeBlocksResolvedPrivateIP` **FAIL**（`lastError` 变成 `请求失败: … lookup rebind.example.test: no such host`，不再是「解析到私网地址」） |
| ②′ 第二道闸变 no-op | `blockPrivateDial` 直接 `return nil` | `TestBlockPrivateDialGuardsActualIP` **FAIL**（5 个私网地址全部漏放行） |

变异都是临时改、跑目标用例、`git checkout` 复原；复原后同一条用例重新为 PASS。

### 验收 9 · 门禁

见 §5（`make ci` exit 0、`make e2e-affected` 绿、`known-env-failures.json` entries 仍为空）。

### 验收 10 · 五件套

| 面 | 位置 / 证据 |
| --- | --- |
| `api/resource.yaml` | `SourceSpec.kind`（`webhook`/`cron`/`manual`/`poll`）、`config` 的 poll 12 键与 items 语义、`SourceStatus` 的 `cursor`/`lastPollAt`/`consecutiveFailures`/`skippedOverlaps`（后两者进 `required`）+ `lastError` 描述改写 |
| parity | `TestOpenAPIParityDTOFields`（`srcStatusView ↔ SourceStatus` 双向字段核对，无新增 schema 所以棘轮不动）；`make contract` 的 `check_openapi` / `check_envelope` 通过 |
| DTO | `internal/server/resource_source.go` 的 `srcStatusView` + `srcEnvelopeOf` 填充 |
| e2e | `scripts/e2e/suites/routing.mjs` §14（48 条断言，见 §5 数字） |
| 单测 | `sourcespec/poll_test.go`、`source/poll_test.go`、`store/source_test.go` 两个用例 |
| UI | `web-src/pages/source-detail.html` 基本信息追加「最近轮询」（`lastPollAt`）与「连续失败」（`consecutiveFailures`），`web/partials.js` 的 `modelSource` 建模；`check-wire-fields` 绿 |

## 4. 与 issue 的差异 / 明确没做

- **`secretRef` 的写入口**：issue 说「指向已有命名空间凭据」，但仓库没有这个概念的写入面；
  本版把「写」挂在 poll 源的 write-only `config.secret` 上（理由见 §1.3b）。若将来要一个
  独立的凭据资源，`namespace_secrets` 这张表可以直接复用，不需要改 poll 的语义。
- **`pagination.next`**（issue 说「成本低可加，不在验收内」）：没做。翻页仍由用户在 `url` 里自己带。
- **重定向**：跨主机不跟随、同主机 ≤3 跳（issue 的底线），**不**做「跟随并重新校验」的复杂策略。
- **多副本部署**：「不重叠」是进程内纪律（本仓没有 leader 选举；`internal/lease` 是巡检，
  不是租约锁）。真要多副本，需要先把调度单点化，那是另一个 issue。
- 未做：IMAP/RSS/Kafka/GraphQL、connector（E）、CEL/outcomes（A2/A3）。

## 5. 门禁数字（本次实测）

### 5.1 `make ci`（exit 0 · 3m07s）

```text
$ export PATH=/usr/local/go/bin:$PATH && time make ci
… 43 个 Go 包 ok（含 store 21s / server 72s / v4test 44s）…
✅ 契约静态断言（A2/A4/A5/A6/A7 + openapi 一致性 + 字段级核对 + anode 可编译）通过
✅ 契约字段守卫通过（240 个 schema、762 个已知字段；已知遗留 9 个）
✅ class 校验通过：865 个类（HTML class 属性 615 + JS class 串新增 250）
✅ 文案守卫通过：4 份 locale / 9484 条值 · 22 个页面 / 218 个文本节点
🔍 注释密度棘轮（docs/lint-guards.md · L12）
✅ 与基线完全一致（超与降都算不一致）—— 有收紧请同批下调基线
   扫描 696 个文件 · 注释行 43335 / 基线 43335 · 75 个目录 0 个不一致
🔍 测试纪律棘轮（docs/lint-guards.md · L3）
✅ 五项全部与基线一致（go/time.Sleep 37/37、e2e/swallow-catch 38/38、t.Skip 3/3）
✅ 注解即外键守卫通过：全部引用可解析、锚点可解析、棘轮未超基线。
✅ 秘密扫描通过：扫描 861 文件 / 检测器 17 / 命中 0 / 放行 3
✅ 临时目录零残留（隔离 TMPDIR 内本次新增 = 0）

real    3m06.896s
EXIT=0
```

注释密度是**逐目录等值**的双向棘轮，所以这次是「新代码少写注释 + 同批删等量陈旧注释」：
`internal/store/stmt.go` 的驱动实测流水账删 12 行（并把该文件的密集预算从 169 下调到 157），
`sourcespec` 的重复段落合并（-35 行），`internal/source` 净增 0（poll 的注释与压缩掉的旧注释抵消），
`internal/server` 净增 0。基线**只降未升**（目录基线 74 → 75 是新增 `scripts/lib: 0`）。

### 5.2 `make e2e-affected`（exit 0 · 7m20s · 30/30 套件）

```text
$ make e2e-affected
选中 4 个矩阵类套件（i18n/i18n_coverage/frontend/ui_shift）→ E2E_CONCURRENCY=2
▶ api_contract contract ws_protocol routing persistence security edge_cases passkeys capacity
  orphans deploy live_agent node_v4 playbook_release i18n i18n_coverage frontend vars_ui lang_hint
  cli_auth passkey_enroll admin_flow deeplink auth_flow ux_regression_19_23 recovery_next
  events_paging ui_shift p2_guards admin_login
  ✅ routing            425通过/0失败   9.0s  mode=real     ← 含新增 §14 的 48 条（另有 #522 的 §13）
  ✅ persistence        130通过/0失败  30.2s  mode=real
  ✅ frontend            43通过/0失败 209.1s  mode=real
  ✅ ui_shift            87通过/0失败 187.4s  mode=explicit-mock
  …（30 个套件）

✅ E2E 全绿：30/30 个请求套件均达标（断言 2940 通过 / 0 失败）
  case 级重试：首次 incomplete 0 条 · 首次 timeout 0 条 · 重试通过 0
  套件级重试：0 个套件
  ✅ 临时目录零残留（隔离 TMPDIR 内本次新增 = 0）
real    7m20.302s · EXIT=0
```

`known-env-failures.json` 的 `entries` 仍是 `[]`（本次没有新增任何「已知环境失败」豁免）。
本次改到 `internal/server/**`，所以 affected 映射把 31 个套件全选中——这也顺带证明了 B1 的改动
没有扰动其它面（persistence / edge_cases / p2_guards 全绿）。

## 6. 复算命令速查

```bash
export PATH=/usr/local/go/bin:$PATH
go test ./internal/sourcespec/ -run 'TestPoll|TestLookupItems|TestKinds' -count=1   # 形状 + 写入期 SSRF
go test ./internal/source/     -run 'TestPoll|TestBlockPrivateDial|TestSameHostRedirect' -count=1
go test ./internal/store/      -run 'TestSourcePollStateRoundTrip|TestNamespaceCredential' -count=1
make ci                     # 全量门禁（含注释密度/测试纪律棘轮）
make e2e-affected           # 按改动选套件（改 internal/server 会拉全量）
```
