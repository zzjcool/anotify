> **历史快照**：本文件是当时的交付记录，不随代码演进；行为以代码、测试与 `docs/contracts/` 为准。
<!-- snapshot-doc: 2026-11-21 交付当次的门禁与断言数字（报告是定格记录）；复算命令逐条写在 §5 -->

# 预案实例卸载（#510 · 按 release 撤行、保留回执与他人的行）

as_of: 2026-11-21 · area: `store` / `server` / `web` / `e2e` · 迁移块 `3 playbook_uninstall`
分支 `pi-subagent/hw-nj/anotify-510-uninstall-d1b8a9b3` · 起点 `main@81e83579`（含 #505 / PR #509）

## 0. 结论

1. **端点按 release，不按 installId。** `DELETE /v1/namespaces/{namespace}/playbooks/{slug}/releases/{release}`
   是「撤掉一份实例」——归属键是 #505 的安装级身份，不是某一次 install。`installId` 退化成**可选的钉桩**
   （`?installId=ins_…`）：对不上就 409，防止「页面停在旧回执、实际 release 已被重装」时删掉新一代的行。
   追溯仍然成立：响应回 `installIds[]`（该 release 的全部回执 id），单条回执继续走
   `GET …/playbooks/{slug}/installs/{installId}`。
2. **归属判据是 `installed_from`，不是名字。** 候选行 = 本 release 回执 id 装出来的未删行；能力按**行**软删
   （不是按名字全删），源按行删。同名的手工行、别的 release 的行、以及**被另一个仍生效的 release 记在
   `skipped` 里复用**的行都不删，进 `kept[]` 并带原因码 —— 这就是「保留他人的行」。
3. **`kept[]` 是「没删的清单」，不是失败清单。** 三个封闭原因码：
   `manual`（同名的行是手工建的）/ `other_release`（同名行属于别的安装）/ `shared_by_release`
   （本 release 建的，但另一个生效中的 release 正复用它）。前两码回答「同名的行不归我」，
   第三码回答「归我但别人在用」——后者不加就是「删掉别人的实例依赖」。
4. **CAS 没有绕过。** 被删的是能力/源行，调用方手里没有它们的令牌（DELETE 一个 release 时没有「一个」
   令牌可传），所以 CAS 落在**行级**：每行 `UPDATE … WHERE id=? AND resource_version=? AND deleted_at IS NULL`
   + `requireCASRow`，并发改动某行 → 409 `resource_version_conflict`，绝不静默删。
5. **幂等靠回执上的标记列，而不是「再算一遍看有没有行」。** 卸载只给该 release 的回执打
   `uninstalled_at`（回执 JSON 原文一个字不改，它仍是审计快照）；第二次调用命中「全部已标记」就**零 UPDATE**
   返回 `alreadyUninstalled: true`。实例清单（`GET …/installs`）过滤 `uninstalled_at = 0`，
   所以卸载后的 release 不再出现在清单里，但按 id 仍能读回执。
6. **schema 是迁移块 3，不是改 baseline。** `playbook_installs` 加 `uninstalled_at INTEGER NOT NULL DEFAULT 0`，
   `headVersion` 2 → 3；不加新索引（查询走 #505 的 `idx_playbook_installs_release`）。老库重开就地迁移，
   不需要删库。
7. **UI 是「一个 release 一个卸载按钮」。** 分组来自 `GET …/installs`（已经按 release 分组），按钮挂在
   release 段上（不是每个 values 组一个）；二次确认走页面的 `confirmDanger`，结果区显示删了几项、
   留了几项（结构化明细用代码块，与回执视图同族）。文案不含端点 / 字段名 / CAS 之类实现术语
   （`make check-ui-copy` 的「实现词检查面」100 个词面全绿）。
8. **变异能红。** 把「只删本 release 装出的行」改成「删同名全部可见行」→ 验收 2 的 3 条断言红
   （手工行被带走：`GET` 404、版本 `undefined`、v2 行 `deleted_at` 非空）；恢复后 173/0 绿。见 §5.4。

