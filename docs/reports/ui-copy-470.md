# #470 · 预案/命名空间/源页文案里的协议叙述（GET/PUT/CAS/字段名）清理 + 守卫扩面

<!-- snapshot-doc: 2026-11-21 #470 交付时的定格数字（make ci / e2e 的真实输出与变异自证），复算命令见 §8 -->

> 交付：`scripts/check-ui-copy.mjs` 新增三条**散文态**规则（R1 方法名+端点、R2 CAS/resourceVersion、R3 端点路径/wire 字段名），
> 23 个 key × 4 份 locale（实测 94 条）按用户视角改写，`docs/lint-guards.md` L9 同步。
> 两条已定死的判断点：**409 这类用户真会看到的状态码保留**（Q1）；**豁免面只有三处**——`docs.*` 技术文档页、
> 「原始 YAML（高级）」面板、`<pre>`/`<code>` 里的示例，且一律**显式登记**（Q2，见 §3）。

## 1. 一句话结论

issue 列的 12 个 key 在 main 上**实测 0 条仍在**（PR #479 `85de629c` 已清）；本轮把同源簇扩散到
「端点路径 / wire 字段名」整簇，实测 **P0 = 23 个 key / 94 条**，全部改写完毕；
并把这三类叙述变成**构建期事实**（`make fe` / `make ci` 会红），附四段变异自证。

## 2. issue 12 个 → 实测 P0 23 个：差集说明

issue 正文按「CAS / resourceVersion 一条线」列了 12 个 key（部分行是两个 key 一行）。逐条核对结果：

| 归属 | key 数 | 本轮实测 | 结论 |
| --- | --- | --- | --- |
| issue 点名的 12 条：6 个具名 key 仍在（`docs.v4.group_yaml_note`、`uxaudit.dev.edit_raw_hint`、`nodes.v4.stale`、`pb.v4.draft_preserved`、`sources.v4.rv`、`pb.v4.reloaded`），另 6 条是「行号 + 原文」（`包名已存在…用 PUT 整体更新模板`、`原文来自 GET includeBody=true 的 spec.files`、`命名空间资源与写入 CAS`、`以当前 CAS 保存`、`409：CAS 已过期…`、`只有一条写路径 —— 左侧的 CAS 表单（PUT /v1/namespaces/{name}/config）`） | 12 | **0** | 6 个具名 key 现值为「版本令牌 / 409：版本已过期，先刷新…」（PR #479 `85de629c` 随审计债批量清）；另 6 条里 `nodes.v4.save_current`、`sources.form.yaml_hint`(ns) 这类 key 在 #476 的 `/config` 读投影改造后**已不存在**，其余原文全仓 grep 0 命中。R2 在 HEAD 上实测 0 条（守卫绿即证），本轮不重复改，只把**回归**变成事实 |
| 同源簇多出来的 | 24 | **23**（94 条） | 见下表：issue 只点了「CAS/resourceVersion」，实测同一句式还扩散到**端点路径与 wire 字段名** |

多出的 23 个分四类（差集的口径就是「同一句式换了 token」）；命中按语言分布 = zh 24 · en 23 · ja 24 · es 23
（每条 key 四语同病，差的两条是 `sources.form.hook_url_pending` 在 en/es 里路径被散文判据剔除后只剩 `(e.g. )` 一个 token，不算句子；
按规则分布 = R1 4 条 · R2 0 条 · R3 90 条——R2 的 0 正是 §2 第一行那批已被 #479 清掉的差集）：

