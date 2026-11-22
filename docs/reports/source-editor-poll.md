> **历史快照**：本文件是本次交付的记录，不随代码演进；行为以代码、测试与 `api/` 为准。

<!-- snapshot-doc: 2026-11-22 #540 交付时定格的数字（make ci / make e2e-affected 的真实输出），复算命令见各节 -->
# 编辑源的下拉补上「拉取」（#540）交付报告

> 分支 `pi-subagent/host-workspace/anotify-540-editor-d0431f7c` · base `main@fefd9822`
> （含 #533 的 poll 后端与 #537 的 poll 只读面）
> issue 的「四件要做的事」在 §2、「七条验收」在 §4 逐条对上号。

一句话：**poll 源能建、不能改**。编辑器 kind 下拉里没有 poll，于是打开编辑再保存，
要么被 `source_kind_immutable` 拒（下拉被当成「改 kind」），要么表单把 config 整份重建、
把 `url` / `schedule` / `envelope` / `headers` / 凭据引用一起抹掉。这次给 poll 补一条完整的
传输配置表单，并把「编辑不许丢配置」写成一条**变异自证**的 e2e 判据。

---

## 1. 根因：两个事实叠在一起

| # | 事实 | 证据 |
| --- | --- | --- |
| 1 | 编辑器只有 cron / webhook / manual 三个选项，poll 源打开编辑时 `#f-kind` 回落到 `manual` | 本文件 §2 落点 1（改前的 `web/source-editor.js` MARKUP） |
| 2 | poll 的 `PATCH spec.config` 是**整体替换**，不是深合并：`MergeConfig` 对非 webhook 只跑 `NormalizeConfig(patch)`，而 `url` / `schedule` 是必填 | `internal/sourcespec/config.go` 的 `MergeConfig`；`internal/sourcespec/poll.go` 的 `normalizePollConfig` |

两条合起来就是现象：表单按 manual 重建 config（`{payloadSchema, payload}`）→ 服务端看到「poll 但没有
url」→ 400；或者把 kind 一起改了 → 400 `source_kind_immutable`。用户侧看到的是「编辑一次配置就没了」。

还有第三条把「修好表单」这件事变得没那么简单：

| # | 事实 | 后果 |
| --- | --- | --- |
| 3 | 读投影**刻意不投影** `config.headers` / `config.secretRef` / `config.allowInsecure`（`Anotify.model.source` 只取 url / schedule / timeoutMs / maxItems / envelope；#537 的理由是 headers 可能夹带凭据，不该进只读面） | 只靠 `model.source` 填表单，一次保存必然把这些键丢掉 → 必须另读一次原始 config 当基底 |

## 2. 四件落点

| # | 要做 | 落点 | 判据（e2e / 静态） |
| --- | --- | --- | --- |
| 1 | 下拉加 `poll`，选中显示**传输配置表单**（不是原始 JSON 框） | `web/source-editor.js`：kind 四选一（文案走 `sources.kind.*`）；新增 `#f-poll-wrap` 一段两块：① 传输 = `url` + `headers` 键值对增删（上限 10，行内 `remove_target` 删行、`#add-poll-header` 加行）+ `secretRef{name,header,prefix}` + 只写 `secret` + `timeoutMs` / `maxItems`；② 信封 = `items` / `type` / `deliveryId` / `payload` 四指针 + 「整个条目作为载荷」开关（`payload: {whole: true}` 的还原通道）。排期复用 cron 的 `#f-expr` / `#f-tz` / `#cron-presets`，标签换成「排期」 | `POLL-EDIT-02` / `POLL-EDIT-03` / `POLL-EDIT-09`（重名请求头客户端拦住；重新打开无残留错误提示） |
| 2 | 已存在源的 kind **只读** | 沿用既有的 `$("f-kind").disabled = !!src`（语义与 `source_kind_immutable` 对齐：不是「能点但保存失败」） | `POLL-EDIT-02`（`isDisabled() === true`）+ `POLL-EDIT-07`（真 PATCH 换 kind 仍然 400） |
| 3 | 保存走现有 PATCH；读回不回显 secret | 保存仍走 `request(name, body, "", "PATCH")`；`#f-poll-secret` 永远渲染成空框（只写），留空 = 不改动（服务端 `resolveSourceSecretOn` 此时只校验命名空间凭据**存在**） | `POLL-EDIT-01` / `03` / `04`（提交体无 `secret`；读面与页面文本都没有明文） |
| 4 | 新建时切 kind 清掉另一类的字段 | `syncKind()` 记住上一个 kind，切走时 `clearKindFields(prev)`：webhook → 清 `#f-token` / `#f-secret` / `#f-sample` / `#hook-read`；poll → `resetPollForm()`。**当场清 UI**，不是只在提交时忽略 | `POLL-EDIT-08`（表单三个框当场变空 + POST 体里没有 `token` / `secret` / `sample` / `hookUrl`） |