## 1. 判据（为什么这样切）

### 1.1 三个候选端点的取舍

| 形状 | 结论 |
|---|---|
| `DELETE …/playbooks/{slug}/installs/{installId}` | ❌ installId 是「一次安装」，不是实例：一个 release 装两次有两条回执，删哪一条都不是「卸载这份实例」 |
| `DELETE …/playbooks/{slug}/releases/{release}` | ✅ 与 #505 的归属键一致；实例清单本来就按 release 分组，UI 的按钮与端点一一对应 |
| 复用 `DELETE …/playbooks/{slug}`（删清单） | ❌ 那是「删模板」，语义相反（§7：包不是所有权） |

`installId` 保留为 query 参数而不是路径：它只回答「我删的是不是我刚看到的那一份」，钉不上就是
`409 install_conflict`（客户端视图过期，重读即可），而不是换一个 release 继续删。

### 1.2 候选行与 `kept[]` 的判据

```
候选（能力）: capability_versions WHERE installed_from IN (本 release 回执 id) AND deleted_at IS NULL
候选（源）  : sources            WHERE installed_from IN (本 release 回执 id)
```

`installed_from` 是 #505 定下的「首次安装来源」，也是 `DELETE …/playbooks/{slug}` 的 `kept` 反查用的同一条
判据（同一个问题只有一份答案）。**同名**按身份取：能力是 `name`（跨版本），源是 `name`（源名本来就唯一）。
同名但候选之外的未删行 → 不删，进 `kept[]`：

| reason | 判据 | 为什么不能删 |
|---|---|---|
| `manual` | 同名行 `installed_from` 为空 | 它是用户手工建的，安装只是复用过它 |
| `other_release` | 同名行 `installed_from` 指向别的回执 | 它属于另一份实例（`ownerRelease` 指认） |
| `shared_by_release` | 候选行本身，但另一个生效中的 release 回执把它记在 `skipped` | 删了会让那个实例失去依赖（回执是它复用的证据） |

第三码需要读别人的回执：`skipped.capabilities`（`name@version`）/ `skipped.sources`（名字），只扫
**同命名空间、`uninstalled_at = 0`、且不是本 release 自己**的回执。跨包复用也算（两个不同的包用同一个
release 名时允许复用），所以扫面是命名空间而不是单个包。

### 1.3 CAS：没有调用方令牌时落在行级

常规删除 `DELETE …/capabilities/{name}` 的 CAS 是「调用方传 `?resourceVersion=`，服务端拿它跟行比」。
卸载一次要删多行，调用方不可能为每行准备令牌（也不该为此先 GET 一遍再拼一个批量请求）。所以这里用
**服务端侧乐观 CAS**：事务内读出候选行的 `resource_version`，逐行 `UPDATE … WHERE id=? AND resource_version=?`，
`requireCASRow` 判 0 行 → `409 resource_version_conflict`。判据与 `DeleteCapability` / `deleteSourceOn`
完全相同（`DeleteCapability` 内部也是同一行 SQL 形状），只是把「令牌来自调用方」换成「令牌来自同一事务的读」。
所有权判据（`installed_from ∈ 本 release`）在候选集里就已经成立，写路径不再按名字匹配。

### 1.4 幂等与「不删回执」

- 回执保留：`playbook_installs` 行一条不删，`receipt` JSON 原文不动（它是「这次安装做了什么」的审计，
  改它等于改历史）。
- 标记：`uninstalled_at`（秒）。第二次调用先读该 release 的回执，**全部非 0** 就直接返回
  `alreadyUninstalled: true`（事务里零 UPDATE、零 DELETE）。
- 重新安装：软删行不占名字（`capability_versions` 的唯一索引是 `WHERE deleted_at IS NULL` 的分部索引，
  源是硬删），所以「卸载 → 再装同一个 release」会建出新一代行，新回执 `uninstalled_at = 0`，
  release 重新出现在实例清单里。这条不在验收里，但语义上是通的。

### 1.5 边投影：删到名字的 latest 才重算

