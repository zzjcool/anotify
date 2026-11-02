# W3-C 冻结计划（playbooks / playbook / trace / docs / message）

> centurion 冻结。worker 以本文 + `UI-V2-CONTRACT.md`（v1.2 + §12）为唯一事实源。
> 与本文冲突的直觉一律让步；需要改本文 → 停下来报给 centurion，不要自行解释。

## 0. 文件所有权（硬性，越界即退回）

| 负责人 | 可写文件 |
|---|---|
| worker-pb | `web-src/pages/playbooks.html`、`web-src/pages/playbook.html`、`.pi-orchestrator/w3c-locale/pb.yaml` |
| worker-trace | `web-src/pages/trace.html`、`.pi-orchestrator/w3c-locale/trace.yaml` |
| worker-docs | `web-src/pages/docs.html`、`.pi-orchestrator/w3c-locale/docs.yaml` |
| worker-msg | `web-src/pages/message.html`、`.pi-orchestrator/w3c-locale/message.yaml` |
| centurion | `web-src/locales/{zh-CN,en,ja,es}.yaml`、`web/tailwind.css`、集成与门禁 |

**任何人不得修改**：`web/ui.css`、`web/tokens.css`、`web/partials.js`、
`web-src/layouts/*`、`web/graphs.js`、`web/traceview.js`、`web/flograph.js`、
`internal/**`、`scripts/**`、`design/**`、`web-src/locales/*`（locale 归 centurion）。

## 1. locale 协议（避免 4 个 worker 并发写同一 YAML）

1. **优先复用已有 key**。4 份 locale 各 1680 个叶子键，绝大多数文案已存在。
2. 确实需要新 key 时：写进 `.pi-orchestrator/w3c-locale/<你的分组>.yaml`，结构为
   ```yaml
   zh-CN:
     pb:
       detail_title: 预案详情
   en:
     pb:
       detail_title: Playbook detail
   ja: { ... }
   es: { ... }
   ```
   **四个语言都必须给值**（en/es 零 CJK）。只在你的命名空间内（`pb.*` `trace.*` `docs.*` `message.*`）。
3. 页面里该用新 key 就直接写（`{{t "pb.detail_title"}}` 或 `t("pb.detail_title","兜底")`）。
   合并前 sitegen 会 WARN「missing i18n key」并回落，**不影响你的构建**——这是预期。
4. 在 verdict 里列出「新增 key 清单」+ 一句为什么要新 key。

## 2. 组件与样式规则

- 组件类（`.page/.page-head/.page-title/.page-sub/.page-actions/.card*/.btn*/.badge*/.table-wrap/.data-table/.tabs/.tab/.empty/.skeleton/.list*/.stat*`）
  **一律用 `web/ui.css` 已有的**，不要再发明一套。
- 缺组件 → 页面 `<style>` 里定义页级类，**颜色只用 `tokens.css` 的 token**（`--text/--muted/--faint/--surface-1..3/--line/--line-strong/--accent*/--success/--error/--warn/--st-*`）。
  报告里写明「建议提升到 ui.css」。
- **死类守卫规则**（`scripts/check-classes.mjs`）：页级 `<style>` 里新类的**行首（可缩进）必须是 `.类名`**，
  写成 `body[data-page="x"] .foo {` 或 `#page-main > .foo {` 会被判死类 → 构建失败。
- 硬编码色值归零：`#[0-9a-fA-F]{3,8}`、`rgba(255,255,255,…)`、`rgba(0,0,0,…)` 全部改 token 或 `color-mix(… var(--token) …)`。
- `text-white` / `bg-white/` / `border-white/` / `text-zinc-*` 目标 **0 命中**（逐页数：docs 29、message 5、trace 2、playbooks 1、playbook 1）。
- **不要新增 Tailwind 工具类**：优先用仓库里已有出现过的。若确实需要（`check-tailwind-css.mjs` 报错），
  跑 `node scripts/build-tailwind.mjs`，并在 verdict 里写「新增工具类 X」。
- `header h1` 全站唯一：页面内容里**只能**用 `<h2 class="page-title">`，禁止 `<h1>`。
- 页标题一律 `.page-title`（`ui.css` 自带 Inter 600 28px）；**不要**再写 `font-display`。

## 3. 逐页硬锚点（e2e 断言，改坏即红）

### playbooks.html
- `.pb-card`（类名保留，至少 4 张）、`.pb-badge`（文案「就绪/缺节点/缺定义」由 locale 提供，**不要改这几个中文值**）、`.pb-next`（去掉 `→` 后长度 >3）。
- `.pb-bar > i` 的 `backgroundColor`：`.is-ready` 与 `.is-needs-node` **必须不同色**。
- `#pb-grid` 子元素数 >0 是「数据就绪」信号 → **骨架不能放进 `#pb-grid`**。
- `#pb-empty` 成功时必须带 `hidden` 类（`real_backend.mjs:375-385`）。
- 不使用 `#toast` 元素。

