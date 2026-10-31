# 批次 4B 冻结契约（#234 / #235 / #236）

> 状态：**已冻结**（规划阶段产出）。任何偏离必须先由 centurion-30 改本文件，worker 不得自行解释。
> 分支：`pi-subagent/centurion-30-071daaf8`（worktree `worktrees/centurion-30`），基线 `741f694`。
> 父级裁决记录：**#235 需要 store 游标查询 → 授权新增文件 `internal/store/event_page.go`（方案 A）**，
> 约束：不改 `internal/store/event.go` 一行、语义与 `ListEvents` 完全一致、必须带等价性测试 + EXPLAIN 计划断言。

## 0. 验收清单（Done 的定义）

| # | 验收 | 判定方式 |
|---|---|---|
| V1 | `GET /v1/events?limit=501..1000` 不再返回 50 条；返回 ≤500 条且响应回显 `limit`=生效值 | 单测 + e2e |
| V2 | `GET /v1/console/events?limit=1000` 同理（≤500，回显 `limit`） | 单测 + e2e |
| V3 | console events 支持 `before=<cursor>` 游标分页，1000 条事件可一路翻到最旧（第 10 页），无重复/无遗漏 | 单测 + e2e + 手工 playwright |
| V4 | events 页有分页导航，URL 带 `?before=`，刷新/分享后回到同一窗口 | e2e + playwright |
| V5 | index/events/playbooks 等非图页面**不再加载** dagre/flograph/graphs/traceview（-150.7KB / -4 请求）；trace/source 两页仍然正常画图 | playwright 资源清单 + e2e trace 页 |
| V6 | 首屏 DCL/load 与资源总字节有前后对照数字（旧：index DCL 565.5ms / load 706.7ms、首屏 402KB） | perf 脚本原始输出 |
| V7 | `make fe` / `make ci` / `make e2e` 全绿；`check-classes.mjs` 无死类 | 命令退出码 |

## 1. 文件所有权（每个写入文件只能有一个 owner）

| 文件 | owner |
|---|---|
| `internal/api/events.go`、`internal/api/events_test.go` | W-A（后端） |
| `internal/server/console_handlers.go`（**仅** events 函数及其紧邻注释/游标辅助） | W-A（后端） |
| `internal/server/console_events_page_test.go`（新建） | W-A（后端） |
| `internal/store/event_page.go`、`internal/store/event_page_test.go`（新建，唯一被授权的 store 变更） | W-A（后端） |
| `api/openapi.yaml`、`docs/contracts/CONTRACT.md` | W-A（后端） |
| `web-src/layouts/base.html`、`web-src/pages/trace.html`、`web-src/pages/source.html` | W-B（首屏） |
| `web-src/pages/events.html`、`web-src/locales/{zh-CN,en,ja,es}.yaml` | W-C（事件页 UI） |
| `scripts/e2e/suites/events_paging.mjs`（新建） | W-D（e2e，后端契约落地后开工） |

**禁区（谁都不许碰）**：`internal/server/static.go`、`internal/sitegen/**`、`internal/store/**`（除上表两个新文件）、`internal/auth/**`、`internal/ws/**`、`web/**` 下的**非 HTML** 源文件（`partials.js` / `graphs.js` / `mock.js` 等）、`.git`、`node_modules`。
`web/*.html` 只能由 `make fe` 生成，禁止手改。

## 2. #236 冻结口径（`limit` 三处不许再分叉）

**「50」的来源（已查清，写进注释）**：
`internal/store/event.go:232` 的 `if limit <= 0 || limit > 500 { limit = 50 }` 由 `443111b2`（#55 store 数据层）引入，
注释本意只有「`limit ≤ 0` 时用默认 **50**」（store 层历史默认值，同期 `/admin/messages` 的 openapi 也是
`default: 50, maximum: 500`，`ClaimCentralTasks` 至今同样兜底 50）。
把「超上限」并进同一个 if 是**误合并**。而 handler 侧 `internal/api/events.go:205`（`20e0add7`）把默认改成 **100**、
接受范围开到 **1000** → 两边默认值分叉，501..1000 落进「看起来非法」的桶里被兜底成旧默认 50。
结论：**不要只把数字改大**；要在 handler 侧把区间收回到 store 真实硬顶，并回显生效值。

- 硬顶常量：`internal/api/events.go` 内 `const eventListLimitCap = 500`（注释写明「store 的 ListEvents 对 `>500`
  会兜底成 50（#236 根因），所以 handler 必须先夹住」）。