## 3. 关键设计决策

- **D1 表单 own 的键 vs 原样保留的键**。`pollConfig()` 从「存储的原始 config」深拷贝出发，只覆盖表单
  own 的键：`url` / `headers` / `secretRef` / `secret`(只写) / `schedule{expr,tz}` / `timeoutMs` /
  `maxItems` / `envelope`。`allowInsecure` / `sample` / `payloadSchema` 以及**将来新增的键**都不在表单里，
  因此原样留着 —— 这正是「编辑一次不许丢配置」的落点，也是变异 ② 的判别点。
- **D2 `allowInsecure` 不做 UI 开关**。issue 的字段表里没有它，而它不是一个普通字段：`true` 会**跳过
  全部私网判据**（https 指向 `10.0.0.5` 也能存）。它该由 YAML 那条面显式改；编辑器只负责**别弄丢**。
- **D3 读面不扩投影**。`config.headers` 不塞进 `Anotify.model.source`：读面是给人看的，写面才是编辑面；
  同一份事实两个投影各自演化正是 #537 刻意避开的东西。代价是打开 poll 编辑时多一次
  `GET …/sources/{name}`（只对 poll、只在编辑态）。
- **D4 排期只有一套输入**。poll 的 `schedule` 与 cron 的 `expr`/`tz` 复用同一组 DOM（`#f-expr` / `#f-tz` /
  四个预设按钮），服务端也是同一处校验（`ParseCron`）。kind 在编辑态只读，所以两组输入不可能同时需要。
- **D5 数字留空 = 用默认值**（`timeoutMs` 10000 / `maxItems` 100）。范围（1000–120000 / 1–500）不在前端
  预判：服务端会给出带字段路径的 cause，前端负责把字段标红（`flagField`）而不是复刻一份会漂移的规则。
- **D6 `whole` 与 `pointer` 是两种写法**，不能用一个输入框表达。多一个复选框、勾上时禁用指针框，
  换来 `payload: {whole: true}` 的**逐字节**往返（深度比较要求它一字不差）。

## 4. 七条验收的实测

