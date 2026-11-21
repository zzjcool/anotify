# 守卫（lint）总账 · 从返工事故反推的机械门禁

snapshot-doc: 2026-11-16 体检快照（本文件里的数字是那次的定格；每节的「复算命令」才是事实源）

> 本文是「最近犯错 → 变成什么 lint」的冻结契约与 backlog。
> 事实来源：`git log -400` 返工 commit 逐条 diff、`docs/history/UI-EPIC-369/`（Epic #369 的交付契约与逐轮报告）、
> `docs/history/UI-REVIEW-2026-11-13.md`、`docs/history/UI-AUDIT-v3.md`、issue #338/#355/#356/#357/#362/#369~#385。

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

## 1. 本批落地（10 条）

| 编号 | 文件 | 抓什么事故 | 判据要点 | 接在哪 |
|---|---|---|---|---|
| L1 | `scripts/check-css.mjs` | `fdba2013`（丢 `}` → 整段 ui.css 被吞成后代选择器，`check-classes` 结构性失明） | 去注释/字符串后括号平衡；**禁止 CSS 嵌套**（规则块内再出现规则块 ⇒ 疑似丢失右括号）；顶层不得裸声明；规则数下限 | `make fe` |
| L2 | `scripts/check-i18n-keys.mjs` | `8e253d4e`（补四语言键）、`7dc2c260`（en/es 中文残留）；sitegen 缺键只 `WARN`（`sitegen.go:356,370`） | 模板里引用的 key 必须在 4 份 locale 里都存在；4 份 locale 的 key 集合互等；解析必须 fail-loud（不认识的 YAML 语法直接报错，不许静默跳过） | `make fe` |
| L3 | `scripts/check-test-discipline.mjs` | `edc71233`/`44184536`（`waitForTimeout` 当就绪）、`.catch(()=>{})` 吞断言、`t.Skip` 静默、裸标签定位器 + `.first()` 掩盖双命中 | **棘轮**：现值为基线，超基线红、低于基线也红（必须同步下调）；每项打印 count/baseline | `make ci` |
| L4 | `internal/store/orderby_tiebreak_test.go` | `1461f7e4`（`usr_`+`crypto/rand` 破平局 → `TestAdmin_ListUsers` 42.5% 随机红） | store 里每条 `ORDER BY` 的末键必须唯一（`rowid` / 主键 / UNIQUE 索引覆盖，含部分唯一索引的 `WHERE deleted_at IS NULL` 谓词）；白名单条目过期即红；7 条变异子测试自证判据有牙 | `make test`（→ `make ci`） |
| L5 | `scripts/check-annotations.mjs` | `189a57f3`（文档把 bug 诱导回来）、`e16c2536`（契约缺口写成注释）、`30856f69`（过期计数注释）、`DEVELOPMENT.md:720`（把未接线的脚本称「回归测试」） | ①文档/注释里的 `scripts/*`、`make <target>`、`npm run <x>` 引用必须解析（含 `docs/v4/e2e-inventory.md` 指向已删套件这类真事实）；②`@guard/@decision` 锚点必须解析到存在的规则码/决策号；③约束性注释无锚点数量、计数型注释无 `@count` 数量 —— **棘轮，只许降**；④反假绿下限 = 扫描面覆盖（`git ls-files` 里落在扫描面的文件必须全被读到，漏读即红）+ 解析器存活（注释/引用/锚点不得归零）—— 2026-11-21 起不再写死「仓库存量」型下限（#495 B 前置，见 `docs/reports/doc-density-b.md`） | `make ci` |

