> **历史快照**：本文件是当时的交付记录，不随代码演进；行为以代码、测试与 `docs/contracts/` 为准。
<!-- snapshot-doc: 2026-11-21 交付当次的门禁与断言数字（报告是定格记录）；复算命令逐条写在 §4 -->

# 预案安装的 release 身份（#505 · Helm 语义）

as_of: 2026-11-21 · area: `store` / `playbook` / `server` / `web` / `e2e`
分支 `pi-subagent/hw-nj/anotify-505-release-e9024221` · 迁移块 `2 playbook_release`

## 0. 结论

1. **release 是安装级身份，不是包上的标签。** 同一个包在同一个命名空间里可以有多个
   release，各自一套名字与 values、互不干扰。它有且只有两个作用面：模板
   `{{release.name}}`（让名字能分开）与**认领边界**（要复用的行若属于另一个 release，
   整包拒绝）。缺一不可：只有模板没有边界，第二个 release 会静默复用第一个的行；
   只有边界没有模板，多实例根本连名字都长不出来。
2. **缺省 release = 包名（slug）**，这是「老包逐字不变」的落点。迁移把历史回执的
   release 回填成 `playbook_slug`，所以本版之前装出来的行全部归在「包名这个 release」
   名下——默认路径上的规划结果与回执（created / skipped / upgraded / unapplied）逐字相同，
   `.pi-legion` 之外的既有断言一条没改（只多传一个空串参数）。
3. **跨 release 认领冲突是 409 `release_conflict`，不是 400。** 它不是「你输入的内容有错」，
   而是「这个对象已经有主人」。details 指认到 `{name, kind, ownerRelease, ownerInstallId}`
   （外加逐条 `conflicts[]` 与 `hint`），事务在规划期返回 ⇒ **0 写入**。
4. **迁移引擎从「单块」升级为「按记账版本重放」。** 这是 release 需要**存量回填**逼出来的：
   原地改 baseline 做不到回填，而 `ALTER TABLE … ADD COLUMN` 只有在新块里才跑得到。
   改法见 `internal/store/migrate.go` 的 `schemaLayoutAt`（还没应用增量块的库按旧一块校验，
   应用完再按 head 复核），`internal/store/AGENTS.md` §3.2 同步改了口径。
5. **一个包 -> 多份实例 -> 两组回执**这条链在 HTTP 面上是通的：`install` 吃 `release`、
   回执回 `install.release`、`installs` 按 `(release, valuesDigest)` 折叠、页面按 release
   分组展示。UI 没有新增任何 `PUT …/config`（配置写路径守卫仍绿）。

## 1. 判据（为什么这样切）

### 1.1 三种「重名」必须分开处置

| 情形 | 处置 | 依据 |
|---|---|---|
| 同 release，同名同内容 | `reuse`（跳过，不写） | 幂等：装两次是同一件事 |
| 同 release，同名不同内容 | `upgraded`（新版本行 + 一条 upgrade 记录） | 本版之前的语义，未改 |
| **跨 release**，命中同一行 | **409 `release_conflict`，整包 0 写** | 认领别人的行 = 两份实例悄悄共用一个能力/源 |
| 无主行（手建，`installed_from` 为空） | `reuse` | 安装一直允许复用库里已有的行，本版不改 |

### 1.2 为什么认领信息挂在行上，而不是「按名字前缀猜」

行的 `installed_from` 指向装出它的回执，回执上有 release；所以「这行是谁的」是一条
事实，不是字符串推断。代价是每次安装要多一次按 id 的读，而同一批行常常指向同几次回执，
`installSnapshot` 因此带一个 `owners` 缓存（`internal/store/playbook_tx.go`）。

悬挂引用（回执被清理过）**当无主处理**，不报错：schema 上没有 FK 允许它存在，而把一条
脏引用当成「别人的 release」会让一次正常安装莫名 409——报错的代价比当无主更大。

### 1.3 模板只加一种引用

`{{values.点号路径}}` 与 `{{release.name}}`，两种，仅此两种。规则与既有的 values 引用
逐条对齐：整值单引用保类型、掺常量变拼接、对象/数组拼进字符串 → `render_type_mismatch`。
**未知形态是 400**：`{{release.foo}}` 报 `invalid_playbook`（details 带 `field` 与
`reference`），因为「静默当字面量」会让作者以为名字按 release 分开了，直到装第二份才在
库里撞车，而错误指不到模板本身。

`{{payload.x}}` / `$NAME` / `$(vars.X)` / `${…}` 一律原样透传（§1.4 的定界符隔离没有被
新语法碰坏），这条有单测与 e2e 双向钉住。

## 2. 改动清单

