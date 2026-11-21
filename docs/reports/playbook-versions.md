> **历史快照**：本文件是当时的交付记录，不随代码演进；行为以代码、测试与 `docs/contracts/` 为准。
<!-- snapshot-doc: 2026-11-21 交付当次的门禁与断言数字（报告是定格记录）；复算命令逐条写在 §5 -->

# 包版本历史 + 按版本安装（#511 · 同一预案的 v1/v2 可并存）

as_of: 2026-11-21 · area: `store` / `playbook` / `server` / `web` / `e2e` · 迁移块 `4 playbook_versions`
分支 `pi-subagent/hw-nj/anotify-511-versions-ca56c983` · 起点 `main@5e1ea740`（含 #505 / PR #509、#510 / PR #514）

## 0. 结论

1. **版本历史是「按版本安装」的直接前提，不是给包看的展示字段。** `PUT` 会把 `playbooks.files` 整体换掉，
   所以「v1 装出去之后再也装不回来」——本 PR 把每次**内容变化**的那份包体留在
   `playbook_versions` 里，`install` 才能按 `version` 取回旧的那一份。
2. **只在内容变了才写一行。** 判据是包体自己的 canonical 摘要（`playbook.FilesDigest`，与回执的
   `playbook.filesDigest` 同一个算法）：同一个包连续保存 3 次、内容没变 → 历史仍然只有 1 行。
   版本号涨了、内容没变（只改 `metadata.version`）**也不追加**：历史回答的是「有哪几份包体」。
   反过来，版本号没动、内容变了 → **覆盖**该版本号那一行（一个版本号只对应一份包体，否则
   「按 v1 装」有两种答案）。
3. **读路径不 join 这张表。** 当前版本的真身始终是 `playbooks.files`（`GET …/playbooks/{slug}`
   一次读、零 join）。版本表只服务两个新读面：`GET …/versions`（列表摘要）与 `install/render`
   的 `version` 参数。
4. **`PlaybookActionRequest.version` 可选，缺省 = 当前版本。** 缺省路径与上一版逐字同形
   （不传参数还能装、回执字段只增不改、`created/skipped/values` 完全一致）：有对照测试
   `TestInstallPlaybookDefaultVersionMatchesCurrentPin` 用「同一份包体、两个命名空间，一次走旧入口、
   一次显式钉住当前版本」比 `created` / `values` / 包身份。
5. **回执可追溯。** `InstallReceipt.playbook` 现在必带 `version` + `filesDigest`，两者都是
   「**这次真的用的那一份**」：按 v1 装出来的回执不会自称 v2。单回执 `GET …/installs/{id}`
   与列表同源（回执是审计快照，不按当前包状态倒推）。
6. **UI 是「当前版本 + 历史列表 + 每行一个按此版本安装」。** 版本是读模型 + 动作参数，不是第二条写
   路径：页面没有新增任何写 `PUT …/config` 或直接改版本表的能力（配置写路径守卫仍绿）。

## 1. 判据（为什么这样切）

### 1.1 表形状与归属

```
playbook_versions(playbook_id, version, files, digest, created_at, created_by)
PRIMARY KEY (playbook_id, version)
```

- `files` 与 `playbooks.files` 同形状（`{相对路径: 正文}` 的 JSON）——「按版本装」就是把这一列喂给同一个
  `playbook.Parse`，不新增解析路径。
- `digest` 是 `playbook.FilesDigest(files)` 的落库副本：读列表时不必把每份包体反序列化一遍。
  写入时**由 files 现算**（不接受调用方传值），否则会出现「列表说的摘要与装进去的不是同一份」。
- `playbook_id` 是**历史 id，不是外键**：与 `playbook_installs` 同一条纪律——删包不该连带删历史，
  也不该因为历史行而删不掉（`ON DELETE` 两种都是错：CASCADE 抹掉审计，RESTRICT 让删包永远失败）。
- `created_by` 落操作者 id（与能力审计 `actor_id` 同源：`auth.UserIDFromContext`）；手传包体之外
  的路径（测试/迁移脚本）允许空串。

### 1.2 为什么同版本号改内容是「覆盖」而不是拒绝

`PUT` 的既有语义是「整体替换」，`metadata.version` 由包作者填写、服务端不强制递增。三种候选：