| L6 | `scripts/check-secrets.mjs` | GitHub 上的 `GitGuardian Security Checks` 是**服务端 App**：推上去之后才扫 PR diff，本地 `git commit` 时无感。本仓的秘密面又很具体（`install.json`/`anode.yaml` 里的令牌、VAPID 私钥、`docs/` 里的示例命令） | PEM 私钥块 / 供应商形状（`ghp_`、`AKIA`、`sk-`…）/ 本仓令牌形状（`VAPID_PRIVATE`、`token:` + ≥32 字符字面量、`Bearer`）/ 被跟踪的 `.env`、`*.pem`、`id_*`。**不做熵检测**（夹具里全是假令牌）；`ALLOWLIST` 每条带理由且过期即红；`--staged` 供 pre-commit。**首次提交就自证了一次**：默认模式只扫**被跟踪**文件，于是它把自己的 self-test 夹具当成泄漏——已把夹具改成运行时拼接（源码里不留秘密形状）。这与 L5 那个「未跟踪文档漏扫」是同一类不对称 | `make ci` + `make hooks` |
| L7 | `Makefile` 的 `lint-go` | Go 侧原先只有 `gofmt` + `go vet`；AGENTS.md §3 写的「包注释」「`%w` 包装」两条纪律**无机检**（实测 `%v` 包 err 为 0 处、3 个包缺包注释、staticcheck SA 类 5 条） | gofmt + 包注释（`go list -f '{{if not .Doc}}'`）+ vet + `staticcheck -checks=SA*,U1000`（正确性类 + 未使用符号；`U1000` 已清零后再开，见 backlog #14；`S*`/`ST*` 噪声大仍另立项） | `make ci` |
| L8 | `scripts/dist-stamp.mjs` | 陈旧 dist 假绿 `22d14d47` / `1256350f` / 一次波末假绿（`run_all.sh:31-43` 只在 dist 缺失或为空时才重建 ⇒ 改了 `web-src/` 却忘了 `make fe` 时，e2e 验的是上一轮的前端产物） | 对输入面（`web-src/**` + `api/*.yaml` + `web/` 的 git 源文件，**不含** `internal/server/dist` 自己）逐个 sha256 → `dist/.source-stamp`；`--check` 重算并逐条列出变动/新增/删除（路径 + 旧前 8 位 → 新前 8 位）；stamp 缺失、输入面解析为空、stamp 字段与 digests 不自洽一律判红；二进制照算（字体/图标变 dist 就变），超上限的大文件跳过但打印计数（不静默） | `make fe`（写）+ `make check-dist-fresh` / `scripts/e2e/run_all.sh` 前置（验） |
| L9 | `scripts/check-ui-copy.mjs` | `9ec8afff`（PR #447）节点页挂着「命名空间与系统配置不在这页…已迁到…」，用户看到直接问「搞笑吧？你没有审核出来吗？」——但这类文案不只一处，而且**旧门禁完全没有抓它的面**：`check-i18n-keys` 只验 key 在不在、e2e 只验页面 200，于是任何旁白都能绿到生产。`#458` 补的第二个面：散文也会直接写在页面模板里（「写给维护者与排障者」当时就在 docs 页的 `<p>` 里），只扫 locale = 漏一半 | ①扫 `web-src/locales/*.yaml` 的**值**；②扫 `web-src/pages/*.html` 的**文本节点**——排除 HTML 注释、`<script>`/`<style>` 内容、标签（属性名/属性值/类名）、`{{…}}` 表达式，以及没被 layout `{{block}}` 接进渲染树的 define 体（sitegen 只执行 `__layout__`）。5 类高危特征 = 迁移叙述 / 对照内部实现（工单号、`v4`、`docs/`、构建命令）/ 对开发者喊话 / 历史形态对照 / TODO。词表**窄而准**：宽带扫定稿前在现有 locale 上实测命中 0 条；被故意排除的误报词（`make test`、`移行`、`旧 `、`XXX`）逐条记在脚本头注释里。行内豁免 `# ui-copy-ok: <理由>`（locale 值）/ `<!-- ui-copy-ok: <理由> -->`（页面），理由必填。反假绿：locale 文件/值、页面/文本节点四个下限（页面扫到 0 条不许算绿）；`--self-test` 20 红 / 13 绿 / 5 豁免 + 页面 6 红 / 7 绿 / 4 豁免（绿样例钉住抽到的节点数）+ 反向证明 | `make fe` + `make check-guard-selftest` |
| L10 | `scripts/check-doc-endpoints.mjs` | #457：接入文档教**已删除的端点** —— `docs` 页「接入 Agent」步骤与 `events.empty` 文案都写 `POST /v1/notify` / `POST /v1/events`（v4 入口是 `POST /v1/namespaces/{namespace}/events`，旧扁平路由的下线由 `internal/server/api_integration_test.go` 钉住），照文档打过去必 404。文档里的 URL 没有执行者复算，所以只能随时间变错 | 事实源是 `api/*.yaml` 的 `paths:` 键；扫面 = sitegen 的输入面（`web-src/`）+ 两份 README；文档里的每个 `/v1/<path>` 必须命中一条 spec 路径（去 query / 行尾标点，`{param}` 与 `:param` 按段比对，以斜杠结尾的基路径另有前缀规则）。历史对照走「同一行标注已废弃」或 `<!-- docs-endpoints:historical -->` 整段标记，**只对引用不存在生效**；`<script>` 与 HTML 注释先遮罩（那是客户端代码，不是教学文案）。反假绿：spec 路径 / 扫描文件 / 引用数三条下限，任一归零 ⇒ 退出码 2；`paths:` 块内出现无法识别的行 ⇒ 退出码 2（不静默跳过）。`--self-test` 含变异用例：写一个不存在的端点必须判红 | `make ci` |
| L11 | `scripts/check-config-writes.mjs` | #476：`/config` 降级成**读投影**（YAML 是唯一编辑面）之后，节点页与命名空间页仍各留着一条 `PUT …/config` 的第二写路径（清理前实测：`node.html:550` 的 `write(path + '/config', body)`、`namespace.html:156` 的 `write(nsPath(saveScope), …)`）——YAML 那一面看不到表单改的值，两个入口各自 200，只有用户看到的值对不上（§0 第 ⑤ 类缝隙） | 扫 `web-src/pages/*.html` 的内联 `<script>`（`<script src>` 跳过），用状态机剥注释（字符串 / 模板 / 正则里的 `//` 不是注释，否则 `'https://…/config'` 被吞成假绿）；「config 路径源」= 静态尾部以 `/config` 结尾的字面量，或 `const\|let\|var NAME =` 初始化器里含它的名字（含 `NAME = ns => …/config` 这种 helper 形态）；写调用点 = 页面自定义 `write(...)`（显式第 3 参 `'GET'` 除外）/ `….api(` 且 `method:` 字面量是写方法 / `fetch(` 同判；第一个实参括号配平切出后，**含 `/config` 字面量**或**等于 / 以 `NAME(` / `NAME +` 引用路径源**即红。输出 `文件:行 → 调用原文 → 命中原因`，退出码 0 绿 / 1 违规 / 2 解析或用法错误。**唯一例外面**：明写 `…/system/config` 的系统保留期单例不判红（system YAML 只读 ⇒ 它是保留期唯一写入口，不在 #476 范围），且例外只认后缀精确匹配，其余 `/config` 一律 fail-closed | `make fe` + `make check-guard-selftest` |
| L12 | `scripts/check-comment-density.mjs` | #495：注释被当成第二份真相 —— `check-annotations`（L5）只保证「留下的注释不说谎」，没有任何东西保证「这些注释不该存在」；Go 注释行 24,947 而代码行 145,191，`docs/v4/` 的清单快照早已指向被删的套件。判据：注释只在「代码无法自述」处（非显然取舍、外部约束、危险操作）；文档只留操作手册，设计与契约定居代码与测试 | **棘轮（超与降都算不一致）**：①每个目录的注释行冻结；②仓库注释行总量冻结；③单文件注释行 ≤ max(12, 代码行 × 0.5)，当前已超限的 58 个文件登记预算，登记表过期即红；④`docs/` 面：`design`/`specs`/`plans` 禁止重建，待收缩目录（`contracts`/`v4`/`research`/`perf`）的文件数只许下调。`--update` 只降不入表（抬高直接拒），`--init` 仅首次可建。逐目录判据与 B 阶段计划见 [reports/comment-and-doc-ratchet.md](reports/comment-and-doc-ratchet.md) @count node scripts/check-comment-density.mjs --report | `make ci` |

