> **历史快照**：本文件是本次交付的记录，不随代码演进；行为以代码、测试与 `api/` 为准。

<!-- snapshot-doc: 2026-11-22 #537 交付时的定格数字（make ci / make e2e-affected 的真实输出），复算命令见各节 -->
# 把 `kind: poll` 在界面上露出来（#537）交付报告

> 分支 `pi-subagent/hw-nj/anotify-537-poll-ui-67387499` · base `main@ab0a4fc9` · 前序 #532（后端）/ #533
> issue 的「四件要做的事」与「六条验收」在本文件 §2 / §3 逐条对上号。

一句话：**后端能拉，界面却还看不见** —— 这次只补界面：种类徽章出中文、源详情页多一块
「拉取」（配置摘要 + 五个状态事实）、从 YAML 新建的弹层里给一份能直接提交的 poll 示例，
并在 360px 下不横滚。后端、API 字段、页面骨架一行没动。

---

## 1. 四件落点

| # | 要做 | 落点 | 判据 |
| --- | --- | --- | --- |
| 1 | kind 徽章本地化 | `web/partials.js` 新增 `Anotify.sourceKindLabel(kind)`（四份 locale 的 `sources.kind.<kind>` 唯一真源，**表里没有的 kind 回落原值**，空值给 `—`）；`web-src/pages/sources.html` 与 `source-detail.html` 的徽章都改走它；四份 locale 补 `sources.kind.poll`（`cron 定时器` / `webhook` / `manual 手动` / `poll 拉取`，en/ja/es 各自对齐） | `check-i18n-keys` 键集一致；`POLL-03` 在 zh-CN 读「拉取」、en 读「Poll」；`POLL-09` 未知 kind 原样显示 |
| 2 | 详情页「拉取」区块（仅 poll） | `web-src/pages/source-detail.html`：`pollSection(m)` 只在 `m.kind === "poll"` 时 `body.append`；配置摘要 = `url`（可复制）/ 排期（表达式 + 时区）/ `timeoutMs` / `maxItems` / envelope 四指针（缺省维度显示默认语义：整份响应 / 源名 / 不去重 / 整个条目）；状态 = `lastPollAt` / `nextFireAt` / `consecutiveFailures` / `skippedOverlaps` / `cursor`（截断 + `title` 全文）/ `lastError`（多行折叠块，取不到失败事实时说「没有失败记录」） | `POLL-04`~`POLL-06`、`POLL-08` |
| 3 | YAML 弹层里的 poll 示例 | `web-src/pages/sources.html`：`#src-yaml-sample` 折叠块内是整份可提交的 poll 文档（含 `schedule` 与 `targets: []`），`#src-yaml-sample-fill` 一键抄进粘贴框；一句说明（排期与定时器同语法、目标留空合法）走 locale `sources.v4.yaml_create.poll_hint` | `POLL-01`（抄进去的字节 = 示例正文本身） |
| 4 | 360px 不横滚 | 新区块只用既有网格（`.sd-kv` 的 `minmax(min(200px,100%),1fr)`）+ `overflow-wrap:anywhere`；长 `lastError` 落在 `.sd-pre`（`white-space:pre-wrap`）；游标行 `.sd-cursor` 可折行 | `POLL-07` / `POLL-08` 的 360px 几何断言（元素 `right` 全部 ≤ 362）与 `clean()` |

**一处顺手的收口**：`lastPollAt` / `consecutiveFailures` 原先在「基本信息」里常驻两行，
对 cron/webhook/manual 源永远显示 `—` / `0`（它们没有这些事实）。这次把这两行搬进
poll 专属区块 —— 同一页不摆两份同一事实，也不给别的 kind 留一行永真的空话。
键沿用 `source_detail.last_poll` / `source_detail.consecutive_failures`，没有新增死词条。

**为什么 `cursor` 要截断**：它是上一次成功响应里原样回传的 `ETag`/`Last-Modified`，
长度不受控（实测 GitHub 形态 60+ 字符）。截到 48 字符 + `…`，`title` 保留全文：
布局不被一串游标撑破，需要核对时鼠标停一下就能读完。

## 2. 没动的东西（约束）

- **后端与 API 字段零改动**：`git diff --stat internal/ api/` 为空。区块读的字段全部来自既有
  `SourceSpec.config`（`url` / `schedule` / `timeoutMs` / `maxItems` / `envelope`）与 `SourceStatus`
  （`lastPollAt` / `nextFireAt` / `consecutiveFailures` / `skippedOverlaps` / `cursor` / `lastError`）。
