<!-- snapshot-doc: 2026-11-27 #638 r2 的红灯定性 + 修法 + 本机（macOS）实跑输出，复算命令见 §4 -->
# 事件列表「已剪枝」读面 · PR #686 r2 补丁（`e2e（affected）` 的 `2 !== 1`）

> 分支 `pi-subagent/issue-638-r1` · base `origin/main` @ `e7a1c8a1`（本提交 `8b93c0eb`）
> Issue #638 · PR #686 · 上一条评论：`#issuecomment-7208954943`
>
> 下面每个数字都是**这台机器（macOS）重新跑出来的**；CI 是 Linux，其中的跨平台差异（§4.5）
> 已逐条说明"stash 本提交后同样红"，不冒充本次改动的结果。

一句话：红灯来自**读面把同一份剪枝事实投影了两遍**，不是断言写错。修的是实现（代表剪枝只在
响应里看不到观察行时给），e2e 断言一行未动，`?trace=` 列表的剪枝事实从 2 条恢复成 1 条。

---

## 1. 红灯原文与定性

```
- frontend / capability-lifecycle：capability-lifecycle — AssertionError [ERR_ASSERTION]:
  trace 上必须留一条 capability_disabled 剪枝事件
  2 !== 1
  at f3-capability-lifecycle.mjs:120:9
```

断言本体（#432 就写下、本 PR 未动过）：

```js
const pruneRows = (trace.body.items || []).filter(
  (e) => e.spec?.eventType === 'capability_disabled' || e.status?.pruned?.reason === 'capability_disabled',
);
assert.equal(pruneRows.length, 1, 'trace 上必须留一条 capability_disabled 剪枝事件');
```

先看清事实（临时探针用例打 `GET …/events?trace=<traceId>` 的真实响应，用完已删）：

```
row eventType="capability_disabled" dispatched=0 prunedCount=1 pruned={reason:capability_disabled}  ← 观察行，报自己
row eventType="push"                dispatched=0 prunedCount=1 pruned={reason:capability_disabled}  ← 投递行，借「代表」
items=2
```

同一条 trace 里 `source=route`（且 `type=capability_disabled`）的观察行**只有一条**
（`internal/route/emit_test.go:TestPlanFromEmitDisabledDownstream` 早就钉住"剪枝留痕行数 = 1"）。
所以第二条命中不是第二条剪枝，而是本 PR 的 `eventStatusOf` 给投递行补的
「同 trace 首条纯剪枝代表」。

| 判据 | 结论 |
| --- | --- |
| 1. 读面把同一事实投影两遍 | **命中**：`?trace=` / `?traceId=` 的列表把观察行和投递行放进**同一份响应**，两行都带 `pruned` |
| 2. 用例本来就该看到 2 条 | 不成立：这条 trace 的剪枝事实就是 1 条，没有第二次剪枝、没有第二个步骤 |
| 3. 历史（`git log -p --follow`） | `1` 来自 #432 的形态：**只有观察行**带 `pruned`。本 PR 让投递行也带（改的是读面），计数口径才翻倍 |

同一 PR 的 `e7a1c8a1` 已经在 `GET …/traces/{traceId}` 上用过同一条论据（提交信息原文：
"该视图另有 `pruned[]` 全量，代表值会让读者以为这条投递事件被剪了"）。`?trace=` 的列表是
同一个视图的另一种入口——观察行同样就在响应里，判据必须一致，否则同一份事实在
`traces/{id}` 里数 1 条、在 `events?trace=` 里数 2 条。

**定性：实现（读面）问题。断言是对的。**

## 2. 改了什么

### 2.1 判据（`internal/server/resource_task.go`）

```go
// eventWantsRepresentativePrune：只在响应里看不到观察行时才给投递行「代表剪枝」。
// 代表值是列表端点为「一条都没派发」的行补的原因（事件流会过滤掉 source=route）；
// 按 trace 查时观察行就在同一份响应里，再给代表 = 同一份事实投影两遍（按行计数会翻倍）。
func eventWantsRepresentativePrune(traceID, source string) bool {
	if traceID == "" {
		return true
	}
	return source != "" && source != store.EventPruneSource
}
```

| 请求窗口 | 响应里有观察行吗 | 投递行的 `status.pruned` |
| --- | --- | --- |
| 事件流（无 `trace`、无 `source`） | 没有（store 过滤 `source=route`） | **代表**（#638 要补的缺口，不变） |
| `?trace=X`（或 console 的 `?traceId=X`） | 有 | **`null`**（观察行自己报原因） |
| `?trace=X&source=<投递源>` | 没有（被 `source` 滤掉） | **代表**（仍给） |
| `?source=route` | 全部是观察行 | 每行报自己（代表无意义） |