### 1.1 本批顺带发现（真 bug，已修/待裁）

- **缺 i18n 键**（L2 首次运行即抓到，2 处）：
  - `web-src/pages/sources.html:946` 的 `t("save_failed")` 经页面内 `t = key => A.t("sources.v4."+key)` 前缀解析成 `sources.v4.save_failed`，**四份 locale 都没有** → toast 直接渲染裸 key；
  - `web/flograph.js:3548` 的 `tt("graph.silk_source", "源")` 四份 locale 都没有 → `tt()` 回退到中文默认值，**ja/es/en 用户看到中文「源」**（正是 `7dc2c260` 同型）。
  - 已各补 4 行翻译（复用同语言既有的 `save_failed` 口径），`make fe` 重新生成通过。
- **两处「测试退化」被 staticcheck 抓出来**（`SA4010`）：`api_page_test.go` 的 `liveIDs` 与
  `page_test.go` 的 `created` 都是「只 append、从未被读」——测试里建了 ground truth 却没有断言用它，
  于是「条数对」掩盖了「条不对」。已补回断言（不删变量），判据见 commit body。
- **11 条 `ORDER BY` 无唯一末键**（已登记进 `orderByKnownGapBudget = 11`，只许降）：
  `admin.go:220/290/328`、`auth.go:208/355/531/600`、`deployment.go:279`、`devices.go:33/55`、`playbook.go:608`（**WHERE 缺 `deleted_at IS NULL`**，软删行可与活行撞 `(name,version)`，疑似功能 bug）。
  其中 `admin.go ListUsersWithStats`（**与 1461f7e4 完全同形**：分页版早修、非分页版漏着）已于
  2026-11-18 补上 `, u.rowid` 并下调预算 12 → 11 —— 守卫当场报「登记项过期」，按纪律一起擦账。