| 处置 | 后果 |
|---|---|
| 拒绝「内容变了但版本号没动」的 PUT | 老工作流直接 400（本版之前这是合法操作）——违反「老包不回归」 |
| 追加两行同 version | 「按 v1 装」有两种答案，`?version=1` 变成不确定 |
| **覆盖该 version 那一行** | 一个版本号一份包体；已装出去的回执仍各自记着自己那份摘要（审计不受影响） |

选第三种。代价写在明处：历史行的 `created_at` / `created_by` 会随之更新，它回答的是
「这个版本号现在是哪份内容、谁写的」，而「某次安装装了哪一份」由回执回答。

### 1.3 迁移前的包（没有历史行）怎么装

`resolvePlaybookFilesOn` 的三条分支就是判据：

1. `version <= 0` → 当前版本（`playbooks.files`），**一次历史表都不读**（缺省路径零额外查询）；
2. 历史里有这一行 → 用它；
3. 历史里没有、但它等于当前版本号 → 用 `playbooks.files`（迁移前建的老包从没有过历史行，
   而当前版本的内容一直在这一列上；不回退就会把老包的默认安装突然变成 404）；
4. 其余 → `404 version_not_found`。

第 3 条是「老库能被就地迁移」的落点：迁移块只建表，不回填历史（SQL 里算不出 canonical 摘要，
而在 SQL 里再造一份摘要实现就是两份真相）。历史从下一次内容变化开始积累。

### 1.4 错误面

| 情形 | 状态 | reason |
|---|---|---|
| `version` 指向不存在的版本 | 404 | `version_not_found` |
| `version < 1`（含显式 0 / 负数） | 400 | `invalid_playbook` |
| 包不存在 | 404 | `NotFound` |
| 渲染不存在的版本 | 404 | `version_not_found`（与 install 同判据） |

`version` 用指针解码（`*int`）：缺省与显式 0 必须能分辨——把 0 当「没给」会让用户以为装的是历史
版本、装的却是当前版本。

## 2. 传输面

### 2.1 `GET /v1/namespaces/{namespace}/playbooks/{slug}/versions`

200（读模型，不套资源信封）：

```json
{
  "total": 2,
  "currentVersion": 2,
  "versions": [
    { "version": 2, "filesDigest": "sha256:9fb4…", "createdAt": "2026-11-21T13:47:51Z", "createdBy": "usr_BCft…", "current": true },
    { "version": 1, "filesDigest": "sha256:6272…", "createdAt": "2026-11-21T13:47:51Z", "createdBy": "usr_BCft…", "current": false }
  ]
}
```

`versions` 按 `version` 降序；空列表是 `[]`。`currentVersion` 是 `playbooks.version`（当前版本号），
`versions[].current` 只是标记历史里哪一行就是它。

### 2.2 `POST …/playbooks/{slug}/install`（新增可选 `version`）

```json
{ "values": { "project": "demo" }, "release": "vb", "version": 1 }
```

回执里的包身份（**必填两个字段**）：

```json
{ "playbook": { "name": "pb-versions", "version": 1, "filesDigest": "sha256:6272…" } }
```

同一个包先装 release `va`（当前 v1）、改包成 v2、再按 v1 装 release `vb`：两份回执分别是
`v1/6272…` 与 `v2/9fb4…`，两份实例的能力行脚本正文分别是 `echo v1` / `echo v2`（e2e 用落库正文比对，
不看人眼）。

### 2.3 `render` 同样吃 `version`

预览与安装共用 `resolvePlaybookFilesOn`：否则会出现「预览是 v2、装进去是 v1」这种无法解释的差异。

## 3. 改动面

