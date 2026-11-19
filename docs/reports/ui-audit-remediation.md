# UI 黑盒审计处置（#453–#458 · 批量清债）

snapshot-doc: 本文是 2026-11-19 一次批量处置的**当时快照**。正文里的数量、行号、grep 计数都取自那次收尾实测，不承诺随后续提交漂移；复核命令写在每行证据里，换基线后重跑即可。

- 清单来源：`docs/reports/ui-audit-blackbox.md` §2（36 个 ID + P0 假恢复码 = 37 条）。
- 基线：`origin/main` @ `141edd25`（开工时 fetch 确认未前进）。
- 交付分支：`pi-subagent/hw-nj/anotify-audit-bulk-315ed2d5`（PR #479）。
- 方法：先冻结契约（`.pi-legion/contract.md`），7 个 worker 按文件所有权并行实现，1 个 worker 同步 e2e/单测；文案统一走 locale 补丁串行写入，避免 4 份 YAML 互相覆盖。

## 0. 先校准三件事（与交接说明不一致的现场事实）

交接说明里的「已修 5 条」在 HEAD `141edd25` 上有 3 条与实测不符。按实测处理，并把证据写在第 1 节对应行：

| 项 | 交接说法 | HEAD 实测 | 本 PR |
| --- | --- | --- | --- |
| A-1 | 已修 | **未修**：`nodes.html` 与 `node.html` 两块 `details.nd-more` 仍在（「槽位判定与批量写规则」原文可 curl 到） | 删 |
| A-6 | 已修 | **部分**：按钮文字已改「保存整份模板」，但 ① 文案、③ 两个「说明」折叠块仍在 | 补完 |
| B-2 | 已修 | 已修（`86553eb8`：`scopeParam:'name'` + `adoptScopeParam`），不是 `scope:false` | 只补证据 |
| B-6 | 已修 | **未修**：`nodes.html` 行内仍是「轮换身份令牌」直接按钮 | 移进 ⋯ 菜单 |
| P0 假恢复码 | 已修 | 已修（首跑一次展示 8 个、只展示一次） | 只补证据 |

## 1. 处置表（37 行）

证据格式：`curl` 计数是**同一台机器上**用 e2e harness 起服务 + devseed 播种后，对基线与本分支各抓一次页面 HTML 的 `grep -c` 对比（`before → after`）；e2e 用例名可在 `scripts/e2e/fixtures/f3/` 里搜到。

