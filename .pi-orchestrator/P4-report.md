# DONE P4（部分验证完成，已提交）

commit: `df8f575 feat(web,server): 能力页收 spec 编辑器（多文件/env），请求体切到 spec`
（分支 `feat/p1a-artifact-spec-v3`，**未 push**）

## 1. 已完成的部分

### 1.1 服务端 `internal/server/console_handlers.go`

- `capabilityListRequest` 重写为 `{namespaceId?, name, version?, displayName?,
  spec: {params, outputs, requires, lifecycle, sticky, run{entrypoint,files,timeoutSec,env,workDir}}, force?, links?}`
  —— 删除 `paramsSchema / outputs / run / requires / sticky / lifecycle / balance / source`。
- `createCapability`：decode → name 校验 → resolveNamespace → `rejectUnacceptableSpec`
  → `CapabilityVersion{... Spec: *req.Spec}`（**不填任何派生列**）→ 有 links 走原子路径。
  响应里的 `artifact`/`digest` 变成无条件产出（P4 起只接受脚本形式，不存在“建成了却没产物”）。
- **I1 红线**：新增 `rejectUnacceptableSpec`，`spec.handler != ""` → 400 `handler_not_allowed`；
  `spec` 缺失或 `spec.run` 既无 entrypoint 又无 files → 400 `impl_missing`。
  单建与**批量每一项**共用同一函数（批量错误文案带 `capabilities[i]` 下标）。
- 批量：元素改读 `item.Spec`，删除 `item.Run/item.Requires/item.ParamsSchema` 与
  `buildArtifact` 预校验（`buildArtifact`/`capabilityRunInput` 已从建能力路径彻底移除）。
- 列表：删掉旧的 `run` 投影（`capView.Run`/`capabilityRun`/`capabilityRunView`），
  只留 `spec`（`capabilitySpecView` + `files {count,bytes}`）；`capabilityRunSpecView` 保留。
- 升级闸门：`CheckUpgrade` 预计算前把 spec **同向**投影到 `Outputs`/`ParamsSchema`
  （见 §3 的 bug 说明）。

### 1.2 前端 `web-src/pages/capabilities.html`

- 脚本区 → **多文件编辑器**：`FILES`/`FILES_ENTRY`/`FILE_SEL` + `#cap-files-list`
  （点名字切文件、入口标记、设为入口、删除，至少留一个）+ `#cap-file-name` 加文件（回车可加）。
  提交体里带 **全部文件** `spec.run.files`（旧代码只发入口文件 → 其余文件静默消失）。
- **env 行编辑**：`ENV_ROWS` + `#cap-env-rows`（key/value/删除，空值合法），提交 `spec.run.env`。
- 删掉全部「还没有产物」分支：详情脚本块 404→`state:"empty"`、`caps.detail.script_empty`、
  `caps.edit.no_artifact`、列表无 run 的兜底；产物读不回来时 `artifactFailed` → **禁止发布**并说明。
- handler 形式（`spec.handler != ""`）：详情脚本块显示「内置执行器 X」、运行参数区多一行执行器、
  部署按钮**禁用** +「由中枢执行，无需部署」、不给编辑入口（行尾与详情区都没有）；
  `openDeployModal` 里再兜一层。
- 提交体全部切到 `spec`（单建 / 批量 / 发新版本）；`runtimeNode` 改读 `c.spec.run`。
- i18n：`web-src/locales/*.yaml` 四语言同步新增 26 个键、删 4 个失效键。

### 1.3 `web/mock.js`

- `cap()` 删掉 balance；`specOf`/`specSummaryOf` 从扁平字段汇出线上 spec（files → {count,bytes}）。
- POST /capabilities、/batch 认 `spec` 请求体；`handler` → 400 `handler_not_allowed`；
  缺 run → 400 `impl_missing`；多文件与 env 整份接住。
- **删掉 P3 遗留的假 balance 分支**（`checkBalance` / `balance_unimplemented` 全部移除）。
- `/artifact` 增 `env` + 完整 `spec`；`executedBy` 改按 handler 判（不再只靠名字表）。
- 种子：`notify.phone`/新增的 `notify.demo` 是 handler 形式（部署按钮分叉的可测样本），
  其余能力都补了最小实现（P1-b1 起不存在“还没有产物”的能力）。