| # | issue 的验收 | 实测 |
| --- | --- | --- |
| 1 | e2e：建 poll 源 → 编辑 → 改 `maxItems` 与 `schedule.expr` → 保存 → 重开断言新值；源仍能轮询 | `source-poll-edit`（frontend，真实权威）`POLL-EDIT-01` → `06`：重建后 `#f-poll-maxitems` 读回 `42`、`#f-expr` 读回新表达式；详情页「单轮条目上限 = 42」「排期 = 新表达式 · UTC」。**「仍能轮询」的判定口径**见下方注 |
| 2 | 编辑不丢 `url` / `envelope`：保存前后 GET 的 config 深度比较，只差改动字段 | `POLL-EDIT-05`：`assert.deepEqual(afterCfg, want)`，`want` = 保存前的 config 只改 `maxItems` 与 `schedule.expr` |
| 3 | `secretRef.secret` 只写：保存后读回只有 `secretRef{name,header,prefix}` | `POLL-EDIT-01`（`JSON.stringify` 不含明文）+ `POLL-EDIT-03`（框为空）+ `POLL-EDIT-04`（提交体 `'secret' in config === false`）+ `POLL-EDIT-05`（读回无 `secret`） |
| 4 | 新建时 webhook → poll，提交体里不含 `token` / `secret` | `POLL-EDIT-08`：表单三个框当场清空；POST 体对 `token`/`secret`/`sample`/`hookUrl`/`secretSet`/`expr` 逐个断言不存在 |
| 5 | 已存在源改 kind → 400 `source_kind_immutable`，且 UI 下拉禁用 | `POLL-EDIT-02`（`isDisabled()`）+ `POLL-EDIT-07`（真 PATCH 换 kind → 400 + `reason === 'source_kind_immutable'`） |
| 6 | 变异 ①去掉 poll 分支 → 验收 1 红；②编辑时整份重建 config → 验收 2 红 | 见 §5 |
| 7 | `make ci` exit 0；`make e2e-affected` 绿；known-env 空；注释密度不上调基线 | 见 §6（`make ci` exit 0；16/16 套件 · 1061 断言 0 失败；known-env `entries: []`；注释密度恰好等于基线） |

> **关于「源仍能轮询（`lastPollAt` 前进）」**：真轮询一轮要等 cron 的分钟边界（≤60s），而 frontend 套件
> 单 case 预算 52s、整包 360s —— 在 e2e 里等下一次真实轮询必然是 flaky，不是证据。
> 所以 e2e 断言的是**调度面**的事实：PATCH 改了 `schedule.expr` 之后服务端按新表达式重算
> `status.nextFireAt`（夹具排期 1 月 1 日 → 改成 7 月 1 日，断言 UTC 月份从 0 变 6）。
> 真跑一轮并断言 `lastPollAt` 前进的是 `go test ./internal/source`（httptest 真抓取，含游标/304/
> 退避/maxItems 的用例），那是这一步的确定性来源。

## 5. 变异自证（两条，都红过再复绿）

改 `web/source-editor.js` 的两处，每次重跑 `make fe` + 重建带 `embeddist` 的服务端二进制，再单跑
`source-poll-edit`（frontend 的 `--case` 入口）。

| 变异 | 改法 | 结果 |
| --- | --- | --- |
| ① 去掉 poll 分支 | `const config = kind === "poll" ? pollConfig() : {payloadSchema…}` 退回改前的 `const config = {payloadSchema…}` | **红**：`POLL-EDIT-04` —— `保存必须走既有 PATCH 路径并成功，实际 400`（服务端说 poll 缺 url，正是 issue 描述的现象） |
| ② 编辑时整份重建 config | `pollConfig()` 的基底从 `POLLCFG`（存储的原始 config）换成 `{}` | **红**：`POLL-EDIT-05` —— 深度比较里 `allowInsecure: true` 整行消失（表单不提供的键被洗掉） |

变异 ② 的第二次取证：把提交体上那几条「原样带上」的断言临时去掉，让**深度比较**单独当哨兵，红的还是
`POLL-EDIT-05`，diff 就一行 `- allowInsecure: true`。也就是说验收 2 那条断言自己就够抓住这个 bug，
不依赖前面任何一条 wire 断言。

两条改回去之后再跑一遍：`1 通过 / 0 失败`（10 条 behavior assertion）。

## 6. 门禁数字

复算命令：`make ci`、`make e2e-affected`、`node scripts/check-comment-density.mjs`。

- `make ci` → **exit 0**（在最终提交上复跑过一次，含 `contract` / `lint-go` / `fe` / `check-comment-density` / `test-frontend` /
  `go test ./... -shuffle=on` / `check-annotations` / `check-doc-links` 等全部目标）。