| ID | 处置 | 证据 |
| --- | --- | --- |
| A-1 | 删 `nodes.html` / `node.html` 两个「开发者说明：槽位判定与批量写规则」折叠块与内联样式；`slot_note`/`batch_hint` 只保留在 `/docs.html` 节点章节 | `curl /nodes.html`：`槽位判定` 1→0、`批量写规则` 1→0 |
| A-2 | 删 `namespaces.html` 的「命名空间能配什么」整块 + `.nd-more` 样式；`namespaces.v4.note_summary`/`note_body` 四语键删除 | `curl /namespaces.html`：`命名空间能配什么` 1→0、`namespace_configs` 1→0 |
| A-3 | 删 ①「CAS 判据」②「不写进节点 applied」两段；③ YAML 提示改「服务端按当前配置生成的只读文档。」；④ 删 Go 常量 `namespaceYAMLPrelude`（单测改为反向断言：文档首行是 `apiVersion:` 且不含那段英文） | `curl /namespace.html`：`CAS 判据` 1→0、`不写进节点 applied` 1→0、`刻意只读` 1→0、`PUT /v1/namespaces` 1→0；`go test ./internal/server -run 'YAML|Namespace' -count=1` 绿 |
| A-4 | 删四段自述：① 副标题改「这台节点：连接状态、运行能力与配置」② YAML 卡长说明（YAML 卡现在只留编辑器，见 B-1）③「严格解析」提示 ④「这是授权列表，不是副本」；授权段留一行「只有这台节点申报过的命名空间才会生效。」；安装段 `vars 非秘密 / envPassthrough` 一句删 | `curl /node.html`：`保存这份 YAML` 1→1（B-1 恢复编辑后按钮仍在）、`严格解析` 1→0、`授权 ∩ 本机申报` 1→0、`envPassthrough` 2→0 |
| A-5 | 删副标题与「部署只在这个按钮里发生」两处；条件标题改「能不能跑」，行标签走 `caps.cond.<Type>`（已实现 / 有节点可用 / 下游可用 / 已部署），协议值挂 `data-cond`；「安装来源」收进 `<details class="devf">` | `curl /capability.html`：`部署只在这个按钮里发生` 1→0；e2e `capability-actions`（条件行读 `data-cond`、标签为本地化词） |
| A-6 | `detail_hint` 改「安装只新增，不覆盖已有能力与源」；按钮改「保存模板」；删两个「说明」折叠块（键同批删除） | `curl /playbook.html`：`只做加法` 1→0、`说明` 3→0 |
| A-7 | 删「统计口径」行与整块「管理后台能做什么」；`group_yaml_note` 去 CAS 半句；按钮改「保存」；删 `systemYAMLPrelude` | `curl /admin.html`：`统计口径`/`管理后台能做什么`/`CAS 保存动作`/`以当前 CAS 保存` 各 1→0 |
| A-8 | 删 message 页 devf 里「等待不消耗执行尝试…」与「租约是执行授权边界…」两段（四语键同删） | 两键在 4 份 locale 中 0 命中（`check-i18n-keys` 键集合 2294 一致）；`check-ui-copy` 该页 0 违规 |
| A-9 | 删「v4 没有独立的更新时间字段」整行与键；更新时间字段本身保留 | `curl /source-detail.html`：`没有独立的更新时间字段` 1→0 |
| A-10 | ①「投递去重与窗口」折叠块删除，同一句复用 `sources.v4.dedup` 键搬进 `/docs.html` 事件章节 ② 每行常驻的 `resourceVersion（CAS 令牌）` 移入行尾 ⋯ 菜单的只读信息项 | `curl /sources.html`：`投递去重与窗口` 1→0；`curl /docs.html`：dedup 句 0→1 |
| A-11 | docs 顶部说明改「端点清单」并删「v4 / 旧 spec」半句 | `curl /docs.html`：`五份 spec` 1→0 |
| A-12 | 删页尾重复的「两条独立的轴 / 认证轴…/ 投递轴…」整块（开头那句保留） | `curl /security.html`：`两条独立的轴` 2→1 |
| B-1 | **唯一写入口 = YAML**（用户新方向，覆盖审计报告的「只读」建议）：恢复 `#node-yaml-edit`（textarea + 「保存这份 YAML」+「按当前配置重写」），保存 `PUT …/yaml`；删掉「节点默认配置」表单的 `PUT …/config` 写路径与「编辑原始 JSON」折叠块。理由：#454 的根因是表单按 `GET …/config` 投影序列化再整份写回，投影缺字段就会清空（`tags` 就是这么丢的）；改成「读整份 `/yaml` → 表单只是同一份文本的另一种编辑 → 写回 `/yaml`」后，这个 bug 类别从结构上消失 | e2e `node-deployments`：`#node-yaml-save` 存在、保存只发 `PUT …/yaml`、页面 0 次写 `/config`、`#node-config-save` 不存在；页内写请求 grep = `write(path + '/yaml', …)` 一处（另有 drain/upgrade/tokens 三个生命周期动作） |
| B-2 | 已修，不改；只补证据 | e2e `ns-scope-topbar`：顶栏切 scope 后 URL/页面表单一致，PUT 仍只写 `default` 且逐字节不变 |
| B-3 | 删「应用筛选」按钮；`#ev-source`/`#ev-trace`/`#ev-limit` 改 `change` 即重查；`#ev-namespace` 继续 `saveScope`（仍是顶栏 scope） | `curl /events.html`：`应用筛选` 1→0；e2e `observe-events`：改 `#ev-limit` 后请求 URL 带新值、无需点按钮 |
| B-4 | 删 `web-src/pages/source.html`；`/source.html` 走静态未知文件路径返回 **404**（选择 404 而不是重定向：该页无入口，重定向会保留死链）；`playbook.html` 的引用入口改指 `source-detail.html` | 同机探针：`/source.html?name=x` 200→404（无参同样 404）；e2e `events_paging` 的 404 断言 + `pages-21-*` |
| B-5 | 「任务总数/失败」改「近 24h 任务 / 近 24h 失败」；节点状态标签 `deployed/stale` 改「已同步 / 版本落后」（这是状态标签，不是取值行，原来「当前 digest 没值」的观感由此消失） | e2e `capability-actions`：observed 标签为新词、不含「任务总数」；`curl` capability 的 SSR 文本无旧词 |
| B-6 | 轮换身份令牌移入行尾 ⋯ 菜单（`[data-act="rotate"]`，danger 样式），列表行只留「进入详情」 | e2e `node-deployments`：行内无轮换按钮、菜单项存在且点击需二次确认 |
| C-1 | `STATS.total > 0` 时 `#first-event` 整块 `hidden`（不再渲染可见内容），`data-ready=1`/`data-complete=1` 照旧置上；顺带省掉 3 次列表请求 | e2e `observe-index`：有事件的实例下 `#first-event` 不可见；变异 ② 证明该断言会红 |
| C-2 | 「接入你的 Agent」与引导同条件（仅首次出现）；`delta_na` 无对比时 `hidden` 且文本清空，不再写「暂无对比」 | `curl` index 静态：`暂无对比` 1→0；e2e `observe-index`：`#quickstart`、`#kpi-total-trend` 均 hidden |
| C-3 | 节点页最近任务默认 5 行 + 「查看全部 N 条」。**偏离**：不链到 `events.html?source=`——事件流没有「按节点过滤」，也不列任务，链过去会落在一个解释不了的数字上；改为同页展开剩余行 | e2e `node-deployments`：夹具造 ≥6 条真实任务，默认显示 `min(5, n)` 行，`#node-tasks-all` 存在且点击后展开 |
| C-4 | message 页三个常驻空块（环境诊断 / 执行尝试事实 / emit 事实）无数据时整块不建 DOM（标题+空态都不渲染） | e2e `observe-message`：空实例下三块不存在；有数据时照常渲染 |
| C-5 | playbook：只留页头「刷新」（第二个删除）；三个「说明」折叠块删两个、留一个改 `title` tooltip；危险区标题改「删除这个预案」，按钮仍「仅删除清单」 | `curl /playbook.html`：`重新体检` 2→0、`仅删除清单` 2→1、`说明` 3→0 |
| C-6 | capability 两个留痕空块无数据时整块不渲染；「删除」移入 ⋯ 菜单（`data-act="delete"`），动作条留编辑/部署/启停/试跑 | e2e `capability-actions`：空块不渲染、删除按钮在菜单内且仍走二次确认 |
| D-1 | 失败 KPI 卡包 `<a id="kpi-failed-link" href="events.html?status=failed">`，`#kpi-failed`/`#kpi-failed-sub` 全部保留 | e2e `observe-index`：`#kpi-failed-link` 是 `a` 且包住 `#kpi-failed`、href 正确；变异 ③ 证明该断言会红。**已知缺口**见 §7 新 issue（事件 API 无 `status` 过滤） |
| D-2 | 没有下一页（`continuation` 为空）时 `#ev-more` 整块 `hidden`，有下一页时显示；`disabled` 只表示加载中 | `curl` 静态：`#ev-more` 带 `hidden`；e2e `events_paging`：单页实例下按钮不可见 |
| D-3 | node / capability / trace 三页错误态不再裸拼 `err.reason`/`err.message`（英文原文只留给「本地网络错误」这一种情况），只留中文标题 + 重试/返回 | 三页 grep：`err.reason`/`err.message` 拼接 0 处；e2e `pages-21-*` 的 404 页不出现英文原文 |
| D-4 | receivers 空态删第二句同义句（四语键同删） | `curl /receivers.html`：`开放后这里会出现接收设备` 1→0 |
| E-1 | 「刷新事实 / 重新体检 / 重新加载最新资源 / 刷新 →」统一成「刷新」 | `curl`：node/nodes/namespace 的 `刷新事实` 0、playbook 的 `重新体检` 0、sources 的 `重新加载最新资源` 0；`curl /` 的 `刷新 →` 0 |
| E-2 | UI 文案统一「命名空间」：`中枢授权命名空间`、`当前命名空间的最近任务`、`上下文命名空间`、`节点实际生效的 vars`；YAML 里的字段名不动 | locale 4 语 `nodes.v4.*` 4 个键改词；页面侧 `check-ui-copy` 0 命中 |
| E-3 | 事件流「接收时间」改用与源详情同一个 `toLocaleString()` 本地时间 formatter；相对时间只留在「最近」类列表 | `grep -c toLocaleString web-src/pages/events.html` = 1；e2e `observe-events`：时间列匹配本地时间形状、不再是 ISO `T…Z` |
| E-4 | 全站唯一 `h1` 改成页面名；上下文（分组 · 命名空间徽标）降级为同一 header 里的 `div.app-topbar-context`；`#scope-crumb` id 与 `scopeSwitcherSync`/`paintScopeCrumb` 全部保留 | `web/partials.js`：`el("h1", …o.title)` + `div.app-topbar-context`；e2e `pages-21-*`：每页唯一 h1、顶栏高度 64px、`#scope-crumb` 仍在 |
| E-5 | 「安全与登录」从「资源」组移入「账户」组（复用既有导航键，不新增词条）；「总览 / 工作台」统一成「总览」 | `web/partials.js` NAV 三分组（工作台 / 资源 / 账户）；`curl` 导航文本：`账户` 组出现、`安全与登录` 不再在 `资源` 组 |
| E-6 | sitegen 加 tag 别名合并 `node → nodes`、`ingress → events`，合并后标题取规范 tag；其余派生 tag（console / aggregates / hooks / source-actions）补四语中文 | `curl /docs.html`：`事件入口` 1→0、`>Node<` 1→0、端点分组卡 21→19；`go test ./internal/sitegen/ -count=1` 绿 |
| F-1 | 版本说明改「静态资源走根路径并带内容指纹（如 `/ui.<hash>.css`）」 | `curl /docs.html`：`/static/*` 1→0 |
| F-2 | 删 cli-auth / passkey-enroll 列表里重复的「短码 10 分钟过期」一条；输入框下的有效期提示保留 | `curl /cli-auth.html`：`短码 10 分钟过期` 1→0；两页同改 |
| P0 | 已修，不改；只补证据 | e2e `auth_flow`「首跑：恢复码一次展示 8 个」+ `internal/auth` 单测（8 条未使用、明文仅此一次） |

