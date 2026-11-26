> **历史快照**：本文件是当时的交付记录，不随代码演进；行为以代码、测试与 `docs/contracts/` 为准。

<!-- snapshot-doc: 本报告里的数字是本次交付落地时的定格（复算命令见 §2/§5） -->

# PR #658 的注释密度付账（#644 第 1/2/3/4 条）

> 症状：`#658`（分支 `pi-subagent/issue-644-r1`）修的是行为（动作 body 的 `version` 下界、
> poll `timeoutMs` 区间、源错误 `message` 的前缀剪裁、写路径前缀常量化），但新加的注释把
> 全仓注释行推过了基线。注释棘轮是全仓双向冻结：**新注释必须由别处删除「付账」，不上调基线**。

## 1. 前 / 后

| 面 | 前 | 后 | 基线 | 差额 |
| --- | --- | --- | --- | --- |
| 仓库总量 | 43017 | 42905 | 42905 | −112 |
| `internal/server` | 7827 | 7734 | 7734 | −93 |
| `internal/store` | 6779 | 6760 | 6760 | −19 |

前测的 112 行差额正好等于本 PR 动过的两个目录的增量（93 + 19），其余 78 个目录与基线逐字一致。
后测**逐字等于基线**（超判红、降也判红），`BASELINE` 块未改一个字。

| 文件 | 前 | 后 | Δ |
| --- | --- | --- | --- |
| `internal/server/resource_source_test.go` | 173 | 136 | −37 |
| `internal/server/openapi_bounds_test.go` | 23 | 5 | −18 |
| `internal/server/resource_capability_lifecycle_test.go` | 98 | 81 | −17 |
| `internal/server/api_status.go` | 39 | 29 | −10 |
| `internal/server/resource_source.go` | 235 | 230 | −5 |
| `internal/server/resource_capability.go` | 341 | 337 | −4 |
| `internal/server/resource_capability_test.go` | 134 | 132 | −2 |
| `internal/store/source.go` | 102 | 90 | −12 |
| `internal/store/source_write_prefix_test.go` | 11 | 4 | −7 |

## 2. 复算

```console
$ node scripts/check-comment-density.mjs
🔍 注释密度棘轮（docs/lint-guards.md · L12）
   扫描 751 个文件（下限 600）· 注释行 42905 / 基线 42905
   docs 面 104 份 / 18078 行 · 待收缩 contracts=15份/2719行
   目录：80 个有基线，0 个与基线不一致

✅ 与基线完全一致（超与降都算不一致）—— 有收紧请同批下调基线，新增必须在此刻被拦住
```

## 3. 删的是哪三类注释（以及为什么它们可以删）

### ① 「改了什么」的叙述（本 PR 新增，主体）

- **修前 / 修后对照**：`schema 曾写 minimum: 1`、`400 文案曾回 version must be ≥ 1`、
  `store 曾手抄 create source: 前缀`、`实测的旧 message 是三层叠加` —— 改动的形状由 diff 与
  测试断言直接读出，注释只是把同一件事第二次讲。
- **review 轮次编号**：`review 第 1 轮第 3 条`、`review 第 5 条指出旧注释写…` —— 属于 PR 讨论，
  不属于代码；关掉 PR 页之后这些编号没有可追溯对象。
- **迁移史**：`v3 有专门用例 TestWriteSourceErrReservedNameIs400…v4 迁移时丢了，这里恢复同一安全意图`
  —— 历史流水账（棘轮自己的判据就把这一类列为可删）。

### ② 同一件事写两遍（本 PR 新增）

- 测试头部把七个子用例枚举一遍（`①POST 配置 ②timeoutMs 越界 ③边界内侧…`），每个用例上一行
  再标一次 `① … ⑦ …`，而断言自己的失败文案已经把「哪一步、为什么」写死了：注释只是第三遍。
- `三条一起钉： - POST 非法 target → 400 …` 与紧随其后的 `①/②/③` 各标一遍 —— 删掉清单后，
  三条各自由断言表达。
- 一个大块用 `//` 空行切成 3–4 段讲同一句（「为什么是常量而不是字面量」），压缩成一段
  陈述句，信息不减。

### ③ 复述签名的旧注释（同文件 / 同域，非本 PR 行）

`// 列表。`、`// 单对象读。`、`// createSource 建一个源并返回 201 文档。`、
`// newSourceAPI 构造 Source 资源域。`、`// newCapabilityAPI 构造 Capability 资源域。`、
`// srcSpecKind 取 spec.kind（去空白）。`、`// GetSourceByName 按 (namespaceID, name) 取源…`、
`// IsValidSourceKind 判断种类是否合法。`、`// nullableUnix 把 *int64 转成 SQL 参数…`

判据：**函数名与签名已经把这句话说完**，注释没有提供第二份信息（没有边界、没有坑、没有
反直觉的取舍）。带信息的一律留（见 §4）。

## 4. 刻意保留的「为什么 / 坑 / 边界」注释

- `openapi_bounds_test.go`：`TestOpenAPIParity` 只对账 properties ↔ json tag、**从不施加 `minimum`**
  （样本里的 `version` 带 `omitempty`、永不进 JSON）—— 这正是「下界改回去也没人红」的坑；
  以及**测试名必须以 `TestOpenAPIParity` 开头**，否则 `make contract` 的 `-run` 选不中它。
