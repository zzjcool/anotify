# 报告 · w-e-peek（批次 4B 后端收尾补丁，游标「多看一行」）

commit：`70f430e fix(server): 游标页末多看一行，消掉「满页后多一个空页」（#235）`

## 做了什么

1. `internal/server/console_handlers.go`（**只改 `events()` 函数 + 紧邻注释**）
   - `perNS := limit; if perNS < listLimitCap { perNS = limit + 1 }`（必须 ≤ 500，否则 store 的 `>500 → 50` 兜底会把整页变 50 条）。
   - `events := mergeEvents(perNS, groups...)` → `hasMore := len(events) > limit` → 截断到 `limit` →
     `nextCursor` 从**截断后**末行算，且仍在 `traceViewRootFirst` 之前。
   - `limit == listLimitCap`（500）时 `perNS == limit`，拿不到「多一行」→ 退化为旧判据
     （`len(events) >= limit ⇒ 给游标`），注释写明这是硬顶带来的已知上限。
   - 响应字段/语义、游标编码、400 语义都没动。
2. `internal/server/console_events_page_test.go`
   - 删除旧用例 `TestConsoleEventsNextCursorOnFullPageWithoutMore`（它断言的正是要消灭的行为），
     换成三条新用例：
     - `TestConsoleEventsNoEmptyPageOnExactFullPage`：200 条 / limit=100，第 2 页 100 条且 `nextCursor == ""`，
       并用第 2 页末行手工造游标反向确认「确实没有更旧的行」（0 条）；
     - `TestConsoleEventsLastPageOneRow`：201 条 / limit=100，第 2 页 100 条且 `nextCursor != ""`，
       第 3 页 1 条且 `nextCursor == ""`；
     - `TestConsoleEventsCursorDegradesAtLimitCap`：500 条 / limit=500 ⇒ `nextCursor != ""`（已知退化，注释写明原因）。
   - 同步修正文件头/`collectPages`/`TestConsoleEventsCursorPaging` 里「nextCursor 语义 ⇔ 行数 < limit」的旧说法。
3. `docs/contracts/CONTRACT.md`（事件列表 `limit` 与游标分页小节）
   - 判据改为「`nextCursor` 为空 ⇔ **没有更旧的行**（服务端多看一行确认；`limit=500` 上限时退化为近似）」。

## 测试覆盖

- 精确整页不再多空页（200/100 第 2 页游标为空）、多一行必须给游标（201/100 第 3 页 1 条）、
  500 硬顶退化保留、跨命名空间翻页、坏游标 400、trace 视图重排 —— `./internal/server -run ConsoleEvents` 全绿。
- 全包 `go test ./internal/server/` 全绿（51.7s）。

## 验证输出（原样）

```
$ taskset -c 4-7 env GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto go test ./internal/server/ -run 'ConsoleEvents' -count=1
ok  	github.com/zzjcool/anotify/internal/server	0.478s

$ taskset -c 4-7 env GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto go test ./internal/server/ -count=1
ok  	github.com/zzjcool/anotify/internal/server	51.707s

$ gofmt -l internal/server
（空输出）
```

先红（改 handler 之前、只改完测试）：

```
--- FAIL: TestConsoleEventsNoEmptyPageOnExactFullPage (0.06s)
    console_events_page_test.go:373: 第 2 页已是最后一页（200/200），nextCursor = "MTcwMDAwMDAwMDpleGFjdF9ldnRfMDAw"，want ""（否则用户要多看一个空页）
FAIL	github.com/zzjcool/anotify/internal/server	0.540s
```

## 未决问题 / 遗留风险

- `limit=500` + 库里正好 500 条仍会多一个空页（已知上限，已用用例与注释固化；真要拍平需突破 store 500 硬顶 = #236 的另一件事）。
- 前端（`web-src/pages/events.html`，W-C owner）与 e2e（`scripts/e2e/suites/events_paging.mjs`，W-D owner）已按
  「`nextCursor == "" ⇔ 没有更旧」写；本次改动让它们的注释与实际一致，无跨 owner 冲突。
- 未跑 `make fe/ci/e2e/build`（按任务要求留给父级串行门禁）。
- 工作区里 `.pi-legion/` 仍为未跟踪（含本报告），未进 commit。
