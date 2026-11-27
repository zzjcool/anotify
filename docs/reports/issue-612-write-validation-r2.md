# #612 写入期判据 · review r2 返工取证（PR #694）

<!-- snapshot-doc: 2026-11-27 的返工取证快照（数字与门禁输出是当次实跑的定格，不是对未来的承诺；复跑以新输出为准） -->

分支 `pi-subagent/issue-612-r1`，本轮提交 `246beefe`（r1 = `58ef8f59`，首个 = `85f3d086`）。
review findings 原文见任务附带的 `/tmp/rev612.txt`（4544 字）：2 条主要 + 5 条次要（编号 3–7）。

## 0. 一句话结论

- **主要 1**（metadata-only PATCH 被 #612 判据打死 + 400 形状退化）：PATCH 现在走
  `artifact.ValidateMetadataUpdate`（跳过 params 可用性判据），存量坏行可以只改
  `displayName` / `spec.next`；store 层的 `*artifact.Error` 保留在错误链上，短码与
  `details.causes[].field` 不再退化。用例红→绿 + 变异红都留下了输出。
- **主要 2**（契约写在错的端点）：#612 的写入期契约从 `api/ingress.yaml` 的
  `InlineSourceSpec.config`（dry-run 请求体，实测 200 + `event_schema` 剪枝）搬到
  `api/resource.yaml` 的 `SourceSpec.config`（POST/PATCH 的真 400）；ingress 侧改写成准确描述，
  并补用例钉住 dry-run 语义。
- 次要 3–7 全部按 review 原文处理（见 §2）。注释密度按目录逐条配平，总量与基线仍是 42873。

## 1. 两条主要项的改法与取证

### 1.1 主要 1：metadata-only PATCH

**链路（review 是对的，r1 的自述是错的）**：
`internal/server/resource_capability.go` 的 `patch` → `DB.UpdateCapability`
→ `internal/store/resource_cas.go:75` `updateCapabilityOn` → `normalizeCapability`
（`internal/store/capability.go`）→ `artifact.Normalize` + `artifact.Validate`
→ 本 PR 新增的 `validateParamSchemaUsable`（`artifact.go`）→ `schema.ParseParams`。
**每一次 CAS 都整份重校验**，与 handler 无关。

**修法**：

| 位置 | 改动 |
| --- | --- |
| `internal/artifact/artifact.go` | 新增 `ValidateMetadataUpdate(a)`（= `validateArtifact(a, false)`，跳过 #612 的 params 可用性判据）；`Validate` 保持严格（= `validateArtifact(a, true)`）。写新内容的路径（POST / PUT / playbook 安装 / 节点侧 `Cache.Store`）全部走 `Validate`，语义没动。 |
| `internal/store/capability.go` | `normalizeCapability` / 新增 `normalizeCapabilityMetadata` 收敛到 `normalizeCapabilityWith(c, validate func(*artifact.Artifact) error)`；`fmt.Errorf("%w: %s", ..., err.Error())` → `fmt.Errorf("%w: %w", ...)`（错误文本逐字不变），`*artifact.Error` 留在链上。 |
| `internal/store/resource_cas.go` | `updateCapabilityOn` 改调 `normalizeCapabilityMetadata`（该函数只服务 PATCH / 改 displayName 两条元数据路径）。 |
| `internal/server/api_status.go` | `case errors.Is(err, store.ErrCapabilityInvalid)` 从 `writeStatus(400, BadRequest, err.Error())` 改成 `writeAPIError(w, capabilitySpecError(err))`：链上有 `*artifact.Error` 时透出短码 + `causes[].field`，取不到才回落裸 400（与之前一致）。 |
| `internal/server/resource_capability.go` | `capabilitySpecError` 的注释改成实际契约（不再是「不复用 store.ErrCapabilityInvalid」）。 |

只改 (a) 不做 (b) 也能过 review，但 (b) 让「store 层的 400」这一整类错误都有机器可读形状，
两条一起做才不会留下「换个路径又是裸 BadRequest」的洞。

**红（修复前，测试先落地）**：

```
$ go test ./internal/server -run TestCapabilityPatchMetadataOnlyOnLegacyParamSchema -count=1
--- FAIL: TestCapabilityPatchMetadataOnlyOnLegacyParamSchema (0.05s)
    resource_capability_schema_test.go:123: 存量坏行的 metadata-only PATCH 应 200，得到 400：{"apiVersion":"anotify/v1","kind":"Status","status":"Failure","code":400,"reason":"BadRequest","message":"capability_invalid: param_schema: 参数 schema.type 必须是 object"}
FAIL
FAIL	github.com/zzjcool/anotify/internal/server	0.301s
```