- **页面骨架不动**：仍是 `.sd-sec` 区块序列（基本信息 → 定义与字段 → 拉取 → 目标能力 → 近期回执 → 原始 YAML），
  没有改类名、没有重排既有区块。
- **读投影仍走白名单**：`modelSource` 只逐字段取，`config.headers` 与 `config.secretRef` **不进模型**
  （headers 可能夹带凭据；secretRef 只是引用，本轮界面不需要）。
- 「立即拉取一次」按钮按 issue 的「不做」留白（要新端点，另开）。

## 3. 六条验收实测

| # | 验收 | 实测 |
| --- | --- | --- |
| 1 | 从 YAML 建 poll 源 → 详情页出现「拉取」区块、五个状态字段都在；未跑过显示「尚未轮询」；徽章显示「拉取」 | `POLL-02`（201，POST 体只有 `{yaml}`）→ `POLL-04`/`POLL-05`/`POLL-06`（13 个 `.sd-pair` 逐个断言）→ `POLL-03` 徽章「拉取」 |
| 2 | 未知 kind 徽章显示原值，不出现空白或 `undefined` | `POLL-09`：页内 fetch 把该源的 `spec.kind` 改写成 `smtp-relay` 后重渲染列表 —— 徽章文本 `smtp-relay`，页面上没有 `undefined`/`null`；单测 `sourceKindLabel` 钉住「有词条用词条 / 无词条回落原值 / 空值给 —」 |
| 3 | 四语言键集一致、新增文案不出现实现术语 | `make ci` 里的 `check-i18n-keys`（4 份 locale 键集一致，2172 个引用点全部有词条）与 `check-ui-copy`（4 份 locale 9592 条值 + 22 个页面 241 个文本节点，0 命中） |
| 4 | 变异 ①删 poll 分支 → 验收 1 红；②「尚未轮询」改回 `—` → 空状态断言红 | 见 §4 |
| 5 | `make ci` exit 0；`make e2e-affected` 绿；known-env 保持空；注释密度棘轮不上调基线 | 见 §5 |
| 6 | 360px 不横滚（长 url / lastError 能换行） | `POLL-07`（新区块每个元素 `right ≤ 362`，`clean()` 判定文档无横滚）+ `POLL-08`（多行长错误在 360px 下 `scrollWidth ≤ clientWidth` 且整体不越界） |

## 4. 变异（先红后绿，都是真跑）

两条变异都落在产品代码上，改完重新 `make fe` + 重新构建 embed 二进制再跑同一条 e2e 用例：

| 变异 | 改法 | 结果 |
| --- | --- | --- |
| ① 删掉 poll 分支 | `web-src/pages/source-detail.html` 删掉 `if (m.kind === "poll") body.append(pollSection(m));` | **红**：停在 `POLL-04`，`locator.waitFor: Timeout 15000ms exceeded`（等不到 `[data-sec="poll"]`）——即验收 1 先红 |
| ② 「尚未轮询」改回 `—` | `const never = t("source_detail.poll_never", "尚未轮询")` → `const never = "—"` | **红**：停在 `POLL-06`，`从未轮询过必须说「尚未轮询」，不是 —` |

复原后同一条用例 11 个断言全绿（`1 通过 / 0 失败`）。

## 5. 门禁数字

- `make ci`（`timeout 1200`）：**exit 0**。含 `check-i18n-keys` / `check-ui-copy` / `check-classes` /
  `check-test-discipline`（`swallow-catch 38/38`、`waitForTimeout 21/21` 均未上调）/ `check-comment-density`
  （注释行 43330 / 基线 43330，75 个目录全部与基线一致 —— **基线一个字没改**）/ `go test ./...`。
- `make e2e-affected`（`E2E_CONCURRENCY=2`，`timeout 2400`）：改动面选了 **16 套 / 30 套**
  （`frontend` / `i18n` / `i18n_coverage` / `ui_shift` 四套矩阵 + 12 套 Chrome 类），
  **16/16 全绿 · 断言 1071 通过 / 0 失败**；新增的 `source-poll` 用例贡献 11 个断言
  （`frontend` 套件 44 → 其中 11 条是这个用例）。踩到过一次 `vars_ui` 的
  `waitForResponse` 15s 超时（并发 2 下加载抖动）；单跑该套件 74/74 全过、整轮重跑也全绿，
  因此没有往 `known-env` 里加任何条目。