- `GET /v1/events`：`limit` 缺省 100；`n > 0` → `min(n, 500)`；非整数 / `n ≤ 0` → 100（沿用现有宽松语义，不新增 400）。
- `GET /v1/console/events`：`intParam(q.Get("limit"), 100, 1, 500)`（`maximum` 从 1000 收到 500），
  合并后的截断也用同一个生效值（`mergeEvents(limit, …)` 传生效值，不要把 1000 传进去）。
- **两个端点响应都回显生效值**：顶层 `"limit": <int>`。
- openapi：两个端点的 `limit` `maximum: 500`；响应 schema 增加 `limit`（required）。
- `docs/contracts/CONTRACT.md`：在事件相关小节补 3–6 行（硬顶 500、回显生效值、游标分页），不要重写整篇。

## 3. #235 冻结口径（游标分页 wire 契约）

### 3.1 store 新增函数（唯一被授权的 store 变更）

```go
// internal/store/event_page.go（新文件；event.go 一行不动）
func (d *DB) ListEventsBefore(ctx context.Context, namespaceID, traceID, source string,
    beforeCreatedAt int64, beforeID string, limit int) ([]*Event, error)
```

- 语义与 `ListEvents` **完全一致**：同样的 `source != 'route'` 剪枝（`source==""` 且 `traceID==""` 时）、
  `ORDER BY created_at DESC, id DESC`、空结果返回 `[]*Event{}`（不是 nil）、`limit` 夹取规则与 `ListEvents` 一致
  （≤0 → 50；>500 → 500 还是 50？→ **与 ListEvents 保持同一实现**，即复用同一段夹取逻辑；注意 ListEvents 的 `>500` 兜底是 bug 但**不改**它，见父级约束「event.go 一行不动」；本函数的调用方永远传 ≤500）。
  注释里**必须**指向上面的等价性测试。
- 谓词：`AND (created_at < ? OR (created_at = ? AND id < ?))`，参数 `(beforeCreatedAt, beforeCreatedAt, beforeID)`。
- **必须**有：`internal/store/event_page_test.go`
  1. **等价性**：`ListEventsBefore(beforeCreatedAt=+∞, beforeID="", limit=N)` 与 `ListEvents(limit=N)` 逐条 id 相同、顺序相同
     （覆盖：纯事件流、`source=""`+`traceID=""`、显式 `source`、显式 `traceID`（含剪枝行）四种口径）；
  2. 游标边界：同 `created_at` 多行时按 id 继续走，不重不漏；翻完全部页的并集 == 全量集合；
  3. 空结果 `[]` 而非 `nil`；
  4. `EXPLAIN QUERY PLAN` 断言用到索引：plan 文本**含** `idx_events_retention` 且**不含** `SCAN events`。

### 3.2 游标编码（服务端唯一实现）

- `nextCursor` = `base64.RawURLEncoding("<createdAt>:<id>")`，例：`MTc2MTc3NjAwMDpldnRfYWJj`（URL 安全，无需再转义）。
- 解码：按**第一个** `:` 切分；`createdAt` 必须是 `int64 ≥ 0`，`id` 非空；任何失败 → **400** `{"error":"invalid before cursor"}`（不静默降级——这是 #236 的病根，不能在新参数上重犯）。
- 游标取自**合并后 DESC 列表的最后一行**（在 `traceViewRootFirst` 重排之前算，避免 trace 视图重排污染单调性）。
- `nextCursor` 为 `""` ⇔ 本次返回行数 < 生效 `limit`（没有更旧的了）。

### 3.3 `GET /v1/console/events` 最终形状

```
GET /v1/console/events?namespaceId=<id|*>&limit=<1..500>&before=<opaque>&traceId=&source=
200 {"events":[...], "namespaceId":"...", "limit":100, "nextCursor":"MTc2..."|""}
400 {"error":"invalid before cursor"}   // before 解析失败
```

- `namespaceId=*`：每个命名空间各自按 `before` 查（`perNS = min(limit,500)`），合并、DESC、截断到 `limit`（现有 `mergeEvents` 不变）。
- 不带 `traceId` 时事件流语义不变（不含 `source=route` 剪枝行）；带 `traceId` 时仍 `traceViewRootFirst`。
- `/v1/events`（机器入口）**不加**游标，只做 §2 的夹取 + 回显（控制台与机器入口的差异保持现状）。

### 3.4 events 页（前端）冻结行为