能力行的边投影（`edges.from_is_source = 0`）只属于该名字的**最高可见版本**。卸载可能删掉
`x@2`（latest）留下手工的 `x@1`：此时投影必须按 `x@1` 的 `spec.next` 重建；如果三行全没了，
投影必须删掉（不能让派发落到一个不存在的能力）。实现是「删前记住每个名字的最高可见版本，
只有它被删掉时才重算」——删低版本不动投影，避免无意义的边重写（`created_at` 抖动）。

源的删除直接复用 `deleteSourceOn`：CAS + 删 `from_is_source = 1` 投影 + 删行，与常规删源同一条路径。

### 1.6 schema：为什么是迁移块 3

`playbook_installs` 是回执表，卸载状态是「这条回执被撤销了」的事实，落在列上最自然（幂等查询与实例清单
过滤都在 SQL 侧）。规则是 store 模块约定 §3.2：**已发布的 baseline 不再改，只追加块**。所以：

```
-- +migration 3 playbook_uninstall
ALTER TABLE playbook_installs ADD COLUMN uninstalled_at INTEGER NOT NULL DEFAULT 0;
```

`headVersion` 同步 +1（不然新块永远不会被应用）。既有本地库重开时就地迁移；不需要新索引
（`idx_playbook_installs_release(namespace_id, playbook_id, release, created_at)` 已覆盖）。

## 2. Wire

请求：

```
DELETE /v1/namespaces/default/playbooks/pr-chain/releases/a
DELETE /v1/namespaces/default/playbooks/pr-chain/releases/a?installId=ins_abc123
```

200（动作文档，不套四键信封）：

```json
{
  "release": "a",
  "alreadyUninstalled": false,
  "uninstalledAt": "2026-11-21T12:00:00Z",
  "installIds": ["ins_abc123"],
  "removed": { "capabilities": ["a-demo-fixer@1", "a-demo-review@1"], "sources": ["a-demo-on-push"] },
  "kept": [{ "kind": "capability", "name": "share-demo-review", "version": 2, "reason": "manual" }],
  "note": "已删除该 release 装出的对象；安装回执保留作为审计，他人创建的对象未删除"
}
```

第二次（幂等）：`alreadyUninstalled: true`，`removed` 两个列表与 `kept` 都是 `[]`，`installIds` /
`uninstalledAt` 与第一次相同。

错误面（全部 0 写）：

| 情形 | 状态 | reason |
|---|---|---|
| release 不在这个包名下 | 404 | `release_not_found` |
| `installId` 不属于该 release | 409 | `install_conflict` |
| release 形状非法（如 `Prod`） | 400 | `invalid_playbook` |
| 包不存在 | 404 | `NotFound` |
| 删除中某行被并发改动 | 409 | `resource_version_conflict` |

## 3. 改动面

| 文件 | 做了什么 |
|---|---|
| `internal/store/schema.sql` | 追加 `-- +migration 3 playbook_uninstall`（`uninstalled_at`） |
| `internal/store/migrate.go` | `headVersion` 2 → 3 |
| `internal/store/playbook_uninstall.go`（新） | `UninstallPlaybookRelease`：单事务、候选行、kept 三码、逐行 CAS、投影重算、回执标记 |
| `internal/store/playbook.go` | `ListPlaybookInstalls` 过滤 `uninstalled_at = 0` |
| `internal/store/playbook_tx.go` | 只压缩注释（买注释预算），逻辑零改动 |
| `internal/store/v4test/playbook_uninstall_test.go`（新） | 7 组语义：只撤本 release / 手工行 / 别的 release 行 / 被复用的行 / 幂等指纹 / 错误面 / 投影 |
| `internal/server/resource_playbook.go` | 路由 + `uninstallRelease` handler（动作文档、`?installId=`、空列表纪律） |
| `internal/server/api_status.go` | 注册 `release_not_found`（404）/ `install_conflict`（409） |
| `api/resource.yaml` | 新 path + `PlaybookUninstallResult` / `PlaybookUninstallKept` |
| `internal/server/openapi_parity_test.go` | pair 与四个现状快照数同步（pairs 202→204 等） |
| `internal/server/playbook_uninstall_test.go`（新） | handler 层：卸载后回执仍在 / 幂等零变化 / 手工行 kept / 错误面 |
| `web-src/pages/playbook.html` | 每个 release 一个卸载按钮 + 二次确认 + 结果区 |
| `web-src/locales/{zh-CN,en,ja,es}.yaml` | 同组 6 个 key（四份键集合一致） |
| `scripts/e2e/suites/playbook_release.mjs` | 新增 4 个 part（78 条断言） |
| `scripts/e2e/affected-map.mjs` | `playbook_release` 的 why/paths 补上 uninstall 面 |
| `scripts/e2e/fixtures/f3/f3-playbook.mjs` | 单回执按钮选择器排除新增的卸载按钮 |
| `scripts/check_envelope.py` | E8「CAS DELETE 必须带 ?resourceVersion=」的路径正则收窄（见 §4.3） |
| `scripts/check-annotations.mjs` | `COUNT_UNMARKED_BASELINE` 210 → 209（本 PR 压掉了一条计数型注释） |