### 1.2 L5「注解即外键」首次体检结果（已修）

复算命令：`node scripts/check-annotations.mjs --report`（盘点模式，不判红）。首次全量体检把①坏引用、②过期指令、③谎报已接线这三类全扫了一遍，已全部处置：

- **过期指令**：`internal/version/version.go:16` 注释让人 `go run ./cmd/agent`（该目录已删除）→ 改成 `./cmd/anode`。
- **坏引用**：`cmd/agent`（v3 节点，已删除）/ `make demo-chain`（已退役）/ `scripts/e2e/suites/real_backend.mjs`（已删除）/ `make mountpoint`（把报错原文当成了目标名）—— 逐条改成「已删除/已退役」的历史提及，或改指向现存目标。
- **v3 设计文档**挂了 `frozen-doc:`（随 2026-11-19 设计文档整套移除，已不存在），
  冻结文档不再参与引用检查；**标记本身也被校验**：哪天引用全部解析了，标记会红（强迫删）。
- **谎报已接线**：`DEVELOPMENT.md:720` 把零调用点的 `scripts/v4-frontend.test.mjs` 称「回归测试」
  → 改成「尚未接线、手工跑、判据是失败集合与 base 逐条相同」，并把旁边的禁裸读 `AnotifyGraphs`
  那条改成带锚点的形式：`@guard scripts/check-graph-access.mjs#no-direct-graph-lib`。

收益就在这一条：**被引用方改规则码/删文件时，注解会立刻红，而不是安静地变成谎**。
棘轮基线写在脚本常量里（`CLAIM_UNBACKED_BASELINE` / `COUNT_UNMARKED_BASELINE`，带实测日期与命令），只许降。

**未接线的既有工具**（本批只登记，不改行为）：`scripts/ui-audit-mobile.mjs`（需站点+浏览器，属合并前手工门禁）、`scripts/v4-frontend.test.mjs`（179 个 `test()`，相对 main 基线红 11 条 —— **带基线红的门禁不许进 CI**，否则重演「恒红即无门禁」）。`scripts/check-contrast.mjs` 已接进 `make fe`。

### 1.3 L11 判据的三个易踩点（#476）