| 文件 | 做了什么 |
|---|---|
| `internal/store/schema.sql` | 追加 `-- +migration 4 playbook_versions`（新表，含列注释与三条纪律） |
| `internal/store/migrate.go` | `headVersion` 3 → 4 |
| `internal/playbook/digest.go`（新） | `FilesDigest`：排序 + 长度前缀的 canonical sha256（回执与历史表共用的唯一算法） |
| `internal/playbook/playbook.go` | `PlaybookRef` 加 `filesDigest`（必填） |
| `internal/playbook/plan.go` | `PlanForRelease` 填 `filesDigest` |
| `internal/store/playbook.go` | `CreatePlaybookAs` / `ReplacePlaybookAs`（带操作者、单事务写历史行）、`ListPlaybookVersions`、`Render/InstallPlaybookVersion`、`ErrPlaybookVersionNotFound` |
| `internal/store/playbook_tx.go` | `recordPlaybookVersionOn`（内容变了才写、同版本号覆盖）、`resolvePlaybookFilesOn`（版本 → files 的三条分支） |
| `internal/store/playbook_versions_test.go`（新） | 5 组语义：只在内容变化时追加 / 按版本装的两份实例内容各自正确 / 版本不存在 / 缺省与钉住当前版本逐字同形 / 列表与老包 |
| `internal/store/v4test/*` | 结构门禁：表集合 + 必需列 + 记账版本 3 → 4（三处快照） |
| `internal/server/resource_playbook.go` | 路由 + `listVersions`、`playbookActionBody.version` + `pinnedVersion()`、create/replace 传操作者 |
| `internal/server/api_status.go` | 注册 `version_not_found`（404） |
| `internal/server/resource_playbook_versions_test.go`（新） | handler 层：列表形状 / 钉住版本的两份回执 / 404 与 400 / render 钉住 |
| `api/resource.yaml` | 新 path `…/versions`、`PlaybookVersionEntry` / `PlaybookVersionList`、`PlaybookActionRequest.version`、`PlaybookRef.filesDigest` |
| `internal/server/openapi_parity_test.go` | 两对 DTO + 四个现状快照数（operations 139→140、mux 128→129、pairs 204→206、schemas 242→244） |
| `web-src/pages/playbook.html` | 「版本历史」卡片：当前版本号 + 每行摘要 + 「按此版本安装」 |
| `web-src/locales/{zh-CN,en,ja,es}.yaml` | 同组 12 个 key（四份键集合一致） |
| `web/partials.js` | 回执模型透传 `playbook.filesDigest` |
| `scripts/e2e/suites/playbook_release.mjs` | 新增 `partVersions`（包版本历史 + 按版本安装 + 错误面） |
| `scripts/e2e/affected-map.mjs` / `shard.mjs` | 套件说明补 #511；`playbook_release` 成本 4.5 → 5.5s |

## 4. 变异自证（两次都真的红了）

变异只在工作区做，跑完立刻 `git checkout` 复原；命令与输出见 §5.3。

| 变异 | 破坏了哪条判据 | 结果 |
|---|---|---|
| ① 把「按 version 取 files」改成「永远取当前」（`resolvePlaybookFilesOn` 首条分支恒真） | 语义 3：钉住的版本不生效 | `playbook_release` **211 通过 / 11 失败**：`按 v1 装的回执 filesDigest = v1 指纹`、`release vb 的能力行脚本正文是 v1`、`单回执 playbook.version = 1`、`两份回执的包版本与文件指纹都不同`，以及三条 404 面（版本不存在变成 200） |
| ②a 删掉「内容相同不写历史」的判据（`changed` 恒为 true） | 语义 2：重复保存既不追加也不重写 | `playbook_release` **224 通过 / 1 失败**（`同内容保存不重写历史行（createdAt 不变）`）+ store 单测红（`同内容 PUT 不该追加、也不该重写历史行`，`created_by` 被改成 `usr_b`） |
| ②b 把「内容变了才写」换成「每次保存都追加一个新版本号」 | 语义 1：历史 = 有哪几份包体，不是每次保存 | `playbook_release` **223 通过 / 2 失败**：`内容相同的重复保存不追加历史行（行数不变）` 期望 2 实际 3、`同内容保存后仍只有 2 版` 期望 2 实际 3 |

**②a 为什么要分两条：** 版本表的主键是 `(playbook_id, version)`，所以「同内容重复 PUT」这一动作在
`changed` 被删掉之后**行数依然不变**（UPSERT 覆盖同一行）——只靠计数抓不到它。抓得到的是「那一行被重写了」：
`created_by` / `created_at` 会被刷新。于是验收 3 同时钉住两件事——行数不变 **且** 已有行的写入时刻不变
（store 单测比 `created_by`，e2e 比 `createdAt`，因此不需要依赖秒级时钟的分辨率）。②b 才是「计数会变」的那种错
实现（历史 = 每次保存），专门用来证明那条计数断言不是恒绿。

