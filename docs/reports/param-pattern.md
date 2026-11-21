> snapshot-doc: 2026-11-20 · #475 交付记录（文中数字是交付时实测，不复算）
>
> **历史快照**：本文件是当时的交付记录，不随代码演进；行为以代码、测试与 `docs/contracts/` 为准。

# 交付报告 · 参数 `pattern` 写入期校验（#475）

**issue**：https://github.com/zzjcool/anotify/issues/475
**分支**：`pi-subagent/hw-nj/anotify-475-pattern-c13514fd`
**PR**：https://github.com/zzjcool/anotify/pull/498

## 结论

按方向 1 实现 `pattern`，**不删文档承诺**。

承诺的原落点 `docs/design/v4/capability.md` 已随 #473 整套移除（`docs/README.md` §2），
所以现行文档面改成三处真正能被读到的落点，并逐处与实现对齐：

- `api/resource.yaml` 的 `CapabilitySpec.params` 描述（HTTP 形状的正本）；
- `docs/contracts/cap-lifecycle/WIRE.md` 的试跑端点（`details.causes[].field` 口径）；
- `examples/capabilities/run-per-param-dir.yaml` 的样例注释（用户抄走的那份）。

## 定死的语义

| 维度 | 语义 | 依据 |
|---|---|---|
| 正则方言 | RE2（Go `regexp`）：线性时间，不存在灾难性回溯 | `internal/schema/schema.go:427` `CompilePattern` |
| 作用类型 | **只约束字符串值**；非 string 值不判 pattern（JSON Schema 标准），类型由 `type` 管 | `internal/schema/schema_test.go`、`internal/route/params_test.go` |
| 匹配方式 | 子串匹配（`MatchString`）；整串匹配要在 pattern 里自己写 `^...$` | 同上 |
| 长度上限 | 1024 字节；超长按 schema 不可用拒绝（不截断） | `schema.MaxPatternLen` / `artifact.maxParamPatternLen` |
| 编译缓存 | 派发期缓存 256 条（写入期每条只编一次，不缓存） | `internal/schema/schema.go:418-447` |
| `pattern` 不是字符串 | 写入期 400 | `validateParamPatterns` |
| 坏正则 | **能力保存时就 400**，不写进库；旧数据派发期记 `param_schema` 剪枝 | `internal/artifact/artifact.go:701` |
| 值不匹配 | 试跑 → 400；派发（源事件 / emit）→ `pruned[]` reason=`param_type`，不建任务 | `internal/route/params.go:304` |
| 数组元素 | **不递归**：`items` / `prefixItems` 里的 pattern 只在写入期编译，不在派发期执行 | route 参数子集原有边界 |
| reason 词表 | 不新增剪枝码：值不匹配复用 `param_type`；坏正则用写入期短码 `param_pattern_invalid` | 封闭词表不动 |

## 实现位置

| 位置 | 职责 |
|---|---|
| `internal/schema/schema.go:427` | `CompilePattern`：RE2 + 长度上限 + 进程内缓存 |
| `internal/route/params.go:119` | `parseParamProperty` 解析并编译 `pattern`；坏 pattern → `param_schema` |
| `internal/route/params.go:304` | `paramProperty.check`：pattern 只对 string 执行，错误带精确字段路径 |
| `internal/route/params.go:335` | `checkObject` 递归嵌套对象，字段路径逐级拼接 |
| `internal/artifact/artifact.go:413` | `Error.Fields`：与 `Problems` 平行的机器可读字段定位 |
| `internal/artifact/artifact.go:701` | `validateParamPatterns`：写入期遍历 schema 并编译每条 pattern |
| `internal/server/resource_capability.go:1609` | `capabilitySpecError`：把 `Fields` 翻成 `details.causes[].field` |

## 400 的字段路径样式

| 场景 | HTTP | reason | `details.causes[0].field` |
|---|---|---|---|
| 试跑：值不匹配 | 400 | `param_type` | `params.project` / `params.meta.dir` / `params.tree.root.name` |
| 写入：坏正则 / 非字符串 / 超长 | 400 | `param_pattern_invalid` | `spec.params.properties.project.pattern` / `spec.params.properties.meta.properties.dir.pattern` / `spec.params.properties.list.items.pattern` |
| 派发：值不匹配 | 200（事件已收） | `pruned[].reason=param_type` | 不进 wire（既有形状）；`route.Prune.Field` 内部保留 |

message 同时给「期望 / 实际」两边：

- 值校验：`参数 meta.dir 应匹配 pattern "^[a-z]+$"，实际 "A"`；
- 坏正则：`spec.params.properties.project.pattern 不是合法的 RE2 正则 "["：error parsing regexp: …`。

写入期不再只回第一条问题：命中定位错误时，所有 problem 都会进 `details.causes[]`（无定位的为空 field）；
没有定位信息的历史错误响应体逐字节不变。