与 review 的实测逐字一致（`reason` 退化成 `BadRequest`、message 里塞着 `capability_invalid:`、
没有 `details.causes[]`）。

**变异红（修复后把 `resource_cas.go` 改回 `normalizeCapability`）**：

```
$ sed -i 's/normalizeCapabilityMetadata(&updated)/normalizeCapability(&updated)/' internal/store/resource_cas.go
$ go test ./internal/server -run TestCapabilityPatchMetadataOnlyOnLegacyParamSchema -count=1
--- FAIL: TestCapabilityPatchMetadataOnlyOnLegacyParamSchema (0.05s)
    resource_capability_schema_test.go:121: 存量坏行的 metadata-only PATCH 应 200，得到 400：{"apiVersion":"anotify/v1","kind":"Status","status":"Failure","code":400,"reason":"param_schema","message":"参数 schema.type 必须是 object","details":{"causes":[{"reason":"param_schema","message":"参数 schema.type 必须是 object","field":"spec.params.type"}]}}
FAIL
FAIL	github.com/zzjcool/anotify/internal/server	0.304s
```

这条变异同时证明了两件事：① 用例真的钉住了「PATCH 不受 #612 判据影响」；
② (b) 生效——同一份 400 现在带 `reason: param_schema` 与 `causes[0].field = spec.params.type`，
不再是 `BadRequest` + `capability_invalid:` 前缀。变异已回滚（`git diff` 只剩预期的 1 行）。

**绿**：

```
$ go test ./internal/server -run 'TestCapabilityPatchMetadataOnlyOnLegacyParamSchema|TestCapabilityWrite|TestSourceWrite|TestSourceDraftPreview' -count=1
ok  	github.com/zzjcool/anotify/internal/server	0.661s
```

**用例形状**（`internal/server/resource_capability_schema_test.go`）：
`seedLegacyParams` 直连 sqlite 把 `spec.params` 换成 `{"type":"array",...}`（写入期今天会 400，
只有改库造得出存量行）→ PATCH `metadata.displayName` + `spec.next` → 断言 200、displayName 与
next 都落地、**库里那份坏 params 原样留着**（PATCH 不许静默「修好」内容）。

**范围说明（写进 PR 正文，避免读者误读）**：修好的是「存量行不会被新判据锁死」，
不是「存量行自动变好」。它仍以 `Implemented=False/Invalid` 出现在 status 里，改内容仍需发新版本；
部署到新节点仍会被节点侧 `Cache.Store` 拒（r1 的 `TestCacheStoreRejectsLegacyBadParamSchema`
钉的就是这个 fail-closed）。

### 1.2 主要 2：契约端点

**核对（我先自己复跑了一遍 review 的实测，没有照抄）**：

- `POST /v1/sources/dry-run` + `payloadSchema:{"type":"string"}` → **200**
  `{"targets":[],"pruned":[{"reason":"event_schema","detail":"payloadSchema 不可用：参数 schema.type 必须是 object"}]}`
  （`previewInline` 不进 `sourcespec.NormalizeConfig`；剪枝来自 `route.ValidatePayload`）。
- 同一个 schema 走 `POST /v1/sources` → 400 `invalid_source_config`，
  `causes[0].field = spec.config.payloadSchema.type`。
- 顺带核了一条 ingress 的断言：草稿的 `config` **键集**也不校验——
  `config:{"bogusKey":1}` 走 dry-run → 200（只 `no_target` 剪枝），走写端点 →
  400 `manual 不认字段 "bogusKey"`。

**改法**：

- `api/resource.yaml` 的 `SourceSpec.config`（3249 起那段）新增 `config.payloadSchema` 段：
  写入期过运行期同一份解析器、不可用 → 400 `invalid_source_config` + `causes[].field` 指到
  `spec.config.payloadSchema.<键>`、不这么做的后果（201 落库 + health ok + 每个事件被剪），
  并补一句「草稿预览不跑这条判据，它只按 `event_schema` 剪枝」。
- `api/ingress.yaml` 的 `InlineSourceSpec.config` 那 8 行改成 3 行准确描述：
  草稿不跑写入期校验（config 键集与 payloadSchema 判据都不看），不可用只按 `event_schema`
  剪枝写进 `pruned[].detail`，真正的 400 见 `resource.yaml` 的 `SourceSpec.config`。
