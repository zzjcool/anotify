> **历史快照**：本文件是本次交付的记录，不随代码演进；行为以代码、测试与 `api/` 为准。

<!-- snapshot-doc: 2026-11-21 #516 交付时的定格数字（make ci / make e2e-affected 的真实输出），复算命令见 §7 -->
# payloadSchema 只写 required 不生效（#516）交付报告

> 分支 `pi-subagent/hw-nj/anotify-516-required2-fa50a942` · base `main` `8f264b1c` · PR：[#518](https://github.com/zzjcool/anotify/pull/518)（正文 `Fixes #516`）

一句话：`config.payloadSchema` 只写 `required`、不写 `properties` 时，**结构闸完全不生效** ——
`ValidatePayload` 只遍历 `properties` 的键名（`s.order`），不在 properties 里的 required 名字永不被检查。
修法是补一轮「只查键存在」的 required 校验；既有 properties 分支的类型 / pattern / 嵌套语义一字不动。

---

## 1. 根因（代码事实）

`parseParamSchema` 把 `required` 收进 `s.required`，但 `s.order` **只在解析 `properties` 时**追加：

```go
for name, rawProp := range props {   // 只有 properties 分支追加 order
    ...
    s.properties[name] = prop
    s.order = append(s.order, name)
}
```

于是 `ValidatePayload` 的主循环 `for _, name := range s.order` 只看得到 properties 里声明过的名字：

```go
for _, name := range s.order {
    prop := s.properties[name]
    value, ok := payload[name]
    if !ok {
        if s.required[name] { missing = append(missing, name) }   // ← 仅限 properties 里出现过的
        continue
    }
    ...
}
```

`{"required":["action"]}` 这种合法 schema 解析出的 `s.order` 是**空切片**，循环一次都不跑，
`ValidatePayload` 恒返回 nil。写路径（`sourcespec` 的 `schemaField`）只要求「是 JSON 对象」，
所以这种配置能存下来，运行时却什么都没校验 —— 看起来闸住了，实际全开。

## 2. 修法（`internal/route/params.go` · `ValidatePayload`）

主循环之后补一轮，只对**不在 properties 里**的 required 名字查「键存在」（值为 `null` 也算存在）：

```go
// required 独立于 properties：只写 required 时上面一个名字都看不到 —— 这里只查「键存在」（null 也算）。
for name := range s.required {
    if _, declared := s.properties[name]; declared {
        continue
    }
    if _, ok := payload[name]; !ok {
        missing = append(missing, name)
    }
}
```

合并后的 `missing` 排序一次再拼 detail（`required` 是 map，不排序则 detail / Field 不可复现；
properties 那半边本来取自 `s.order`，已排序，故既有文案逐字不变）：

| schema | payload | 修正前 | 修正后 |
| --- | --- | --- | --- |
| `{"required":["action"]}` | `{}` | 通过（bug） | 剪枝 `event_schema`，detail `载荷缺 action`，Field `payload.action` |
| `{"required":["action"]}` | `{"action":null}` | 通过 | 通过（键存在即通过） |
| `{"required":["action"]}` | `{"action":1}` | 通过 | 通过 |
| `{"properties":{"a":{"type":"string"}},"required":["a","b"]}` | `{"a":"x"}` | 通过（bug：b 永不检查） | 剪枝 `event_schema`，detail `载荷缺 b` |
| 只有 properties / required 全在 properties 里 | 任意 | —— | **输出逐字不变**（对照组钉住） |

## 3. 边界（明确不改的东西）

- **嵌套对象与 `PrepareParams` 不动**：`checkObject`（嵌套 `payloadSchema` 对象）与能力 `spec.params`
  的对齐路径保持原语义。issue 的范围是源投递的顶层校验；把同一处补丁扩到 `checkObject` 会顺带改
  `PrepareParams` 的参数对齐语义，那是另一条契约面。
- **`Field` 现在对「缺必填」也填 `payload.<第一个缺失名>`**（此前只有类型 / pattern 错误才填）。
  `Field` 是 `json:"-"`，`pruned[]` 的 wire 形状不变；detail 文案也不变。
- **不新增 API 字段、不改 `api/*.yaml`**：`api/ingress.yaml` 只列 `config` 的合法键名
  （`payloadSchema` 只是其中一项），`api/resource.yaml` 全文没有 `payloadSchema` 字样（grep 实测），
  所以「声称 required 生效」的文案不存在，没有需要对齐的句子。

## 4. 证据

### 4.1 单元测试（`internal/route`）

`TestValidatePayloadRequiredWithoutProperties`（#516 主用例，6 例子 + 合并排序 32 次复跑）：

- 只写 required 缺字段 → 剪枝，reason `event_schema`，detail 含 `action`，Field `payload.action`
- 键存在即通过（`null` 也算存在）
- 键存在即通过（任意值 `1`）
- required 一半在 properties 里（`{"a":"x"}`）→ 剪枝，缺 `b`
- properties 里的必填缺失也落到 `payload.<名>`
- properties 的类型判定不受影响（`参数 a 类型应为 string，实际 number`）
- 合并 3 个缺失连跑 32 次，detail 恒为 `载荷缺 a, b, c`、Field 恒 `payload.a`（排序可复现）

`TestValidatePayloadPropertiesOnlyUnchanged`（对照组，5 例子）：**只有 properties / required 全在
properties 里**的五种输入，钉住修正前的 reason 与 detail 原文（`载荷缺 environment`、
`参数 environment 类型应为 string，实际 number` …）。这组用例在**修正前的 `params.go` 上实测同样绿**
（`git checkout origin/main -- internal/route/params.go` 后跑过），所以「既有输出逐字不变」不是声称而是实测。

### 4.2 server 层（HTTP 实测）

`TestHookPayloadSchemaRequiredOnlyGatesDelivery`：建一个 `payloadSchema` 只有 `required` 的 webhook 源
（`{"required":["action"]}`），投递 `{"payload":{}}`：

- HTTP **200**（剪枝不是 4xx：事件已接收）
- 回执 `eventId` 非空、`tasks` 为 `[]`
- `pruned` 恰好 1 条，`reason = event_schema`，`detail = 载荷缺 action`
- `tasks` 表行数不变（剪枝不建任务）

同一份契约的另一侧：投递 `{"payload":{"action":null}}` → 200 且真的建 1 个任务（`tasks` 表 +1），
证明闸门不是「一律剪」。

### 4.3 变异

删掉 §2 那段 required-only 循环（其余代码不动）：

- `internal/route`：`TestValidatePayloadRequiredWithoutProperties/只写_required_缺字段` 与
  `/required_一半在_properties_里` 变红（`应剪枝 event_schema，得到 <nil>`），
  合并排序用例变红（`剪枝 = "载荷缺 a" / "payload.a"，want "载荷缺 a, b, c"`）
- `internal/server`：`TestHookPayloadSchemaRequiredOnlyGatesDelivery` 变红 ——
  `载荷不合契约不得建任务，得到 [map[capability:notify taskId:tsk_… version:1]]`
  （变异体上任务真的被建出来了，正是 issue 报的现象）
- 恢复循环后两侧立即回绿

### 4.4 门禁

| 门禁 | 结果 |
| --- | --- |
| `make ci` | **exit 0**（`real 2m22s`）：contract / gofmt / vet / build / 全部包单测 / `check-comment-density` / `check-annotations` / `check-test-discipline` / harness 自测 全绿 |
| `make e2e-affected` | **exit 0**：`✅ E2E 全绿：30/30 个请求套件均达标（断言 2707 通过 / 0 失败）`，`real 6m28s` |
| `internal/route` / `internal/server` 单测 | 50 + 27 子测试 / 422 + 258 子测试，全绿 |
| `scripts/e2e/known-env-failures.json` | 未改动，`entries` 仍为 `[]` |

改动选择器判定：`internal/route` / `internal/server` 落在二进制风险面（server + devseed），
因此自动升到全量 e2e —— 30 套件全跑，不是抽样。

## 5. 文件清单

- `internal/route/params.go`：`ValidatePayload` 补 required-only 一轮 + 合并排序 + `Field`；
  顺带把三处「纯复述」的注释合并成两处（注释密度棘轮双向冻结，本目录注释行数与基线持平）
- `internal/route/params_test.go`：新增 2 个测试函数（主用例 + 对照组）
- `internal/server/hook_payload_schema_test.go`：新增 server 面用例（复用既有 `newSourceResEnv` / `hookPost` / `hookSnapshot`）
- `internal/server/resource_source_test.go`：删一行复述类型名的注释（为新的 server 测试文件让出一个注释行预算）
- `docs/reports/payload-schema-required.md`：本报告

## 6. 未决与边界登记

- **嵌套 `payloadSchema` 的 required-only 仍是旧语义**（`checkObject` 未改）。它与本次修法同形，
  但改动会顺带影响 `PrepareParams` 的参数对齐（另一条契约面），建议单独 issue 决策。
- `s.required` 里出现一个名字、properties 里也声明了它 → 仍走 properties 分支（类型 / pattern 全查），
  新增的一轮会 `continue` 跳过，不会重复计入 missing。

## 7. 复算命令

```bash
make ci                 # 门禁（含注释密度棘轮、注解外键、测试纪律）
make e2e-affected       # 改动自适应 e2e（本次升到 30/30 全量）
go test ./internal/route/ -count=1 -run TestValidatePayload -v
go test ./internal/server/ -count=1 -run TestHookPayloadSchemaRequiredOnlyGatesDelivery -v
node scripts/check-comment-density.mjs --report
```
