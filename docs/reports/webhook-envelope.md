> **历史快照**：本文件是本次交付的记录，不随代码演进；行为以代码、测试与 `api/` 为准。

<!-- snapshot-doc: 2026-11-21 #513 A1 交付时的定格数字（make ci / e2e 的真实输出），复算命令见 §7 -->
# webhook 配置化信封 + github-sha256 + match.types（#513 A1）交付报告

> 分支 `pi-subagent/hw-nj/anotify-513-envelope-0eb1b980` · base `main`
> 设计记录：#512 · 本阶段范围：#513 · PR：见分支上的 PR（正文 `Fixes #513`）

一句话：**把「从哪取 type/deliveryId/payload」与「用哪种签名口径」从 Go 搬到配置里**，
让 `kind: webhook` 能读懂任意平台（含 GitHub）的推送；传输仍在 Go（信任边界不外包），
适配是数据。不引入新 kind、不引入新资源、不引入表达式语言。

---

## 1. 冻结语义（实现里定死，PR 正文同稿）

### 1.1 `config.envelope` 三指针

```yaml
config:
  envelope:
    type:       { header: "x-github-event", joinFrom: "$.action" }   # → issues.opened
    deliveryId: { header: "x-github-delivery" }
    payload:    { whole: true }                                      # 或 { pointer: "$.data.object" }
```

- **三个维度各自独立缺省**：只写 `type` 时，`deliveryId` / `payload` 仍按今天的语义取
  （`body.id` / `body.payload`）。因此「不写 envelope」≡「写一份全默认的 envelope」≡
  今天的行为——回落不是特例，是缺省值。
- `type` / `deliveryId`：`header` 与 `pointer` **二选一**；`type` 额外允许 `joinFrom`
  （`deliveryId` 写了 → 400）。
- `payload`：`whole: true`（只接受 `true`）与 `pointer` 二选一。
- 未知键 / 空对象 / 双来源 / 缺来源 / 非法指针 → 写入期 **400**（与 config 白名单同纪律）。

### 1.2 指针语法（与 params 模板**同一套方言**）

点号路径，`$.a.b` 与 `a.b` 等价；数字段是数组下标（`a.0.b`）；不支持通配/函数/负下标；
非法段（`a-b`、`a..b`、空串）在写入期 400。实现上是**一份** `walkPath`：`lookupPath`
（模板）与 `LookupPointer`（信封）共用，避免两种路径语言分叉。

### 1.3 映射与回落

| 维度 | header 来源 | pointer 来源 | 该维缺省 |
| --- | --- | --- | --- |
| `type` | 头值（trim） | 路径取值转文本（标量：字符串/数字/布尔） | `body.type` |
| `deliveryId` | 头值（trim） | 同上 | `body.id` |
| `payload` | — | 必须解析成 JSON **对象** | `body.payload` |

- `joinFrom`：`主来源 + "." + joinFrom`，**两段都非空才拼**（不会拼出 `issues.` / `.opened`）。
- 指针取不到 / 类型不对：`type` → 空 → **回落源名**（今天语义）；`deliveryId` → 空 →
  **不去重**（今天语义）；`payload` → **400 `ErrBadPayload`**（不静默当 `{}`——坏指针必须
  当场可见，否则只会在下游长成一条莫名的 `event_schema` 剪枝）。

### 1.4 `config.signature.scheme`

```yaml
config:
  signature: { scheme: "github-sha256" }
```

- 合法值：`""`（缺省）/ `anotify-sha256` / `github-sha256`；其它 → 400。
- `github-sha256`：头 `X-Hub-Signature-256`，值 `sha256=<64 位小写 hex>`，
  `HMAC-SHA256(secret, 原始字节)`——**与缺省口径算法完全相同，只有头名不同**。
- 缺省 / `anotify-sha256`：头 `X-Anotify-Signature`（今天的行为，逐字不变）。
- `secret` 为空 = 不校验（三档一致，不看签名头）。
- 归一化把 `anotify-sha256` 折叠成缺省（`signature: {}` 整键消失）：否则「显式默认」与
  「不写」落库字节不同，playbook 的安装比较会把同一份配置判成漂移。

### 1.5 `targets[].match.types`

```yaml
targets:
  - capability: issue-router
    match: { types: ["issues.opened"] }
```