## 4. 取舍与已知边界

### 4.1 预览桩（`web/mock.js`）没有实现卸载

`make mock` / `fe-preview` 的假服务端只实现了只读与安装/渲染；点卸载会落到「未知路由」。它是浏览器预览
桩，不在任何门禁与 e2e 断言里（e2e 用真二进制），本 PR 不动它。要补的话是同一套 kept 语义的 JS 副本，
不值得在交付里造第二套判定。

### 4.2 `other_release` / `shared_by_release` 由 store 单测钉，e2e 只覆盖 `manual`

e2e 覆盖「同名手工行」这条（它也是变异靶子）。另两码的构造需要跨包同名 release / upgraded 版本，
在 HTTP 面上要铺很多夹具，收益不如在 store 的 v4test 里直接断言（`TestPlaybookUninstallKeepsOtherReleaseRow`、
`TestPlaybookUninstallKeepsRowReusedByOtherPlaybookRelease`）。

### 4.3 `check_envelope.py` 的 CAS 正则收窄

E8 原来要求「`…/playbooks/*` 的 DELETE 必须声明 `?resourceVersion=`」。本端点按契约 §1 明确不吃调用方令牌
（CAS 逐行执行），所以把 playbooks 那条正则收窄成**单资源** `…/playbooks/{slug}$`，并在脚本里写清理由；
不是删规则，而是把「哪些 DELETE 需要调用方令牌」缩小到原来那五条资源的形状。

### 4.4 注释密度按目录买预算

`internal/store`（6821）、`internal/server`（7765）、`api`（271）、`web-src/pages`（2400）、
`scripts/e2e/suites`（2182）都是**每目录冻结**。本 PR 新增注释一律在同目录压缩旧注释买预算：
`playbook_tx.go` 与 `resource_playbook.go` 的散文被压缩，新测试文件只留必要注释，
期间还发现并修正了一处「新增文件未入 git 索引时棘轮看不见它」的假绿窗口（先 `git add -N` 再量）。

## 5. 验收证据

复算前提：`make fe`（dist 新鲜度门禁）→ 各条命令在仓库根执行。

### 5.1 装 release a（2 能力 + 1 源）→ 卸载 → 三行都没了、回执仍在、其他 release 一行不动

命令：`./scripts/e2e/run_all.sh --serial playbook_release`