| 类 | key 示例 | 为什么算同一类 |
| --- | --- | --- |
| 方法名 + 端点（R1） | `events.empty` | 「用 POST /v1/… 提交第一个」——和 issue 里「保存通过带 CAS 的 PUT」是同一句法：教用户发请求 |
| 端点路径（R3-a） | `sources.form.hook_url_pending` | 「形如 /v1/hooks/src_xxx」——协议路径直接进了说明句 |
| wire 字段名（R3-b） | `sources.v4.name`（`metadata.name`）、`sources.v4.schema`（`config.payloadSchema`）、`sources.v4.installed`（`installedFrom`）、`sources.v4.resolved`（`targetsResolved`）、`sources.v4.blank_credentials`（`hookUrl/secretSet`）、`sources.v4.duplicate`/`dedup`/`uxaudit.res.sources.dedup_summary`（`deliveryId`）、`nodes.v4.override_stale`（`overrideStale`）、`prune.event_schema`（`resultSchema`）、`events.v4.event_uid`（`deliveryId`）、`sources.v4.sample`/`payload`/`suggest_*`（`config.sample` / `config.payload` / `payloadSchema.properties`）、`sources.v4.schema_hint` / `sources.form.result_schema*`（`payloadSchema` / `event_schema`）、`sources.v4.no_name`（`metadata.name`） | 用户看不到 `spec.`/`metadata.` 这一层，看到的是「资源名」「载荷契约」「去重键」 |
| 面板块的型别抬头 | ``nodes.v4.yaml_title`` | 「节点 YAML（apiVersion anotify/v1 · kind Node）」——按 Q2 属「原始 YAML（高级）」面板，**登记豁免**（§3） |

另外两类**不计入 P0**（说明清楚免得当下次误报）：

- `docs.toc.events`（`POST /v1/namespaces/{namespace}/events`）这类**纯端点值**：没有句子，只是表格/TOC 的一格 ⇒ 由「散文判据」放行（§3），不是豁免；
- `docs.design.playbooks`、`docs.design.sources`、`docs.events.params.idem`、`docs.v4.resource_note` 这类：**只被 `docs.html` 引用** ⇒ 文件级豁免（§3）。

## 3. 守卫扩面（`scripts/check-ui-copy.mjs`）

### 3.1 三条规则（只新增，不删既有规则/断言）

| id | 判据 | `minHits` | 反例（自测红样例） |
| --- | --- | --- | --- |
| `method-endpoint-prose` | `GET/POST/PUT/PATCH/DELETE` 后 24 字符内出现 `/路径` | 4 | 「还没有事件。用 POST /v1/… 提交第一个。」「安装是单独的 POST …/install」 |
| `cas-prose` | 句子里出现 `CAS` / `resourceVersion` | 4 | 「以当前 CAS 保存」「409：CAS 已过期，先重载再保存。」 |
| `wire-token-prose` | 句子里出现 `/v1/…`、`spec./status./metadata./config.` 或显式的 wire 字段名（`deliveryId`、`payloadSchema`、`resultSchema`、`idemKey`、`installedFrom`、`targetsResolved`、`hookUrl`、`secretSet`、`appliedRevision`、`configRevision`、`apiVersion`、`overrideStale`、`event_schema`） | 5 | 「资源名 metadata.name」「形如 /v1/hooks/src_xxx」「deliveryId 默认去重 30 天」 |

**「散文」判据**（窄口径的关键，写在 `isProse()` 里）：中文/日文有一个汉字或假名即算句子；拉丁文要
**≥3 个词**（端点路径与 `{占位}` 先剔除，不算词）。于是：

- 绿：`POST /v1/namespaces/{namespace}/events`（纯值，2 token）、`resourceVersion`（纯字段名标签，dev 面板里合法的就是它本身）、`409：你手里的版本旧了，先重载再保存。`（Q1 的可执行信息，不含协议词）；
- 红：`Event UID (not deliveryId)`（4 词）、`还没有事件。用 POST /v1/… 提交第一个。`（中文）。

**minHits 怎么断言**：`minHits` 是常量，`--self-test` 统计每条规则在**内置红样例**上的命中数，
低于 `minHits` 立即红（正则写坏成永不命中 → 自测红）。真实语料修完后命中必为 0，不能当下限，
所以下限钉在红样例上；`--self-test` 每次打印实测值：

```
#470 散文规则命中数（下限 minHits）：method-endpoint-prose=4/4 · cas-prose=6/4 · wire-token-prose=8/5
```

### 3.2 豁免面（Q2：显式登记，禁止通配）