| 层 | 文件 | 关键点 |
|---|---|---|
| 迁移 | `internal/store/schema.sql` | 追加 `-- +migration 2 playbook_release`：`ADD COLUMN release`、回填 `= playbook_slug`、索引 `(namespace_id, playbook_id, release, created_at)` |
| 迁移引擎 | `internal/store/migrate.go` | `headVersion = 2`；`baselineLayout()` → `schemaLayoutAt(version)`（重放 1..N + hooks）；`validateV4Layout` 按**已记账版本**取参照结构；应用后复核 |
| 领域 | `internal/playbook/release.go`（新） | `ReleaseRef`、`releasePattern`、`NormalizeRelease`、`ReleaseConflict`、`releaseConflictHint` |
| 领域 | `internal/playbook/render.go` | `tokenizeTemplate` 认两种引用；`RenderForRelease`；`{{release.foo}}` → 400；`Render` 保留为「缺省 = 包名」的入口 |
| 领域 | `internal/playbook/plan.go` | `CapabilitySnapshot` / `SourceSnapshot` 加 `OwnerRelease` / `OwnerInstallID`；`PlanForRelease`；`claimedByOtherRelease`（行 2/5/9 的认领判定）；`conflictError` 认领优先 |
| 领域 | `internal/playbook/playbook.go` | `CodeReleaseConflict`、`InstallRef.Release`、`InstallsFoldKey(ns, pb, release, digest)` |
| 领域 | `internal/playbook/sensitive.go` | 敏感性推导只认 `values` 根的引用（release 不是 values 路径） |
| store | `internal/store/playbook.go` / `playbook_tx.go` | `InstallPlaybook` / `RenderPlaybook` 收 `release`；回执列写 release；`ListPlaybookInstalls` 按 `(release, valuesDigest)` 折叠、组带 `release`；`GetPlaybookInstall` 对迁移前回执按列补齐 release |
| server | `internal/server/resource_playbook.go` | 动作体 `release`；409 分支 + `playbookReleaseConflictDetails`（不编造：取不到结构化字段就留空 + `conflicts: []`） |
| api | `api/resource.yaml` | `PlaybookActionRequest.release`（含 pattern）、`PlaybookInstallRef.release`（required）、`PlaybookInstallGroup.release`（required）、`StatusDetails` 文档补 409 形状 |
| web | `web-src/pages/playbook.html` | `#pb-release` 输入（缺省 slug）、动作体带 release、实例卡按 release 分组、409 `release_conflict` 分支 |
| web | `web/mock.js` | 预览用的 mock API 同步收 `release`：动作体白名单、`{{release.name}}` 展开、回执 `install.release`、按 `(release, valuesDigest)` 折叠（否则 explicit-mock 套件在真页面上拿到 400） |
| web | `web-src/locales/*.yaml`（4 份） | `pb.v4.release*` / `attempts_n` / `release_group` 等新 key，4 份键集合一致 |
| e2e | `scripts/e2e/suites/playbook_release.mjs`（新） | 真 HTTP + 真 sqlite 的五节验收；注册表 29 → 30（`parallel_runner` / `affected` / `affected-map` / `e2e-inventory` 同步） |

## 3. 验收证据

> 命令里的 `timeout` 只是防挂死；每条都是本机实跑，退出码见括号。

### 3.1 老包不回归（缺省 release = 包名）

`go test ./internal/store/... ./internal/playbook/... ./internal/server/ -count=1` —
既有用例**一条断言没改**（只多传了 `""` 这一个实参），全绿。
其中 `TestPlaybookInstallReleaseDefaultsToSlug` 与 e2e 第 1 节把「逐字相同」钉死：
第二次安装 `created` 全空、`skipped.capabilities` 与第一次 `created.capabilities` **逐字
相等**（含 `name@N` 版本号）、`installs` 只有一个折叠组、`attempts === 2`、两次
`install.release` 都等于包名。

### 3.2 多实例并存

e2e 第 2 节：名字含 `{{release.name}}` 的包，release `a` / `b` 各装一次 → 能力列表同时有
`a-demo-review` 与 `b-demo-review`，源列表同时有 `a-demo-on-push` 与 `b-demo-on-push`；
`installs` 两组、成员各带自己的前缀；两份单回执的 `install.release` 各自对得上。
单测侧同一件事由 `TestRenderReleaseName` / `TestPlaybookMultiReleaseCoexists` 双层覆盖。

### 3.3 跨 release 冲突 = 409 + 0 写

e2e 第 3 节：先 release `a` 装成功，再 release `b` 装同一份 values → 409
`release_conflict`，`details.ownerRelease = "a"`、`details.ownerInstallId` 等于第一次回执的
`install.id`、`details.name` / `kind` 非空、`details.conflicts` 非空、`hint` 给出两条出路；
**409 前后能力行 / 源行 / 实例组数完全一致**（判据是库里的行数：单事务在规划期返回，
不是「先写再回滚」）。

### 3.4 透传回归