- 精确、**大小写敏感**的字符串白名单；非空数组、元素非空且不重复（否则 400）。
- 不写 `match` → 全部命中（现状）；`match` 写了但没 `types` → 400（`cel` 属 A2）。
- 求值顺序：**match → resolve → params → 能力开关 → ready**。不命中 → 事件照常入库、
  回执 `pruned[].reason = "no_match"`、HTTP **200**、不建任务；不为一条永远不会派发的边
  去查库（也不会把 `no_match` 报成 `downstream_missing`）。
- `no_match` 进 `protocol.pruneReasons` 闭集（4 道 fail-closed 守卫自动覆盖：
  `AcceptSourceEvent`、`validateEmitPlannedPrunes`、`insertPruneFactEventOn`、`source` 包的
  编译期断言）。
- 剪枝事实的 `capability` = 声明里的能力名（`name@version` 取 `name`），`detail` 带事件
  type 与声明的 types。

### 1.6 顺带修掉的坑：webhook 的 `config.payload` 从「接受但丢弃」改为**真生效**

三个 kind 同语义：`config.payload` 静态打底 + 入站载荷一层覆盖（浅合并、不做深合并），
**冲突时入站覆盖静态**（与 cron/manual 一致）。非对象 payload 三个 kind 同判据 400。

### 1.7 dry-run 的事件类型

`POST …/sources/{name}/dry-run` 与 `POST …/sources/dry-run` 的请求体新增**可选** `type`：
`match.types` 求值用它，缺省 = 源名（与投递侧的回落完全一致）。`fire` 忽略它——手动触发的
事件类型恒为源名。

---

## 2. 改了什么（按层）

| 文件 | 改动 |
| --- | --- |
| `internal/sourcespec/envelope.go`（新） | `Envelope`/`FieldSource`/`PayloadSource`/`Signature`/`Match` 类型；`ParsePointer`（`$.a.b`≡`a.b`）、`LookupPointer`、`ScalarText`；`normalizeEnvelope`/`normalizeFieldSource`/`normalizePayloadSource`/`normalizeSignature`/`normalizeMatch`；`(*Match).Matches`；`scheme` 两个常量 |
| `internal/sourcespec/template.go` | `walkPath` 抽出（`lookupPath` 与 `LookupPointer` 共用一份路径实现） |
| `internal/sourcespec/config.go` | `knownFields[webhook]` 加 `envelope`/`signature`；`webhookConfig` 加 `Payload`/`Envelope`/`Signature`；`ValidateTargets` 校验 match |
| `internal/sourcespec/sourcespec.go` | `Target.Match`；`Config.Envelope/Signature`；`keepsPayload` 删除（webhook 也保留 payload）；`Config.Scheme()`（口径的唯一解释点） |
| `internal/source/envelope.go`（新） | `hookInput`：头 + 原始 body → `{type, deliveryId, payload}` 的运行期映射与三条回落 |
| `internal/source/source.go` | `HookBody` 加 `Raw`/`Header`（`json:"-"`，handler 注入的原始事实）；`assemblePayload` 三个 kind 同语义；`Ingest` 把事件 type 传给路由；`Preview`/`PreviewSource` 加 `eventType` |
| `internal/source/webhook_sign.go` | `HookSignatureGitHub = "X-Hub-Signature-256"`；`HookScheme(src)`；验签实现不变（算法没分叉） |
| `internal/route/route.go` | `PlanFromSource` 加 `eventType`；match 闸门 + `pruneNoMatch`（resolve **之前**） |
| `internal/protocol/enums.go` | `ReasonNoMatch = "no_match"` 进 `pruneReasons` |
| `internal/server/resource_source.go` | hook handler 按 scheme 选签名头（两个分支都写**常量**，供 parity 的 `in: header` 对账）；把原始字节与头读取带下去；dry-run 的 `type` 透传 |
| `api/resource.yaml` | `SourceTarget.match` + 新 schema `SourceMatch`；`SourceSpec.config` 描述补 envelope/signature 的形状与 400 规则 |
| `api/ingress.yaml` | hook 声明第二个签名头 `X-Hub-Signature-256`；`PayloadBody`/`InlinePreviewBody` 加 `type`；`InlineSourceSpec.config` 补合法键 |
| `internal/server/openapi_parity_test.go` | **只调 ratchet 数字**：`headerParamFloor` 7→8（新增一条 header 声明）。判据/白名单一个字未改 |
| `scripts/e2e/suites/routing.mjs` | 追加 §11（11 组 / 49 条断言），既有断言一字未动 |
| `scripts/check-comment-density.mjs` | 删除一条**过期**的密集文件登记（`internal/source/webhook_sign.go` 已降到阈值内）——基线只降不升 |