### playbook.html
- `#cap-list .chk` ≥2、`#node-list .chk` ≥2、`#edge-list .chk` ≥1、`#trg-list .chk` ≥1（`.chk` 行结构保留）。
- `.chk-dot.is-bad` 存在；`#node-list` 内某 button 文案含 `YAML`。
- `#yaml-text`、`#pb-badge`（文案 === 「缺节点」）、`#btn-install`（点击后 `.chk-dot.is-bad` 数量下降）。
- 「补齐」只补定义/边/源，不得假装修好节点。

### trace.html
- `.wf-switch button` 数量 === 3、激活按钮带 `.on`、激活按钮 `textContent` === `t("events.wf.graph")`（zh「图」）。
  → **这三个 button 由 `web/traceview.js` 运行时生成，`events.*` key 不属于本组，禁止改文案/结构**。
  页面只能**用 CSS** 把 `.wf-switch` / `.wf-switch button` / `.on` 画成 `.tabs/.tab/.tab.active` 的视觉。
- 默认视图必须仍是 `graph`（`.wf-row` === 0、`.flograph` 存在、`.wf-node` ≥1），`?view=` 兼容保留。
- `#trace-body` 必须有子元素且 text >20；`.trace-id` 元素必须是 `#trace-body` 的后代（`events_paging.mjs:588`）。
- graph-libs 顺序 dagre → flograph → graphs → traceview，保持 `defer`，仍在 `DOMContentLoaded` 里调 `TraceView.mount`。
- 匿名打开 `trace.html`（无 traceId）→ 走 `trace.no_id` 分支，**不能有 JS pageerror**，1280/390 无横向溢出。

### docs.html
- `tHTML` 已正确渲染，**不要改回 `t`**；`doc-em/doc-link/doc-code` 三个类继续用（在 ui.css，冻结）。
- `#endpoints`（`frontend.mjs:1248-1275`）：`tbody tr` ≥80、内部 `.card` ≥10、文本含 `/v1/events` 与 `/agent-login.sh`、不得含 `/v1/devices`。
  → 每个 tag 分组卡**保留 `.card`**；表格换 `.table-wrap`/`.data-table` 时必须仍然满足上面 5 条。
- 侧栏 active：`Anotify.mountLayout({active:"api"})` 不变；`#sidebar .side-link.active` href === `docs.html`。
- 锚点 id 不变：`#agent #api #endpoints #auth #events #webpush #node`（`index.html`/`keys.html` 有深链）。
- 1280 与 390 无横向溢出（表格必须包在 `.table-wrap` 或 `.overflow-x-auto` 里）。

### message.html
- `#msg-title` 必须存在、非空、`textContent` 含 `@`（`deeplink.mjs:204-211`）。
- 页面文本必须含 `能力` / `出口` / `执行节点` / `任务链`（来自 `common.field.*`、`message.out.*`、`message.chain_title`，**不要把这些文案改成硬编码或换 key**）。
- 任务不存在时 `#msg-title` 含「不存在」。
- 三卡顺序：正文主卡 → 字段卡 → 投递记录/任务链。

## 4. 每页验收（centurion 会逐条核）

1. `.page` + `.page-head` + `<h2 class="page-title">` + `.page-sub` + `.page-actions`（trace 必带返回/刷新）。
2. `.card`（16px 圆角 + `--surface-1` + `--line` + `--shadow-1`）为统一卡片；卡内/块间 24px 节奏。
3. `.empty` 与 `.skeleton` **真实使用**（不是只引入类名）：加载态骨架、空态有 icon/标题/说明/动作。
4. toast 全走 `Anotify.toast`，页内不得再定义 `.toast` 或 `#toast`。
5. 失败态：用 `Anotify.loadErrorNode(err)` 渲染在**内容容器内** + `Anotify.toast` 提示，不得只留一句文字。
6. 硬编码 hex / `rgba(255,255,255,` / `rgba(0,0,0,` 归零（页内 `<style>` 与内联 JS 都算）。
7. `text-white|bg-white/|border-white/` 归零；`text-zinc-600` 归零。
8. `.app-topbar-sub` 与 `.page-sub` 不得同句（`page-sub` 存在时顶栏副标题会被自动吞掉，符合预期）。

## 5. 门禁（每个 worker 都跑，第一个非零退出即失败）

```bash
node scripts/check-classes.mjs
node scripts/check-tailwind-css.mjs
make sitegen
```

`make fe` 由 centurion 在集成时统一跑（并发写 `internal/server/dist` 会互相打断）。

## 6. 交付格式（verdict 必含）

```
DONE <分组>
改动文件: ...
新增/复用的 locale key: ...
新增页级类: ...（建议提升到 ui.css：...）
门禁: check-classes=OK check-tailwind=OK sitegen=OK
自测: 渲染抽查结论（可选：起 780x 端口看页面）
遗留风险: ...
```