### 1.4 `api/openapi.yaml`

`CapabilityInput` 改 spec 形状（`required: [name, spec]`）；删除 `CapabilityRunInput`；
`Capability` 删 `run` 属性；`/capabilities` 与 `/capabilities/batch` 的 400 码表补
`handler_not_allowed` 并改写 `impl_missing` / `invalid_artifact` 的语义。

### 1.5 新增/改写的单测

- `internal/server/capability_spec_p4_test.go`（新）：
  `TestPostCapabilityRejectsHandler`（noop / notify-phone / 自造 handler 三条子测，
  断言 400 + `handler_not_allowed` + 文案提到 run + 不落库）、
  `TestPostCapabilityBatchRejectsHandler`（带下标、整批不落库）、
  `TestPostCapabilityLegacyFlatBodyRejected`（旧扁平体 → 400 impl_missing，失败要响）、
  `TestPostCapabilitySpecRoundTrip`（多文件 + env 空值 + lifecycle/sticky/params 往返，
  列表侧 files→{count,bytes} 且不泄漏正文）。
- `zz_spec_fixture_test.go`：`child` 夹具改为把旧扁平造数体翻译成 spec 形状
  （`bodyToSpec(addRun)`），`env.do` 机械翻译、`doRaw` 不翻译（给钉形状的用例）。
- `capability_run_workdir_test.go`：`TestListCapabilitiesExposesRun` → `...ExposesSpecRun`
  （钉 `spec.run.timeoutSec/workDir/files 摘要` 且**旧的 `run` 投影不许回来**）。
- `capability_spec_p1b2_test.go`：③ 改写成「旧的 run 投影 P4 已删」。

### 1.6 e2e 夹具（全量 `rg v1/capabilities` 覆盖）

deeplink / events_paging / persistence / orphans / i18n_coverage / live_agent / capacity /
deploy / api_contract / contract / routing / ws_protocol / real_backend / frontend
全部造数改 spec 形状。断言语义变了的**改写**（未删）：
- `frontend`：`withWd.run.workDir` → `withWd.spec.run.workDir`；批量请求体读 `spec.outputs`；
  balance 断言从「非 priority → 400」改成「带 balance 不报错也不落库」；
  「没有产物显示还没有产物」改写成三条新断言（脚本形式永远有正文 / handler 形式部署按钮禁用 +
  无编辑入口 / 脚本块说「内置执行器」）。
- `deploy` / `api_contract`：`dep.noartifact`、`contract.noartifact` 改发 `spec`（无 run）；
  新增 `api_contract` 的 `handler_not_allowed` 两条 + 「被拒一行都没建」。

## 2. 剩下哪些红

**当前无红**（截至最后一次跑）：

```
✅ frontend      649通过/0失败  253.3s
✅ api_contract   72通过/0失败
✅ contract       80通过/0失败   18.6s
✅ real_backend  147通过/0失败    4.3s
✅ deploy         58通过/0失败    3.7s
✅ ws_protocol   137通过/0失败   11.3s
✅ routing        78通过/0失败    4.1s
✅ go test ./internal/...  全部 ok（含 server 56s）
✅ make contract（openapi ↔ mux 一致）
✅ make fe（check-classes + check-tailwind + sitegen + hash）
```

### 2.1 过程中遇到并已修掉的一条红（值得记录）

`deploy` 套件 `同内容显式 v2 → 200（显式版本永不折叠）` 变成 409：

```json
{"code":"upgrade_breaking","error":"升级 dep.hello@2 会破坏已有的 name 型 link（1 处契约变化，
 0 条受影响 link）; removed_port: 删掉了一个出口（旧版有 [go]）..."}
```

判断：**本次改动引入的真实产品 bug**，不是断言问题。根因是 `store.checkUpgradeOn`
的 `diffContracts` 读的是 `next.Outputs`（**派生列**），而 P4 起 HTTP 层不再给派生列赋值
（契约 §4：唯一写者在 store，且发生在**写入时**），于是写入前的预计算把「新版的契约」
看成了空的 → 任何带出口的显式新版本都会被误判成删出口，永久 409。

