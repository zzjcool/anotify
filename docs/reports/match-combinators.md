> **历史快照**：本文件是本次交付的记录，不随代码演进；行为以代码、测试与 `api/` 为准。

<!-- snapshot-doc: 2026-11-22 #519 A2 交付时的定格数字（make ci / e2e 的真实输出），复算命令见 §8 -->
# targets[].match 的字段级组合子（#519 A2）交付报告

> 分支 `pi-subagent/hw-nj/anotify-519-match-e9990912` · base `main`（`5f4f570e`）
> 设计记录：#512 · 本阶段范围：#519 · PR：[#520](https://github.com/zzjcool/anotify/pull/520)（正文 `Fixes #519`）

一句话：**让 `targets[].match` 在事件入库、建任务之前做字段级判断**（标签、assignee、
「这个 issue 其实是 PR」），`types` 之外新增 `all` / `any` 两个组合子与叶子开关 `negate`。
不引入新 kind、不引入新资源、不引入表达式语言（CEL 属 A3，这里明确不做）。

---

## 1. 冻结语义（实现里定死，PR 正文同稿）

### 1.1 形状

```yaml
targets:
  - capability: issue-router
    match:
      types: ["issues.labeled"]                        # 白名单（#513 已有）
      all:                                             # 全部满足（AND）
        - { field: issue.pull_request, op: exists, negate: true }
        - { field: issue.labels.0.name, op: in, value: ["bug", "regression"] }
      any:                                             # 任一满足（OR）
        - { field: repository.full_name, op: "==", value: "me/infra" }
```

- `types` / `all` / `any` **同时出现是 AND**；`all` 内 AND；`any` 内 OR；三组都必须非空
  （空组合子恒成立 = 等于没写，写入期 400）。
- 叶子：`field`（点号路径 + 数字下标）、`op`（封闭 10 个：`== != > >= < <= contains
  matches in exists`）、`value`、`negate`。词表与 #512 的 router 规则表**逐字一致**。
- 求值顺序不变：`match → resolve → params → 能力开关 → ready`。不命中 → 事件照常入库、
  回执 `pruned[].reason = no_match`、HTTP **200**、0 任务（不新增 reason、不报失败）。

### 1.2 求值规则（缺字段/类型不符一律 false，不报错）

| op | 判据 |
| --- | --- |
| `==` / `!=` | JSON 语义相等：数字按值（`3` == `3.0`）、数组/对象深比较、`null` 只等于 `null` |
| `> >= < <=` | 只比数字；任何一侧不是数字 → false |
| `contains` | 字符串包含，**大小写敏感**（#512 的「不敏感 + caseSensitive」待定项没有采纳：多一个开关就多一处静默偏差） |
| `matches` | 左侧字符串 + RE2 正则（写入期编译；读路径上手改坏的 pattern 按不命中处理，不 500） |
| `in` | `value` 是数组，字段值 ∈ 数组（元素比较同 `==`） |
| `exists` | 只看路径取不取得到（显式 `null` 也算「存在」）；不看 `value` |
| `negate: true` | 翻转**整行**结论，含「字段缺失」——想要「缺失或不等于 X」就写 `==` + `negate`（`!=` 在字段缺失时是 false，与 `==` 一样） |

`field` 与 `config.envelope` 的指针、params 模板**共用同一份 `walkPath`**
（`sourcespec.ParsePointer` / `LookupPointer`）：`$.a.b` 与 `a.b` 同义、数字段是数组下标、
连字符在写入期 400。本次没有新增第二份路径解析。

### 1.3 写入期 400 与定位

`details.causes[].field` 是**文档路径**（相对 spec，前缀 `spec.`），例如
`spec.targets[0].match.all[1].op`——与 YAML 写面（`yamldoc`）的 causes 同一套写法。