- known-env：`scripts/e2e/known-env-failures.json` 未改动，保持空。
- 新增 e2e 用例：`source-poll`（注册在 `scripts/e2e/fixtures/f3/f3-cases.mjs`，实现
  `scripts/e2e/fixtures/f3/f3-source-poll.mjs`），11 个断言。

实测回执（本次交付）：

<!-- e2e-receipt: 下面这行起是真实终端输出，勿改 -->
```
$ E2E_CONCURRENCY=2 make e2e-affected
  e2e 套件: 跑 16 套 / 跳过 14 套（共 30 套）
  ✅ frontend           44通过/0失败  169.6s  mode=real
  ✅ i18n               33通过/0失败   53.7s  mode=explicit-mock
  ✅ i18n_coverage      44通过/0失败   70.4s  mode=explicit-mock
  ✅ ui_shift           87通过/0失败  151.5s  mode=explicit-mock
  ✅ vars_ui            74通过/0失败    4.1s  mode=real
  ✅ p2_guards          53通过/0失败    9.9s  mode=real
  ✅ events_paging     179通过/0失败   15.5s  mode=real
  ✅ cli_auth          133通过/0失败   25.7s  mode=real
  ✅ passkey_enroll     85通过/0失败   15.0s  mode=real
  ✅ lang_hint          86通过/0失败   20.3s  mode=real
  ✅ admin_flow         52通过/0失败    3.7s  mode=real
  ✅ deeplink           50通过/0失败    3.0s  mode=real
  ✅ auth_flow          49通过/0失败    3.7s  mode=real
  ✅ recovery_next      16通过/0失败    5.3s  mode=real
  ✅ ux_regression_19_23 8通过/0失败    9.9s  mode=explicit-mock
  ✅ 另一套登录流套件   78通过/0失败    7.0s  mode=real

✅ E2E 全绿：16/16 个请求套件均达标（断言 1071 通过 / 0 失败，authority 清单覆盖 16 个）
  ✅ 临时目录零残留（隔离 TMPDIR 内本次新增 = 0）

$ make ci
CI EXIT=0

```

## 6. 发现的产品 bug（本轮不修，需另开）

- **编辑源对 poll 源必然 400**。共享编辑器 `web/source-editor.js` 的 kind `<select>` 只有
  `cron` / `webhook` / `manual`（`web/source-editor.js:73-77`）；打开 poll 源时
  `$("f-kind").disabled = true` 但 `value` 落成空串，而 `writeSource()` 无条件把 `spec.kind`
  写进 PATCH（`web/source-editor.js:409,443`）→ 服务端 `source_kind_immutable`；
  即使 kind 对了，`writeSource()` 也只重建 cron/webhook 那套 config，poll 的
  `url` / `schedule` / `timeoutMs` / `maxItems` / `envelope` 会被整份丢掉 → `invalid_source_config`。
  这是 #533（后端上线）之后 poll 源一出现就存在的缺陷，**不是本轮引入**；修它需要一张
  「poll 传输配置表单」，与 issue 里「不做：重做源页信息架构」同属另一件事，因此本轮只报不改。
- **URL 可能带凭据**：`CheckPollURL` 只拒 userinfo，不禁止 query 里的凭据；
  详情页把 `url` 全文显示并给了一键复制。不是新增的泄漏面（原始 YAML 块本来就有），
  但「复制」让它少了一次点击 —— 若要收紧，应在写入期拒绝可疑 query，而不是藏起界面。

## 7. 遗留风险

- **未覆盖的界面分支**：`cursor` 与 `lastError` 的「有值」形态由 e2e 用页内 fetch 改写响应验证
  （`POLL-08`），真实拉到脏数据的路径没有端到端造（那要起一个真 feed 服务，属 #532 的
  `internal/source` 单测面）。
- **`#536` 并行面**：本轮只碰 `sources.html` / `source-detail.html` / `web/partials.js` / 四份 locale；
  `#536` 碰 playbook 页与 locale。locale 冲突按并集解（新增键互不相邻）。
- **`envelope` 的默认语义文案**是界面复述服务端语义（整份响应 / 源名 / 不去重 / 整个条目）：
  若哪天 `internal/source/poll_items.go` 的缺省规则变了，这四个词条要跟着改（目前没有守卫钉住）。