## 2. 结构决策与偏离说明

- **B-1 走「YAML 唯一写入口」而不是审计建议的「YAML 只读」**：用户在收尾阶段把方向定为「所有资源一个模式：YAML 是编辑面与写入口，表单只是同一份文本的另一种编辑方式」。按新方向，节点页恢复 YAML 编辑，删掉的是表单的 `PUT …/config` 写路径与「编辑原始 JSON」折叠块。
- **C-3 不链事件流**：`/v1/console/events` 没有 node 维度过滤，事件流也不列任务；「查看全部 N 条」同页展开，数字与行都能对账。
- **B-4 无参访问返回 404**：`source.html` 已无任何入口，重定向会把死链伪装成正常路径；404 更诚实。
- **D-1 目标页暂不过滤**：见 §7 新 issue（链接形状按审计要求保留，缺口显式登记，不伪造过滤）。
- **A-5 条件行协议值保留在 `data-*`**：e2e 与排障都需要原始条件名，但不上可见文本。

## 3. 文案守卫扩面（`scripts/check-ui-copy.mjs`）

新增规则 `impl-word`：页面可见文本 / locale 值里出现实现词即红。

- 词表（10 个，大小写敏感）：`CAS`、`schema`、`PUT`、`GET`、`resourceVersion`、`digest`、`租约`、`幂等`、`spec`、`applied`。
- 结构化例外（不是白名单 key，而是按容器判定）：`<pre>`/`<code>`/`<kbd>`/`<samp>` 内、`class` 含 `dev`/`devf`/`dev-only`/`method`/`code-body`/`doc-code` 的容器内、带 `data-dev` 的元素内、HTML 注释内；locale 值里的 `<code>…</code>`/`<pre>…</pre>` 区间同样豁免。例外之外 hit 一次即红，失败信息给 file:line + 命中的词。
- 防假绿下限（同一脚本里断言，低于下限直接判失败）：locale ≥4 份、值 ≥9000 条；页面 ≥20 个、文本节点 ≥160 个、**实现词检查面 ≥90 个**（当前实测：4 / 9160 / 22 / 217 / 100）。单独加「实现词检查面」是因为「把所有节点都标成 dev 容器」能让新规则静默扫空——这个下限就是防这种假绿。
- 自测（`--self-test`）扩到：规则 21 条 + 词 10 个；locale 红 27 / 绿 16 / 豁免 5；页面红 8 / 绿 13 / 豁免 4。新增样例覆盖：`<p>以当前 CAS 保存</p>` 真红、`<pre>PUT …</pre>` 真绿、`<code>resourceVersion</code>` 真绿、`class="devf"` 容器真绿、`data-dev` 真绿、HTML 注释真绿、删词后转绿（反向证明规则没在乱扫）。
- 当批清理：locale 侧 112 + 17 + 1 + 3 条补丁（含四语同步），页面侧 0 条残留；`check-ui-copy` 正跑绿。