| 拒绝原因 | causes[].field | causes[].reason |
| --- | --- | --- |
| 未知键（`match.type` / 叶子写 `values`） | 精确到键 | `unknown_field` |
| 空组合子（`all: []` / `any: []`） | `…match.all` | `value_invalid` |
| 非法 op（`=~`） | `…all[0].op` | `value_invalid` |
| `matches` 正则编不过 | `…all[0].value` | `value_invalid` |
| `field` 路径语法非法（`issue.labels.0-name`） | `…all[0].field` | `value_invalid` |
| 单组行数 > 32 | `…match.all` / `…match.any`（文案给上限 32） | `value_invalid` |
| `value` 形状与 op 不符（`> "3"` / `matches 3` / `in` 非数组） | `…[0].value` | `value_invalid` |
| `field` / `op` 缺失、`negate` 非布尔、行不是对象、`match` 不是对象 | 精确到该位置 | `value_invalid` |

- **所有问题一次报完**（不是撞到第一条就返回）：前端逐个点红点，一次改完。
- 顶层 `reason`：整条错误**只有一个未知键**时沿用文档层的 `unknown_field`（客户端已按它
  分支，A1 的 e2e 11.10 断言逐字保持绿）；其余仍是 source 域的既有封闭码
  `invalid_source_config`。`errors.Is(err, ErrInvalidConfig)` 恒成立。
- 归一化：`types` 去空白、`field` 去掉 `$.` 前缀、默认值不变；读投影原样回显
  `types`/`all`/`any`/`negate`/`value`（UI 仍走 YAML 编辑面）。

---

## 2. 改了什么（按层）

| 文件 | 改动 |
| --- | --- |
| `internal/sourcespec/match.go`（新） | `Match` 加 `All`/`Any`；`MatchLeaf`；10 个 op 常量 + `MatchMaxRows=32`；`(*Match).Matches(eventType, payload)`；叶子求值/比较表；`normalizeMatch` 全部改写成「带路径的 causes 收集」；`Match.UnmarshalJSON` 留底原始字节（未知键只有它认得出来，判定要等到有 `targets[i]` 下标的 `ValidateTargets`） |
| `internal/sourcespec/envelope.go` | `Match` / `Matches` / `normalizeMatch` 迁出（信封与签名不受影响） |
| `internal/sourcespec/sourcespec.go` | `Cause` / `CausesError`（带位置的多条校验失败，`Unwrap → ErrInvalidConfig`） |
| `internal/sourcespec/config.go` | `ValidateTargets` 改为聚合 causes（空 `capability` 也带位置）；模板错误保持既有形状 |
| `internal/route/route.go` | 闸门改用 `Matches(eventType, payload)`；`pruneNoMatch` 的 detail 由判定方给（点名 `types` 没中 / `all` 第几行 / `any` 全不成立） |
| `internal/server/api_status.go` | `ErrInvalidSourceConfig` 分支落 `details.causes[]`（补 `spec.` 前缀）+ 顶层 reason 规则 |
| `api/resource.yaml` | `SourceMatch` 重写（三组语义/400 规则）、新增 `SourceMatchLeaf`（`op` 给 `enum`）、`SourceMatch.required` 取消（三组任选） |
| `internal/server/openapi_parity_test.go` | `specSchemaFloor` 245 → 246（新增 `SourceMatchLeaf`）+ 注释 |
| `scripts/e2e/suites/routing.mjs` | 追加 §12（8 组 / **51 条断言**），既有断言一字未动 |
| `docs/reports/match-combinators.md` | 本文件 |

**五件套**：spec（`api/resource.yaml` 的 `SourceMatch` + `SourceMatchLeaf`）↔ parity
（`TestOpenAPIParityDTOFields` 顺着 `SourceTarget.match` 递归比到叶子，**没有新增 pair**，
`specSchemaFloor` 只 +1）↔ DTO（`sourcespec.Match` / `MatchLeaf`，`srcSpecView`、
`spec.targets` 直接复用）↔ e2e（routing §12）↔ 单测（`sourcespec/match_test.go`、
`route/match_test.go`、`server/resource_source_match_test.go`）。

---

## 3. 验收 9 条实测

命令前缀（下同）：
`export PATH=/home/zzjcool/.local/share/mise/shims:/usr/local/bin:/usr/bin:/bin`

### 3.1 PR-as-issue（真实坑：`issues.labeled` + PR）