| 面 | 实现 | 判据 |
| --- | --- | --- |
| `docs.*` 技术文档页 | `FILE_ALLOWLIST` + **引用面推导** | 页面：`web-src/pages/docs.html` 整页不扫这三条规则；locale 值：某 key 的**全部**静态引用点都落在白名单文件里才豁免（`sources.v4.dedup` 同时被文档页和源页引用 ⇒ 不豁免，照样改） |
| 「原始 YAML（高级）」面板 | `EXEMPT_KEYS`（**精确 key**，每条带理由） | 面板不是文件，按 key 登记；`--self-test` 断言登记表里没有 `*`，主流程断言登记的 key 真的存在（过期即红） |
| `<pre>`/`<code>` 等示例 | 结构判据 `inLiteralContext()` / `blankLiterals()` | 只认 `pre/code/kbd/samp` 与 `code-body`/`doc-code` 类；**「开发者信息」折叠（`class=dev*`）不在豁免面**——`uxaudit.dev.*` 是内部可见文案，照查 |

反假绿：新增 `散文检查面 ≥ 70` 个页面文本节点下限（实测 76）；`--self-test` 另有四条断言钉住豁免的**边界**：
docs 页同一句话绿、别的页同一句话红；key 只被 docs 引用才豁免、多一个引用点就红；无人引用的 key 不豁免；
面板抬头按登记表豁免。

## 4. 冻结 inventory（先落表，后改文案）

`web-src/locales/*.yaml` 行号是 **HEAD（`0dadd143`）** 上的位置；四份 locale 同键同病（en/ja/es 的行号见第二列）。

| key | HEAD 行号（zh/en/ja/es） | 改前（zh-CN 原文） | 规则 |
| --- | --- | --- | --- |
| events.empty | 1251 / 1251 / 1251 / 1251 | 还没有事件。用 POST /v1/namespaces/{namespace}/events 提交第一个。 | R1, R3 |
| events.v4.event_uid | 1300 / 1300 / 1300 / 1300 | 事件 UID（非 deliveryId） | R3 |
| prune.event_schema | 1612 / 1612 / 1612 / 1612 | 事件 payload 不符合源的 resultSchema | R3 |
| nodes.v4.override_stale | 1770 / 1768 / 1768 / 1768 | overrideStale：下层默认值更新，但本机覆盖仍生效 | R3 |
| sources.v4.name | 1866 / 1864 / 1864 / 1864 | 资源名 metadata.name | R3 |
| sources.v4.sample | 1874 / 1872 / 1872 / 1872 | 提示样例 config.sample（不参与派发） | R3 |
| sources.v4.payload | 1877 / 1875 / 1875 / 1875 | 静态载荷 config.payload（JSON 对象） | R3 |
| sources.v4.schema | 1878 / 1876 / 1876 / 1876 | 载荷契约 config.payloadSchema（可选） | R3 |
| sources.v4.schema_hint | 1879 / 1877 / 1877 / 1877 | 每次投递先检查 payloadSchema；不合规事件仍入库并留下 event_schema 剪枝，不派发。 | R3 |
| sources.v4.suggest_sample | 1895 / 1893 / 1893 / 1893 | 来自 config.sample（仅提示） | R3 |
| sources.v4.suggest_schema | 1896 / 1894 / 1894 / 1894 | 来自 payloadSchema.properties | R3 |
| sources.v4.suggest_payload | 1897 / 1895 / 1895 / 1895 | 来自 config.payload | R3 |
| sources.v4.blank_credentials | 1875 / 1873 / 1873 / 1873 | 编辑时 token/secret 留空不提交，保留已有凭据；读取只有 hookUrl/secretSet。 | R3 |
| sources.v4.duplicate | 1901 / 1899 / 1899 / 1899 | 重复 deliveryId：返回原事件/trace，不重新派发，也不保证任务成功。 | R3 |
| sources.v4.dedup | 1902 / 1900 / 1900 / 1900 | deliveryId 默认去重30天：同源同 id 只表示已接收，不保证下游成功；修改 payload/type 或需要重新执行，请用新 id。窗口过期后同 id 可再次接收。无 replay API。 | R3 |
| sources.v4.installed | 1929 / 1927 / 1927 / 1927 | 安装来源 installedFrom | R3 |
| sources.v4.resolved | 1925 / 1923 / 1923 / 1923 | targetsResolved（布尔） | R3 |
| sources.v4.no_name | 1937 / 1935 / 1935 / 1935 | 链接缺少 metadata.name。请从源列表打开详情。 | R3 |
| sources.form.result_schema | 2022 / 2020 / 2020 / 2020 | 事件 payload 契约（resultSchema，可选） | R3 |
| sources.form.result_schema_hint | 2023 / 2021 / 2021 / 2021 | 可选。填一份 JSON Schema（如 {...}）后…trace 里留一条 event_schema 剪枝。留空 = 不校验。 | R3 |
| sources.form.hook_url_pending | 2041 / 2039 / 2039 / 2039 | 保存后生成（形如 /v1/hooks/src_xxx） | R3 |
| uxaudit.res.sources.dedup_summary | 2489 / 2485 / 2485 / 2485 | 投递去重与窗口（deliveryId） | R3 |
| `sources.v4.yaml_create.hint` | 1916 / 1914 / 1914 / 1914 | 粘贴一份 Source 文档。`spec.config.token` / `secret` 只写不读，保存后只显示 hookUrl。 | R3 |