- 新增 `TestSourceDraftPreviewPrunesUnusablePayloadSchema`（`internal/server/resource_source_schema_test.go`）：
  断言 dry-run 200 + 恰好 1 条 `reason=event_schema` 剪枝 + `detail` 含「payloadSchema 不可用」。
  这样文档里那句「草稿不校验」有测试背书，不会下次又漂回去。

## 2. 次要项（review 编号 3–7）

### 3. `api/resource.yaml` 的 `spec.params` 比实现严

实测（`internal/schema` 临时探针，跑完已删）：`{"required":null}` → `problems=[]`；
`{"properties":{"a":null}}` → `problems=[]`；而 `{"properties":null}` → 400
`properties 不是对象`、`{"type":null}` → 400 `type 必须是 object`。

改法：措辞改成「`required` 与属性值写 `null` 视作未声明，与运行期同一口径」，
并把「`properties` 必须是对象」保留（那一条是真的）。没改判据（review 也说这是旧语义）。

### 4. 「两包」→ 三包

`scripts/check_anode_skill.py` 的 docstring（v3↔v4 对照表那一行）、`docs/anode-cli.md` §6、
`DEVELOPMENT.md` 的 #334 纪律条目，都改成
`internal/protocol` + `internal/artifact` + `internal/schema`。
另外发现 Makefile 里 `check-anode-skill` target 上方的注释也是两包说法，一并改（同一处漂移）。
`make check-anode-skill` 复跑通过（输出的边界行也是三包）。

### 5. f3 夹具 `contentIdentity`

按要求「至少加注释点明前提」，没有改成 Go 侧出 digest（那要动 fixture 的造数协议，
本轮风险大于收益，方案记在 §4）。注释写明三条隐式前提：

1. 夹具里没有会被 `json.Marshal` 转义的 `<` / `>` / `&`（Go 侧走 `\\u003c…`，JS 不转）；
2. 数字没被 `JSON.parse→stringify` 重排版（大数 / 科学计数）；
3. `run` / `lifecycle` / `requires` 的子键序 = Go struct 序（只有 `params` 走了 `canonicalJSON`）。

并点明两件事：`seedLegacyParamSchema` 造出的行 **id 与 digest 对不上**（store 的 id 由 digest
派生，所以这行 store 自己永远写不出来——它只是存量快照，不是 store 的代言人）；以及
**`deployApplied` 用的是守协议夹具节点，收到什么 digest 就回显什么**（`f3-node.mjs` 的 `_react`），
所以「digest 算对」这条防线在 e2e 里是关着的。

### 6. `TypeMatches` 注释

改成：它是 `Value` 的通用判据（边字面量 / 事件载荷 / emit 的 data，未知类型名判 `false`）；
参数派发走 `param.go` 的 `valueMatchesType`（认 `json.Number` / `int32`、`integer` 拒 `e`/`E`、
未知类型名放行），两处各自自洽、改一处要复查另一处。没合并成一份——合并要动派发期语义
（未知类型名放行 vs 判 false），不是本 issue 的面。

### 7. 基线口径（PR 正文已改）

`check-comment-density` 的 `commentLines`：main 42879 → 本分支 **42873**（−6，全部来自
`internal/route` 299→294、`internal/schema` 73→72 的收紧，r1 就已发生）。同一份基线里的
`files` 763→777 与 `codeLines` 293019→295432 是 **main 漂移的补记**，不是本 PR 的增量；
`evaluate()` 不读这两个字段（判据只有 `commentLines` / 目录注释行 / 单文件密度 / docs 面），
所以不构成放宽门禁。

本轮我新加的注释在**同一目录内逐条配平**（`internal/artifact` / `internal/schema` /
`internal/store` / `internal/server` / `scripts/e2e/fixtures/f3`），付账方式是删掉同目录
若干条「复述函数名」的 doc（`readJSON` / `taskSpecOf` / `taskStatusOf` / `newNodeAPI` /
`scanNodeRow` / `errStatus` 那条没动——它下面有 AGENTS.md 的行号引用）与 `paramEnvName` 的
英文两行注释收敛成一行、`f3-driver.mjs` 的 2 条空 `//` 分隔行。**基线文件一字未改**，
总量仍是 42873。

## 3. 验证（本轮在本 worktree 实跑）