```
$ ./scripts/e2e/run_all.sh --serial playbook_release
  ✅ 卸载 release a → 200
  ✅ 动作文档回显 release
  ✅ 首次卸载 alreadyUninstalled=false
  ✅ removed.capabilities 只列 a 装出的两行（按名字排序）
  ✅ removed.sources 只列 a 装出的源
  ✅ kept 是空数组（没有同名的手工/他人行）
  ✅ installIds 只有 1 条（a 只装了一次）
  ✅ installIds[0] 就是 a 的回执 id
  ✅ uninstalledAt 是能被 Date.parse 解出的时间
  ✅ note 非空（回执保留、他人对象不删的说明）
  ✅ 能力列表 → 200
  ✅ 能力列表 items 是数组（不是 null）
  ✅ a 的两个能力都不在列表里
  ✅ b 的两个能力仍在列表里
  ✅ 源列表 → 200
  ✅ 源列表 items 是数组（不是 null）
  ✅ a 的源不在列表里
  ✅ b 的源仍在列表里
  ✅ 卸载 a 后的包的实例清单 → 200
  ✅ 卸载 a 后的包的实例清单 installs 是数组（空列表也要 []）
  ✅ 已卸载的 release 不再出现在实例清单里
  ✅ 清单里只剩 release b
  ✅ a 的回执仍能单条读（回执是审计）→ 200
  ✅ 单条回执仍然说 release=a
  ✅ 卸载不删能力历史行（capability_versions 行数不变）
  ✅ 卸载删掉 a 的源行（sources -1）
  ✅ 库里 a 的 2 行能力都打了 deleted_at（软删不物理删）
  ✅ 库里 a 的能力没有一行还可见
  ✅ 库里 b 的 2 行能力仍可见（一行没动）
  ✅ 库里 a 的源已经删行
  ✅ 库里 b 的源还在
  ✅ a-uninst-fixer 的出边随行撤掉
  ✅ b-uninst-fixer 的出边还在
```

### 5.2 共享行：手工同名能力 → 卸载 → 进 `kept[]`、不删、给原因

```
  ✅ 建包 pb-uninstall-share → 201
  ✅ release a 安装 → 200
  ✅ 手工再建同名能力（内容不同）→ 201
  ✅ 手工行落在 v2（同名新内容分配 max+1）
  ✅ GET 同名能力 → 200
  ✅ 读回来的名字就是它
  ✅ 读回来的是 v2
  ✅ 卸载 release a（同名手工行在场）→ 200
  ✅ kept 只有一条
  ✅ kept 说清留下谁、哪版、为什么
  ✅ removed.capabilities 仍是本 release 的 v1
  ✅ removed.sources 仍是本 release 的源
  ✅ 手工行没被删：GET 仍 200
  ✅ 手工行仍是 v2
  ✅ 同名两版都在库里（软删不是物理删）
  ✅ v1（本 release 装的）已软删
  ✅ v2（手工的）仍可见
```

### 5.3 幂等：第二次卸载 → `alreadyUninstalled`，库里 0 变化

```
  ✅ 卸载 release b → 200
  ✅ first.alreadyUninstalled=false
  ✅ first.removed.capabilities 是 b 的两行
  ✅ first.removed.sources 是 b 的源
  ✅ 再卸一次 release b → 200
  ✅ 第二次 alreadyUninstalled=true
  ✅ 第二次 removed.capabilities 是 []
  ✅ 第二次 removed.sources 是 []
  ✅ 第二次 kept 是 []
  ✅ 第二次 installIds 非空（幂等也返回审计锚点）
  ✅ 第二次 installIds 与第一次相同
  ✅ 第二次 uninstalledAt 与第一次相同（标记不刷新）
  ✅ 第二次卸载后库指纹逐字节相同（0 写）

  错误面（同样每条都验 0 写）：
  ✅ 卸载release 不存在 → 404 → 404
  ✅ 卸载release 不存在 → 404 → reason=release_not_found
  ✅ release 不存在之后库指纹不变（0 写）
  ✅ 卸载installId 与该 release 对不上 → 409 → 409
  ✅ 卸载installId 与该 release 对不上 → 409 → reason=install_conflict
  ✅ installId 与该 release 对不上之后库指纹不变（0 写）
  ✅ 卸载release 形状错 → 400 → 400
  ✅ 卸载release 形状错 → 400 → reason=invalid_playbook
  ✅ release 形状错之后库指纹不变（0 写）
  ✅ 包不存在 → 404 → 404
  ✅ 包不存在 → 404 → reason=NotFound
  ✅ 包不存在之后库指纹不变（0 写）
```