判据只看**请求**而不是"这一页里恰好包含什么"：同一条事件行的字段不该随分页窗口变
（500 上限下，观察行可能落在别的页里）。`GET …/events` 与 `GET /v1/console/events`
共用这一个判据；`dispatched`（trace 级任务数）与 `prunedCount`（纯剪枝条数）**口径不变**，
三个视图都照给。

### 2.2 其它三个文件

- `api/events.yaml`：`EventStatus.pruned` 的三条来源与两个列表端点的 description 改口径。
  原文"本行是投递行（两个**列表**端点）→ 给同 trace 的首条纯剪枝（代表）"正是这次翻倍的
  口径来源，现在写明"且该响应里看不到这个 trace 的观察行"。
- `web/mock.js`：console 列表按 `traceId` 查时走 `ownPruneOnly`（与真实服务同判据），
  预览不再比真实服务多一次投影。
- `scripts/e2e/fixtures/f3/f3-capability-lifecycle.mjs`：**一行未改**。
- 注释按棘轮预算付账：压缩 `events` handler 的过滤口径 bullet 与 `consoleEvents` 循环里
  的一条历史流水账（-4 / +4），仓库注释行 42879 与基线**完全一致**，`check-comment-density`
  基线文件未改动。

## 3. 测试与变异

`TestTaskResourceEventOutcome`（`internal/server/resource_task_test.go`）新增三条断言：

| 断言 | 修前 | 修后 |
| --- | --- | --- |
| `?trace=` 列表 3 条；投递行 `status.pruned` 必须 `null` | 红（拿到了代表 `param_missing`） | 绿 |
| 同响应里两条观察行各报**自己**的 reason（`param_missing` / `no_target`） | 绿 | 绿 |
| `console/events?traceId=` 同口径 | 红（同第一行） | 绿 |
| `?trace=&source=github`（观察行被滤掉）仍给代表 `param_missing` | 绿 | 绿（这条不许被"顺手关掉"） |

修前红（本机真实输出）：

```
--- FAIL: TestTaskResourceEventOutcome (0.06s)
    resource_task_test.go:1102: evt_BCrPt2WYpLYaEK0Nrf status.pruned 必须是 null，得到 map[detail:需要 n, tag reason:param_missing]
```

两次变异（改坏 → 红 → 改回，改回后同一用例绿）：

```
# 变异 A：恒给代表（回到修好前的形态）
--- FAIL: TestTaskResourceEventOutcome (0.05s)
    resource_task_test.go:1102: evt_… status.pruned 必须是 null，得到 map[detail:需要 n, tag reason:param_missing]

# 变异 B：从不给代表（#638 那个缺口又回来了）
--- FAIL: TestTaskResourceEventOutcome (0.04s)
    resource_task_test.go:1089: evt_… status.pruned 必须是对象，得到 <nil> (<nil>)

# 改回：
ok  	github.com/zzjcool/anotify/internal/server	0.320s
```

## 4. 验证（本机 macOS，真 server + 真 Chrome）

### 4.1 e2e 入口说明

CI 的套件名是 `frontend`，`capability-lifecycle` 是套件里的 case id，所以单 case 的真实入口是
`node scripts/e2e/suites/frontend.mjs --case capability-lifecycle`（authority=REAL，非 mock）。
前置：

```
ln -sfn ~/code/anotify/node_modules node_modules      # playwright-core（本机 node_modules/ 缺 yaml，另补了一份）
make fe                                       # web/ + internal/server/dist（dist-stamp 必须与源一致）
go build -tags embeddist -o .e2e-bin/anotify ./cmd/server ; go build -o .e2e-bin/devseed ./cmd/devseed
export E2E_CHROME_PATH="$HOME/Library/Caches/ms-playwright/chromium-1208/chrome-mac-arm64/Google Chrome for Testing.app/Contents/MacOS/Google Chrome for Testing"
```

本机没有 `flock`（util-linux），`scripts/e2e/run_all.sh` 的机器级互斥跑不了，所以直接用
套件入口（不是绕过验收：authority 与断言集与 CI 同一条代码路径）。

### 4.2 e2e：修前 / 修后