## 验收

| # | 验收 | 结果 |
|---|---|---|
| 1 | 合法值通过；非法值 400 且字段路径精确（顶层 / 嵌套 / 深嵌套三处） | ✅ `go test ./internal/route ./internal/server -run Pattern -count=1` |
| 2 | 坏正则写入期即 400（不是运行期） | ✅ `go test ./internal/artifact ./internal/server -run 'Pattern' -count=1` |
| 3 | 非 string + pattern 的语义定死并写测试、文档同步 | ✅ `TestValuePatternOnlyConstrainsStrings`、`TestPrepareParamsPatternAcceptsMatchingValues`（`{"count": 7}` 通过） |
| 4 | 变异：把校验短路成 `return nil` → 验收 1 必红 | ✅ 见下节 |
| 5 | `make ci` exit 0；`make e2e-affected` 全绿 | ✅ 见下节 |

### 变异记录（两次都真跑过，跑完已还原）

1. `internal/route/params.go` 的 `paramProperty.check` 首行插入 `return nil`：
   `go test ./internal/route -run Pattern` → **FAIL**（「输入 … 应被拒」四例全红）；
   `go test ./internal/server -run Pattern` → **FAIL**（原 400 变 200，且「参数被拒时不应建任务，实际 4 条」）。
2. `internal/artifact/artifact.go` 的 `validateParamPatterns` 首行插入 `return nil`：
   `go test ./internal/artifact -run TestValidateRejectsInvalidParamPattern` → **FAIL**；
   `go test ./internal/server -run TestCapabilityWriteRejectsBadParamPattern` → **FAIL**（坏正则的 POST 返回 201）。

两次还原后重跑，同一批用例恢复绿。

## 命令与结果

```text
$ go test ./internal/route ./internal/schema ./internal/artifact -count=1
ok  github.com/zzjcool/anotify/internal/route
ok  github.com/zzjcool/anotify/internal/schema
ok  github.com/zzjcool/anotify/internal/artifact

$ make ci
EXIT=0
  unbacked 299 / 基线 299（未上调）
  unmarked 312 / 基线 312（未上调）
  ✅ 注解即外键守卫通过（check-annotations）
  ✅ REF_DEAD 0（check-doc-links）
  ✅ 文档端点检查：109 条 spec 路径 · 31 个文档文件 · 99 处 /v1/ 引用全部存在
  ✅ 样例全部过 artifact.Parse，且都带 workDir
  ✅ 节点依赖边界：cmd/anode 只依赖 protocol + artifact

$ make e2e-affected        # api/resource.yaml 落在共享面 → 全量 29/29
  断言通过: 2455
  断言失败: 0
  ✅ E2E 全绿：29/29 个请求套件均达标

$ node scripts/e2e/ci-e2e-verdict.mjs --results .e2e-bin/results --known scripts/e2e/known-env-failures.json
✅ e2e 判定：清单内 0 条（已容忍），清单外 0 条
```

> 第一次全量 E2E 的 `vars_ui` 在一次 `waitForResponse` 上 15s 超时（另一 worktree 同时在跑全量 E2E，
> 机器过载）；单独重跑全量后 61/61 通过。登记清单未新增任何条目（`entries` 仍为空）。

## 新增测试

`internal/route/params_test.go`：pattern 通过 / 拒绝（顶层、嵌套、深嵌套、字符串值）/ 坏 pattern 剪枝。
`internal/schema/schema_test.go`：编译缓存命中、坏正则与超长拒绝、pattern 只作用于字符串。
`internal/artifact/params_pattern_test.go`：写入期坏 pattern 的短码与字段路径（顶层 / 嵌套 / items / 非字符串 / 超长）、
合法 pattern 通过、`default` 数据里的 `pattern` 不当关键字、长度上限与 `internal/schema` 不漂移。
`internal/server/resource_capability_pattern_test.go`：能力写入坏正则 400 + `causes[].field`；
试跑值不匹配 400 + 三处精确路径 + 拒绝时不建任务；非 string 值通过。

## 边界与遗留

- **数组元素不递归**是 route 参数子集的既有边界：写入期会编译 `items` / `prefixItems` 里的 pattern，
  但派发期只校验顶层与嵌套对象。文档已照实写，不暗示数组元素会被执行。
- 复用 `param_type` 表示「值不匹配 pattern」：封闭剪枝词表没有新增成员；如果以后要单独观测，
  再按词表变更流程加 `param_pattern`，不在本 issue 里顺手扩。
- 旧库里若已存在坏 pattern（本 PR 之前写入的）：不会自动迁移；重新保存会被 400，
  `GET` 的 `Implemented` 条件会显示 false；派发期按 `param_schema` 剪枝。v4 未发布，实际数据面为零。