## 4. e2e 覆盖（结构性 7 条 + 受影响旧断言）

| 结构项 | 断言位置 | 断言要点 |
| --- | --- | --- |
| B-1 | `node-config`（`f3-node-retained.mjs`） | `#node-yaml-save` 存在且可编辑；保存只发 `PUT …/yaml`；0 次写 `/config`；`#node-config-save` 不存在；`#node-yaml-reset` 行为 |
| B-3 | `observe-events` | `#ev-filter` 不存在；改 `#ev-limit` 即重查且 URL 带新值；`#ev-more` 无下一页时不可见 |
| B-4 | `source-actions` / `events_paging` | `/source.html`（带参/无参）均 404；playbook 的引用链接指向 `source-detail.html?…` |
| C-1 | `observe-index` | 有事件时 `#first-event`、`#quickstart` 均不可见，`data-ready=1` |
| C-3 | `node-deployments` | 夹具真派发 ≥6 条任务；默认 `min(5, n)` 行；`#node-tasks-all` 存在且点击展开 |
| D-1 | `observe-index` | `#kpi-failed-link` 是 `a[href="events.html?status=failed"]` 且包住 `#kpi-failed` |
| B-5 | `capability-actions` | observed 标签为「近 24h 任务 / 近 24h 失败」；状态标签「已同步 / 版本落后」 |