### 5.4 变异：「只删本 release」→「删同名全部」

把 `internal/store/playbook_uninstall.go` 里能力行的删除改成按名字删所有可见版本（去掉 `id`/`rv` 条件），
再跑同一个套件：

```
  [playbook_release] 170 通过 / 3 失败
  失败项:
    - 手工行没被删：GET 仍 200 — 期望 200 实际 404 {"apiVersion":"anotify/v1","kind":"Status","status":"Failure","code":404,"reason":"NotFound","message":"not found"}
    - 手工行仍是 v2 — 期望 2 实际 undefined
    - v2（手工的）仍可见 — {"version":2,"deleted_at":1795264680}
  ❌ 套件 playbook_release 失败（assertion）
  ❌ E2E 未全绿
```

`cp` 恢复后同一条命令回到 `✅ playbook_release 173通过/0失败 3.6s mode=real`（§5.5 的全量复跑里同一套件是 173/0）。
这就是「验收 2 会红」的证据：手工行被带走，`kept[]` 变空，`GET …/capabilities/share-demo-review` 404。

### 5.5 门禁

```
$ make ci
✅ 契约静态断言（A2/A4/A5/A6/A7 + openapi 一致性 + 字段级核对 + anode 可编译）通过
✅ class 校验通过：861 个类（HTML class 属性 611 + JS class 串新增 250）全部落在设计系统 / Tailwind 工具类内
✅ i18n 键守卫：4 份 locale 键集合一致，2123 个引用点全部有词条
✅ 文案守卫通过：4 份 locale / 9388 条值 · 22 个页面 / 217 个文本节点（实现词检查面 100 个）
✅ 注解即外键守卫通过：全部引用可解析、锚点可解析、棘轮未超基线。
✅ 与基线完全一致（超与降都算不一致）—— 有收紧请同批下调基线，新增必须在此刻被拦住
go test ./... -count=1 -shuffle=on
ok  	github.com/zzjcool/anotify/internal/playbook	0.582s
ok  	github.com/zzjcool/anotify/internal/server	68.902s
ok  	github.com/zzjcool/anotify/internal/store	18.531s
ok  	github.com/zzjcool/anotify/internal/store/v4test	46.148s
BenchmarkDispatchChain-12  1  2201795 ns/op  123376 B/op  2282 allocs/op
$ echo $?
0
```

```
$ make e2e-affected         # api/** 与 internal/store/schema.sql 落在共享面 ⇒ 全量 30/30
  ✅ playbook_release   173通过/0失败  4.8s  mode=real
  ✅ frontend           43通过/0失败  163.4s  mode=real
  ✅ capacity           84通过/0失败  147.8s  mode=real
  ✅ ui_shift           87通过/0失败  152.0s  mode=explicit-mock
  ✅ E2E 全绿：30/30 个请求套件均达标（断言 2641 通过 / 0 失败，authority 清单覆盖 30 个）
  ✅ 临时目录零残留（隔离 TMPDIR 内本次新增 = 0，耗时 355.6s）
$ echo $?
0

```

known-env：`scripts/e2e/known-env-failures.json` 未改动（保持空登记），本 PR 没有新增环境豁免。

## 6. 遗留风险

1. **`?installId=` 的钉桩只保护「同一 release 的回执集合变化」**：如果 release 被卸载后重新安装、并且
   老回执仍被保留，客户端拿老 id 会 409 —— 这是设计意图（让页面重读），不是缺陷；但调用方要能处理 409。
2. **边投影重算的触发条件是「删掉了该名字删前的最高可见版本」**：删低版本不动投影。若将来出现
   「边投影本身就脏」的库，卸载不会顺手修它（那是 `PUT …/capabilities` 的职责），符合「一次只做一件事」。
3. **大命名空间的 `shared_by_release` 扫描是逐行解 receipt JSON**：回执是十位量级（每次安装一条），
   卸载是低频动作，没有为它建索引或反查表。真到万级回执时应该改成「安装时把复用引用落成行」。