- `make e2e-affected` → **全绿**：16/16 个请求套件达标（断言 **1061 通过 / 0 失败**，wall clock 430.0s）。
  本次改动命中 16 个浏览器套件：`web/source-editor.js` 与 `web-src/locales/**` 都是页面共用资源，
  按 `affected` 的口径它们把 12 个真实权威套件 + 4 个 explicit-mock 套件全拉进来了。
  - `frontend`（real，211.0s / 360s 预算）45 通过 / 0 失败，含新登记的 `source-poll-edit`（10 条断言，`POLL-EDIT-01`～`09` + 一次 `clean()` 几何检查）。
  - 其余：`i18n` 33/0 · `i18n_coverage` 44/0 · `ui_shift` 87/0 · `ux_regression_19_23` 8/0 ·
    `vars_ui` 74/0 · `lang_hint` 86/0 · `cli_auth` 133/0 · `passkey_enroll` 85/0 · `admin_flow` 52/0 ·
    `deeplink` 50/0 · `auth_flow` 49/0 · `recovery_next` 16/0 · `events_paging` 179/0 · `p2_guards` 42/0 ·
    `password_login` 78/0 · frontend 45/0。
  - **一次负载型 flake（如实记录）**：第一次跑 `make e2e-affected` 时 `vars_ui` 红了一次
    （`locator.fill: Timeout 30000ms exceeded`，跑到命名空间 vars 那一段，整套墙钟 36.6s）。
    该套件单独重跑是 **74 通过 / 0 失败、3.2s**，且它只碰节点/命名空间 vars 表格、与源编辑器无关 ——
    是并行负载下的超时，不是本次改动。重跑整个 `make e2e-affected`（第二次）即全绿，即 §6 顶部的数字。
- `scripts/e2e/known-env-failures.json` 的 `entries` **保持空数组**（这次没有往白名单里放任何东西）。
- 注释密度：`web` 目录与全仓总量都**恰好等于基线**（新增注释行已同批从同一文件里删等量，
  基线没有被抬高；新 e2e 文件写 0 注释行，理由全部落在断言消息里）。
- 其它静态守卫：`check-i18n-keys`（4 份 locale 键集一致、引用点全部有词条）、
  `check-ui-copy`、`check-classes`（868 个类全部落在设计系统/Tailwind 内）、
  `check-test-discipline`（五项与基线一致）、`check-annotations`（引用/锚点/棘轮全绿）——都在
  `make ci` 里跟着跑过。

## 7. 不做 / 边界

- 不做「立即拉取一次」按钮（要新端点），也不把编辑器重造成通用 JSON Schema 驱动表单 —— 与 issue 的
  「不做」一致。
- 不动后端与 `api/`：`git diff --stat internal/sourcespec internal/server api` 为空。
- 不动 `web/partials.js` 的 `modelSource`（理由见 D3）。
- `headers` 的 UI 上限 10 是**新增行**的上限；已有源若经 YAML 写进更多，编辑时照常全部列出、不截断、
  保存原样带回（否则「打开编辑器就丢行」）。
- 已知未覆盖：poll 源的**草稿试运行**（`#btn-dry` → `POST …/sources/dry-run`）没有专门的 e2e；
  它走的是同一份 `writeSource()`，但没有 poll 专属断言。

## 8. 途中撞到的一个真坑（值得记）

第一版表单一律 `.trim()` 所有输入框，于是 `secretRef.prefix = "Bearer "` 被改成 `"Bearer"` ——
**尾随空格是这个字段的语义**（`Authorization: Bearer <token>`）。e2e 的深度比较当场红
（`+ prefix: 'Bearer'` / `- prefix: 'Bearer '`）。修法：`prefix` 与 header 的**值**按原样提交，
只 trim 标识符类（`url` / 凭据名 / 请求头名 / 指针）。这条正好说明验收 2 那条断言不只是防「整份清空」，
它也抓住「悄悄地改写一个字段」。