```
# 修前（base e7a1c8a1）
[F3 capability-lifecycle] authority=REAL H.startServer, no mock fallback
PASS capability-lifecycle :: capability lifecycle: 默认启用徽标 + 停用按钮（无开关行 = enabled）
PASS capability-lifecycle :: capability lifecycle: 停用走真实确认 → 徽标「已停用」→ 返回整个 Capability（status.enabled=false）
CASE_FAIL capability-lifecycle :: trace 上必须留一条 capability_disabled 剪枝事件
  ❌ capability-lifecycle — AssertionError [ERR_ASSERTION]: trace 上必须留一条 capability_disabled 剪枝事件

2 !== 1
[frontend] 0 通过 / 1 失败

# 修后（本提交，dist stamp efa158aa1a90…）
PASS capability-lifecycle :: capability lifecycle: 默认启用徽标 + 停用按钮（无开关行 = enabled）
PASS capability-lifecycle :: capability lifecycle: 停用走真实确认 → 徽标「已停用」→ 返回整个 Capability（status.enabled=false）
PASS capability-lifecycle :: capability lifecycle: 停用 → 源触发 0 任务 + pruned[capability_disabled] + trace 留痕
PASS capability-lifecycle :: capability lifecycle: 启用 → 徽标恢复「已启用」，状态来自服务端而不是前端缓存
PASS capability-lifecycle :: capability lifecycle: 启用后同一源触发 → 真任务 pending（停用只拦自动触发，不是删功能）
PASS capability-lifecycle :: capability lifecycle: 试跑弹窗 → 200 回执（origin=test、真任务、traceId 可见）
PASS capability-lifecycle :: capability lifecycle: 审计区读到 enable/disable/test 三条且倒序
  ✅ capability-lifecycle (7 behavior assertions)
[frontend] 1 通过 / 0 失败
```

### 4.3 相邻读面回归（同一次改动直接影响的页面）

```
node scripts/e2e/suites/frontend.mjs --case observe-events   → ✅ observe-events (3 behavior assertions)
node scripts/e2e/suites/frontend.mjs --case observe-trace    → ❌ page.screenshot: Timeout 4500ms exceeded
                                                              （stash 本提交后**同样**卡在同一行 → 本机环境问题）
```

### 4.4 单测 / 门禁

```
go test ./internal/server ./internal/store -count=1        ok / ok
go test ./internal/server -run 'Openapi|Parity'            PASS（events.yaml ↔ mux / DTO 双向核对）
make lint-go                                               gofmt + vet + staticcheck SA*,U1000 全绿
make test                                                  20+ 包 ok（internal/artifact 例外见 §4.5）
node --test scripts/v4-frontend.test.mjs                   213 tests / 208 pass（例外见 §4.5）
node scripts/check-comment-density.mjs                     注释行 42879 / 基线 42879 —— 与基线完全一致
node scripts/check-test-discipline.mjs                     五项全部与基线一致（t.Skip 3/3）
node scripts/check-annotations.mjs                         通过
node scripts/dist-stamp.mjs --check                        ✅ dist 与源一致（stamp efa158aa1a90…）
```

`make contract` 的 Python 段在本机跑不了（缺 PyYAML，pip 被 PEP 668 挡住），替代证据：
`internal/server/openapi_parity_test.go` 的 7 条 parity 用例（含 mutations 子用例）、
`make fe` 里的 `check-wire-fields.mjs`（用 node 的 `yaml` 解析 api/*.yaml）与 sitegen 的
"146 operations from 5/5 specs"。

### 4.5 本机既有红（不是本次改动）

| 红 | 证据 |
| --- | --- |
| `internal/artifact` 3 条（`TestAgentTUIWorktree*`） | 断言比 `/var/folders/…` 与 `/private/var/folders/…`；**stash 本提交后同样 3 条红** |
| `v4-frontend` 5 条 `PARENT-GEOMETRY*`（几何/字体） | **stash 本提交后同样 5 条红** |
| `observe-trace` 的 `page.screenshot` 超时 | 见 §4.3，stash 后同样红 |

这些都是 macOS 平台差异（CI 是 Linux），与本次读面改动无关。

## 5. 未决 / 已知边界

1. `?trace=X&origin=test` 这类"按 trace 又只看试跑"的窗口：观察行（`origin=source`）被 origin
   过滤掉，投递行拿不到代表 —— 只是少一个便利字段（`prunedCount` 仍如实给条数），**不会**
   重复投影，不构成错误事实。要更精确需把 `origin` 纳入判据；目前没有真实使用者，留作以后。
2. mock 的 `GET /v1/namespaces/{ns}/events` 至今**不支持** `?trace=`，且事件流里仍会带出
   `source=route` 行与试运行（真实服务两者都过滤）。这是本次改动之前就存在的 mock/真实差异，
   不在本轮范围；它不会造成本次这类重复投影，因为该端点没有 trace 入口。
3. e2e 断言本身（"1 条剪枝事件"）保持原样：它描述的是**事实条数**，与修好后的读面一致。
