# DONE T22 · 统一前端布局层（T14 三页迁移到 T13 partials.js）

## 目标回顾

把 `keys.html` / `security.html` / `docs.html` 从 T14 的 `layout.js`（HTML 字符串式 `AnotifyLayout.render()`）迁移到 T13 的 `partials.js`（DOM 构建式 `Anotify.mountLayout()`），删除 `layout.js`，全站统一为唯一公共布局层。只动 `web/`，不碰 Go。

## 产出文件（改动清单）

| 文件 | 改动 |
| --- | --- |
| `web/keys.html` | ① `<style>` 裁剪到仅本页特有（`.key-mask`/`.toast`），共享样式走 ui.css；② 加 `<link href="ui.css">`；③ `layout-root` → `#page-main` 内联主内容（含「+ 新建 Key」头部按钮迁入头部行）；④ `AnotifyLayout.el`→`Anotify.el`；⑤ 末尾 `render()` 模板串 → `Anotify.mountLayout({active:"keys",...})` |
| `web/security.html` | 同构迁移：保留 `.recovery-code`/`.toast` 页级样式；「+ 添加 Passkey」迁入头部；`mountLayout({active:"security",...})` |
| `web/docs.html` | 迁移 + 结构拆分：长正文注入 `#docs-content`，右侧 TOC 静态保留在 `#page-main`；`mountLayout({active:"api",...})`；目录滚动跟随高亮功能保留；**额外修复移动端参数表横向溢出**（table 包 `overflow-x-auto` + `.param-name` 移动端允许换行） |
| `web/layout.js` | **已删除**（188 行，全站无引用） |

未改动：`partials.js`/`ui.css`/`tokens.css`（复用现有共享层，未新增硬编码色值）；`index/login/receivers.html`（本就用 partials.js）；所有 Go 文件。

## 迁移要点（决策记录）

- **active 值对齐**：docs.html 用 `active:"api"`（partials.js NAV 中"API 文档"项的 id），高亮正确。
- **actions 按钮**：layout.js 的 `actions` 参数是 HTML 字符串，partials.js 无此参数 → 把「新建 Key」「添加 Passkey」按钮迁入各页头部行（flex justify-between），视觉/交互一致。
- **样式去重**：三页原各自复制了一整套共享样式（card/side-link/btn-*/status-badge 等），全部裁剪为仅页级特有项，共享样式统一引 `ui.css`。
- **docs 结构**：layout.js 把"正文+右目录"作为一个 content 字符串注入；partials.js 是 DOM 包裹 `#page-main`，故把正文改注入 `#docs-content`，右目录静态放 `#page-main` 内 flex 布局，TOC IntersectionObserver 高亮逻辑不变。

## 自测结果（真实执行，系统 Chrome / playwright-core）

### 六页 × 两视口（桌面1280 + 移动390）验证

断言：无 JS 错误、无非 /v1 失败请求、侧栏渲染、桌面移动无横向溢出、可滚动到底。

```
✅ [desktop] index/receivers/keys/security/docs.html  JS错误=0 失败请求=0
✅ [mobile]  index/receivers/keys/security/docs.html  JS错误=0 失败请求=0
✅ [desktop+mobile] login.html（公开页，本就无侧栏，符合预期）
总计问题: 0
```

（初次 mobile-docs 报参数表横向溢出 → 已修复，复测归零）

### 侧栏一致性专项（5 个工作台页）

```
✅ index/receivers/keys/security/docs：侧栏链接序列完全一致（8 项），每页恰好 1 个正确高亮
   （总览/通知接收/API Keys/API 文档/安全与登录 各自高亮）
```

### 残留检查

```
grep -rn "layout\.js\|AnotifyLayout\|layout-root" web/  → ✅ 无残留
web/layout.js 已删除（git 历史 101cca4 记录）
```

## 截图证据

`/tmp/t22-shots/{desktop,mobile}-{index,login,receivers,keys,security,docs}.png`（12 张，全页）

## 关于 git 提交（重要说明）

工作分支 `integrate/backend` 上**父进程在并发提交**。我的 keys/security/docs 迁移与 layout.js 删除已被父进程在其清扫提交中并入历史（`5f62bfe`、`101cca4`），内容与本任务产出一致（HEAD 已含 `page-main`/`mountLayout`，无 `layout-root`/`AnotifyLayout` 残留，layout.js 已从 git 删除）。**当前工作区仅剩 docs.html 移动端溢出修复这 6 行未提交**（`overflow-x-auto` 包裹 + `.param-name` 换行媒体查询）。为避免与父进程在飞工作竞争/冲突，我**未执行 git add/commit**，保持工作区现状交由协调者统一提交。diff 已核验干净（仅上述 6 行）。

## 遗留风险

- 低：三页 `.toast` 与 partials.js 的 `Anotify.toast()` 是两套实现（页面用本地居中 toast，partials 用右下 toast host）。功能各自正常，视觉位置不同（页面居中 vs partials 右下），属页面原有行为，未强行统一。
- 低：login.html 引 `partials.js` 但仅用于工具函数（不调 mountLayout），为既有设计，未改动。

---