## 现状（按当前代码重写）

功能面**已经修完**：`fea34b1`（PR #272，提交信息误标 #273）删掉了「后端失败就降级演示数据」，`Anotify.loadErrorNode` 上线，login 页不再用 `String(e).includes("HTTP")` 判后端可用性，`make mock`（`cmd/fe-preview` 注入 `mock.js`）是唯一假数据来源。

剩下的是**死代码与守卫漏洞**，不影响线上行为，但会让人（和未来的 bug）复活演示模式。

## 剩余项

### 1. `api()` 的第 3 参 `demo` 还能复活假数据（唯一的真风险）

```js
// web/partials.js:1664 / 1730-1734
async function api(path, opts, demo) {
  ...
  } catch (e) {
    if (demo && typeof demo.then === "function") return demo;
    if (demo !== undefined) return demo;   // ← 传 null 也会吞掉所有失败
    throw e;
  }
}
```

全仓 0 个真实 3 实参调用点，但任何人新写 `api(p, o, null)` 就会静默吞掉错误。删形参 + 删这两行回退。

### 2. 10 处 `{ demo: null }` 落在 opts 里（从来无效）

`trace.html:697`、`capabilities.html:1411`、`node.html:473`、`sources.html:822,883,884`、`source.html:382,383,384,759-762`。它进的是 `fetch` 的 `RequestInit`，被忽略——纯安慰剂，删。

### 3. security 页 3 段不可达分支

`security.html:1474-1481`、`:1527-1532`、`:1766-1771`。gate 为 `loadFailed.passkeys/sessions` 时，渲染函数已用错误卡替换列表并 `return`（`:1357-1362`、`:1677-1682`），按钮根本不存在 ⇒ 分支进不去。删分支 + 对应 3 条 `security.toast.*_demo` key。

### 4. 死 i18n key：56 条 × 4 语言

`index.demo_badge/trend_demo/demo+16`、`login.status.demo/demo_enter`、`receivers.demo_badge/added_demo/demo+5`、`keys.summary_demo/revoked_demo/demo+4`、`security.demo+17`、`events.demo_badge/demo_hint`。
**必须保留**：`sources.form.demo*`（「填入示例」按钮在用）、`admin.users.demote*`。

### 5. 过期 e2e

`scripts/e2e/suites/i18n_coverage.mjs:223,257,424` 仍带 `?demo=1`（现在是空操作，断言与 query 无关）。

## 已确认**不在**本 issue 的问题（别顺手改）

- 活文案仍在说「演示模式」：`keys.error.create_failed`、`security.alert.backend_down`、`passkeyenroll.panel.error_backend_desc`。这些 key 是活的（用户可见错误提示），属**内容修复**，另开。
- `i18n_coverage.mjs` 的 A 组**没有 `injectSession`**：受保护页会 401 跳 login，实际只扫了 login/docs —— 该用例对四个受保护页是空转。属测试覆盖问题，另开。
- `web/mock.js` 仍拦截 `/v1/test-notify`（`mock.js:5184-5199`）：已被 `hash.mjs` 的 DEV_ONLY 排除出生产，无害，随 #273 一并处理。

## 验收

- `make fe` 通过（`check-classes` 死类守卫必须绿）
- `./scripts/e2e/run_all.sh --serial frontend i18n i18n_coverage p2_guards` 全绿
- 全仓 `rg "demo: null|api\(path, opts, demo|demo-badge"` 零命中（`mock.js` 除外）