- **为什么必须真解析而不是 grep 一遍字符串**：节点页的读与写在**同一段脚本**里（`A.api(path + '/config')` 读、
  `write(path + '/config', body)` 写），所以 grep `'/config'` 会把读也算成违规；只 grep 写方法又会漏掉
  `write(nsPath(scope), …)` 这种「路径在 helper 里拼」的形态。判据因此分两层：先按**值**找出路径源，
  再只对**写调用点**求值 —— 读调用根本不进这个集合（`A.api(nsPath(x))` / `A.api(nsPath(x), { method: 'GET' })`
  两种读法在自测里都是绿的）。
- **为什么系统保留配置是例外而不是漏判**：`admin.html` 对系统保留期的 `PUT …/system/config` 是管理员单例，
  而它的 system YAML 是**只读**的（`…/system/yaml` 只有 GET）⇒ 那个 PUT 就是保留期的**唯一**写入口
  （服务端 `putSystemConfig`），收掉它等于删功能，且 #476 的范围是节点/命名空间 vars 的投影。
  例外实现成「后缀精确匹配 `/system/config`」：`…/system/nodes/x/config` 这类路径仍判红（自测里钉住了这条）。
- **反假绿**：页面数 / script 块数 / 写调用点数 / config 路径源数四个下限（任一为 0 即红），目录读不到退 2；
  输出单列一行「例外：识别为写调用并豁免 N 处」—— 否则「豁免」与「根本没扫到」同形。

接线：`make check-config-writes`（先 `--self-test` 再全扫）；它是 `make fe` 的依赖（排在
`check-wire-fields` 之后），自测同时挂在 `make check-guard-selftest`。

变异口径（`--self-test`，本批落地时为 49 项断言）：红 9（源名调用 / 字面量 / 源名等于 / `fetch` + PATCH /
模板尾部 / URL 含 `//` / 写 helper 定义在另一个 script 块 / 例外不溢出）+ 绿 10（GET 两种形态 /
`path + '/yaml'` 的 PUT / `write(path + '/yaml')` / 第 3 参 `'GET'` / 注释里的写调用 / `<script src>` 块 /
系统配置的两种写法）+ 解析错误 2（括号不配平、字符串不闭合 ⇒ 退出码 2，不静默跳过）+ 下限 2 + 行号 1 +
变异自证 2（`'/yaml'` 改 `'/config'` 必转红、GET 改 PUT 必转红）。每条红断言**命中的是哪条规则**
（防碰巧被另一条规则抓到），每条绿断言写调用计数 > 0（证明扫描面非空）。

实测（本批）：对齐到清理前的页面（HEAD 的 `node.html` / `namespace.html`）时全扫红，逐条点名
`web-src/pages/node.html:550 → write(path + '/config', body) → config-literal-write` 与
`web-src/pages/namespace.html:156 → write(nsPath(saveScope), …) → config-source-write`；
lead 删掉这两条写路径后全扫转绿（23 页 / 32 个写调用点 / 1 处系统配置例外）。

## 2. Backlog（按性价比排序，未开工）

1. ~~**打码 fail-open 的一致性 property 测试**（#356，9 个 fix）~~ —— **已完成（PR #412 + #413）**：
   `internal/playbook/mask_consistency_property_test.go`（100 固定种子 / P1 两 walker 同判（含 param 粒度）/ 
   P2 无明文 / P3 顺序无关 + 覆盖账本）；它一上线就抓到 **GAP-1**：`spec.values` 用 `$ref` 藏 writeOnly 时
   明文进回执 → 已按 **fail-closed 整值打码**修掉（判据反转成 allowlist 语义，契约 §1.2 同步）。
   原始构想：判据：「`MaskValues` 打成 `***`」⇔「`refHitsWriteOnly` 同判」；随机生成含 `writeOnly`/`additionalProperties`/`prefixItems`/`items`/`type:[object,array]` 的 schema 对撞。一次性覆盖 6 个历史 fix，判据无启发式，误报≈0。