## 5. 验收证据

### 5.1 Go 侧

```
$ go test ./internal/store/ -count=1
ok  github.com/zzjcool/anotify/internal/store  12.8s
ok  github.com/zzjcool/anotify/internal/store/v4test  40.4s

$ go test ./internal/server/ -count=1
ok  github.com/zzjcool/anotify/internal/server  62.7s

# 单跑本 PR 的用例（复算上面两组里的名字）
$ go test ./internal/store/ -run 'TestPlaybookVersion|TestInstallPlaybookVersion|TestInstallPlaybookDefaultVersion|TestListPlaybookVersions' -count=1
$ go test ./internal/server/ -run 'TestPlaybookVersions|TestPlaybookInstallPinned|TestPlaybookInstallVersionRejected|TestPlaybookRenderPinned|TestOpenAPIParity' -count=1
```

### 5.2 门禁

```
$ make ci
EXIT=0        # contract / gofmt / fe（死类·wire 字段·CSS·i18n·文案·对比度·sitegen）/ vet / build /
              # 单测（tmp 守卫）/ bench 冒烟 / harness 自测 / 注释密度（每目录等值）/ 注解即外键
```

`make e2e-affected`：见 §5.4（本 PR 改到 `api/resource.yaml`，共享面按设计升级为全量 30 套）。

### 5.3 变异（输出摘录）

```
# ① 钉住的版本不生效
$ node scripts/e2e/suites/playbook_release.mjs
  ❌ release vb 的能力行脚本正文是 v1（按版本装拿到了旧那一版） — #!/bin/sh
  echo v2
  ❌ 按不存在的版本装 → 404 version_not_found → 404 — 期望 404 实际 200
[playbook_release] 211 通过 / 11 失败

# ②a 同内容也写历史（行数不变，但把已有行重写了）
$ node scripts/e2e/suites/playbook_release.mjs
  ❌ 同内容保存不重写历史行（createdAt 不变） — 期望 2026-11-21T14:15:27Z 实际 2026-11-21T14:15:28Z
[playbook_release] 224 通过 / 1 失败
$ go test ./internal/store/ -run TestPlaybookVersionsRecordedOnlyOnContentChange
    playbook_versions_test.go:89: 同内容 PUT 不该追加、也不该重写历史行: map[1:{sha256:a908… usr_b}]

# ②b 每次保存都追加一个新版本号
$ node scripts/e2e/suites/playbook_release.mjs
  ❌ 内容相同的重复保存不追加历史行（行数不变） — 期望 2 实际 3
  ❌ 同内容保存后仍只有 2 版 — 期望 2 实际 3
[playbook_release] 223 通过 / 2 失败
```

复原后同一条命令回到 **225 通过 / 0 失败**。

### 5.4 e2e

```
$ make e2e-affected
✅ E2E 全绿：30/30 套件达标（断言 5306 通过 / 0 失败）
```

`playbook_release` 从 222 → **225** 条断言（新增的 `partVersions` 7 条 + 版本历史相关断言）。
本 PR 改到 `api/resource.yaml`（共享面）→ `affected` 按设计升级为全量 30 套；`i18n` / `i18n_coverage` /
`ui_shift` 三个浏览器套件会走 `web/mock.js` 的预览桩，所以预览桩也实现了 `GET …/versions`
（见 §3 表）。

## 6. 已知边界

1. **不回填历史。** 迁移只建表；迁移前建的包在历史里是空的（当前版本仍可按版本号装，见 §1.3）。
   这是刻意的：canonical 摘要只能在 Go 里算，在 SQL 里再写一份就是两份真相。
2. **版本号由包作者维护。** 服务端不强制递增，也不为「同号不同内容」造第二个版本号；
   要并行保留两份内容就换版本号（历史按版本号索引）。
3. **`playbook_versions` 不参与保留期清理。** 它跟 `playbooks` 同生命周期（删包不删历史，历史行很小）。
   如果将来要按包分组清理，应与回执一并设计，而不是单独给这张表加删除路径。
4. **UI 的「按此版本安装」用当前表单的 values / 实例名。** 版本只钉住包体，不钉 values——
   同一次安装的身份仍然是 `release + valuesDigest + packageVersion` 三元组。