---

## 3. 验收 9 条实测

### 3.1 老契约不回归（含**对照测试**，不是只靠旧用例还绿）

```
$ go test ./internal/source/ -run TestHookInputLegacyControl -v
=== RUN   TestHookInputLegacyControl
--- PASS: TestHookInputLegacyControl (0.00s)
```

对照点：没有 `envelope` 时 `hookInput` 的三元组逐字取自投递体
（`type=t-old / id=delivery-1 / payload={"env":"prod"}`），即使请求里带了
`X-GitHub-Event` 头也被忽略。e2e 侧另有一条同语义的黑盒对照（`11.1 老契约`）。

### 3.2 GitHub 夹具

```
$ go test ./internal/source/ -run TestHookEnvelopeEndToEnd -v
--- PASS: TestHookEnvelopeEndToEnd (0.04s)
```

黑盒（e2e §11.2–11.4）：`X-GitHub-Event: issues` + `{"action":"opened","issue":{...}}`
→ 事件 `type = issues.opened`、`deliveryId` 来自 `X-GitHub-Delivery`、1 个任务且参数
`event=opened` / `issueTitle=hello`；`pull_request` → `no_match`、0 任务、200；
`ping` + 不合规 `payloadSchema` → **200**、0 任务、`pruned` 非空（不是 400）。

### 3.3 幂等

同一 `X-GitHub-Delivery` 重投 → `duplicate: true`、回执带回**第一次的 taskId**、
trace 上任务数与事件数不变。Go 侧与 e2e 都有；⚠️ 语义澄清：duplicate 返回的是第一次
持久化的**同一份**回执（`tasks` 不空），「0 新任务」= 任务行数不变，不是 `tasks: []`。

### 3.4 签名

`github-sha256` 正确 → 200；改一个字节（头不变）→ 401；不填 scheme 时旧头
`X-Anotify-Signature` 仍工作，不带签名头 → 401（见 §4 变异 3 与 e2e §11.6/11.7）。

### 3.5 `config.payload` 生效 + 入站覆盖

webhook 源写 `payload: {tenant:"acme", env:"static"}`、目标参数
`tenant=$(payload.tenant)` / `env=$(payload.env)`，投递 `{"payload":{"env":"inbound"}}`
→ 任务参数 `tenant=acme`、`env=inbound`（冲突时入站赢）。

### 3.6 `payloadSchema` 仍在

不合规 → 200、0 任务、`pruned[0].reason = "event_schema"`（e2e §11.9）。

### 3.7 变异 3 条（删了必须红，红后复绿）

| 变异 | 实测 |
| --- | --- |
| 删 `joinFrom` | `TestHookInputEnvelopeMapping`：`头 + joinFrom 应拼成 issues.opened, got "issues"`；`TestHookEnvelopeEndToEnd`：`type "issues" 不在 match.types [issues.opened] 里` → 0 任务 |
| 删 `deliveryId` 映射 | `deliveryId 应来自头, got ""`；`同 delivery 应幂等（回原 taskId）` 失败（第二次 `duplicate=false`、新 eventId） |
| 签名头写回旧口径 | e2e routing：`github-sha256：对原始字节签名 → 200 — 期望 200 实际 401`、`签名正确时建 1 个任务 — 期望 1 实际 0`（232 通过 / 2 失败） |

三次都回到源文件后复绿（`234 通过 / 0 失败`）。

### 3.8 门禁

```
$ make ci
MAKE_CI_EXIT=0

$ make e2e-affected
✅ E2E 全绿：30/30 个请求套件均达标（断言 2629 通过 / 0 失败，authority 清单覆盖 30 个）
E2E_EXIT=0

$ python3 -c "import json;print(json.load(open('scripts/e2e/known-env-failures.json'))['entries'])"
[]
```

`e2e-affected` 这次选中**全量 30 套**：`api/ingress.yaml` 是五份 OpenAPI 真源之一（共享面），
选择器按设计兜到全量。信封这一套自己的数字：`routing 234通过/0失败`（其中 §11 新增 49 条）；
同批最相关的三套：`contract 185/0`、`api_contract 220/0`、`security 100/0`。
没有登记任何 known-env 例外。