修法（在授权文件内、最小改动，已在 commit 里）：`createCapability` 在调 `CheckUpgrade`
之前，把 `c.Spec` **同向**投影到 `Outputs`/`ParamsSchema`（方向与 store 的
`deriveFromSpec` 一致，不做反向反推），落库值仍由 store 在事务里重算。

> 建议（**需要授权**）：更正确的修法是在 `internal/store/upgrade.go:checkUpgradeOn`
> 开头对入参调一次 `deriveFromSpec(c)` —— 那是 `internal/store/**`，不在本任务的
> 文件所有权内，所以我没动。若允许，我把 server 侧那 6 行投影删掉，换成 store 内一行。

## 3. 两条数据丢失 bug 怎么证修好了

1. **其它文件被静默删除**（真后端 + 真浏览器，`frontend` 套件 `checkCapabilityEditReal`）：
   造数时给能力带两个文件（`run.sh` + `lib/helper.sh`）；点「编辑」后断言
   `#cap-files-list` 列出**两个**文件（入口有标记）、点第二个文件时编辑器显示
   `helper v1` 正文；改入口脚本发 v2 后回到 API 断言
   `v2 files == ["lib/helper.sh","run.sh"] == v1 files` 且 `lib/helper.sh` 正文未变
   （断言名：`③ [数据丢失回归] 发 v2 后 lib/helper.sh 仍在且正文未变`）。
   实现侧：提交体从 `{entrypoint: 正文}` 换成 `Object.assign({}, FILES)`（全部文件）。
2. **发一版清空 env**：同一条造数带 `env: {GH_TOKEN: "", LOG_LEVEL: "info"}`；
   编辑弹层断言两行 env 都被预填（含空值 `GH_TOKEN: ""`），发 v2 后回到 API 断言
   `v2.env.GH_TOKEN === "" && v2.env.LOG_LEVEL === "info"`
   （断言名：`③ [数据丢失回归] 发 v2 后 env 原样还在（含空值 GH_TOKEN）`）。
   实现侧：`envRows` 镜像 + `envToObject()`（整行空才丢弃，空值保留），提交体不再是 `{}`。

两条断言都是**先红后绿**：第一次跑 frontend 正是因为「第二条文件被改掉 / v2 看不到新脚本」
而红（`③ v2 的脚本是新的`、`③ [数据丢失回归] ...`），修好选中文件回切后转绿，
现在 649 条全绿。

## 4. 发现的产品 bug

1. 上面 §2.1 的 `CheckUpgrade` 读派生列（已在本 commit 内以 server 侧投影绕过，
   根治点在 store，需授权）。
2. `internal/store/deriveFromSpec` 未导出，导致「写入前的闸门」无法复用同一份推导；
   建议导出或让 `checkUpgradeOn` 内部自推导（同上）。
3. 顺带观察（未改，不在所有权）：`/v1/capabilities` 的 batch 预校验也曾经用
   `buildArtifact` 重复实现了一次「正文组装」，这次随 P4 一起删掉了 —— 现在两条路径
   都只把 spec 原样交给 store。

## 5. 改了但没验证的部分

- **`make e2e` 全量 27 套件没跑**（用户检查点指令要求停止长跑）。已跑的 7 个套件包含
  与本次改动直接相关的全部（frontend / api_contract / contract / real_backend /
  deploy / ws_protocol / routing）；未跑：auth_flow、events_paging、persistence、
  orphans、capacity、live_agent、deeplink、i18n、i18n_coverage、mock_coverage、
  deploy 之外的其余套件。
  → 其中 `deeplink / events_paging / persistence / orphans / capacity / live_agent /
  i18n_coverage` 的造数我按 spec 形状改过（`node --check` 通过），但**未实跑**。
- `web/` 产物是 `make fe` 生成的（hash 指纹已更新），未提交（gitignore）。
- mock 种子里新增了 `notify.demo`（handler 形式）与「其余能力补最小实现」，
  frontend 套件已覆盖它，但**其它**用到 mock 的套件（i18n_coverage 等）未实跑。