改后（zh-CN，其余三语同批同义）：

| key | 改后 |
| --- | --- |
| events.empty | 还没有事件。Agent 或脚本按接入文档上报后，事件会实时出现在这里。 |
| events.v4.event_uid | 事件 UID（与去重键不同） |
| prune.event_schema | 事件载荷不符合源声明的载荷契约 |
| nodes.v4.override_stale | 下层默认值已更新，但本机覆盖仍生效 |
| sources.v4.name | 资源名 |
| sources.v4.sample | 提示样例（不参与派发） |
| sources.v4.payload | 静态载荷（JSON 对象） |
| sources.v4.schema | 载荷契约（可选） |
| sources.v4.schema_hint | 每次投递都先按它校验：不合规的事件照常入库、但不派发，链路里会标一条「载荷不合契约」。 |
| sources.v4.suggest_sample | 取自示例载荷（仅提示） |
| sources.v4.suggest_schema | 取自载荷契约里的字段 |
| sources.v4.suggest_payload | 取自静态载荷 |
| sources.v4.blank_credentials | 编辑时把令牌/密钥留空就不会改动，已有凭据保留；页面上只显示 hook 地址与「签名已设置」。 |
| sources.v4.duplicate | 去重键重复：返回原来的事件与链路，不重新派发，也不保证任务成功。 |
| sources.v4.dedup | 去重键默认保留 30 天：同一来源的同一个键只表示「已接收」，不保证下游执行成功；改了内容或要重新执行，请换一个新键。窗口过后同一个键可以再次接收；不支持重放。 |
| sources.v4.installed | 安装来源 |
| sources.v4.resolved | 目标解析 |
| sources.v4.no_name | 链接里缺少源名。请从源列表重新打开详情。 |
| sources.form.result_schema | 事件的载荷契约（可选） |
| sources.form.result_schema_hint | 可选。填一份 JSON Schema（如 {...}）后，每次投递都先按它校验：不合规的事件照常入库、但不派发，链路里标一条「载荷不合契约」。留空 = 不校验。 |
| sources.form.hook_url_pending | 保存后生成（形如 `<code>/v1/hooks/src_xxx</code>`）——路径是**字面量值**，进 `<code>` 后由结构判据放行，信息一分不少 |
| uxaudit.res.sources.dedup_summary | 投递去重与窗口 |
| `sources.v4.yaml_create.hint` | 粘贴一份 Source 文档。`spec.config.token` / `secret` 只写不读，保存后只显示 hook 地址。 |

**语义保住的清单**（Q1 的判据）：`409`（能力删除冲突、节点保存冲突）原样保留；「缺令牌 400 / 旧令牌 409 + 保留草稿」
这一组只在**文档页**（豁免面）出现；产品页里改成「版本旧了先重载再存」；`/v1/hooks/src_xxx` 作为字面量进 `<code>`。

**顺带修掉的重复文案**（页面 JS 的兜底串是同一句话的第二份拷贝，不在守卫扫描面内，见 §7）：
`web-src/pages/capability.html` 三处 `resourceVersion` → `版本令牌`；`web-src/pages/source-detail.html`
两处兜底串（`载荷契约（可选）`、`链接里缺少源名…`）。