```yaml
match: { types: ["issues.labeled"], all: [{ field: issue.pull_request, op: exists, negate: true }] }
```

- 带 `issue.pull_request` 的 `issues.labeled` 投递 → HTTP **200**、0 任务、
  `pruned[0].reason = "no_match"`、detail 含 `match.all[0]`。
- 同一 body 去掉 `pull_request` → 200、**1 任务**。

实测：e2e `12.1`（6 条断言全绿）；单测 `TestMatchCombinatorSemantics`（`prGate` +
`prIssue`/`plainIssue`）与 `TestPlanFromSourceCombinatorGate`（真 store 的闸门）。

### 3.2 标签过滤（数组下标路径）

`all: [{ field: "issue.labels.0.name", op: "in", value: ["bug","regression"] }]`：
`bug` → 1 任务；`docs` → 0 任务 + `no_match`；`["p2","regression","bug"]`（`regression` 在
第 2 位）→ 0 任务（证明钉的是**下标 0**，不是「数组里任意一个」）。e2e `12.2`（6 条）。

### 3.3 缺失字段

`all: [{ field: "issue.assignee.login", op: "==", value: "alice" }]`：无 `assignee` →
200（**不是 500**）+ 0 任务 + `no_match`；补上 `assignee.login=alice` → 1 任务。
e2e `12.3`（5 条）；单测逐条钉住「缺失字段 + `!=` 仍 false」「缺失 + `negate` 成立」。

### 3.4 数值与正则

`all: [{ field: "issue.number", op: ">", value: 3 }, { field: "issue.title", op: "matches", value: "^URGENT" }]`：
`number=5 & URGENT…` → 1 任务；`number=2` → 0；`WIP…` → 0。写坏正则 `(`：
400 `invalid_source_config`，`details.causes[0].field = "spec.targets[0].match.all[0].value"`。
e2e `12.4`（7 条）；单测 `TestNormalizeMatchRejects/正则编不过`。

### 3.5 组合语义（`types` + `all` + `any` = AND）

- 只有 `any` 命中（`pull_request` 存在 + 标签 `bug`）→ 0 任务 + `no_match`；
- 事件 type 是 `pull_request.opened`（`types` 不中）但组合子会成立 → 0 任务 + `no_match`；
- 三者都成立 → 200 / 1 任务。

e2e `12.5`（6 条）；单测 `TestMatchCombinatorSemantics/onlyAny`。

### 3.6 越界（成本闸）

`all` 写 **33 行** → 400，`message` 含 `32`，`causes[0].field = "spec.targets[0].match.all"`；
**32 行** → 201（边界是 `> 32`，不是 `≥ 32`）。e2e `12.6`（4 条）；
单测 `TestNormalizeMatchRowLimit`（`all`/`any` 各测边界）。

### 3.7 变异 2 条（红后复绿）

| 变异 | Go 侧实测 | e2e 侧实测 |
| --- | --- | --- |
| ① 叶子求值恒 `true`（`eval` 直接 `return true`） | `TestMatchLeafOps` 11 条 red、`TestMatchCombinatorSemantics` red、`TestPlanFromSourceCombinatorGate` red（`PR-as-issue 不该派发也不该报失败`） | routing **282 通过 / 13 失败**：`12.1`（3 条）、`12.2`（4 条）、`12.3`（2 条）、`12.4`（2 条）、`12.5`（2 条）全红 |
| ② 去掉 `negate`（不翻转整行） | `TestMatchLeafOps` 2 条 red、`TestMatchCombinatorSemantics` red、`TestPlanFromSourceCombinatorGate` red | routing **288 通过 / 7 失败**：`12.1`（4 条）、`12.5`（3 条）全红 |

两次都回到源文件后复绿：`go test ./internal/sourcespec/ ./internal/route/` PASS，
routing **295 通过 / 0 失败**。

### 3.8 门禁

