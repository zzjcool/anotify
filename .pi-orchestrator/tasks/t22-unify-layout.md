# 任务 T22 · 统一前端布局层（T14 三页迁移到 T13 partials.js）

你在主仓库 `/Users/zheng/code/anotify`（当前分支 `integrate/backend`）工作。**这是一次精细重构，只动 web/，不要碰 Go 代码。**

## 背景

合并后有 6 个页面、2 套布局层：

- **T13 partials.js**（保留，作为唯一公共层）：DOM 构建式，页面主内容放 `<div id="page-main">`，调 `Anotify.mountLayout({active,title,subtitle,username})`。已用于 index/login/receivers.html。还提供 `el/api/copyText/toast/timeAgo/b64urlToBuf/bufToB64url/detectPlatform` 等工具。
- **T14 layout.js**（移除）：HTML 字符串式，页面用 `<div id="layout-root">` + `AnotifyLayout.render({...})` + `mount()`。用于 keys/security/docs.html。

两套侧栏 IA 一致，视觉也接近。**目标：让 keys/security/docs.html 改用 partials.js，删除 layout.js。**

## 步骤

1. 读 `web/partials.js`（重点：`mountLayout` 如何包装 `#page-main`、`Anotify` 导出的工具）和 `web/ui.css`（T13 共享样式，含 side-link/side-label/badge-dot/dot/reveal 等）。
2. 读 T13 一个完整页面（如 `web/index.html`）理解标准用法：主内容包在 `<div id="page-main">…</div>`，末尾 `mountLayout({...})`。
3. 改造 `keys.html` / `security.html` / `docs.html`：
   - 把 `<div class="flex min-h-screen" id="layout-root"></div>` 改为 `<div id="page-main">…原页面主内容…</div>`（把原来 render() 注入的主内容放进来）
   - 页面 `<script src="layout.js">` 改为 `<script src="partials.js">`
   - 把 `document.getElementById("layout-root").innerHTML = AnotifyLayout.render({...}); AnotifyLayout.mount();` 改为 `Anotify.mountLayout({active,title,subtitle,username})`
   - 原来用 `AnotifyLayout.el` 的地方改用 `Anotify.el`（两者签名一致）；若用了 layout.js 独有但 partials.js 没有的工具，从 partials.js 找等价物或内联实现
   - 确保引用了 `ui.css`（partials.js 依赖的 side-link 等样式在这里）；三页各自的页面级 `<style>` 保留
4. 确认各页 `active` 值正确（keys→"keys"、security→"security"、docs→"docs"），与 partials.js 的 NAV id 对齐；如 NAV 缺 docs/agent 项请补齐（参考 layout.js 的 NAV）
5. **删除 `web/layout.js`**，并确认没有页面再引用它

## 视觉一致性要求

- 统一后侧栏/顶栏/用户卡片/logo 视觉必须与 T13 页面一致（同一套渲染）
- 颜色一律来自 tokens.css / ui.css 变量，不新增硬编码色值
- docs.html 右侧目录（xl 起滚动跟随高亮）功能保留

## 自测（必须真实执行）

```
cd /Users/zheng/code/anotify && python3 -m http.server 5717 --directory web &
# 用 playwright-core（系统 Chrome）逐页打开桌面1280+移动390：
#   keys/security/docs/index/login/receivers 六页
# 断言：无 JS 错误、无 404 失败请求（/v1/* 降级 404 除外）、侧栏渲染正常、
#       六页侧栏视觉一致、桌面移动无横向溢出、可滚动到底
grep -rn "layout.js\|AnotifyLayout" web/   # 应为空
```

## 上报

`DONE T22` + 改动清单 + 六页自测结果 + 遗留风险
完成后 commit：`refactor(web): 统一布局层到 partials.js，移除 layout.js`