2. ~~**`openapi_parity_test.go:4329` 放开到 `in: header`**~~ —— **已完成（PR #412）**：双向对账 + 两张例外表过期即红 + `headerParamFloor` + 7 条变异子测试。实况：契约 7 条 header 全部已实现（`X-Anotify-Signature` 早已由 e897e015 落地），缺口是门禁本身。原始构想：（抓 #355：`X-Anotify-Signature` 验签在契约里、实现里不存在）。复用现成 `literals` 判据 + 白名单 + 过期即红。
3. ~~**dist 新鲜度守卫**~~ —— **已完成**：`scripts/dist-stamp.mjs`（`make fe` 末尾 `--write` 把输入面 sha256 写进
   `internal/server/dist/.source-stamp`；`--check` 重算并逐条列出变动/新增/删除，stamp 缺失、输入面解析为空、
   stamp 字段与 digests 不自洽一律判红）+ `make check-dist-fresh` + `scripts/e2e/run_all.sh` 前置 `--check`。
   **为什么陈旧就硬失败、不自动重建**：自动 `make fe` 会把「你忘了 `make fe`」这条缝隙重新变成静默 —— 重建后永远绿，
   人再也看不到「刚才验的那份是旧的」；而且每次 e2e 都要多付一次 sitegen + 指纹。硬失败把「忘了」换成一次
   带文件名与新旧指纹的显式报错，代价只有一次 `make fe`。
   原始构想：`make e2e` 前断言 `internal/server/dist` 含本次 `web-src` 指纹（`run_all.sh:31-43` 至今只在缺失时重建，已三次假绿）。
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
12. **脚本里「必须成功的步骤」不许吞子进程退出码**：`scripts/install-anode.sh` 的 `anode_install()`
    在提权分支里 `run_maybe_root … ; return 0`，把 `anode install` 的失败码清零 —— 于是失败后脚本
    继续 `enable/restart` 并报「已装好」、exit 0（契约要求退 2 且回滚旧服务）。它只在**非 root + 有 sudo**
    （GH runner）现形，本机 root 直跑走另一条分支 → 用例长期「本机绿、CI 红」。已修（2e51156b），
    并给夹具加了 `forcePrivileged` 强制走该分支（修前必红，负向对照已复核）。同族可扫：`cmd ; return 0`、
    `cmd || true`（清理路径允许，但必须写明理由）、以及 `| tail`/`| head` 取错退出码（见 #11）。
14. ~~**`U1000`（未使用符号）与风格类（S*/ST*）另立项**~~ —— **`U1000` 已完成（`chore/u1000-cleanup`）**：
    台账记 41 条（本条原先这么写的），本次在 `cd5f79b5` 上实测 **40** 条 —— 差的那条在前序改动里已消失，
    未逐条追溯；其中 11 条在**产品代码**（`fixedWindow.allow`、`nodeDecodeStrictJSON`、
    `scanCapability`、`checkCapabilityInNamespace`、`nsQueryer`、`nsRefIndex`…）、29 条在测试夹具。
    逐条判过：**全部是真死代码 —— 删 40 条，零条值得留抑制注释**（判据：`git grep` 复核 + 无 build tag/
    平台变体 + 无反射/脚本/文档引用）。现在 `make lint-go` 的检查面是 `-checks=SA*,U1000`，全仓 0 条，
    所以它只会点名**新**写下的死符号。
    为什么现在敢开——不对称：基线是 0 的检查不会重演 §0 第一类缝隙（恒红即无门禁）；当初不开正是因为那 41 条
    存量会让它长期红着。落地时做了变异自证：往任意包里加一个没人调的私有函数，`make lint-go` 立刻红并点名它。
    **真正的收获不是删掉的代码行数，而是两处「断言从没跑过」被暴露**：`internal/store/v4test/fixture_test.go`
    整套（6 个夹具构造器，零调用点）与 `internal/store/schema_baseline_test.go` 的 `baselineTables`/
    `baselineIndexes` 期望清单（配 5 件探针，同样零调用点 —— 而文件头一直声称「表集合 / 外键 / 索引 /
    唯一约束 / 主键」都被守）。后者的覆盖**现在真的不存在**：要么补成真断言，要么承认没有，不能在注释里假装有
    （§3.1）。同理，产品侧 `checkCapabilityInNamespace`（跨命名空间校验）零调用点并不构成 bug —— 隔离靠
    `WHERE namespace_id = ?` 的 SQL 作用域已成立；但它是「补丁式校验没接线」的活标本。
    `S*`/`ST*`（风格类）仍另立项：那是口味，不是可判定负债。
