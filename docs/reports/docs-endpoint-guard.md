> **历史快照**：本文件是当时的交付记录，不随代码演进；行为以代码、测试与 `docs/contracts/` 为准。

<!-- snapshot-doc: 2026-11-19 #457 交付时的定格数字（make ci 的真实输出），复算命令见 §6 -->
# 接入文档端点漂移 + 文档端点检查（#457）交付报告

- Issue：[#457](https://github.com/zzjcool/anotify/issues/457)
- 分支：`pi-subagent/host-workspace/anotify-457-r3-aa45c4fb`
- 基线：`origin/main` @ `370fcd69`（分支已 rebase 到它；期间 main 新增了 `scripts/check-doc-links.mjs` / `scripts/check-ui-copy.mjs`，接线是并存的）
- PR：[#465](https://github.com/zzjcool/anotify/pull/465)（`Fixes #457`）

一句话：**接入文档教了两条不存在的端点**（`POST /v1/notify`、`POST /v1/events`），照抄必 404；
本轮把四份 locale 的接入文案换成 v4 事件入口，并把「文档提到的端点」变成对 `api/*.yaml` 的**外键**
（`scripts/check-doc-endpoints.mjs`，接进 `make ci`，变异自证会红）。

---

## 1. 现象与根因

`web-src/pages/docs.html` 的「接入 Agent」第 2 步是一条**可照抄的指令**：携带 Key 发 `POST /v1/notify`。
v4 里这条路由已经删了（`api/ingress.yaml` 对它 0 命中；`internal/server/api_integration_test.go`
的 `TestAPIIntegrationRouteTableIsV4Only` 明确钉住扁平路由必须 404），所以文档教的是一条必 404 的调用。
同类还有 `events.empty`（「用 `POST /v1/events` 提交第一个」）与 README 的 v3 端点表 / 示例 curl。

根因不是「写错了」，而是**文档里的 URL 没有执行者复算**：`check-i18n-keys` 只验 key 在不在、
`check-ui-copy` 只验文案有没有开发旁白、e2e 只验页面 200。端点名是自由文本，随时间只能变错。

## 2. 改了什么

| 文件 | 改动 |
| --- | --- |
| `web-src/locales/zh-CN.yaml`、`en.yaml`、`ja.yaml`、`es.yaml` | `docs.agent.step2.desc`、`docs.toc.events`、`events.empty` → `POST /v1/namespaces/{namespace}/events`（并按语言补上「namespace 换成你的命名空间」）；删掉只教死端点的死键 `docs.toc.notify`（全仓 0 引用） |
| `README.md`、`README.en.md` | v3 扁平端点表里三条死行标「**已删除** —— 改用 X」（`/v1/notify` → events 入口、`/v1/stream` → 节点流 / `GET /v1/console/events`、`/v1/notifications` → `GET /v1/console/events`）；Agent 上报示例换成 v4 事件入口的可跑 curl（与 docs 页同一份形状）；源码树注释 `api/  /v1/notify 上报` 改指向 `api/*.yaml` |
| `scripts/check-doc-endpoints.mjs` | 新增检查（见 §3） |
| `Makefile` | 新目标 `check-doc-endpoints`（先 `--self-test` 再体检），接进 `make ci` |
| `docs/lint-guards.md` | 登记 L10（判据要点 + 接线） |

口径：**v4 没有为通知单开端点**，通知语义已并入事件流 —— 所以 docs 页第 2 步教的是
「发一条事件」，不是「换个 URL 继续调通知接口」。

## 3. 检查设计（判据可自证）

事实源：`api/*.yaml` 的 `paths:` 键（五份 spec 合并去重，109 条）。文档里的每个 `/v1/<path>` 必须命中一条：

| 判据 | 内容 |
| --- | --- |
| 归一化 | 去 `?query` / `#fragment` / 行尾标点 / 尾斜杠；按 `/` 切段逐段比对 |
| 段级宽容 | spec 的 `{param}` 可被文档里的**具体值**命中（`/v1/hooks/src_xxx` 合法）；文档里的 `{ns}` / `:id` / `...` 也命中对应段。以斜杠结尾的基路径（`/v1/system/`）按前缀判定 |
| 历史提及 | 同一行（或上一行）写「已废弃 / 已删除 / 已下线 / 已退役」或 `deprecated` / `retired` / `removed` / `deleted` ⇒ 引用不存在算历史提及（计数不判红）；或整段包进 `<!-- docs-endpoints:historical -->` … `<!-- /docs-endpoints:historical -->`。**只对「引用不存在」生效**（端点存在时正常算解析成功）—— 沿用 `scripts/check-annotations.mjs` 的历史提及口子 |
| 遮罩 | `.html` 里 `<script>` 与 HTML 注释先遮罩：客户端代码/维护者注释不是教学文案 |
| 反假绿 | `spec 路径` / `扫描文件` / `引用数`三条下限（50 / 10 / 60），任一归零 ⇒ **退出码 2**；`paths:` 块里出现无法识别的行 ⇒ 退出码 2（fail-loud，不静默少算）；`--self-test` 24 条自证（红/绿双侧 + 行号 + 下限 + spec 解析 + 真实 `api/*.yaml` 能解析） |

## 4. 变异自证（本轮实测）

| 变异 | 结果 |
| --- | --- |
| `--self-test`：「已删除的 `/v1/notify`」「已下线的扁平 `/v1/events`」 | 红（`doc-endpoint-missing`） |
| 真实 locale 里写 `POST /v1/nonexistent-thing`，跑 `make check-doc-endpoints` | **红**：`1 处端点不存在于 api/*.yaml`，点名 `web-src/locales/en.yaml:1264`；`make` 退出非零 |
| 对**未修的 `origin/main`**（`--root=<main 的 worktree>`） | **红**：26 处（4 份 locale × 4 处 + 两份 README 各 5 处） |
| 修完的本分支 | 绿：109 条 spec 路径 · 32 个文档文件 · 102 处 `/v1/` 引用 · 历史提及 14 处 |
| `--root` 指向没有 `api/` 的空树 | 退出码 2（「事实源缺失 ⇒ 检查面为空，不是通过」） |

## 5. 检查面与已知盲区（有意为之）

- 扫面 = **sitegen 的输入面**（`web-src/pages`、`web-src/layouts`、`web-src/locales`）+ 两份 README。
  这是「用户最终看到的文档」的全集。
- **不扫** `docs/**`、`DEVELOPMENT.md`、`IOS_TESTING.md`、`scripts/**`：它们是历史快照 /
  内部排障笔记 / 测试夹具，很多地方**故意**引用旧端点（v3 契约、复盘报告、`scripts/ws_test.mjs` 的旧探针）。
  把它们也管起来只会逼人到处贴豁免，最终得到「到处是豁免的假绿」。
- **不扫** `.html` 里的 `<script>` 与其它 `web/*.js`：那是客户端代码，URL 是拼出来的
  （`/v1/namespaces/{ns}/{plural}`），验它属于 e2e 的活。句柄命中的是**教学文案**。
- 已知盲区（登记在此，不假装覆盖）：`IOS_TESTING.md` 的 v3 示例 curl 仍是 `POST /v1/notify`
  （内部真机测试笔记，本轮不改，避免越出 #457 的范围）；`DEVELOPMENT.md` 里若干旧路径同理。

## 6. 门禁

```bash
flock /tmp/anotify-ci.lock -c "make ci"
```

结果：**PASS（exit 0）**，关键数字（`make ci` 真实输出，定格；运行于 `779f213f` + 本报告的工作区版本）：

- `check-doc-endpoints`：`--self-test` 24 条全绿；体检 `109 条 spec 路径 · 32 个文档文件 · 102 处 /v1/ 引用 · 历史提及 14 处`
- `check-annotations`：`unbacked 300 / 基线 300`、`unmarked 385 / 基线 385`（新增检查的文字按「检查」措辞，不涨棘轮）
- `check-i18n-keys`：4 份 locale 键集合一致，2189 个引用点全部有词条
- `check-ui-copy`：PASS（4 文件 / 9280 条值 / 20 条规则）
- `go test ./... -count=1 -shuffle=on`：PASS（41 个包 ok / 0 fail）
- `contract`（A2/A4/A5/A6/A7 + openapi 一致性 + 字段级核对 + `cmd/anode` 可编译）：PASS

环境备注（诚实记录，不是本 PR 的产物）：本 worktree 的 `lint-go` 需要 `.bin/staticcheck`，首次运行时该目录为空且
`GOPROXY=direct` 取不到 `honnef.co`（网络受限）→ 从主 checkout 复制同一版本（`2025.1.1`）后重跑才绿；
`.bin/` 在 `.gitignore` 里，不进交付。

复算：`node scripts/check-doc-endpoints.mjs --self-test && node scripts/check-doc-endpoints.mjs`、
`node scripts/check-annotations.mjs`、`node scripts/check-i18n-keys.mjs`、`node scripts/check-ui-copy.mjs`。

## 7. 遗留风险

- `events.empty` 目前是**未被引用**的词条（文案已修正，但页面没有接线）——本轮只保证「不教死端点」，
  接线属于事件页文案的工作，不在 #457 范围。
- 检查只判「端点存在」，不判「方法对不对」：`GET /v1/namespaces/{ns}/events` 与 `POST` 同 path，
  文档写成 GET 也算绿。方法级核对需要 `paths:` 下的 method 键，属于下一步（未在本轮实现，避免判据变宽后误报）。
- `README` 的 v3 端点表仍是历史清单（逐行标了「已删除」），没有整表删除：它对读 v3 文档的人仍有对照价值，
  且现在每一行死端点都带「改用 X」。

## 8. es 横向溢出（本 PR 自测红 → 已修，同分支追加）

本 PR 自己的 `ui_shift` 矩阵判了两条红（CI 实测，两档主题都红）：

```text
ui_shift / grid-es-light-desktop-1 — docs: docs·es·light·desktop: document horizontal overflow 1446>1440
ui_shift / grid-es-dark-desktop-1  — docs: docs·es·dark·desktop:  document horizontal overflow 1446>1440
```

### 8.1 真因：目录 rail 里被换成 v4 长路径的那一项

不是文案变长、不是测试口径松，而是**桌面目录栏**（`.doc-toc`，≥1280 才出现，栅格固定 196px、
内容盒 176px）里的一项标签从

- 旧：v3 的扁平路径（斜杠串 10 字符 ≈ 68px，放得下）
- 新：`POST /v1/namespaces/{namespace}/events`（斜杠串 **33 字符 ≈ 223px**，整串不可断行）

变成了一个**不可断行的长串**。`.toc-link` 是 `display:block` 的 `<a>`：块盒宽度仍是栏宽（176px），
但盒子里那串文字按默认 `overflow-wrap: normal` 直接伸出栏外 —— 文字右缘量到 `x = 1446`
（视口 1440），`document.documentElement.scrollWidth` 因此变成 1446。

两个细节记下来，否则下次还会找错方向：

1. **审计里的 `unbounded=[...]` 是空的**。它遍历元素盒（`getBoundingClientRect`），而这里溢出的只是
   **文本**：`.toc-link` 这个元素盒仍旧老实是 176px，没有任何元素越界，只有 `scrollWidth` 抓到 1446。
   定位手段是遍历文本节点 + `Range.getClientRects()`，命中的就是
   `TEXT "POST /v1/namespaces/{namespace}/events" … parent=A.toc-link left=1223 right=1446`。
2. **不是 es 独有**。`docs.toc.events` 在四份 locale 里是同一个字符串（它是语言无关的路径），
   实测 `grid-en-light-desktop-1` 修前同样 `1446>1440`；任务卡只列了两条 es，是 CI 输出里先被点名的
   那两条。修法必须对四语言同时成立 —— 所以**一个文案都没动**（也不该动）。

### 8.2 修法：只放开断行，不动栏宽 / 栅格

`web-src/pages/docs.html` 的 `.toc-link` 加一行（+7 行含注释）：

```css
overflow-wrap: anywhere;
```

- 只在「放不下」时生效：路径按「下一个字符还放得下就继续」折行，栏宽、栅格、其它语言、
  其它页面零改动。
- 不用 `overflow:hidden` / 缩字号：那会把路径裁掉或变小，读者看不到完整端点；
  也不用 `word-break: break-all`（对所有词生效，属过度改动）。

### 8.3 前后对比（实测）

| 语言·主题（1440×900 桌面） | 修前 scrollWidth | 修后 scrollWidth |
| --- | --- | --- |
| es · light（`grid-es-light-desktop-1`） | 1446（溢出 6px，红） | 1440（绿） |
| es · dark（`grid-es-dark-desktop-1`） | 1446（溢出 6px，红） | 1440（绿） |
| en · light（`grid-en-light-desktop-1`） | 1446（溢出 6px，红） | 1440（绿） |
| en · dark | 1446（溢出 6px） | 1440（绿） |

宽度扫描（es / light，同一页面把 `.toc-link` 覆盖回 `overflow-wrap: normal` 的「预修复口径」对照）：

| 视口宽 | 预修复 scrollWidth | 修后 scrollWidth | 备注 |
| --- | --- | --- | --- |
| 1279 | 1279 | 1279 | rail 未出现（走 chips），本来就绿 |
| 1280 | 1286（溢出 6px） | 1280 | rail 刚出现就溢出 |
| 1366 | 1372（溢出 6px） | 1366 | |
| 1440 | 1446（溢出 6px） | 1440 | e2e 矩阵档 |
| 1600 | 1600 | 1600 | 容器到宽上限，rail 右缘离视口更远 |

修后 rail 里这项折成 4 行（`POST` / `/v1/namespaces/{namespace` / `}/events`），
右缘 1408 < 1440，栏内对齐不变。

### 8.4 变异自证 + 门禁

| 项 | 结果 |
| --- | --- |
| 撤掉修复（删掉 `.toc-link` 里那行 `overflow-wrap` + `make fe` + **重建 embeddist 二进制**），跑两条用例 | **红**：`grid-es-light-desktop-1` 与 `grid-es-dark-desktop-1` 都回到 `document horizontal overflow 1446>1440` |
| 恢复修复后跑两条用例（real authority） | **绿**：各 5 assertions 通过 |
| 四语言抽查 `grid-{es,en,ja,zh-CN}-*-desktop-1` | **全绿** |
| `make e2e-one S=ui_shift` 整套 | **绿**：87/87 case · 433/433 视图（368 矩阵视图）· 0 失败 |
| `flock /tmp/anotify-ci.lock -c "make ci"` | **PASS（exit 0）**，见 §8.5 |

（变异在 rebase 到 PR 新头 `448110df`（合入了 main）之后又跑了一遍，同样是 `1446>1440` 两条红；
恢复后两条绿。）

⚠️ 变异复现的两个坑：

1. **e2e 的中枢是编译期内嵌**：`go build -tags embeddist`（`//go:embed all:dist`）把 dist 打进二进制。
   只 `make fe` 重建 dist 而不重建二进制，跑的还是上一轮的 HTML —— 变异会「假绿」。
   变异测试必须 `make fe` **且**重建 `.e2e-bin/anotify` 两个动作都做。
2. **改动已经提交后，`git stash` 是空操作**：它只暂存工作区改动，对已提交的那行无效
   （第一次变异就因此「绿」了一次，假绿）。此时变异要显式把那行删掉再重建产物。

### 8.5 门禁（本轮真实输出）

```bash
flock /tmp/anotify-ci.lock -c "make ci"     # → PASS（exit 0）
make e2e-one S=ui_shift                      # → 87/87 case · 433/433 视图 · 0 失败
```

`make ci` 关键数字：`check-annotations` unbacked 300 / 基线 300、unmarked 385 / 基线 385
（**没涨棘轮**：注释里只写真实存在的端点，旧的死路径没被写进去 → `check-doc-endpoints`
把 `web-src/pages/docs.html` 的注释也一起体检了，这条我第一版注释就红过，见下）；
`check-doc-endpoints` 103 处 `/v1/` 引用全命中、历史提及 14 处；
`check-i18n-keys` 2189 个引用点；`go test ./... -count=1 -shuffle=on` 41 个包 ok / 0 fail；
`contract` PASS。

自证记录（诚实留痕，不是本 PR 的产物）：

- 第一版修复的注释里写了旧端点（v3 的扁平路径）做对照，`check-doc-endpoints` **判红**：
  `web-src/pages/docs.html:544  /v1/events` —— 该文件在扫面内。于是重写注释，只保留 v4 真实端点，
  不引入历史提及（历史提及计数仍是 14，不靠豁免换绿）。
- 修复期间 PR #465 的头前进到了 `448110df`（把 main 合了进来）：本节的改动已 rebase 到它上面重跑，
  `make e2e-one S=ui_shift`（87/87 case · 433/433 视图）与 `make ci`（exit 0）的数字与上表一致；
  变异也在该基线重跑过（两条 es 用例照样 `1446>1440`）。
- 环境事故（不是代码问题，留痕）：中途本机 `/tmp`（tmpfs 7.5G）的**用户配额**被 agent 运行时的临时目录副本
  占满，`go test` 与写入直接报 `disk quota exceeded`（`bootstrap_test.go` / `rework_e2e_test.go` 的
  `write /tmp/...: disk quota exceeded`），清掉 2 小时以上未触碰的运行时副本（76 个，3.0G）后 `make ci` 全绿。
  tmpfs 上的配额耗尽会伪装成一大批「安装脚本/时序测试失败」，别当成回归。
- `make ci` 的第 2 次运行红在一次与本改动无关的时序测试
  （`cmd/anode/internal/runner` 的 `TestRework2ReplayRechecksSecondPendingAfterBlockedFirst/deadline`，
  报「等待 real executions awaiting result ACK 超时」，2.00s 墙钟等待）：单跑该用例与
  `-run TestRework2Replay` 三次全绿，重跑 `make ci` 全绿 —— 判为负载下的既有 flaky，
  与本轮（纯 CSS 注释 + 一行 `overflow-wrap`）无关，未做任何"让它变绿"的改动。

---

## 9. rebase 冲突处置（重放到最新 main）

**触发**：本条分支的头停在 `61324190`，父提交是 `0358cd75`（#464 的合并点）；main 之后又前进到
`a84d4648`（#466 的「管理后台全局事件」文案改造、#468 的 `make test-frontend` 接线）。
rebase 之前 `gh pr view 465` 报 `DIRTY`，`gh pr update-branch 465` 直接拒绝（有冲突）。

**冲突面是机器判出来的，不是猜的**：`git merge-tree --write-tree origin/main origin/pi-subagent/host-workspace/anotify-457-r3-aa45c4fb`
只报一条 `CONFLICT (content): Merge conflict in Makefile`。四份 locale **没有文本冲突** —— 两边改的区域
不重叠（PR 改 `docs.toc` / `docs.agent.step2` / `events.empty`；main 改 `security.toast_regen_failed` /
`admin.messages` / `messagesEmptySub`）。但它们同文件同键集，语义上仍可能互相抹掉，所以另外按键集合判。

| 文件 | 合入方式 |
| --- | --- |
| `Makefile` | 唯一的文本冲突，逐处取**并集**：① `.PHONY` 那一长行两边都要 —— `check-doc-endpoints` 插在 `check-doc-links` 之后（PR 的位置），`test-frontend` 留在行尾（main 的位置）；② `check-doc-endpoints` 目标与 main 的 `test-frontend` 目标插入点不同，git 自己合上了，两个目标都在；③ `ci` 的依赖串取并集 —— `check-doc-endpoints` 在 `check-doc-links` 之后、`test-frontend` 在 `check-affected-map` 之后，两条都留在链上。没有对整文件取 ours 或 theirs。 |
| `web-src/locales/{zh-CN,en,ja,es}.yaml` | git 自动合并（无文本冲突）。判定不靠「看着对」，靠键集合：`scripts/check-i18n-keys.mjs` 判四份键集合一致；再逐文件 `git diff origin/main..HEAD` 确认只含 PR 自己那几行（`docs.toc.notify` 已删，`docs.toc.events` / `docs.agent.step2.desc` / `events.empty` 已换 v4 入口），main 新加的 `admin.messages.{type,source,namespace}` 与 `messagesEmptySub` 一条都没被抹回旧值。 |
| 其余改动文件 | 两侧没有交叉改动，原样重放：本报告 / `scripts/check-doc-endpoints.mjs` / `docs/lint-guards.md` / 两份 README 只在 PR 侧改过；`web-src/pages/docs.html` 与 `scripts/e2e/**` 两侧动的是不同文件（PR 只碰 `.toc-link` 的注释与 `overflow-wrap`，main 碰的是 `affected-map` / `fixtures` / `suites`）。 |

**结果自证**：rebase 后 `git diff origin/main..HEAD --stat` 与原 PR range 的 stat 逐项相同
（同一份文件清单、`844 insertions(+) 37 deletions(-)`）—— 说明重放既没多改也没少改一行。
本轮只做冲突处置与文档，功能语义零改动。

复算命令：

```bash
git merge-tree --write-tree origin/main origin/pi-subagent/host-workspace/anotify-457-r3-aa45c4fb   # 冲突面
git diff --stat origin/main..HEAD                                                                  # 重放后的净改动
node scripts/check-i18n-keys.mjs                                                                   # 四份 locale 键集合一致
```