e2e 第 4 节：带 release `a` 调 `render`，渲染后正文里 `{{payload.x}}` / `$(vars.REGION)` /
`${ENV}` **原样**存在，而 `{{release.name}}` 已替换成 `a`、`{{values.` 无残留。
（这三段定界符放在正文而不是名字里：能力名的字符集不允许 `{` / `$`，放名字里会先被
`render_name_invalid` 拦住。）

### 3.5 变异验证（三次都必须变红，再还原复绿）

见 §4.2 的脚本与 §4.3 的实测退出码：三条变异各自让**对应的**验收红，还原后复绿。

### 3.6 门禁

`make ci` **退出码 0**（含 `contract` / `check-agents` / `lint-go` / `fe` 全套前端守卫 /
注释与文档密度棘轮 / 测试纪律棘轮 / `check-annotations` / `check-doc-links` /
`check-doc-endpoints` / `check-affected-map` / `make test`）。

`make e2e-affected` **全绿：30/30 个请求套件均达标，断言 2563 通过 / 0 失败**
（authority 清单覆盖 30 个；本次改动落在 `api/**` 与 `internal/playbook/**`，选择器按
fail-closed 判全量）。新增的 `playbook_release` 套件单跑 **95 通过 / 0 失败**（4.5s，mode=real）。

> 稳定性说明：这台机器上同时跑着别的 worktree 的 e2e 时会偶发 `database is locked`
> （`routing` / `orphans` 各命中过一次，单跑立刻复绿：`routing` 168/0、`orphans` 98/0）。
> 最终这份 30/30 是在没有并发负载的一轮里跑出来的。

### 3.7 OpenAPI 五件套

`make contract` 绿；`internal/server/openapi_parity_test.go` 的 `parityPairs()` 把
`PlaybookActionRequest` / `PlaybookInstallRef` / `PlaybookInstallGroup` 与 spec 双向对账
（新增字段在两个方向都能被检测到：给 spec 加一个 Go 没有的字段 → DTO 测试红）。

## 4. 复算命令

### 4.1 门禁与全量 e2e

```bash
make ci                     # 期望 exit 0
make e2e-affected           # 期望 exit 0（改到 internal/playbook 时选全量，fail-closed）
node scripts/e2e/parallel_runner.mjs --authority-plan   # 30 个套件 = 26 real + 4 explicit-mock
./scripts/e2e/run_all.sh --serial playbook_release      # 单跑本套件（95 断言）
```

### 4.2 变异脚本（红/绿双侧）

```bash
bash /tmp/505-mutate.sh M1   # 缺省 release 变成每次都不同 → 老包幂等红
bash /tmp/505-mutate.sh M2   # 去掉跨 release 认领判定   → 验收 3（409）红
bash /tmp/505-mutate.sh M3   # {{release.name}} 当字面量 → 验收 2（多实例）红
```

三次都是「打补丁 → 跑对应单测 + 该 e2e 套件 → 从备份还原」，还原后同一组命令复绿。

### 4.3 变异实测（都按预期变红）

| 变异 | 单测（红） | `playbook_release` 套件（红） |
|---|---|---|
| **M1** 缺省 release 变成每次都不同 | `TestPlaybookInstallReleaseRoundTrip`、`TestPlaybookInstallReleaseDefaultsToSlug` | 第二次安装变成 409 `release_conflict`、回执与折叠组的 release 都不是包名 |
| **M2** 去掉跨 release 认领判定 | `TestPlanReleaseConflict*`（3 条）、`TestPlaybookInstallCrossReleaseConflictZeroWrite`、`TestPlaybookCrossReleaseReuseIs409` | 409 变成 200、details 全空 |
| **M3** `{{release.name}}` 当字面量 | `TestRenderReleaseName`、`TestPlaybookMultiReleaseCoexists` | 两个 release 的安装都 400 `render_name_invalid` |

还原后同一组命令复绿（`git status` 只剩本 PR 的改动）。

## 5. 明确没做的事

- **delete 响应里的 `values[]` 仍按 `valuesDigest` 去重**（不区分 release）：它的语义是
  「这包装过哪些 values」，不是「有哪些实例」；实例维度由 `installs` 承担。
- **mock 不实现跨 release 409**：`web/mock.js` 只服务 explicit-mock 套件（locale / DOM / 几何），
  认领冲突的真语义由 store 单测、server 测试与 `playbook_release` 套件三处守着。
- **`e2e-inventory.md`**：基线上该文件已被删除（注册表以 `parallel_runner.mjs` 的
  `HTTP_SUITES` / `CHROME_SUITES` 为唯一权威），所以本 PR 只改注册表与 affected-map，不再新增台账文件。
- **注释密度棘轮**：本 PR 新增的注释靠「同目录压缩既有注释」买预算（净减 37 行），
  基线随 `--update` 一并下调；`make ci` 的 `check-comment-density` 因此是绿的。