16. ~~**注解棘轮对「测试文档段」是噪音源**~~ —— **已完成（本轮）**：连续两批被批末校准（324→332→338），增量全部来自
    新测试文件里描述「这条测试查什么」的注释段（`openapi_parity_test.go` 的对账说明、
    `mask_consistency_property_test.go` 的不变量说明）。它们不是「无锚点的门禁声称」，而是门禁自身的规格。
    落地做法：文件头 `spec-doc: 理由` 标记（**仅代码文件**，语义与三条硬校验见 §3.1），整份跳过 claim 棘轮并单独记账，
    基线随之 **338 → 301**。原计划同批标的 `mask_consistency_property_test.go` 实测零条 claim 行，
    加标记会直接撞 `SPEC_DOC_STALE`，故未加（没声明就不给豁免）。
15. **「已知红」台账要自证仍然红**：把 CONTRACT 一类的基线红清单变成可执行断言 ——
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
| 不可判定的意图 | 「为什么宁可少报也不伪造 stale」 | 保留，但必须挂**决策编号** `@decision V4-D1`，编号由 lint 校存在性 |
| 历史/废止物 | v3 设计文档里提到的已删脚本 | 文件头打 `frozen-doc: 理由`，**该标记自己也由 lint 校验仍需要**（引用全部解析成功即红，强迫删标记） |
| 检查文件自己的规格段 | 测试文件头写「这条测试查什么」（如 `openapi_parity_test.go` 的对账说明） | 代码文件头（四十行内）打 `spec-doc: 理由`（**仅代码文件**），整份跳过 claim 棘轮并单独记账；三条硬校验：① 必须真是测试/检查文件（Go `func Test…` / JS `test(`、`describe(`、`it(`），否则 `SPEC_DOC_NOT_TEST` 红；② 去掉标记行后必须仍有 claim 行，否则 `SPEC_DOC_STALE` 红；③ markdown 里写它无效（`SPEC_DOC_NOT_CODE` 红），文档类继续用 `frozen-doc` / `snapshot-doc` |

### 3.2 锚点格式（写进代码/文档即可，lint 校验）

```
@guard    scripts/check-css.mjs#NESTED_RULE     ← 脚本必须存在，且真的会抛这个规则码
@decision V4-D1                                  ← 决策编号必须在决策索引里找到（internal/decisions/decisions.go 或 docs/contracts/v4/CONTRACT.md §0）
@count    node scripts/check-contrast.mjs        ← 计数型断言必须附可重跑的命令
frozen-doc: v3 设计快照，不再随代码演进           ← 仅 markdown 前 40 行，且引用已全部解析时必红
spec-doc:  本文件是这条检查自己的规格说明         ← 仅代码文件（测试/检查文件）的文件头，整份跳过 claim 棘轮；
                                                   三条硬校验见 §3.1（不是免死金牌）
```

这样「注解」就从自由文本降级为**外键**：外键的引用完整性由 lint 检查，
被引用的脚本改规则名/被删时，锚点立刻红——**而不是安静地变成谎**。
对应的守卫见 §1 的 `scripts/check-annotations.mjs`。

## 4. 明确不做（不能机械化的）

- 作用域写宽导致桌面逐像素回归（`70ef5776`）：只能视觉基线，仓库无入库基线图。
- 有效命中被祖先 `overflow` 裁掉（`37fea2c4`/`c12bec80`）：`ui-audit-mobile.mjs` 已做 clip-aware，但必须真浏览器跑 ⇒ 合并前手工。
- 需求裁决反转（`9f0154c1`→`b6e3417c`→`e620272e`）：唯一止损是「裁决写进契约 + 显式标注覆盖了哪条旧决定」。
- 状态机 intent 由 reason 反推（#348/#349）：可做弱规则（派生函数必须收 intent 而非自由文本），实质靠判别性用例矩阵。