### 3.9 五件套

| 面 | 位置 |
| --- | --- |
| spec schema | `api/resource.yaml` 的 `SourceTarget.match` + `SourceMatch`；`SourceSpec.config` 描述（未知键/非法组合的 400 规则）；`api/ingress.yaml` 的 `X-Hub-Signature-256` + `PayloadBody.type` / `InlinePreviewBody.type` |
| parity 测试 | `TestOpenAPIParityDTOFields` 把 `SourceTarget.match` 与 `sourcespec.Target.Match` 双向比字段；`TestOpenAPIParityResponsesAndParams` 证明声明的两个签名头都**真被读**（`headerParamFloor` 7→8） |
| DTO | `sourcespec.Target.Match`（Go 侧唯一形状，`srcSpecView`/`srcSpecDoc` 直接复用它）、`sourcespec.Config.Envelope/Signature` |
| e2e | `scripts/e2e/suites/routing.mjs` §11（11 组 / 49 条断言，含 7 条写入期 400 与读投影不回退） |
| 单测 | `internal/sourcespec/envelope_test.go`（归一化/指针/match）、`internal/source/envelope_test.go`（映射与回落）、`internal/source/source_test.go`（端到端 + 静态 payload）、`internal/route/match_test.go`（闸门位置与剪枝事实） |

---

## 4. 注释预算（棘轮是**每目录冻结**，超与降都红）

新增注释必须同目录压缩买预算，基线不许上调。本次：

- `internal/sourcespec` / `internal/source` / `internal/route` / `internal/protocol` /
  `internal/server` / `scripts/e2e/suites` 六个目录**全部回到基线原值**（新增的注释行由同目录
  既有长注释压缩换得）。
- 唯一一次基线改动是**收缩**：删掉 `internal/source/webhook_sign.go` 的密集文件登记
  （实测 27 行 ≤ 阈值 29，`DENSE_STALE` 要求删；守卫自测同时全绿）。

```
$ make check-comment-density
✅ 与基线完全一致（超与降都算不一致）—— 有收紧请同批下调基线，新增必须在此刻被拦住
```

---

## 5. 已知边界与发现（不改，只登记）

1. **`payloadSchema` 只写 `required`、不写 `properties` 时是 no-op**（既有行为，不是本 PR
   引入）：`route.ValidatePayload` 遍历 `schema.order`（只由 `properties` 建立），所以
   `{"type":"object","required":["x"]}` 恒通过。JSON Schema 语义下 `required` 应独立生效；
   本次 e2e 的用例补了 `properties` 才让 `required` 真正生效。
2. **历史坏 payload 行**：webhook 的 `config.payload` 以前「接受但丢弃」，所以库里可能存在
   `payload: [1,2]` 这类非对象值。本 PR 起它是非法配置——只改 metadata 的 PATCH 仍可通过
   （config 原样保留、不重新归一化），但**动了 config 的 PATCH 会 400**（要求修好该字段）。
   这里没有为了「老行永远能改」而在写路径静默丢字段。
3. **`no_match` 的 `capability` 是声明名**：剪枝发生在解析之前（有意的），所以拿到的是
   `name`（`name@version` 取前半），不是解析出的能力行名。
4. **`payload: {pointer: ...}` 在平台 ping 类事件上会 400**（指针取不到）。GitHub 的推荐
   配法是 `payload: {whole: true}`：ping 的 body 形状与业务事件不同，用 pointer 会在入口就拒。

---

## 6. 不做（明确出界）

CEL / `outcomes` / `kind: poll` / connector（分属 A2、A3、B、E）；不改 UI 交互
（`config` 仍走 YAML 编辑面，读投影继续掩码 `token`/`secret`）；不新增 kind、不新增资源。

---

## 7. 复算命令

```bash
# 单测（含对照测试与 match 闸门）
go test ./internal/sourcespec/ ./internal/source/ ./internal/route/ -v

# 五件套的 spec ↔ DTO 对账
go test ./internal/server -run TestOpenAPIParity

# 注释棘轮（含自测）
make check-comment-density

# 门禁
make ci
make e2e-affected
jq '.entries' scripts/e2e/known-env-failures.json   # 期望 []

# 只跑信封这一套（真 server + 真 HTTP）
go build -o .e2e-bin/anotify ./cmd/server
go build -o .e2e-bin/devseed ./cmd/devseed
node scripts/e2e/suites/routing.mjs
```