- `api_status.go`：前缀是 store 的定位信息、`reason` 已把分类说全（为什么可以剪）；
  只剪前缀不重建文本（`CausesError` 的 `field：原因` 拼接仍只有一处实现）；层数不写死；
  前缀表引用 `store.SourceWritePrefixes`（手抄字面量时改一个空格就静默失配）；
  **kind 域的 `invalid source kind: "pollz"（可选：…）` 必须留着** —— 可选值清单是用户改对的唯一线索。
- `resource_capability.go`：`0` 与省略同义（`version <= 0` 走 ResolveRef = live），拒的只是负数；
  旧文案 `≥ 1` 与试跑的 `≥ 0` 矛盾。
- `resource_capability_lifecycle_test.go`：schema `minimum: 1` 与 description「省略 / 0 = live」
  的自相矛盾；deploy 三处（schema / 400 文案 / 判据）修前不同向、修后同向。
- `resource_source_test.go`：前缀清单**不手抄**（遍历 `store.SourceWritePrefixes`）；
  `invalid source kind:` 不剪的理由；`details.causes[]` 的路径定位不因剪前缀而丢。
- `internal/store/source.go`：写路径前缀做成常量的理由（两侧同源，改一处即改全部）；
  新增前缀必须登记进 `SourceWritePrefixes`，否则用户会在 `message` 里看到它。
- `internal/store/source_write_prefix_test.go`：`source.go` 里再现 `<动词> source: ` 字面量
  = 有人绕过了常量；读路径（get/scan/list）不要求走常量。

## 5. 跑过的命令

```console
$ make lint-go
go vet ./...
GOTOOLCHAIN=go1.25.0 ".bin/staticcheck" -checks=SA*,U1000 ./...
（退出码 0）

$ go test ./internal/server/... -count=1
ok  	github.com/zzjcool/anotify/internal/server	59.986s

$ go test ./internal/store/... -count=1
ok  	github.com/zzjcool/anotify/internal/store	20.528s
ok  	github.com/zzjcool/anotify/internal/store/v4test	48.837s

$ make contract
  A2/A4/A5/A6/A7/A8/A9/A11 通过（35 张表；3 张 kind 禁令、4 条内容标识、22 个中枢包、5 份 spec）
  信封/status/cursor/CAS 字段级检查通过（5 份文件 / 264 个 schema / 3 个具名 snake_case 例外）
  OpenAPI exact-five 通过（5 份文件 / 146 个 operation / 146 个唯一 operationId）
✅ 契约静态断言通过
```

`make ci` 全量（同一棵树、同一次会话）逐段结果：

```console
$ make ci     # contract → check-agents → lint-go → fe → check-dist-fresh → guard self-test
              # → check-yaml-doc → check-test-discipline → check-comment-density
              # → check-annotations → check-doc-links → check-doc-endpoints → check-examples
              # → check-affected-map → 前端单测 → check-secrets → build → go test ./...
✅ 契约静态断言（A2/A4/A5/A6/A7 + openapi 一致性 + 字段级核对 + anode 可编译）通过
✅ 模块约定与代码一致
✅ 注释行 42905 / 基线 42905 · 0 个目录与基线不一致
✅ unbacked 295 / 基线 295 · unmarked 209 / 基线 209
ok  	github.com/zzjcool/anotify/internal/server	59.431s
ok  	github.com/zzjcool/anotify/internal/store	10.098s
ok  	github.com/zzjcool/anotify/internal/store/v4test	28.809s
# 前端单测 213 tests / 213 pass / 0 fail
```

`go test ./...` 的余下失败全部来自**平台**而非本改动：`cmd/anode` 的安装/服务用例自带
「只支持 Linux（当前 Darwin）」与 systemd 前提；agent TUI 用例假定路径不被 canonicalize
（本机拿到的 `/var` 实际是 `/private/var`）。CI 的 runner 是 `ubuntu-latest`，本机 macOS 复现不了这一层。

## 6. 没动的东西

- 断言 / 测试覆盖：**一行都没删**（删的与改的全是注释行，`t.Fatalf` 文案原样）。
- `scripts/check-comment-density.mjs` 的 `BASELINE` 块：一个字没改（不上调，也没下调）。
- 任何白名单 / 基线常量（`check-annotations`、`check-test-discipline` 等）。
- `api/resource.yaml` 的契约内容：本 PR 的行为修复保持原样。

### 机械可查：Go 侧改动只有注释

```console
$ git diff -U0 -- '*.go' | grep -E '^[+-]' | grep -vE '^(\+\+\+|---)' \
    | grep -vE '^[+-]\s*(//)?\s*$|^[+-]\s*//'
（无输出 = 每一个增删的行都是注释行或空注释行；代码行、断言、字符串文案一个都没变）
```

## 7. 未决问题：本分支 tip 上还有第二处红

派发卡只列了注释密度一处。实测本分支 tip（`eaa8db37`）在 `check-annotations.mjs` 的 **C 棘轮**
上也超：未锚定的约束性注释 298 > 基线 295（D 棘轮 209 = 基线，正常）。这 3 条正是本 PR 新写的
「pass 只对账…」这类叙述，删除它们与 §3 ① 是同一批动作。删完落在 295 = 基线 ——
`check-annotations` 是双向棘轮，超与降都判错，所以必须**恰好** 295，而不是 ≤295。

本机剩余未复现项：`cmd/anode` 安装/服务用例与 agent TUI 用例（见 §5 的平台说明）—— 与本次改动无关，
但本机无法给出 `make ci` 的绿灯结论，只能给出「除这些平台用例外全绿」。
