# 顶栏命名空间 ≡ 页面命名空间（#455）—— 交付报告

- Issue: [#455](https://github.com/zzjcool/anotify/issues/455)（P1 · 写错对象）
- 证据: [`docs/reports/ui-audit-blackbox.md`](ui-audit-blackbox.md) B-2（黑盒审计 Top1）
- 基线: `origin/main` @ `370fcd69`
- 分支: `pi-subagent/hw-nj/anotify-455-c448d702`
- PR: 见本分支的 PR（标题含 `Fixes #455`）
- 范围: **只修 #455**；#453 / #454 / #456 / #457 / #458 一行未动。

## 0. 选哪一个方案：**A. 顶栏切换 → 跳转 `namespace.html?name=<新ns>`**

issue 给了二选一（跳转 / 本页禁用顶栏选择器），本轮取**跳转**，理由按重要性排序：

1. **顶栏切换本来就是一次「跳页导航」动作。** `switchScope()` 的最后一句在所有页面上都是
   `location.reload()`：它从来不「就地改当前页」，只负责换范围再重取。既然动作语义是导航，
   对一个「这一页的身份就是某个 ns」的页面，正确的落点就是**那个 ns 的同一页**，而不是把控件藏掉。
2. **禁用选择器会让「本页没有命名空间概念」这个错觉成立。** namespace.html 的内容
   （vars 编辑器 / 计数 / 逐节点有效值 / 只读 YAML）100% 属于一个命名空间。把顶栏 switcher
   在本页藏掉，用户看到的是一个空槽位 + 一条「顶栏只影响其它页」的横幅，等于用一句解释
   去掩盖一个本来可以不存在的差异；而且要新增 4 份 locale 文案。
3. **跳转把「显示 A、写 B」的两个方向收成同一条规则：URL 是唯一真源。**
   禁用只堵住了 issue 里那一个方向（顶栏 → 页面），反向（页面内下拉改了 ns、顶栏还停在旧值）
   仍然是一页两套「当前命名空间」。本轮把两个方向都收掉了（§1.2 的 ③）。

## 1. 机制

### 1.1 修复前的两条不一致（代码级，逐条对得上 issue 的复现）

| # | 动作 | 旧行为 | 结果 |
| --- | --- | --- | --- |
| ① | 顶栏切到 prod | `switchScope()` 只改 `localStorage` 与 `?namespace=`；`?namespace=` 不存在时**只 reload、不换身份** | 顶栏 `#prod`、页面 `?name=default` 继续写 `default`（issue 原文） |
| ② | 页内下拉切到 prod | 页面只 `replaceState` 改 `?name=` + 就地重载数据，**不落盘 scope、不重画顶栏** | 页面写 `prod`、顶栏 `#default` 与切换器勾选仍是 `default` |

两条都来自同一个根因：**「当前命名空间」有两个可写面（全局 scope / 页面 URL 参数），没有约定谁是真源。**

### 1.2 修复后的契约（`mountLayout({ scopeParam })`）

新增一个**显式声明**（不是给所有页面加魔法）：页面若用非 `?namespace=` 的 query 表达范围，就声明它。

| 面 | 谁是真源 | 行为 |
| --- | --- | --- |
| 其他 22 页（默认 `scopeParam: ""`） | `?namespace=` + localStorage | 逐字不变 |
| `namespace.html`（`scopeParam: 'name'`） | `?name=`（页面身份） | 见下 ①~③ |

- ① **进场**：`adoptScopeParam()` 让 `?name=` 优先于 `?namespace=`（后者同步成同一个值，
  URL 里不留两个互相矛盾的 ns）。⇒ 顶栏面包屑 / 切换器勾选 / 页面标题 / 编辑目标进场即同值。
- ② **顶栏切 ns**：`switchScope()` 现在会一起改 `?name=`（以及存在的 `?namespace=`）再整页重载
  ⇒ 真的跳到 `namespace.html?name=<新ns>`，新文档重新按 URL 装配。
- ③ **页内下拉切 ns**：新增导出 `A.setScope(ns)` = 落盘 + 重画面包屑 + 就地重新勾选切换器
  （触发器文案 + 菜单 `aria-checked`）。**故意不导航** —— 页内 CAS/ABA 判据依赖「同一份文档里的
  请求代号」（`f3-node-retained` 的 `node-aba` 用例就是这么验的），重新导航会把那套判据一起废掉。

一句话：**范围变了，就先把唯一真源（URL / scope）改成新值，再让所有显示面从它重新读。**

## 2. 改动文件

| 文件 | 改什么 |
| --- | --- |
| `web/partials.js` | `mountLayout` 新增 `scopeParam`；新增 `adoptScopeParam()` / `setScope()`（导出）；`switchScope()` 改页面自有参数；`mountNamespaceSwitcher` 返回 `{syncScope}`（就地重勾选）并把面包屑重画函数登记给 `setScope` |
| `web-src/pages/namespace.html` | `mountLayout({ …, scopeParam: 'name' })`；页内 `#config-namespace` 的 `onchange` 追加 `A.setScope(...)` 且同步 `?namespace=`（若在） |
| `scripts/e2e/fixtures/f3/f3-ns-scope.mjs` | **新增**：`ns-scope-topbar` 用例（真写证据 + 双向一致性） |
| `scripts/e2e/fixtures/f3/f3-cases.mjs` | 注册 `ns-scope-topbar` |

没有新增 i18n key、没有新增 CSS 类、没有改 API 契约（服务端零改动）。

## 3. e2e 断言（不是「两个字符串相等」，是**真写**）

`ns-scope-topbar`（real authority：真 server + 真 Chrome + 真 DB）：

1. 进场 `?namespace=default&name=default`：面包屑 `#default`、`#config-namespace` = `default`、`?name=default`。
2. 点顶栏 → `[data-ns-id="prod"]`：
   - **判真跳转**：上一份文档设的 marker 在新文档里不存在（只看 `waitForURL` 会把 `replaceState`
     之后还没跳的情况判成通过）；
   - `?name=prod` 且 `?namespace=prod`（URL 不留两个矛盾的 ns）；
   - 面包屑 `#prod`、触发器文案含 `prod`、`#ns-name` 含 `prod`、`#config-namespace` = `prod`、
     `Anotify.readScope().namespace` = `prod`（五个显示/持久化面同值）。
3. **写路径证据**：`#namespace-vars` 填入 `F3_TOP455` 并保存 →
   - `PUT` 恰好一次且路径 = `/v1/namespaces/prod/config`；
   - 服务端读到 `prod.vars.F3_TOP455 === 'written-through-topbar'`；
   - `default.vars` 与保存前 **deepEqual**（顶栏显示 prod 时，default 一个字节都没变）。
4. 反向：页内下拉切回 `default`（**不导航**）→ `?name=default`、面包屑 `#default`、
   触发器含 `default`、`#config-namespace` = `default`、落盘 scope = `default`。

## 4. 变异验证（本轮硬要求）

把 §1.2 ② 的跳转删掉（`web/partials.js` 里 `switchScope()` 的 `scopeParam` 分支），
`make fe` + 重建 embed 二进制，再单跑本用例：

```
❌ ns-scope-topbar — AssertionError [ERR_ASSERTION]: 顶栏切 ns 必须把页面身份 name= 一起改（#455 修复点）
```

恢复后同一条用例：

```
PASS ns-scope-topbar :: NS-TOP455 topbar switch navigates namespace.html?name=<ns> in a new document,
     crumb/trigger/page identity/persisted scope all agree, real save PUTs /v1/namespaces/prod/config
     and leaves default byte-identical, in-page picker keeps topbar in sync without navigation
```

## 5. 门禁实跑

| 门禁 | 结果 |
| --- | --- |
| `flock /tmp/anotify-ci.lock -c "make ci"`（**串行**，本机有其它 agent 同时在跑 ci） | 退出码 **0**（contract → check-agents → lint-go → fe → 各守卫 → go build → tmp-leak-guard + `make test` → bench 冒烟 → harness 自测 全绿） |
| 受影响 e2e：`make e2e-affected` | **15/15 套件全绿，961 断言通过 / 0 失败**（436.9s；`E2E_CONCURRENCY=2`，临时目录零残留） @count `make e2e-affected` |
| 本用例单跑 `node scripts/e2e/suites/frontend.mjs --case ns-scope-topbar` | 2 条行为断言 PASS（修复后），变异下红（§4） |
| `node --test scripts/v4-frontend.test.mjs`（不在 `make ci` 里） | 本机基线已有 **85 条**失败（`playwright-core` 环境 + 陈旧断言），本次改动前后**失败集合逐条相同**（`diff` 为空） @count `node --test scripts/v4-frontend.test.mjs` |

选中的 15 套：`i18n` `i18n_coverage` `frontend` `lang_hint` `cli_auth` `passkey_enroll` `admin_flow`
`deeplink` `auth_flow` `ux_regression_19_23` `recovery_next` `events_paging` `ui_shift` `p2_guards`
`password_login`；跳过的 13 套是 `internal/**` 面（Go 服务端零改动）—— 选择理由见 `make plan-affected`。

## 6. 没做什么（边界，防越界）

- node.html `?id=` / capability.html `?name=` 等其他详情页**不受影响**：它们的 `?name=` 是**资源名**，
  不是命名空间，故不声明 `scopeParam`，行为逐字不变。
- 裸 `namespace.html`（无 `?name=`）仍是「缺少 name 参数」空态 —— 它没有写路径，不构成「显示 A 写 B」。
- #453 / #454 / #456 / #457 / #458 的页面与行为：零改动。

## 7. 遗留风险

- 顶栏切 ns 是**整页导航**，页面里未保存的 vars 草稿会丢（页内下拉有 `confirmDanger`，顶栏没有）。
  这与全站其他页面顶栏切换的语义一致；要改得引入跨页草稿确认，不属于 #455。
- §1.2 ③ 的「就地重勾选」依赖切换器挂载成功；挂载失败时 `setScope()` 退化为只更新面包屑与落盘
  （顶栏至少不会停在旧 ns 的文案上）。
