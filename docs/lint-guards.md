# 守卫（lint）总账 · 从返工事故反推的机械门禁

snapshot-doc: 2026-11-16 体检快照（本文件里的数字是那次的定格；每节的「复算命令」才是事实源）

> 本文是「最近犯错 → 变成什么 lint」的冻结契约与 backlog。
> 事实来源：`git log -400` 返工 commit 逐条 diff、`.pi-orchestrator/UI-EPIC-369/*`、
> `UI-REVIEW-2026-11-13.md`、`UI-AUDIT-v3.md`、issue #338/#355/#356/#357/#362/#369~#385。

## 0. 结论先行：反复犯错的不是「不小心」，而是五类结构缝隙

### 0.1 最优先发现：CI 门禁其实一直是红的

`make ci` 在干净 checkout 上必红：`scripts/check-wire-fields.mjs` 依赖 npm 包 `yaml`
（只是 `tailwindcss → postcss-load-config` 的传递依赖，`package.json` 里没有），
而 `.github/workflows/ci.yml` 从不装 npm 依赖 ⇒ `make fe` 直接 `ERR_MODULE_NOT_FOUND`。

```bash
gh run view 43699298020 --log-failed | grep -A3 ERR_MODULE_NOT_FOUND
# → make[1]: *** [Makefile:54: check-wire-fields] Error 1      （每条 PR 都这样）
gh run list --limit 8   # main 上 ci 连续 failure，docker 才是 success
```

这就是「打回率 77%」能长期存在的土壤：PR 门禁红了没人看，问题全靠 reviewer 人肉抓。
修法（已落地）：`ci.yml` 加一步 `npm install --no-save yaml@2`（只装这一个纯 JS 小包，不写 lock）。

### 0.2 第二层：本机 `make ci` 也有一条「合法掩体」——已挖掉

`internal/sitegen TestDocsPageRendersFiveSpecFixturesFourLanguages` 长期报
`want 16 tag groups, got 0`，并被登记成「main 既有确定性失败」，于是没人查。真相是**测试脆弱性，不是产品 bug**：

- `3f3e041b`（docs 页 UI 改版）给容器加了一个 Tailwind 类 → `class="doc-ep-table table-wrap"`；
- 断言写的是**属性整串相等** `strings.Count(page, "class=\"doc-ep-table\"")` → 计数恒 0；
- 更糟的是同一处的**反向断言**（「空态不该出现」）也用了 `strings.Contains` → 属性一变它**永远成立**，静默漏检。

修法：按 **class token** 计数（`countClassTokens`），正反向共用同一判据，数量断言（35 端点 / 16 分组 / 2 行）一条不减。
修完 `make ci` 在本机**恢复全绿**（2026-11-16 实测 `MAKE_CI_EXIT=0`）。

教训（已进 backlog #12）：**「已知红」是合法掩体**——一旦某条恒红被写进台账，它既掩盖新产品 bug，
又让「失败集合与 base 逐条相同」这条判据失去基准。台账条目必须自己也会过期。


| 缝隙 | 事故次数 | 代表证据 | 为什么人肉 review 拦不住 |
|---|---|---|---|
| ① 门禁测错了对象（假绿） | ≥6 | 陈旧 dist 假绿 `22d14d47`/`1256350f`；扫骨架屏 `84dee2d5`；空转扫登录页 `18f32d82`；单次读数当几何事实 `44184536` | 「绿」与「没检查」同形 |
| ② 门禁自己有口子 | ≥5 | `-run` 匹配不到即 exit 0（`check_contract.py:384` 事后补）；`check-wire-fields.mjs:62` YAML 解析失败 `continue`；`openapi_parity_test.go:4329` 只对账 `in: query`；`pageerror` 不进退出码 `37bbef09`；`make ci 2>&1 | tail` 的退出码来自 `tail`（实测把 `Error 2` 记成 exit 0） | 口子本身不会报错 |
| ③ 静默跳过 / 静默豁免 | ≥5 | `t.Skip` on dist（`static_test.go:120/166/1292`）；`waitForAppReady` 超时只 warn（`harness.mjs:335`）；`KNOWN_DEAD` 9 条无期限（`check-wire-fields.mjs:93`） | skip 永远是绿的 |
| ④ 结构件被「写了等于没写」 | ≥8 | CSS 丢两个 `}` 整段被吞（`fdba2013`）；safe-area 被后置规则覆盖 + 选择器从未命中（`11120c23`）；默认态只写在 `@media` 里（`13addbdf`） | 静态看不出来，只能靠真机几何 |
| ⑤ 同一事实两处各自演化 | ≥12 | 打码 3 个 walker 分叉（9 个 fix，#356）；前端读服务端不发字段（`ef0a773b` 一次 6 个）；契约文本把修好的 bug 诱导回来（`189a57f3`） | 单点看都对，缝在中间 |