```
$ make ci
MAKE_CI_EXIT=0
（contract / check-agents / lint-go / fe / guard-selftest / yaml-doc / test-discipline /
  comment-density / annotations / doc-links / doc-endpoints / examples / affected-map /
  test-frontend / check-secrets / build / 单测 + tmp-leak-guard / bench-dispatch /
  harness 自测 全绿；临时目录零残留）

$ make e2e-affected
⚠️ 全量 e2e（30/30 套）：api/resource.yaml 落在共享面（五份 OpenAPI 真源）
✅ E2E 全绿：30/30 个请求套件均达标（断言 2820 通过 / 0 失败，authority 清单覆盖 30 个）
E2E_EXIT=0   总耗时 354.5s   临时目录零残留
```

同批最相关的四套：`routing 295/0`（含 §12 的 51 条）、`contract 185/0`、
`api_contract 220/0`、`security 100/0`；`persistence 82/0`、`ws_protocol 134/0` 也在内。
没有登记任何 known-env 例外（`known-env-failures.json` 的 `entries` 仍是 `[]`）。

### 3.9 五件套

见 §2 末段。parity 的门禁是**递归**的：`SourceTarget.match → SourceMatch → all/any items →
SourceMatchLeaf`，所以叶子的 `field/op/value/negate` 四个键双向受管（`value` 是 `any`，
文档侧只声明存在性，不钉类型）。

---

## 4. 注释预算（棘轮是**每目录冻结**，超与降都红）

新增注释由**同目录既有长注释压缩**换得，基线一个数字都没上调：

- `internal/sourcespec` / `internal/route` / `internal/server` / `scripts/e2e/suites`
  四个目录**全部回到基线原值**。
- `scripts/e2e/suites` 的新增 §12 **没有加一行注释**（用例名自述语义）——目录仍是 2182。
- `docs/reports` 是 evidence 面（按 PR 增长，不定上限），不涉及棘轮。

```
$ make check-comment-density
✅ 与基线完全一致（超与降都算不一致）
```

---

## 5. 已知边界（本次不改，只登记）

1. **`SameTargets` 不比较 `match`**（A1 起就是如此）：playbook 的安装比较看
   `capability` + `params`，所以「装着的源与包里声明的源只有 `match` 不同」不会被判成
   `targets_unapplied`。A2 让 `match` 更有分量之后这是一个真实缺口；修它要连
   `targets_unapplied` 的语义一起定，不在本 issue 范围。
2. **`matches` 的正则在每次求值时才编译**（写入期只验证「编得过」）。一条事件最多
   2×32 行动用，正则编译在微秒级；如果要省，得把编译结果做成有界缓存，而 `Match` 是
   store 里的 JSON 形状，不该带运行时状态。
3. **顶层 `reason` 的两档规则**（单未知键 → `unknown_field`，其余 →
   `invalid_source_config`）是为兼容 A1 的 e2e 断言而定；`causes[].reason` 才是逐条判据。
4. **`contains` 大小写敏感**：设计记录 #512 里「不敏感 + `caseSensitive` 逃生」是待定项，
   本版选敏感（少一个静默偏差）；要改就按 issue 走，别在实现里偷偷加开关。
5. **`match` 子树对 YAML 文档层是 opaque**（`*Match` 实现了 `UnmarshalJSON`）：所以 YAML
   写面的未知键现在由 `normalizeMatch` 报（`invalid_source_config`/`unknown_field` +
   causes），而不是 `yamldoc` 报。位置与机器码一致，只是判定层换了。

---

## 6. 不做（明确出界）

CEL / `targets[].outcomes` / router（A3）、`kind: poll`（B）、derived fields、UI 表单
（`match` 仍走 YAML 编辑面）。也不新增 prune reason（失败仍是 `no_match`）。

---

## 7. 复算命令

```bash
# 单测（判定表 / 写入期 causes / 闸门位置）
go test ./internal/sourcespec/ ./internal/route/ ./internal/server -run 'Match|SourceMatch' -v

# 五件套的 spec ↔ DTO 对账
go test ./internal/server -run TestOpenAPIParity

# 注释棘轮
make check-comment-density

# 只跑本 issue 这一套（真 server + 真 HTTP）
./scripts/e2e/run_all.sh routing

# 门禁
make ci
make e2e-affected
jq '.entries' scripts/e2e/known-env-failures.json   # 期望 []
```