**死词条说明**：23 个 key 里有 7 个按 `check-i18n-keys` 口径没有任何静态引用点
（`events.empty`、`events.v4.event_uid`、`sources.v4.resolved`、`sources.form.result_schema`、
`sources.form.result_schema_hint`、`sources.form.hook_url_pending`、`uxaudit.res.sources.dedup_summary`；
`sources.v4.suggest_*` 则由 `web/source-editor.js` 的动态前缀 `sources.v4.*` 生成，是活词条）。
本轮**不删 key**（保持四份 locale 键集不变、改动可逆），只把值改干净；「`sources.form.*` 整块疑似死代码」
另立后续（§7）。

## 5. 变异自证（四段，命令 + 输出 + exit code）

全部在 `scripts/check-ui-copy.mjs` 的真实扫描面上做（locale 注入点：`web-src/locales/zh-CN.yaml` 的
`common.request_failed`；M4 注入点：`web-src/pages/nodes.html` 的 `{{define "content"}}` 内）。回滚用字节级
备份还原（`cp` 备份文件），还原后重跑为绿。

```text
M1 R2 回灌（CAS + 中文虚词同句）
$ python3 -c "…zh-CN.yaml: request_failed: '请求失败：保存时以当前 CAS 为准就说明过期了'…"
$ node scripts/check-ui-copy.mjs
✗ 文案违规：2 条（用户可见文案里混进了开发过程叙述）
      [cas-prose] CAS / resourceVersion 是协议令牌名：界面说「保存时服务端会校验版本，过期就让你先重载」，409 这类用户真会看到的状态码照留   (key: common.request_failed)
exit=1

M2 R3 回灌（端点路径进说明句）
$ node scripts/check-ui-copy.mjs
✗ 文案违规：1 条（用户可见文案里混进了开发过程叙述）
      [wire-token-prose] 端点路径与 wire 字段名（spec./metadata./deliveryId…）不进用户说明句：要说的信息用用户视角写出来   (key: common.request_failed)
exit=1

M3 R1 回灌（方法名 + 端点）
$ node scripts/check-ui-copy.mjs
✗ 文案违规：3 条（用户可见文案里混进了开发过程叙述）
      [method-endpoint-prose] 散文里给用户派活：方法名 + 端点只该出现在代码样例或接入文档；界面说「点哪里、会怎样」   (key: common.request_failed)
exit=1

M4 同一批文本放进页面 <pre> → 仍绿（证明不是靠「全员变红」过关）
$ node scripts/check-ui-copy.mjs
✅ 文案守卫通过：4 份 locale / 9340 条值 · 22 个页面 / 220 个文本节点（实现词检查面 100 个 · 散文检查面 76 个）· …
exit=0
# 注意 217 → 220：注入的三行**确实被抽成了文本节点**（不是没扫到），只是处在 <pre> 里由结构判据放行。

回滚后
$ node scripts/check-ui-copy.mjs
✅ 文案守卫通过：4 份 locale / 9340 条值 · 22 个页面 / 217 个文本节点（实现词检查面 100 个 · 散文检查面 76 个）· …
exit=0
```

**M5（附）：minHits 下限真的会拦「正则写坏」** —— 把 `cas-prose` 的正则临时改成永不命中：

```text
$ node scripts/check-ui-copy.mjs --self-test
  ✗ 散文规则 cas-prose 只被 0 条样例命中（minHits 4）：正则写坏了？
  #470 散文规则命中数（下限 minHits）：method-endpoint-prose=4/4 · cas-prose=0/4 · wire-token-prose=8/5
  ✗ 自测失败 6 条
exit=1

$ git checkout -- scripts/check-ui-copy.mjs && node scripts/check-ui-copy.mjs --self-test
  ✅ 自测通过：红样例全红、绿样例全绿、豁免正反都对
exit=0
```

## 6. 门禁（实跑输出）

| 命令 | 结果 |
| --- | --- |
| `make check-ui-copy` | 两段绿：`--self-test` **40 红 / 20 绿 / 5 豁免** + 页面 **11 红 / 14 绿 / 4 豁免**；判定 **4 份 locale / 9340 条值 · 22 个页面 / 217 个文本节点**（实现词检查面 100 · 散文检查面 76） |
| `make check-i18n-keys` | 4 份 locale 键集合一致；2110 个引用点全部有词条（793 个未引用 key 仅告警） |
| `make check-comment-density` | 与基线完全一致（注释行 43372 / 基线 43372）—— 本轮守卫注释按**同行注释**写，注释行零净增 |
| `make ci` | **exit 0**（契约静态断言 / check-agents / staticcheck / `make fe`（指纹 33 资源 88 HTML）/ `go test ./...` 全包 ok / tmp-leak 守卫 / harness 自测） |
| `make e2e-affected` | **exit 0**：`✅ E2E 全绿：16/16 个请求套件均达标（断言 1070 通过 / 0 失败，authority 清单覆盖 16 个）`；临时目录零残留（隔离 TMPDIR 本次新增 = 0） |

