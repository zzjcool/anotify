# #274 残留清理（删死代码，不改行为）报告（worker · 分支 power）

结论一句话：**按逐行清单全部执行完毕，行为零变化；`make fe` + 全量 `make e2e` 全绿。**
改动见 commit `949ca07`（在 PR #309 上，head 分支 `power`）。

## 1. 做了什么（严格按清单）

### 1.1 `web/partials.js`：拆掉唯一的「复活假数据」通道
- `:1569` 段注释 `API 封装：失败时回退到 demo` → `API 封装`
- `api(path, opts, demo)` → `api(path, opts)`
- 删 `:1731-1732` 两行回退（`if (demo && ...) return demo;` / `if (demo !== undefined) return demo;`），保留 `throw e;`
- 理由：传 `null` 当第 3 参会吞掉**所有**失败。删掉签名参数后，任何「顺手写第三参」都变成语法可见的错，而不是静默复活演示数据。

### 1.2 删 10 处 `{ demo: null }`（落在 opts 里，对 fetch 无效，纯安慰剂）
`trace.html:697`、`capabilities.html:1411`、`node.html:473`、`sources.html:822/883/884`、
`source.html:382/383/384`（保留 `.catch(...)`）、`source.html:761`（保留 `method: "DELETE"`）。
→ 现全仓 `rg 'demo: null'` 为 0 命中。

### 1.3 两条撒谎的注释同步
- `sources.html:2596-2597` → `// api() 失败即抛：错误必须在本页显式处理，不能静默吞掉（页面渲染时炸在 r.dispatch 上就是历史事故）。`
- `capabilities.html:1898` → `// Anotify.api(path, opts) ——`

### 1.4 `security.html` 删三段**不可达**分支
- `:1474-1481`（`renamed_demo`）、`:1527-1532`（`deleted_demo`）、`:1766-1771`（`revoked_demo`）
- 不可达性核实：`renderPasskeys`（`:1348-1363`）/ `renderSessions`（`:1656-1669`）在 `loadFailed.*` 为真时
  `list.replaceChildren(errorCard(...)); return;` —— 行里根本没有重命名/删除/吊销按钮，那三段分支走不到。
- 两条注释同步：`:1582` → 「情况 2：后端未连接（加载失败，loadFailed.passkeys 为真）」；`:2047` → 「后端不可达拦截（不可伪造数据）」。这两段**代码块保留**（是活路）。
- `recoveryDemo` 相关（`:1813/1875/1904/1908/1932`）与 `openEnrollModal` 的拦截段**未动**。

### 1.5 四语 locales 删死 key（四文件各删 59 行，按行号从大到小）
`1080-1081 events.demo_badge/demo_hint`；`638-655 security.demo`（+17 子键）；`635/630/627 security.toast.*_demo`；
`543-547 keys.demo`（+4）；`533 keys.toast.revoked_demo`；`522 keys.summary_demo`（`:521 summary` 未动）；
`451-456 receivers.demo`（+5）；`434 receivers.toast.added_demo`；`348 receivers.demo_badge`；
`341 login.status.demo_enter`；`325 login.status.demo`；`190-206 index.demo`（n1_title..n8_body）；
`145 index.kpi.trend_demo`；`133 index.demo_badge`。

保留（活代码在用）：`sources.form.demo/demo_hint/demo_name/demo_ok/demo_no_caps`、`admin.users.demote`、
`admin.confirm.demote_title/demote_body`。四语 key 集合仍一致（各 8 个含 `demo` 的 key 行 = 3 个 `demote*` + 5 个 `sources.form.demo*`），
YAML 均可正常解析（pyyaml 四文件 OK，24 个顶层块），无空父 map / 悬空缩进。

### 1.6 `scripts/e2e/suites/i18n_coverage.mjs` 去掉空操作
- URL：`:223` 去 `?demo=1`；`:257` 去 `&demo=1`；`:424` 去 `?demo=1`
- 注释/label 去 demo 字样：`:10`、`:98`、`:215`、`:217`、`:232/237`、`:246/247`、`:263/268`、`:407/411`
- **断言逻辑一行未动**，未加 `injectSession`（另一条 issue）。

## 2. 测试覆盖 / 验证输出（原样）

### 2.1 `make fe`
```
node scripts/check-classes.mjs
✅ class 校验通过：786 个类（HTML class 属性 631 + JS class 串新增 155）全部落在设计系统 / Tailwind 工具类内
node scripts/check-tailwind-css.mjs
✅ tailwind.css 覆盖 894 个源码类名（组件类已排除）
go run ./cmd/sitegen -src web-src -out web -langs zh-CN,en,ja,es -version 'v0.2.0-82-g8af2fcc-dirty'
✅ sitegen done: 4 languages × 19 pages → web (version v0.2.0-82-g8af2fcc-dirty)
node scripts/hash.mjs web internal/server/dist
✅ 指纹完成：30 个资源，76 个 HTML → /root/.herdr/worktrees/anotify/power/internal/server/dist
```
（四语各缺 key 会打 WARN —— 这里 0 WARN。）

### 2.2 `./scripts/e2e/run_all.sh --serial frontend i18n i18n_coverage p2_guards`
```
  总耗时: 270.9s
  断言通过: 994
  断言失败: 0

  ✅ frontend           604通过/0失败  236.3s
  ✅ i18n               299通过/0失败  8.9s
  ✅ i18n_coverage      61通过/0失败  11.4s
  ✅ p2_guards          30通过/0失败  2.8s
==========================================
✅ E2E 全绿
```