设计原则（照抄本仓已验证的范式）：**纯函数判据 + `--self-test` 红/绿双侧自证 + 打印检查数量（0 项也算红）+ 例外表过期即红 + 退出码 0/1/2**。
恒红的门禁等于没有门禁（`check_contract.py:1-30` 的头注释教训），所有新守卫必须在合入时**真的是绿的**。

## 1. 本批落地（5 条）

| 编号 | 文件 | 抓什么事故 | 判据要点 | 接在哪 |
|---|---|---|---|---|
| L1 | `scripts/check-css.mjs` | `fdba2013`（丢 `}` → 整段 ui.css 被吞成后代选择器，`check-classes` 结构性失明） | 去注释/字符串后括号平衡；**禁止 CSS 嵌套**（规则块内再出现规则块 ⇒ 疑似丢失右括号）；顶层不得裸声明；规则数下限 | `make fe` |
| L2 | `scripts/check-i18n-keys.mjs` | `8e253d4e`（补四语言键）、`7dc2c260`（en/es 中文残留）；sitegen 缺键只 `WARN`（`sitegen.go:356,370`） | 模板里引用的 key 必须在 4 份 locale 里都存在；4 份 locale 的 key 集合互等；解析必须 fail-loud（不认识的 YAML 语法直接报错，不许静默跳过） | `make fe` |
| L3 | `scripts/check-test-discipline.mjs` | `edc71233`/`44184536`（`waitForTimeout` 当就绪）、`.catch(()=>{})` 吞断言、`t.Skip` 静默、裸标签定位器 + `.first()` 掩盖双命中 | **棘轮**：现值为基线，超基线红、低于基线也红（必须同步下调）；每项打印 count/baseline | `make ci` |
| L4 | `internal/store/orderby_tiebreak_test.go` | `1461f7e4`（`usr_`+`crypto/rand` 破平局 → `TestAdmin_ListUsers` 42.5% 随机红） | store 里每条 `ORDER BY` 的末键必须唯一（`rowid` / 主键 / UNIQUE 索引覆盖，含部分唯一索引的 `WHERE deleted_at IS NULL` 谓词）；白名单条目过期即红；7 条变异子测试自证判据有牙 | `make test`（→ `make ci`） |
| L5 | `scripts/check-annotations.mjs` | `189a57f3`（文档把 bug 诱导回来）、`e16c2536`（契约缺口写成注释）、`30856f69`（过期计数注释）、`DEVELOPMENT.md:720`（把未接线的脚本称「回归测试」） | ①文档/注释里的 `scripts/*`、`make <target>`、`npm run <x>` 引用必须解析（含 `docs/v4/e2e-inventory.md` 指向已删套件这类真事实）；②`@guard/@decision` 锚点必须解析到存在的规则码/决策号；③约束性注释无锚点数量、计数型注释无 `@count` 数量 —— **棘轮，只许降** | `make ci` |

### 1.1 本批顺带发现（真 bug，已修/待裁）

- **缺 i18n 键**（L2 首次运行即抓到，2 处）：
  - `web-src/pages/sources.html:946` 的 `t("save_failed")` 经页面内 `t = key => A.t("sources.v4."+key)` 前缀解析成 `sources.v4.save_failed`，**四份 locale 都没有** → toast 直接渲染裸 key；
  - `web/flograph.js:3548` 的 `tt("graph.silk_source", "源")` 四份 locale 都没有 → `tt()` 回退到中文默认值，**ja/es/en 用户看到中文「源」**（正是 `7dc2c260` 同型）。
  - 已各补 4 行翻译（复用同语言既有的 `save_failed` 口径），`make fe` 重新生成通过。
- **12 条 `ORDER BY` 无唯一末键**（已登记进 `orderByKnownGapBudget = 12`，只许降）：
  `admin.go:50 ListUsersWithStats`（**与 1461f7e4 完全同形**，分页版已修、非分页版没修）、`admin.go:288/218/326`、`auth.go:208/355/531/600`、`deployment.go:279`、`devices.go:33/55`、`playbook.go:608`（**WHERE 缺 `deleted_at IS NULL`**，软删行可与活行撞 `(name,version)`，疑似功能 bug）。

### 1.2 L5「注解即外键」首次体检结果（已修）

复算命令：`node scripts/check-annotations.mjs --report`（盘点模式，不判红）。首次全量体检把①坏引用、②过期指令、③谎报已接线这三类全扫了一遍，已全部处置：

