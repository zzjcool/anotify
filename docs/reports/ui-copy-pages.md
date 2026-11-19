<!-- snapshot-doc: 2026-11-19 #458 交付时的定格数字（make ci / check-ui-copy 的真实输出），复算命令见 §6 -->
# 页面模板里的开发叙述 + `check-ui-copy` 扫描面扩到 `pages/*.html`（#458）交付报告

- Issue：[#458](https://github.com/zzjcool/anotify/issues/458)
- 分支：`pi-subagent/hw-nj/anotify-458-608fea9c`
- 基线：`origin/main` @ `76ed9311`
- PR：[#469](https://github.com/zzjcool/anotify/pull/469)（`Fixes #458`）

一句话：**用户可见的开发叙述还剩 `make dev` ×4 与按钮「PUT 整体保存模板」**（第三项「写给维护者与排障者」已在
#448 修掉，本轮核对确认），**但这轮的主要价值是把扫描面从「只有 locale 值」扩到「页面模板的非注释文本节点」**——
散文本来就会直接落在页面上（审计时那句最露骨的话就写在 `docs` 页的 `<p>` 里），只扫 locale 等于漏一半。
扩面同时补了反假绿下限（页面数 / 文本节点数）与两侧变异自证。

---

## 1. 现象与根因

Issue 列了三类文案。逐条实测（基线 `76ed9311`）：

| Issue 里的条目 | 基线实况 | 本轮处置 |
| --- | --- | --- |
| `make dev` ×4 | **还在**：`receivers.pair.no_backend`、`keys.error.create_failed`、`security.alert.backend_down`、`passkeyenroll.panel.error_backend_desc`，四语共 16 条值 | 删掉「跑 make dev」这半句（见 §2） |
| 按钮「PUT 整体保存模板」 | **还在**：`pb.v4.save_template` ×4 语 | 改成「保存整份模板」等（见 §2） |
| 「写给维护者与排障者」 | **已不在**：`docs.design.lede` 四语已由 #448（`2969df84`）改成「下面这几条是产品行为里最容易误读的地方…」，本轮未再动 | 核对后不动 |

根因不是这几句，而是**扫描面**：`scripts/check-ui-copy.mjs`（#448 落地）只扫 `web-src/locales/*.yaml` 的值，
理由是「页面里没有散文」。那条实测只对**改完之后**的页面成立，却把「现在没有」当成了「不会有」：
审计报告 F-73 里那句「下面这些是写给维护者与排障者的说明」当时就写在 `web-src/pages/docs.html` 的 `<p>` 里，
还有「为什么界面这样显示」这种标题也在页面上。**散文的落点本来就有两个，检查面只有一个 = 半盲。**

## 2. 改了什么：文案 5 个 key × 4 语 = 20 条值

四份 locale 同键同改，语义对齐（不是逐语直译）：

| key | 语言 | 改前 | 改后 |
| --- | --- | --- | --- |
| `receivers.pair.no_backend` | zh-CN | 后端未连接，无法获取 VAPID 公钥。请确认服务端已启动（本地可运行 make dev）后重试。 | 后端未连接，无法获取 VAPID 公钥。请确认服务端已启动后重试。 |
| | en | … Make sure the server is running (run make dev locally), then retry. | … Make sure the server is running, then retry. |
| | ja | …（ローカルなら make dev で起動できます）。 | …（括弧ごと削除） |
| | es | … (en local, make dev) y vuelve a intentarlo. | … y vuelve a intentarlo. |
| `keys.error.create_failed` | zh-CN | 创建失败：{msg}。确认服务端已启动后重试（本地可运行 make dev 起服务）。 | 创建失败：{msg}。确认服务端已启动后重试。 |
| | en | … then retry (run make dev locally to start one). | … then retry. |
| | ja/es | 同上（括弧里是 make dev 指引） | 删括弧 |
| `security.alert.backend_down` | zh-CN | 无法连接后端。请确认服务端已启动并通过 HTTPS 访问（本地可运行 make dev 起服务），再尝试添加 Passkey。 | 无法连接后端。请确认服务端已启动并通过 HTTPS 访问，再尝试添加 Passkey。 |
| | en/ja/es | 同形（父级括号里是 make dev 指引） | 删括弧 |
| `passkeyenroll.panel.error_backend_desc` | 四语 | 同形 | 删括弧 |
| `pb.v4.save_template` | zh-CN | PUT 整体保存模板 | 保存整份模板 |
| | en | PUT whole template | Save whole template |
| | ja | PUT でテンプレート全体を保存 | テンプレート全体を保存 |
| | es | Guardar plantilla completa con PUT | Guardar plantilla completa |

改法口径：**说「是什么、要做什么」，不说「用哪条命令 / 哪个 HTTP 方法」**。后端没起来的提示留「确认服务端已启动 +
HTTPS」，删掉用户做不到的那半句；按钮去掉 HTTP 方法，只留动作。没有顺手改别的页面文案
（同屏的 `pb.v4.template_hint` 里还有 GET/PUT/CAS 叙述，属新问题，见 §7）。

## 3. 扫描面怎么扩的

`scripts/check-ui-copy.mjs` 现在有两个面，共用同一张词表（21 条规则）：

| 面 | 抽什么 |
| --- | --- |
| ① `web-src/locales/*.yaml` | 值（跳过注释行与 key 名，值里的 HTML 标签不剥）——#448 原有面 |
| ② `web-src/pages/*.html` | **文本节点**：`scanPage()`，见下 |

页面侧「哪些字算用户可见」是逐条可复算的（正反都在 `--self-test` 的页面样例里）：

| 判据 | 做法 | 为什么 |
| --- | --- | --- |
| 排除 HTML 注释 | `<!-- … -->` 整段跳过 | 写给维护者的注释本来就该在（`--self-test` 有绿样例） |
| 排除模板逻辑 | `{{t "…"}}` / `{{if …}}` / `{{range …}}` 整段跳过（不是当作文本） | 表达式是 key/条件，不是渲染给用户的字 |
| 排除脚本与样式 | `<script>` / `<style>` 的内容跳过 | 客户端代码不是文案（`--self-test` 有绿样例） |
| 排除标签本身 | 状态机跳过 `<…>`，**引号里的 `>` 不算结束** | 属性名 / 属性值 / 类名都不是文本节点；SVG 的 `d="…"` 里有 `>` 形状的坑 |
| 排除未渲染的 define | 先用 `defineRanges()` 配对 `{{define}}…{{end}}`（体内 `if/range` 的 `end` 也参与配对），只保留被 layout 的 `{{block "X"}}` / `{{template "X"}}` 接进渲染树的 define 体 | sitegen 只执行 `__layout__`（`internal/sitegen/template.go` 的 `execute`），页面顶层文字与死 define 永远进不了产物 —— 比如 `docs.html` 里没人引用的 `{{define "fonts-extra"}}`，它的体是一段 URL 碎片 |
| 保留其余 | 文本节点逐行归一化后过词表（含 `<pre>` 里的代码样例） | 代码样例真的显示给用户；词表窄，实测零误报 |

新增一条规则 `build-command`：#458 之前 `make dev` 被当成「自托管排障指引的正常文案」整条排除，
现在按 **Makefile 里本仓特有的 target 白名单**查（`dev|fe|ci|sitegen|e2e|docker|anode|manifest|tailwind|lint-go|contract|integration`）。
`make test` / `make build` / `make up` **故意不收**：容器安装提示里举的就是「用户自己项目里跑 make test」这种例句
（`nodes.register.install_intro`），同形词宁可漏也不误伤。英文的 make it / make sure 是动词，不在白名单（写成行内反引号会被 `check-annotations` 当成 `make <target>` 引用，所以这里不加）。

**反假绿（下限断言）**：`FLOORS = { files: 4, values: 9000, pages: 20, pageNodes: 160 }`。
基线实测 **4 份 locale / 9280 条值 · 23 个页面 / 222 个文本节点**；页面与文本节点数留了删页/改版的余量，
但**不允许归零**——「扫到 0 条却报绿」正是这个检查最怕的假绿形态。另外：

- 页面绿样例在自测里**钉住抽到的节点数**（`nodes: N`），不是只看「没违规」；
- 页面红样例**断言规则 id 与行号**（遮蔽若把行号搞丢，审阅者就找不到原文）；
- locale / layout / pages 目录读不到 ⇒ 退出码 2（不是「通过」）。

## 4. 变异自证（本轮实测）

### 4.1 把一句开发叙述**写回页面模板** ⇒ 红

在 `web-src/pages/docs.html`（`{{define "content"}}` 里、`#design-notes` 之前）插入
`<p>下面这些是写给维护者与排障者的说明，本地可运行 make dev 起服务。</p>`：

```
$ node scripts/check-ui-copy.mjs; echo "guard_exit=$?"
✗ 文案违规：2 条（用户可见文案里混进了开发过程叙述）
  web-src/pages/docs.html
    web-src/pages/docs.html:1322 → «make dev» → 下面这些是写给维护者与排障者的说明，本地可运行 make dev 起服务。
      [build-command] 构建/开发命令：用户手上没有本仓，跑不了 make；说「重启服务」而不是「跑 make dev」   (页面文本节点)
    web-src/pages/docs.html:1322 → «写给维护者» → 下面这些是写给维护者与排障者的说明，本地可运行 make dev 起服务。
      [zh-dev-audience] 对维护者/开发者喊话：产品界面不设这个读者   (页面文本节点)
guard_exit=1
```

### 4.2 同一句塞进 **HTML 注释** ⇒ 不红（证明注释被正确排除）

`<!-- 正常文案进注释（变异 2）：下面这些是写给维护者与排障者的说明，本地可运行 make dev 起服务。 -->`
插在同一位置：

```
$ node scripts/check-ui-copy.mjs; echo "guard_exit=$?"
✅ 文案守卫通过：4 份 locale / 9280 条值 · 23 个页面 / 222 个文本节点 · 21 条规则（下限 locale≥4 份、值≥9000；页面≥20 个、文本节点≥160）
guard_exit=0
```

### 4.3 扫描面被改窄 ⇒ 下限拦住（不是绿）

```
$ node scripts/check-ui-copy.mjs --src=/tmp/emptyweb   # locales/layouts 在，pages/ 是空的
✗ 文案守卫：扫描面不足（这是假绿，不是通过）
   · 只扫到 0 个页面（下限 20）
   · 只抽到 0 个页面文本节点（下限 160）
exit=1

$ rm -rf /tmp/emptyweb/layouts && node scripts/check-ui-copy.mjs --src=/tmp/emptyweb
✗ 读不到 layout 目录 /tmp/emptyweb/layouts（页面文本节点的可见性靠它判定）：ENOENT …
exit=2
```

两次变异都还原了（`git diff --stat web-src/pages/` 为空），仓库里没有留下被变异的页面。

## 5. 自测（`--self-test`）

```
$ node scripts/check-ui-copy.mjs --self-test
check-ui-copy --self-test
  规则 21 条；红样例 20 / 绿样例 13 / 豁免 5
  页面样例：红 6 / 绿 7 / 豁免 4
  ✅ 自测通过：红样例全红、绿样例全绿、豁免正反都对
```

页面样例覆盖：散文写进 `<p>`（红）、`<pre>` 代码样例里塞旁白（红）、HTML 注释（绿）、`<script>`/`<style>`（绿）、
属性名/属性值/类名（绿）、`{{if}}`/`{{range}}`（绿）、未进渲染树的 define 体（绿）、被 layout 引用的 define（红）、
`<!-- ui-copy-ok: 理由 -->`（同行/上一行绿，空理由红，写在文本里不算数）。

## 6. 复算命令

```
node scripts/check-ui-copy.mjs --self-test   # 20 红 / 13 绿 / 5 豁免 + 页面 6 红 / 7 绿 / 4 豁免
node scripts/check-ui-copy.mjs               # 4 份 locale / 9280 条值 · 23 个页面 / 222 个文本节点
grep -rn "make dev" web-src/locales web-src/pages   # 0 命中
flock /tmp/anotify-ci.lock -c "make ci"      # 全绿（见 §7）
make e2e-affected                            # 改动选择器选中 frontend / i18n / i18n_coverage / ui_shift
```

## 7. 门禁（实跑输出）

`flock /tmp/anotify-ci.lock -c "make ci"`：**exit 0**（`check-ui-copy` 两段都绿、`check-guard-selftest` 绿、
`check-annotations` 通过且棘轮未超基线：unbacked 300 / 300、unmarked 385 / 385）。

`make e2e-affected`：改 `Makefile` 在映射表里没有分类 ⇒ 选择器**保守跑全套**（28 套），结果全绿：

```
✅ E2E 全绿：28/28 个请求套件均达标（断言 2370 通过 / 0 失败，authority 清单覆盖 28 个）
   ✅ contract 185 · security 100 · ws_protocol 134 · i18n 33 · i18n_coverage 44 · lang_hint 86
   ✅ cli_auth 133 · passkey_enroll 85 · admin_flow 52 · capacity 84 · deeplink 50 · auth_flow 49
   ✅ ux_regression_19_23 8 · recovery_next 16 · events_paging 178 · frontend 43 · p2_guards 30
   ✅ password_login 78 · ui_shift 87（其余 api_contract / routing / persistence / edge_cases /
      orphans / deploy / live_agent / node_v4 同批通过，合计 28 套）
   ✅ 临时目录零残留（隔离 TMPDIR 内本次新增 = 0）
```

## 8. 已知盲区与后续

已知盲区（有意为之，写在这里免得下次当 bug 报）：

- 页面**内联脚本里 `A.t("…")` 的动态文案**不在扫描面（key 由 locale 面覆盖，插值/拼接后的句子两面都看不到）；
- 只支持 Go `html/template` 的 action 形态（`{{…}}`）；表达式里出现字面 `}}` 时配对会偏（本仓 0 处）；
- 词表是**窄口径**：`PUT` / `GET` / `CAS` 这类方法名不在词表里（代码样例里合法），所以 `pb.v4.template_hint`
  那句「保存通过带 CAS 的 PUT 整体替换模板」不会被抓——它是同一类开发叙述，属新问题，已另开 [#470](https://github.com/zzjcool/anotify/issues/470)。

顺带发现（未在本轮修）：`check-wire-fields` 仍未覆盖 `web-src/pages/*.html`（`docs/lint-guards.md` §4 第 7 条已登记），
与本次扩面是同一类「页面内联代码没有检查面」的缺口。