### 2.3 全量 `make e2e`（交活前跑一次）
```
  总耗时: 272.3s
  断言通过: 2273
  断言失败: 0
  ✅ orphans 26 / passkeys 7 / api_contract 64 / deploy 51 / security 30 / routing 78 /
     edge_cases 23 / live_agent 77 / persistence 22 / ws_protocol 137 / contract 80 /
     i18n 299 / i18n_coverage 61 / lang_hint 57 / admin_flow 51 / deeplink 34 /
     passkey_enroll 84 / capacity 47 / auth_flow 28 / ux_regression_19_23 13 /
     cli_auth 132 / recovery_next 5 / events_paging 65 / real_backend 147 /
     p2_guards 30 / ui_shift 21 / frontend 604
==========================================
✅ E2E 全绿
```

### 2.4 `rg 'demo: null|api\(path, opts, demo|demo-badge|demoEnter' web/ web-src/ scripts/e2e/`
```
scripts/e2e/suites/frontend.mjs:6:    *  B. 已登录访问 index → 渲染真实数据（哪怕空）；演示模式已移除（#273），无 demo-badge
scripts/e2e/suites/frontend.mjs:1193:  // 演示模式已整体移除（#273）：不再有 demo-badge，页面要么渲染真实数据、
scripts/e2e/suites/frontend.mjs:1196:      badge: !!document.getElementById("demo-badge"),
scripts/e2e/suites/frontend.mjs:1200:  "index 不再有演示模式残留（#demo-badge 已移除）",
scripts/e2e/suites/frontend.mjs:1262:      badge: !!document.getElementById("demo-badge"),
scripts/e2e/suites/real_backend.mjs:332:  "首页：没有演示模式残留（#demo-badge 已移除，#273）",
scripts/e2e/suites/real_backend.mjs:333:  await page.evaluate(() => !document.getElementById("demo-badge")),
scripts/e2e/suites/i18n.mjs:665:  "AC-4.1 演示模式已移除（登录页不再有 demo=1 / demoEnter）",
scripts/e2e/suites/i18n.mjs:666:  !loginR.text.includes("demo=1") && !loginR.text.includes("demoEnter"),
```
说明：**期望值修正** —— 清单预期「只剩 `web/mock.js` 内无关命中」，实测 mock.js 里那 19 处不含这四个 pattern
（都是 fixture 注释/`username: "demo"`）；这 9 处全部在 `scripts/e2e/suites/{frontend,real_backend,i18n}.mjs`，
语义是**断言 demo 残留不存在**（`!document.getElementById("demo-badge")` / `!text.includes("demoEnter")`），
即反向守卫，恰好证明本单目标已达成；且这三个文件不在我的文件所有权清单内，未动。

### 2.5 `rg -c demo web-src/locales/`
```
web-src/locales/zh-CN.yaml:8
web-src/locales/en.yaml:12
web-src/locales/es.yaml:12
web-src/locales/ja.yaml:8
```
差异来源已核实：`grep -c` 计的是**行**，en/es 比 ja/zh-CN 多的 4 行是**译文正文**里出现 "demo mode" 字样
（`receivers…no_backend`、`keys.error.create_failed`、passkey 面板说明、`passkeyenroll.panel.error_backend_desc`），
属于本单「已知边界」里的**活文案内容修复（另开条）**。按「含 demo 的 key 行」统计，四语完全一致（各 8 行：
3 个 `admin.*.demote*` + 5 个 `sources.form.demo*`）。

## 3. 改动文件清单
- `web/partials.js`
- `web-src/pages/trace.html` / `capabilities.html` / `node.html` / `sources.html` / `source.html` / `security.html`
- `web-src/locales/zh-CN.yaml` / `en.yaml` / `ja.yaml` / `es.yaml`
- `scripts/e2e/suites/i18n_coverage.mjs`

## 4. 环境备注（不是本单改动）
- 本 worktree 原本没有 `node_modules`，e2e 首跑 4 套件全「0通过/0失败」（`Cannot find package 'playwright-core'`）。
  已用 `ln -s /root/code/anotify/node_modules node_modules`（版本 1.62.1 与 package.json 一致，gitignored，只读），之后全绿。
- 工作区里有**他人在途的未提交改动**（`DEVELOPMENT.md`、`internal/server/backpressure.go` 的注释更新），我没有 add、没有改、没有提交。

## 5. 未决问题
- 无阻塞。两处按契约留白：#274 之外的两条（`i18n_coverage` A 组无 `injectSession` 的测试空转、`web/mock.js` 伪造 `/v1/test-notify`）与活文案「演示模式」内容修复，均按要求未动。

DONE 274
产出文件:
- web/partials.js
- web-src/pages/trace.html
- web-src/pages/capabilities.html
- web-src/pages/node.html
- web-src/pages/sources.html
- web-src/pages/source.html
- web-src/pages/security.html
- web-src/locales/zh-CN.yaml
- web-src/locales/en.yaml
- web-src/locales/ja.yaml
- web-src/locales/es.yaml
- scripts/e2e/suites/i18n_coverage.mjs
自测命令与结果:
- make fe → PASS（check-classes 786 类 + sitegen 4 语 × 19 页，0 WARN）
- ./scripts/e2e/run_all.sh --serial frontend i18n i18n_coverage p2_guards → PASS (994 assertions)
- make e2e（全量 28 套件）→ PASS (2273 assertions, 0 failed)
- rg 'demo: null|api\(path, opts, demo|demo-badge|demoEnter' web/ web-src/ scripts/e2e/ → 只剩三份 e2e 套件里的**反向守卫**（断言 demo 不存在）
发现的产品 bug（若有，不要改断言迁就）:
- 无（本单是死代码清理，行为零变化）
遗留风险:
- 已知边界内三条留白（测试空转 injectSession / mock.js 伪造 test-notify / 活文案「演示模式」）按契约为另开条