- **过期指令**：`internal/version/version.go:16` 注释让人 `go run ./cmd/agent`（该目录已删除）→ 改成 `./cmd/anode`。
- **坏引用**：`cmd/agent`（v3 节点，已删除）/ `make demo-chain`（已退役）/ `scripts/e2e/suites/real_backend.mjs`（已删除）/ `make mountpoint`（把报错原文当成了目标名）—— 逐条改成「已删除/已退役」的历史提及，或改指向现存目标。
- **v3 设计文档**（`docs/design/capability-spec-v3.md`、`docs/design/agent-upgrade.md`）挂了 `frozen-doc:`，
  它们不再参与引用检查；**标记本身也被校验**：哪天引用全部解析了，标记会红（强迫删）。
- **谎报已接线**：`DEVELOPMENT.md:720` 把零调用点的 `scripts/v4-frontend.test.mjs` 称「回归测试」
  → 改成「尚未接线、手工跑、判据是失败集合与 base 逐条相同」，并把旁边的禁裸读 `AnotifyGraphs`
  那条改成带锚点的形式：`@guard scripts/check-graph-access.mjs#no-direct-graph-lib`。

收益就在这一条：**被引用方改规则码/删文件时，注解会立刻红，而不是安静地变成谎**。
棘轮基线写在脚本常量里（`CLAIM_UNBACKED_BASELINE` / `COUNT_UNMARKED_BASELINE`，带实测日期与命令），只许降。

**未接线的既有工具**（本批只登记，不改行为）：`scripts/ui-audit-mobile.mjs`（需站点+浏览器，属合并前手工门禁）、`scripts/v4-frontend.test.mjs`（179 个 `test()`，相对 main 基线红 11 条 —— **带基线红的门禁不许进 CI**，否则重演「恒红即无门禁」）。`scripts/check-contrast.mjs` 已接进 `make fe`。

## 2. Backlog（按性价比排序，未开工）

1. **打码 fail-open 的一致性 property 测试**（#356，9 个 fix）。判据：「`MaskValues` 打成 `***`」⇔「`refHitsWriteOnly` 同判」；随机生成含 `writeOnly`/`additionalProperties`/`prefixItems`/`items`/`type:[object,array]` 的 schema 对撞。一次性覆盖 6 个历史 fix，判据无启发式，误报≈0。
2. **`openapi_parity_test.go:4329` 放开到 `in: header`**（抓 #355：`X-Anotify-Signature` 验签在契约里、实现里不存在）。复用现成 `literals` 判据 + 白名单 + 过期即红。
3. **dist 新鲜度守卫**：`make e2e` 前断言 `internal/server/dist` 含本次 `web-src` 指纹（`run_all.sh:31-43` 至今只在缺失时重建，已三次假绿）。
4. **白名单到期机制**：`KNOWN_DEAD`/`vocabWhitelist`/`bareMount405Exceptions` 等所有例外表必须带 `issue#`，缺即红（`vocabulary_test.go:303` 已有「白名单不许过期」的范本，缺的是统一执行）。
5. **`go test -json` 断言 skip 集合为空**（CI 里 dist 必存在 ⇒ `t.Skip` 集合必须为空）。把「静默跳过」从 review 变成运行期事实。
6. **`-shuffle=on`（每次跑）+ `-count=2`（nightly）**：全仓现在 `-shuffle` 0 处、`-count≥2` 0 处，所有 flake 都是人肉 `-count=40` 事后发现的。
7. **`check-wire-fields.mjs` 扩到 `web-src/pages/*.html`**：现只扫 3 个 js 文件（`:83`），页面内联 179 处 `spec./st./status.` 无覆盖。**前置**：先把页面里的 `st`（局部 state）与 `st`（status）改名区分，否则误报爆炸。
8. **契约文本 ↔ SQL 一致性**（`189a57f3`：`api/auth.yaml` 写「id 破平局」而实现已改 rowid）。近似做法：文档里 backtick 包住的列名必须能在那份资源的 `ORDER BY` 里找到。
9. **CSS 覆盖/命中率的窄规则**：`env(safe-area-inset-*)` 所在属性禁止被无 `env` 的简写覆盖（`11120c23` 形态）；`@media` 档内的规则若声明了元素显示必须同时有档外默认态（`13addbdf` 形态）。全量级联分析误报高，只做这两条窄规则。
10. ~~**`check-wire-fields` 的 npm 依赖**~~ —— 已修（见 §0.1）：`ci.yml` 加 `npm install --no-save yaml@2`。
11. **退出码必须来自被测量的进程**：人工与后台命令同样是「判分口子」的重灾区 ——
    `make ci 2>&1 | tail -25` 的 `$?` 是 `tail` 的，实测把一次 `make: *** Error 2` 记成 exit 0
    （同族：`44184536` 删 `pass-warning`、`37bbef09` 里 `pageerror` 不进退出码、`check_contract.py:384` 的
    `go test -run` 匹配不到即 0）。规矩：跑门禁/后台任务一律 `set -o pipefail`，或
    `cmd > log 2>&1; echo EXIT=$?`；可机械化的一面是把 `scripts/**` 与文档代码块里
    「`| tail` / `| head` 且无 pipefail」的形态扫出来（`check-annotations.mjs` 的 `REF` 已能定位代码块，可复用）。