- 页大小 `PAGE_SIZE = 100`；请求 `GET /v1/console/events?limit=100[&before=<cursor>][&traceId=<...>]`。
- URL：`?before=<cursor>` = **当前窗口的锚点**（返回「比该游标更旧」的一页）。首页不带 `before`。
  翻页用 `history.replaceState` 写回（刷新/分享落在同一窗口）。现有 `?source=/?node=/?traceId=/?group=1` 语义不变。
- 导航按钮（文案走 i18n，key 见 §5）：
  - `← 较新`：用本会话内的游标栈回退（刷新后栈为空 → 隐藏该按钮，不报错）；
  - `更旧 →`：`nextCursor` 非空时显示，点击后把 `?before=<nextCursor>` 写入 URL 并加载；
  - `回到最新`：清掉 `before`（存在 `before` 时才显示）。
- 摘要行：`本页 N 条`；`nextCursor == ""` 且无更旧时显示`已到最旧`。
- 客户端筛选（kind / node / 源）仍是**当前窗口内**过滤（保持现状，不改数据来源）；
  窗口内因筛选为空时给出提示「当前页没有匹配项，可继续向更旧翻页或回到最新」。
- 任何 HTTP 失败/`null` 响应走现有 demo 降级（`demoMode = true`），不得白屏、不得抛未捕获异常。

## 4. #234 冻结口径（脚本分层）

- `web-src/layouts/base.html`：`i18n.<lang>.js` 与 `partials.js` **保持同步加载**（页面内联脚本在解析期就要用 `t()` / `Anotify`）。
  四个图脚本（`vendor/dagre.min.js`、`flograph.js`、`graphs.js`、`traceview.js`）从 layout **移出**，改成新的页面级槽位
  `{{block "graph-libs" .}}{{end}}`（默认空），放在 `{{block "script" .}}{{end}}` **之前**。
- 只有 `web-src/pages/trace.html`、`web-src/pages/source.html` 定义该槽位（`grep` 全仓已确认只有这两页用 `AnotifyGraphs` /
  `TraceView`）。四个 `<script>` 一律 **`defer`**：defer 脚本在文档解析完成后、`DOMContentLoaded` **之前**按序执行，
  而两页的绘图调用都在 `DOMContentLoaded` 回调或更晚的 `fetch` 回调/`requestAnimationFrame` 里 → 安全。
- 删掉 base.html 里「不能加 defer」那段过时注释，替换为上述事实说明（保留「为什么放在 layout」的理由），
  并说明为什么其余两页不加载它们（150.7KB / 4 请求）。
- 不许改 `web/` 下任何文件；不许给 `i18n.js`/`partials.js` 加 defer。

## 5. i18n key（新增，四语言必须齐）

`web-src/pages/events.html` 只用 `t(key, 默认文案)`，新增 key 必须同时加进
`web-src/locales/{zh-CN,en,ja,es}.yaml`（`events:` 段下的 `page:` 子段）：

| key | zh-CN | en | ja | es |
|---|---|---|---|---|
| `events.page.newer` | `← 较新` | `← Newer` | `← 新しい` | `← Más recientes` |
| `events.page.older` | `更旧 →` | `Older →` | `古い →` | `Más antiguos →` |
| `events.page.latest` | `回到最新` | `Back to latest` | `最新へ戻る` | `Volver a lo más reciente` |
| `events.page.window` | `本页 {n} 条` | `{n} on this page` | `このページ {n} 件` | `{n} en esta página` |
| `events.page.end` | `已到最旧` | `Oldest reached` | `最古に到達` | `Se alcanzó lo más antiguo` |
| `events.page.no_match` | `当前页没有匹配项，可继续向更旧翻页或回到最新` | `No matches on this page — page older or jump back to latest` | `このページに一致なし。古いページへ進むか最新へ戻れます` | `Sin coincidencias en esta página: avanza a más antiguos o vuelve a lo más reciente` |

## 6. 验证命令（收队门禁）

```bash
make fe      # check-classes + sitegen + 指纹
make ci      # contract + gofmt + fe + vet + build + 单测
make e2e     # 固化门禁（全量）
```
三条都必须绿；`make fe` 会重写 `web/*.html` 与 `internal/server/dist/`，**同时只允许一个进程跑**。
perf 脚本与原始数据放 `/home/zzjcool/perf-4b/`（`/tmp` 是 tmpfs，不放产物）。

## 7. 止损

同一问题重试 ≤ 2 次；仍失败就停下回报（附已试方案、失败输出、当前假设、涉及文件），不要硬扛第三次，
更不许为绿灯改断言或绕过门禁。