同步修掉的旧断言：`#ev-filter` 点击、`#ev-more` 可见性、`#node-yaml-*` 读写、条件行英文标签、`旧 digest`、`admin-notes`/`admin-scope`、`#pb-refresh-installs`、顶栏 h1 文本、`source.html` 深链、`node.html` 表单输入（F2c2 改测 YAML 保存）。

## 5. 变异验证（三条）

| # | 变异 | 期望 | 实测 |
| --- | --- | --- | --- |
| ① | 往页面可见正文写回实现词（`web-src/pages/index.html:555` 加一句「以当前 CAS 保存」） | `check-ui-copy` 判失败并指名该行 | ✅ `✗ 文案违规：1 条` → `web-src/pages/index.html:555 → «CAS» … [impl-word]`；恢复后正跑绿 |
| ② | 去掉 C-1 的 `events>0` 门控（`index.html:1389` 改 `false &&`） | `observe-index` 的 C-1 断言失败 | ✅ `frontend 42通过/1失败` → `observe-index — AssertionError: 有事件时三步引导必须 hidden` |
| ③ | 去掉 D-1 的链接（`<a id="kpi-failed-link">` → `<div>`） | `observe-index` 的 D-1 断言失败 | ✅ `frontend 42通过/1失败` → `observe-index — AssertionError: 入口必须是 <a>（不是 div + onclick）` |

## 6. 门禁