12. **「已知红」台账要自证仍然红**：把 CONTRACT 一类的基线红清单变成可执行断言 ——
    逐条重跑登记的命令，**某条其实已经绿了就红**（清单过期，必须删条目），同时新增红必须**不在**清单里。
    这样「基线红」就不会同时冒充两类掩体（掩盖新 bug / 误导判据）。范本：`openapi_parity_test.go` 的
    `bareMount405Exceptions` 过期即红、`vocabulary_test.go:303` 的「白名单不许过期」。

## 3. 注解即外键（为什么不要用「注释提醒」防呆）

本仓已经吃过太多次「注释当契约」的亏：

| 注释形式的承诺 | 实际发生了什么 |
|---|---|
| `resource_capability.go:716-719` 注释写「本域不查节点上报，所以不声称 stale（宁可少报，不伪造）」 | **契约缺口被写成了注释**，`capabilityNodeStale` 常量全文只出现 1 次（声明本身），stale 永不可达（`e16c2536` / #362） |
| `api/auth.yaml:1400` 散文写「id 破平局」 | 实现已改 `rowid`，**契约文本把修好的 bug 诱导回来**（`189a57f3`） |
| `#372` 文件头写「9 个宽度值 / 33 条内联」，残留 `767` 注释 | 实为 13/38，**反向诱导回退**（`30856f69`） |
| `web/ui.css` 注释称「已给控件让位」 | 实际被后置规则覆盖，390 + inset 34 仍是 8px（`11120c23`） |
| `DEVELOPMENT.md:720` 把 `v4-frontend.test.mjs` 称作「回归测试」 | 它**零调用点、且带 11 条基线红**，任何依赖它的保护都是软的 |

原因很直接：**注释没有执行者**。它的真伪无人复算，于是随时间只能变错；而 agent / 新人对注释的信任度
高于对代码的信任度，一份过期注释的杀伤力大于没有注释。

### 3.1 规则：注释只能回答「为什么」，不能回答「现在是什么」

| 类型 | 例子 | 处置 |
|---|---|---|
| 可判定的事实/约束 | 「不得裸读 `AnotifyGraphs`」「缺键会回退」「有 13 个宽度值」 | **改成可执行断言**（lint/test），注释里只留锚点 `@guard <脚本>#<规则码>` |
| 不可判定的意图 | 「为什么宁可少报也不伪造 stale」 | 保留，但必须挂**决策编号** `@decision D7`，编号由 lint 校存在性 |
| 历史/废止物 | v3 设计文档里提到的已删脚本 | 文件头打 `frozen-doc: 理由`，**该标记自己也由 lint 校验仍需要**（引用全部解析成功即红，强迫删标记） |

### 3.2 锚点格式（写进代码/文档即可，lint 校验）

```
@guard    scripts/check-css.mjs#NESTED_RULE     ← 脚本必须存在，且真的会抛这个规则码
@decision D7                                     ← 决策编号必须能在契约文档里找到
@count    node scripts/check-contrast.mjs        ← 计数型断言必须附可重跑的命令
frozen-doc: v3 设计快照，不再随代码演进           ← 仅 markdown 前 40 行，且引用已全部解析时必红
```

这样「注解」就从自由文本降级为**外键**：外键的引用完整性由 lint 检查，
被引用的脚本改规则名/被删时，锚点立刻红——**而不是安静地变成谎**。
对应的守卫见 §1 的 `scripts/check-annotations.mjs`。

## 4. 明确不做（不能机械化的）

- 作用域写宽导致桌面逐像素回归（`70ef5776`）：只能视觉基线，仓库无入库基线图。
- 有效命中被祖先 `overflow` 裁掉（`37fea2c4`/`c12bec80`）：`ui-audit-mobile.mjs` 已做 clip-aware，但必须真浏览器跑 ⇒ 合并前手工。
- 需求裁决反转（`9f0154c1`→`b6e3417c`→`e620272e`）：唯一止损是「裁决写进契约 + 显式标注覆盖了哪条旧决定」。
- 状态机 intent 由 reason 反推（#348/#349）：可做弱规则（派生函数必须收 intent 而非自由文本），实质靠判别性用例矩阵。