```
$ go build ./... && go vet ./internal/... && gofmt -l .
# 无输出（gofmt 空、vet/build 通过）

$ go test ./internal/store ./internal/server ./internal/schema -count=1
ok  	github.com/zzjcool/anotify/internal/store	9.405s
ok  	github.com/zzjcool/anotify/internal/server	62.753s
ok  	github.com/zzjcool/anotify/internal/schema	0.308s

$ node scripts/check-comment-density.mjs
   扫描 777 个文件（下限 600）· 注释行 42873 / 基线 42873
   目录：80 个有基线，0 个与基线不一致
✅ 与基线完全一致（超与降都算不一致）

$ node scripts/check-comment-density.mjs --self-test   → ✅ 自测全部通过
$ node scripts/check-test-discipline.mjs               → ✅ 五项全部与基线一致
$ node scripts/check-test-discipline.mjs --self-test   → ✅ 守卫自测通过
$ node scripts/check-annotations.mjs                   → exit 0（含 37 组自测）
$ node scripts/check-agents.mjs                        → ✅ 模块约定与代码一致（行号引用全部可解析）
$ make check-anode-skill                               → ✅ 4 个子命令 / 15 flag / 边界：仅依赖三包
$ python3 scripts/check_contract.py                     → A2/A4/A5/A6/A7/A8/A9/A11 通过（35 表 / 5 份 spec）
$ python3 scripts/check_openapi.py                      → 5 份文件 / 146 operation / 跨文件 $ref 可解析
$ node scripts/check-yaml-doc.mjs / check-doc-links.mjs → 通过
```

跑 `check_contract.py` / `check_openapi.py` 需要 PyYAML，本机 `python3` 没有且受 PEP 668 限制装不进
系统环境，所以建了临时 venv（`/tmp/py-venv`）跑；脚本本身与 CI 用同一份。

**两条环境限制（与本 PR 无关，均在 origin/main 的干净 worktree 上复现）**：

1. `make check-dist-fresh` → `❌ 没有 stamp：internal/server/dist/.source-stamp 不存在`。
   `internal/server/dist` 根本没入库（`git ls-files internal/server/dist` 为空），干净 worktree
   必然没有 stamp，要先 `make fe`。`node scripts/dist-stamp.mjs --self-test` 18 组全绿。
2. `go test ./internal/artifact` 有 3 条 `TestAgentTUIWorktree*` 红（断言 `/var/folders/…` 与
   `/private/var/folders/…`），在 `/tmp/wt-main`（origin/main 干净树）上同样红。与本改动无关：
   我的改动只碰 `Validate` 的分层，不碰 worktree 标签。
   本轮实跑：`go test ./internal/artifact ./internal/route ./internal/sourcespec ./cmd/anode/internal/capability -count=1`
   → route / sourcespec / capability ok，artifact 仅上述 3 条红。

不跑全量 `make ci` / e2e（按任务约定：CI 会跑，本机全量 40 分钟）。

## 4. 未做 / 留档

- **e2e 没跑**（`routing` 539 例 / `--serial frontend` 50 例都是分钟级）。本轮代码改动集中在
  store/server 的校验分层与文档，f3 夹具只动注释，行为面无变化；`routing` 里对 #612 的断言
  （写入期 400 + 改库造存量坏行）不受影响。若需要，可在 CI 上确认。
- **f3 夹具改成「让 Go 侧出 digest」**（review 更欢迎的方案）没做：需要给 e2e 增加一个
  「按 spec 算 digest」的服务端入口（或 devseed 子命令），涉及 fixture 与 mock 的造数协议，
  不适合同一轮返工做。注释已把前提与风险写在现场。
- **`valueMatchesType` 与 `TypeMatches` 收敛成一份**没做：两者语义差（未知类型名放行 vs 判 false、
  是否认 `json.Number` / `int32`）会改派发期行为，属另一个 issue 的面。注释已写清差异。
- **`internal/route` / `internal/schema` 之外的目录注释行**没有变化（逐目录与基线一致）。

## 5. 交付

- 提交：`246beefe`（已 `git push origin HEAD:pi-subagent/issue-612-r1`，快进，无 force）。
- PR #694 正文：重写「第 2 轮 review 返工」小节；删掉 r1 那段错误的「PATCH 不受影响」自述；
  基线口径改成区分 `commentLines` / `files` / `codeLines`。
- PR #694 评论：逐条对应主要 1/2 与次要 3–7 的改法、验证命令与输出、红/绿/变异输出。