选中的 16 套（改动面 = locale + 两个页面 + 守卫脚本）：

```
i18n 33 · i18n_coverage 44 · vars_ui 74 · lang_hint 86 · cli_auth 133 · passkey_enroll 85 · admin_flow 52
deeplink 50 · auth_flow 49 · ux_regression_19_23 8 · recovery_next 16 · events_paging 179
frontend 43 · p2_guards 53 · <登录套件> 78 · ui_shift 87          ← 每格 = 通过数，失败全 0
```

## 7. 剩余盲区（守卫抓不到的，写清楚别装已覆盖）

1. **页面内联 JS 的兜底串**：`t("key", "兜底文案")` 的第二参是第二份拷贝，不在扫描面内（本轮手工对齐了
   `capability.html` / `source-detail.html` 里那 5 处）。守卫要覆盖它得解析 JS 实参，属另一个面。
2. **`transform` 出来的句子**：`dev/drift`（`'配置未对齐：配置版本 {config}…'`）用 `replace` 拼装，句子由模板 + 值组成；
   词表扫的是**值**，拼装后的完整句子两面都看不到。
3. **`web/mock.js` 的 API 响应文案**（如 `metadata.name 不可变`）：那是 mock 服务端回的 payload，不是 locale 词条，
   `t("name_immutable", "…")` 的 key 也不是扁平 key（`check-i18n-keys` 记作「非扁平字面量」）。本轮不动。
4. **词表是窄口径**：`POST` 单独出现（`Agent / CI 用来 POST 通知`、`外部系统 POST 到它`）不判红——它在这两句里是
   动词/接口动作，判红会误伤「这个 URL 能收什么」。同样 `?traceId=` 这类**用户要手打的 URL 参数**保留原样
   （可执行信息，不是实现叙述）。
5. **判据只看「句子形态」**：`sources.v4.hmac`（「外部 POST 使用 {type?, id?, payload?} 包装体…X-Anotify-Signature」）
   没有 `spec.`/`/v1/`/方法+路径，三条规则都不命中——它仍是偏协议的说明。收它的正确办法是把它挪进接入文档
   或 `<pre>`，不是放宽正则（放宽会开始误伤文档页）。
6. **reason code 仍是协议态**：`artifact_expired`、`capability_disabled`、`no_target`、`resource_version_conflict`、`heartbeat_sec` 这类 snake_case 出现在中文句里（实测 5 处：`message.v4.artifact_expired`、`caps.act.disable_body`、`caps.conflict.title`、`sources.v4.no_target`；另 `docs.ws.c2s.comment3` 在文档页豁免面）。它们与 API 的 `reason` 一一对应，是用户报障时才会抄的那串字 —— 改它会让界面和日志对不上，所以本轮**不判红也不改**；要不要收、收到哪，需要一份「reason code 展示位」的决策，不是放宽/收紧正则能解决的。
7. **死词条的「干净」不算覆盖**：§4 那 7 个无引用 key 改干净了，但用户看不到；真正的收益在
   `sources.v4.*` / `nodes.v4.override_stale` / `prune.event_schema` 这些活词条上。`sources.form.*` 整块
   （约 50 个 key）疑似旧表单遗留，删它的判据要单独确认（本轮只保证不教坏话）。

## 8. 复算

```text
node scripts/check-ui-copy.mjs --self-test   # 40 红 / 20 绿 / 5 豁免 + 页面 11 红 / 14 绿 / 4 豁免
node scripts/check-ui-copy.mjs               # 4 份 locale / 9340 条值 · 22 个页面 / 217 节点 · 散文检查面 76
node scripts/check-i18n-keys.mjs             # 4 份键集合一致
node scripts/check-doc-endpoints.mjs         # 文档页里的端点仍全部可解析
make check-ui-copy && make check-i18n-keys
```