| 门禁 | 命令 | 结果 |
| --- | --- | --- |
| CI | `flock /tmp/anotify-ci.lock -c "make ci"` | ✅ exit 0（Go 41 个包 ok、`make fe` 全链绿） |
| 受影响 e2e | `make e2e-affected` | ✅ 28/28 套件、2371 断言通过 / 0 失败、exit 0 |
| 结构相关套件 | `./scripts/e2e/run_all.sh frontend` / `events_paging` | ✅ 43/0、179/0 |
| 文案守卫 | `node scripts/check-ui-copy.mjs` | 绿（4 / 9160 / 22 / 217 / 100） |
| 键集合 | `node scripts/check-i18n-keys.mjs` | 绿（4 份 locale / 2294 key / 引用点全有词条） |
| Go | `go test ./... -count=1` | 绿（39 个包含测试的包全 ok，exit 0） |
| 注解棘轮 | `node scripts/check-annotations.mjs` | 绿（unbacked 300/300、unmarked 383/383） |
| 纪律棘轮 | `node scripts/check-test-discipline.mjs` | 绿（五项 21/38/3/37/3） |
| 单测（前端，非 CI 路径） | `node --test scripts/v4-frontend.test.mjs` | 215 tests / 192 pass / 23 fail；与基线（221/198/23）**失败名集合完全相同**，无本批新增 |

### 棘轮同批校准（显式记录）

`check-annotations` 的两条基线在批末按实测重测（该守卫自己的提示就是「改善/新增都要同步基线」）：

- `COUNT_UNMARKED_BASELINE` 385 → **383**（删掉 source.html 的图脚本说明，同时新增 e2e 断言注释里的计数行；批末实测 383）。
- `check-test-discipline` 的 `e2e/swallow-catch` 39 → 38（删 `source.html` 图脚本断言时，连带删掉了那段里唯一一处 `.catch(() => {})`；不是放宽执法）。

## 7. 追加项（同源「原始视图重复」，用户收尾时补入）

`source-detail.html` 的「脱敏后的 config JSON」折叠块是「原始 YAML」的严格子集（两者都脱敏，YAML 是整份资源文档、JSON 只有 `config` 一个字段），同页两个可展开的原始视图讲同一件事：

- 删 `.sd-raw` JSON 折叠块（DOM + CSS + `rawJsonBlock()` + 填充代码 + 四语 `source_detail.config_json*` 两键）。
- 保留唯一「原始 YAML」，并在 summary 的提示里写明「含脱敏后的 config」。
- 与 B-1 同源：`node.html` 的「编辑 YAML（`PUT …/yaml`）」和「编辑原始 JSON（`PUT …/config`）」曾是真的双写入口；按 B-1 已收成唯一写出口。
- 「所有 YAML 对象用一套模板管理」（统一 `internal/yamldoc` 文档层、去掉三份各自一份的 prelude）留给后续 issue，不在本 PR 做。

## 8. 遗留与新 issue

1. **D-1 目标页不过滤**（新 issue）：`/v1/console/events` 没有 `status` 参数（`api/events.yaml`），`events.html?status=failed` 打开的是全量事件流。本 PR 按审计要求保留链接形状，但过滤能力需要 API 补参数（连同「任务列表」缺失）。
2. **namespace / admin System YAML 仍只读**（契约限制，非本 PR 可改）：`/v1/namespaces/{name}/yaml` 与 `/v1/admin/system/yaml` 在 `api/resource.yaml` 里**只定义 GET**，写入口仍是 `/config`。两者都在页面上明确写「只读」，且它们的 `/config` 投影字段集与文档一致（只有 vars / retention），不存在 #454 那类「投影缺字段→整份写回清空」的风险。要统一成 YAML 写入口需要先改 API 契约，留给 §7 的统一文档层 issue。
3. **capability / source 的 YAML 只读**：`…/capabilities/{name}/yaml`、`…/sources/{name}/yaml` 同样只定义 GET；它们的编辑表单走的是完整资源写（`includeBody=true` 读全文），不是投影，故按「明确标注只读」处理。
4. `scripts/v4-frontend.test.mjs` 是零调用点的大单测（不在 `make ci` 路径，基线自带 23 个失败）。本批：删掉随 `source.html` 消失的 3 个用例、把 3 个 `#454`「表单→YAML 草稿」用例改成「YAML 唯一写入口」断言、把 2 个随 A-5②/source-detail 收尾改动而失效的静态断言改成新事实（部署提示句、脱敏 config 视图）。改后 215 tests / 192 pass / 23 fail，**失败名集合与基线逐条相同**（`comm -13` 为空），没有放宽任何既有断言。
5. 统一 YAML 文档层（三份 prelude、`srcYAMLDocument`）与新 issue 一起排期。
