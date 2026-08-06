# CLI 设备授权确认页（cli-auth.html）· 设计规格

> 定义层产物。前端工程师（anotify-frontend）照此稿实现。
> 事实源核对：已通读 `web/tokens.css`、`web/ui.css`、`web/partials.js`、`web-src/pages/{keys,login}.html`、`web-src/layouts/{base,login}.html`、`internal/sitegen/*`、`Makefile`、`DEVELOPMENT.md`。

---

## 1. 目标

用户在终端跑登录脚本后，通过本页完成「CLI 设备 → 账号」的授权确认，下发 API Key。页面必须：

1. **双模式同页**：`?s=<sessionId>` 直达确认态；无参进入短码输入态，lookup 后切到确认态。
2. **防错账号**：确认态显著展示当前登录身份（手机扫码场景常见「手机登的是另一个号」）。
3. **防误授权**：scope 明示勾选、至少一项才允许批准；批准 / 拒绝双按钮。
4. **五种终态全部有明确界面**：已批准 / 已拒绝 / 已过期 / 未找到（短码错）/ 已领取。
5. **安全动作页不做演示模式**：后端不可用时给明确错误态，绝不伪造授权行为（红线，见 §7）。

---

## 2. 信息架构

### 2.1 页面定位与布局选择（方向性决策，已拍板）

**决策：新增轻量布局 `web-src/layouts/focus.html`（克隆自 `layouts/login.html`），页面首行声明 `<!-- layout: focus -->`。**

| 候选 | 结论 | 理由 |
| --- | --- | --- |
| base 布局（侧栏+顶栏） | ✗ | 授权确认是**安全决策页**（类比 GitHub Device Flow / Google OAuth consent，均为聚焦页）。手机扫码是头等场景，侧栏汉堡菜单是噪音；聚焦单列让「设备名 / 身份 / scope」获得全部注意力。 |
| 直接复用 login 布局 | ✗（可接受的降级 fallback） | login 布局顶栏 logo 硬编码链到 `login.html`；已登录用户在授权页点 logo 会落到登录页（login 页对已登录用户无自动跳转），语义错误。 |
| **克隆为 focus 布局** | ✅ | 只改两处：logo `href="index.html"`（回工作台而非登录页）、页脚改用已有的 `common.footer.copyright`（login_copyright 含「Passkey 免密」文案，不适用于本页）。约 90 行，一次复制，零运行时成本。其余（grid-overlay 背景、顶栏语言切换器容器 `#lang-switcher-login`、`max-w-md` 居中列、字体引用）原样保留，视觉与 login 页完全同族。 |

页面信息层级（聚焦单列 `max-w-md`，与 login 页一致的「玻璃卡 + 网格背景」）：

```
grid-overlay 背景
顶栏（focus 布局自带）：logo → index.html ｜ 语言切换器
主列 max-w-md：
  ├ 状态区（JS 切换，同卡内替换）：
  │   ├ #state-entry      短码输入态（模式B起点）
  │   ├ #state-confirm    确认态（模式A直达 / 模式B lookup 后）
  │   └ #state-result-*   五种终态 + 网络错误态（预渲染六个 section）
  └ 信任脚注（3 条，同 login 页信任行模式）
页脚（focus 布局自带）
```

**关键模式：所有状态区块全部用 `{{t}}` 在构建期渲染进 HTML（默认 `hidden`），JS 只做「显隐切换 + 填数据」，不在运行时拼文案。** 这样四语言覆盖零运行时遗漏，与 sitegen 架构一致（现有页面运行时 JS 仍有硬编码中文的历史包袱，新页面不要延续）。动态内容只有数据（设备名、用户名），没有文案。

### 2.2 状态机

```
                 ┌─────────────┐
   无参进入 ───► │ entry 输码态 │ ◄──────────────┐
                 └──────┬──────┘                │
                        │ lookup(code)          │ expired/notfound 态的
   ?s=<id> 进入 ──┐     │ 200 pending           │「输入新短码/重新输入」
                  ▼     ▼                       │（页内切换，清 ?s）
            lookup(sessionId)                   │
                  │                             │
        ┌─────────┼─────────────┐               │
        ▼         ▼             ▼               │
   200 pending  200 非pending   404/网络错       │
        │    (expired/consumed/ denied)          │
        ▼         │             │               │
   confirm 确认态 ▼             ▼               │
   ┌────────┐  直接落对应终态   notfound / error │
   │ approve├─200──► approved                   │
   │        ├─409──► 按 status 落 expired/consumed/denied
   │        └─网络错─► toast + 留在 confirm（可重试）
   │ deny   ├─200──► denied
   │        └─409──► 按 status 落对应终态
   └────────┘
   entry 页内重输 ──────────────────────────────┘
```

### 2.3 入口设计（方向性决策，已拍板）

**决策：主入口放 keys 页头部（ghost 按钮），不新增侧栏导航项。**

- **方案**：`keys.html` 头部操作区「+ 新建 Key」**左侧**加一个 `<a href="cli-auth.html" class="btn-ghost …">`，终端图标 + 文案 `{{t "keys.cli_auth_entry"}}`（CLI 设备授权）。
- **理由**：
  1. 三种到达方式里，a（自动开浏览器）和 b（扫码）都由终端打印/打开的 URL 直达本页，**根本不经过站内导航**；方式 c 的脚本同样会打印完整 URL 和短码。站内入口只是「用户记得短码、丢了 URL」的兜底。
  2. CLI 授权的产物是 API Key，「输码授权 → 得到 Key」与 keys 页心智完全吻合，放在这里是上下文式发现。
  3. 侧栏是全局常驻导航，为一个低频、瞬时性的动作加常驻项是永久的导航噪音（现有 7 项导航的克制应继续保持）。
- **被否方案**：侧栏新增「CLI 授权」项 —— 见上第 3 条；且要动 `partials.js` NAV + 四语言 `common.nav.*`，改动面更大收益更小。

---

## 3. 完整交互流程

### 3.0 公共前置：登录态

- 页面所有 API 调用走 `fetch(..., {credentials:"include"})`；**401 交给 `Anotify.api` 的内置守卫**跳登录页（沿用它构造 `login.html?next=<page+qs>` 的逻辑）。
- **两个注册（前端实现要点，缺一不可）**：
  1. `web/partials.js` → `api()` 内 401 处理的 `PAGES` 白名单数组，加入 `"cli-auth.html"`（否则 401 后 next 被兜底成 index.html，授权流程断掉）。
  2. `web-src/pages/login.html` → `NEXT_MAP` 加 `"cli-auth.html": "cli-auth.html"`（否则登录成功后回跳被兜底成 index.html）。
- 回跳携带 `?s=` 查询串：现有正则 `/^[?][\w\-=&%.]*$/` 天然兼容 `?s=xxx`（word 字符 + `=`），无需改动。
- 登录/注册成功 → `nextTarget()` 回 `cli-auth.html?s=xxx` → 页面重新 load → 重新 lookup → 确认态。流程闭环。

### 3.1 模式 A：`?s=<sessionId>` 直达

1. `DOMContentLoaded`：读 `URLSearchParams.get("s")`。
2. 有 `s` → 立即 `GET /v1/cli-auth/session?s=<id>`（见 §8 契约）。
   - **200 且 `status:"pending"`** → 渲染 confirm 态（填 deviceName、当前用户名）。
   - **200 且 `status` 为 `expired`/`consumed`/`denied`** → 直接落对应终态（用户刷新旧链接的场景）。
   - **404** → notfound 终态。
   - **网络错误/5xx** → error 终态（带重试，重试重放本次 lookup）。
   - **401** → Anotify.api 守卫跳登录（见 §3.0）。
3. 确认态点「批准授权」→ 校验 scope ≥1（否则显示行内错误、不提交）→ 双按钮 disable + 显示 busy 行 → `POST .../approve {scopes:[...]}`：
   - 200 → approved 终态。
   - 409（响应体带 `status`）→ 落对应终态（覆盖「lookup 后到点击前会话过期/被另一标签页处理」的竞态）。
   - 网络错 → `Anotify.toast` 报错，**留在确认态**、恢复按钮可点（允许重试）。
4. 点「拒绝」→ 不弹二次确认（拒绝是可逆的：重跑脚本即可再来；弹 confirm 反而给「阻止可疑请求」添堵）→ 双按钮 disable + busy → `POST .../deny` → 200 → denied 终态；409 → 对应终态；网络错 → toast + 留在确认态。

### 3.2 模式 B：无参输码

1. 无 `s` → entry 态，自动 focus 短码输入框。
2. **输入格式化**（全部在 `input` 事件里做，天然覆盖粘贴）：
   - 取 value → 大写 → 逐字符过滤，只保留字符集 **`ABCDEFGHJKMNPQRSTUVWXYZ23456789`**（32 字符，去歧义：无 0/O、1/I/L）→ 满 4 位自动插 `-` → 回写。
   - `maxlength="9"`（8 字符 + 1 连字符）。
   - 用户敲 `0`/`1`/`O`/`I` 直接被丢弃（合法短码不可能含它们，后端生成端必须用**同一字符集**，见 §8 给 worker 的对齐要求）。
3. 提交触发：① 满 8 位后 300ms 防抖自动提交；② 点「继续」；③ 回车（**必须照搬 login.html 的输入法守卫**：`e.isComposing || e.keyCode === 229` 时忽略）。
4. 提交 → 按钮 disable + busy 行 → `GET /v1/cli-auth/session?code=<8位无连字符>`：
   - 200 pending → confirm 态，并 `history.replaceState` 把 URL 改成 `?s=<sessionId>`（刷新不丢状态、登录回跳也带得上）。
   - 200 非 pending → 对应终态。
   - 404 → **留在 entry 态**，显示行内错误 `cliauth.entry.not_found`，输入框不清空（用户可能看错一位，全清要重打 8 位）、选中全文便于直接重打。
   - 网络错 → error 终态（重试重放 lookup）。

### 3.3 五种终态 + 网络错误态（每个都是预渲染 section）

| 终态 | 语义/文案要点 | 主行动作 |
| --- | --- | --- |
| **approved 已批准** | success。明确告诉用户「页面使命完成，结果在终端」：可关闭页面、回终端看结果 | 链接「前往 API Keys 查看 →」（Key 已下发，承接查看诉求）+ 次链接「返回工作台」 |
| **denied 已拒绝** | warn（用户主动拒绝不是系统失败，不用 error 红）。**必须含安全提示**：「如果这不是你的操作，检查是否有人在尝试登录你的账号」 | 次链接「返回工作台」 |
| **expired 已过期** | warn。说明 10 分钟 TTL，指引回终端**重跑脚本**拿新链接/短码 | 按钮「输入新短码」（页内切回 entry 态、`replaceState` 清 `?s`、清空输入框）+ 次链接「返回工作台」 |
| **notfound 未找到** | error。会话不存在 / 短码错误（模式 A 刷新旧链接、模式 B 打错码） | 按钮「重新输入」（同切回 entry）+ 次链接「返回工作台」 |
| **consumed 已领取** | 中性（`status-off` 灰徽）。Key 已下发过、不可重复授权；如需新 Key 回终端重跑 | 链接「前往 API Keys 查看 →」+ 次链接「返回工作台」 |
| **error 网络错误** | error。无法连接服务器 | 按钮「重试」（重放最后一次 lookup/approve/deny） |

### 3.4 「不是本人？切换账号」流程

确认态身份条内链接。点击 → `POST /v1/auth/logout`（忽略失败）→ `location = "login.html?next=" + encodeURIComponent("cli-auth.html" + location.search)`。**不要用 `Anotify.logout()`**（它固定跳 `login.html` 不带 next）。这样换号登录后仍能回到原授权会话。

---

## 4. 逐区块视觉规格

> 全部颜色只用 tokens 变量 / ui.css 组件类 / 现有页面的 Tailwind 中性色模式（zinc/white-alpha）。**零硬编码色值、零新色相。** 组件全部复用，不新造。

### 4.0 页面骨架（focus 布局内）

- 主列：`max-w-md mx-auto`，与 login 页相同节奏（`pb-16 pt-10` 由布局提供）。
- 卡上方标题区（仅 entry/confirm 态显示标题文字随状态切换）：`<h1 class="font-display text-3xl text-white">` 居中 + `<p class="mt-2 text-sm text-zinc-400">` 居中。
- 主卡：`<div class="glass w-full rounded-3xl p-8">`（与 login 卡完全同款）。
- 信任脚注：卡外下方，`flex flex-wrap items-center justify-center gap-x-5 gap-y-2 text-[11px] text-zinc-600`，三条、间隔符 `·`，第一条带 `badge-dot h-1.5 w-1.5 rounded-full bg-emerald-400` 点（照搬 login 信任行模式）。

### 4.1 状态图标（每个态一个，卡内顶部居中）

照搬 login 指纹图标的结构：`<div class="flex justify-center"><div class="flex h-16 w-16 items-center justify-center rounded-2xl" style="background: var(--xxx-soft)">` + 内嵌 24px SVG（`h-8 w-8`，`style="color: var(--xxx)"`）。

| 态 | 背景 | 图标色 | SVG 内容 |
| --- | --- | --- | --- |
| entry / confirm | `var(--accent-soft)` | `var(--accent)` | 终端提示符（复用 partials.js ICONS.agent 两条 path：`M4 17l6-6-6-6` + `M12 19h8`） |
| approved | `var(--success-soft)` | `var(--success)` | 对勾圆：`M22 11.08V12a10 10 0 1 1-5.93-9.14` + `M22 4L12 14.01l-3-3` |
| denied | `var(--warn-soft)` | `var(--warn)` | 斜杠盾：`M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10z` + `M4 4l16 16` |
| expired | `var(--warn-soft)` | `var(--warn)` | 时钟：`<circle cx="12" cy="12" r="10"/>` + `M12 6v6l4 2` |
| notfound | `var(--error-soft)` | `var(--error)` | 叉圆：`<circle cx="12" cy="12" r="10"/>` + `M15 9l-6 6M9 9l6 6` |
| consumed | `var(--surface-2)` | `var(--muted)` | 信息圆：`<circle cx="12" cy="12" r="10"/>` + `M12 16v-4M12 8h.01` |
| error | `var(--error-soft)` | `var(--error)` | 断链/告警三角：`M10.29 3.86L1.82 18a2 2 0 0 0 1.71 3h16.94a2 2 0 0 0 1.71-3L13.71 3.86a2 2 0 0 0-3.42 0z` + `M12 9v4M12 17h.01` |

confirm 态图标可加 login 页同款 `fingerprint-ring` 脉冲动画（`fpPulse` keyframes 复制到本页 style block），暗示「等待决策」；其余态静态。

### 4.2 entry 短码输入态（`#state-entry`）

```
┌────────────────────────────────────────┐
│              ┌────────┐                │
│              │  >_    │  ← 4.1 图标     │
│              └────────┘                │
│           输入授权码                     │ ← h1 font-display
│   输入终端中显示的 8 位短码，完成授权      │ ← sub, text-sm text-zinc-400
│ ┌────────────────────────────────────┐ │
│ │      A  B  C  D  -  1  2  3  4     │ │ ← .input mono 居中大号
│ └────────────────────────────────────┘ │
│ ⚠ 未找到该短码对应的授权会话…（默认隐藏） │ ← 行内错误 text-xs text-rose-300
│ ┌────────────────────────────────────┐ │
│ │               继续                  │ │ ← btn-primary w-full
│ └────────────────────────────────────┘ │
│ ⓘ 短码 10 分钟内有效 · 不区分大小写 ·    │ ← hint，带 info 小图标
│   不含 0/O、1/I 等易混淆字符            │
└────────────────────────────────────────┘
```

- 输入框：`<input id="code-input" class="input mono text-center" maxlength="9" placeholder="XXXX-XXXX" autocomplete="off" autocapitalize="characters" autocorrect="off" spellcheck="false" inputmode="text" style="font-size:1.5rem; letter-spacing:0.2em;">`。placeholder 颜色由 `.input::placeholder`（`var(--faint)`）自带。
- 行内错误：`<p id="entry-error" class="mt-2 hidden text-xs text-rose-300">`（沿用 keys.html `#create-error` 的类组合）。
- 继续按钮：`btn-primary mt-5 flex w-full items-center justify-center gap-2 rounded-xl px-4 py-3 text-sm font-medium`（与 login 主按钮同款）；busy 时 `disabled`（`.btn-primary:disabled` 自带半透明）+ 按钮右侧显示旋转点或文字切换为 busy 文案（busy 文案预渲染在按钮内第二个 span，JS 切 `hidden`，不运行时拼字符串）。
- hint 行：`mt-4 flex items-center justify-center gap-2 text-[11px] text-zinc-500` + 12px info SVG（照搬 login 的 security_note 行结构）。

### 4.3 confirm 确认态（`#state-confirm`）

```
┌──────────────────────────────────────────┐
│                ┌────────┐                │
│                │  >_    │ ← 图标（脉冲）  │
│                └────────┘                │
│            授权这台设备？                  │
│  批准后它将获得一个 API Key，以你的账号     │
│  执行勾选的权限                            │
│ ┌──────────────────────────────────────┐ │
│ │ [>_] MacBook-Pro (pi agent)          │ │ ← 设备卡
│ │      授权会话 10 分钟内有效            │ │
│ └──────────────────────────────────────┘ │
│ ┌──────────────────────────────────────┐ │
│ │ (Z) 以 zheng 的身份授权               │ │ ← 身份条
│ │     不是 zheng？退出并切换账号 →        │ │
│ └──────────────────────────────────────┘ │
│ 权限范围（至少选择一项）                    │
│ ┌──────────────────────────────────────┐ │
│ │ ☑ 发送通知  notify:send              │ │
│ │   允许 Agent 发送通知                  │ │
│ ├──────────────────────────────────────┤ │
│ │ ☐ 接收通知  notify:receive           │ │
│ │   接收 / 订阅通知                      │ │
│ ├──────────────────────────────────────┤ │
│ │ ☐ 查看设备  devices:read             │ │
│ │   查看设备列表                         │ │
│ └──────────────────────────────────────┘ │
│ ⚠ 请至少选择一个权限范围（默认隐藏）         │
│ ┌─────────┐ ┌──────────────────────────┐ │
│ │  拒绝    │ │         批准授权          │ │
│ └─────────┘ └──────────────────────────┘ │
│ ·· 正在提交…（busy 行，默认隐藏）           │
└──────────────────────────────────────────┘
```

- **设备卡**：`mt-5 flex items-center gap-3 rounded-xl px-4 py-3.5 ring-1 ring-white/10` + `style="background: var(--surface-2)"`。左侧 `plat-icon`（40px 圆角方块，内嵌终端 SVG，`color: var(--accent)`）；右侧两行：设备名 `text-sm font-medium text-white truncate`（`title` 属性放全名，超长省略）；副行 `mt-0.5 text-[11px] text-zinc-500` 放 TTL 提示。
- **身份条**（防错账号核心）：`mt-3 rounded-xl px-4 py-3 ring-1` + `style="background: var(--accent-soft); --tw-ring-color: rgba(139,139,253,0.2)"`（accent 浅底，视觉权重仅次于按钮）。内部 flex：
  - 头像圆：`flex h-9 w-9 shrink-0 items-center justify-center rounded-full bg-gradient-to-br from-indigo-500/30 to-violet-500/30 ring-1 ring-white/15 text-sm font-medium`（照搬 partials.js 侧栏头像类），JS 填用户名首字母大写。
  - 主行：`text-sm text-zinc-200`，结构 `<span>{{t prefix}}</span> <b class="text-white" id="identity-name">{username}</b> <span>{{t suffix}}</span>`（prefix/suffix 拆分是为四语言语序，见 §9 key 表）。
  - 次行切换链接：`mt-0.5 block text-[11px] text-zinc-400` 包 `<a>`（`style="color: var(--accent-strong)"`，hover  underline），逻辑见 §3.4。
- **scope 勾选组**：`mt-5`。标题 `text-xs font-medium text-zinc-400`。三个 `<label>` **逐字复用 keys.html 创建弹层的模式**：`flex cursor-pointer items-start gap-3 rounded-xl border border-white/10 p-3 transition-colors hover:border-white/20 has-[:checked]:border-indigo-400/50 has-[:checked]:bg-indigo-400/5`，内含 `<input type="checkbox" class="mt-0.5" checked（仅第一项默认勾）>` + 名称行（`text-sm text-zinc-200` + `<code class="mono text-[11px] text-zinc-500">notify:send</code>`）+ 说明行（`mt-0.5 block text-[11px] text-zinc-500`）。三项行之间 `space-y-2`。
- **scope 行内错误**：`mt-2 hidden text-xs text-rose-300`。
- **按钮行**：`mt-6 flex gap-3`。拒绝：`btn-ghost rounded-xl px-4 py-2.5 text-sm`（**不用 btn-danger**——拒绝是中性决策，视觉重点留给批准）。批准：`btn-primary flex-1 rounded-xl px-4 py-2.5 text-sm font-medium`。批准占余量更宽，主行动一目了然。
- **busy 行**：`mt-3 hidden items-center justify-center gap-2 text-[11px] text-zinc-500`（显示时去掉 hidden 加 flex）：`dot dot-warn badge-dot` 脉冲点 + busy 文案。

### 4.4 终态区块（`#state-result-*` 六个 section）

统一结构：

```
┌──────────────────────────────────┐
│          ┌────────┐              │
│          │  图标   │ ← 4.1 表格    │
│          └────────┘              │
│        [status-badge]            │ ← 语义徽
│           标题                    │ ← text-lg font-medium text-white
│      描述（1–2 句）               │ ← mt-2 text-sm leading-relaxed text-zinc-400
│   动作行（主链接/按钮 · 次链接）    │ ← mt-6 flex items-center justify-center gap-4
└──────────────────────────────────┘
```

- 图标、标题、徽全部居中（`flex flex-col items-center text-center`）。
- **徽**：`<span class="status-badge status-success|status-warn|status-error|status-off">`，对应关系见 §3.3 表格。
- **主链接样式**：`text-sm` + `style="color: var(--accent-strong)"`，hover underline；**次链接**：`text-sm text-zinc-500 hover:text-zinc-300 transition-colors`。
- **按钮型动作**（输入新短码 / 重新输入 / 重试）：`btn-ghost rounded-xl px-4 py-2 text-sm`。
- 描述里的关键短语（如「重跑脚本」「10 分钟」）用 `<b class="text-zinc-200">` 提亮（沿用 keys.html 安全提示模式）。

### 4.5 信任脚注

三条（key 见 §9）：授权仅在你的服务器上生效 · 短码 10 分钟过期 · Key 可随时在 API Keys 页停用。视觉照搬 login 页信任行（见 §4.0）。

---

## 5. 响应式（1280 桌面 / 390 移动）

本页是 `max-w-md` 单列聚焦布局，**天然响应式**，无栅格断点逻辑：

| 项 | 1280 桌面 | 390 移动 |
| --- | --- | --- |
| 主卡 | `max-w-md`（448px）居中，grid-overlay 径向网格背景可见 | 同卡，`px-5`（布局 main 自带）两侧留白 16px，grid-overlay 同样渲染（移动端不额外隐藏，与 login 页一致） |
| 标题 | `text-3xl` | 同（Fraunces 3xl 在 390 宽「授权这台设备？」单行放得下；en/ja/es 同理可折行，居中无破版） |
| 短码输入框 | `font-size:1.5rem + letter-spacing:0.2em`，9 字符 ≈ 250px，卡内宽 384px 富余 | 卡内宽 ≈ 326px，仍富余；`autocapitalize="characters"` 让 iOS 键盘直接大写 |
| scope 三行 | 单列 | 同单列（label 全文折行，`items-start` 对齐 checkbox） |
| 按钮行 | 拒绝固定宽 + 批准 `flex-1`，一行 | **保持一行不换行**（zh「拒绝/批准授权」、en「Deny/Approve」、ja「拒否/承認する」、es「Denegar/Aprobar」均为短词，390 下两按钮 + gap 放得下；gap-3 不变） |
| 终态动作行 | 主+次链接 `gap-4` 一行 | 允许 `flex-wrap` 折两行，居中不破版 |
| 顶栏 | logo + tagline + 语言切换 | tagline 在 `<sm` 隐藏（布局自带 `hidden sm:block`），语言切换器照常 |
| 侧栏 | 无（focus 布局） | 无 |

无需新增媒体查询；所有行为靠现有类与布局自带类达成。

---

## 6. 空态 / 错误态 / 降级态

| 场景 | 表现 |
| --- | --- |
| 后端完全不可达（fetch 抛错） | **error 终态**（§3.3），重试按钮重放请求。**本页无演示模式**——授权是写操作，伪造「批准成功」是安全事故；这是刻意的降级设计决策，与 keys/index 等读型页面的 demo 降级不同。 |
| 未登录（401） | Anotify.api 守卫自动跳登录并带 next；登录后回跳继续（§3.0）。用户无感知中断。 |
| `?s=` 指向过期会话（刷新旧页/旧链接） | lookup 返回 `status:"expired"` → 直接 expired 终态，用户当场看到原因与下一步。 |
| `?s=` 已被另一标签页批准过 | `status:"consumed"` → consumed 终态，避免二次下发。 |
| lookup 后、点击前会话过期（竞态） | approve/deny 返回 409 + status → 落对应终态，按钮不卡死。 |
| file:// 直开（i18n.js/后端都没有） | i18n 缺失时 `{{t}}` 已在构建期内联，页面文案仍在；fetch 必败 → error 终态。可接受，不额外处理。 |
| 短码含非法字符（0/1/O/I） | 输入时直接过滤丢弃（§3.2），hint 行已预告字符集规则，不报错打断。 |
| scope 全不勾点批准 | 不提交，显示行内错误 `scopes_required`；勾上任一项即隐藏。 |
| 设备名超长 / 用户名超长 | `truncate` + `title` 全名；身份条头像取首字符，不受影响。 |
| 语言切换器 | focus 布局带 `#lang-switcher-login` 容器，**但 partials.js 的自动挂载只在 `login.html` 路径生效**——本页脚本必须手动调一次 `Anotify.mountLoginLangSwitcher()`（它已导出）。无 JS 时模板渲染的平铺链接仍可用（渐进增强，布局自带）。 |

---

## 7. 给前端工程师的实现要点

### 7.1 文件纪律（web-src 是唯一页面源）

1. **新增** `web-src/layouts/focus.html`：克隆 `layouts/login.html`，改两处——logo `<a href="index.html">`；页脚改 `{{t "common.footer.copyright"}}`。
2. **新增** `web-src/pages/cli-auth.html`：首行 `<!-- layout: focus -->`（sitegen 靠首行注释选布局，见 `internal/sitegen/sitegen.go extractLayoutHint`），内含 `{{define "title"/"style"/"content"/"script"}}` 四块。
3. **编辑** `web-src/pages/login.html`：`NEXT_MAP` 加 `"cli-auth.html": "cli-auth.html"`。
4. **编辑** `web/partials.js`（它是**被跟踪的静态资源、直接编辑**，不是生成物）：`api()` 401 分支的 `PAGES` 数组加 `"cli-auth.html"`。
5. **编辑** `web-src/pages/keys.html`：头部「+ 新建 Key」按钮左侧加入口链接（§2.3），用 `{{t "keys.cli_auth_entry"}}`。
6. **编辑** `web-src/locales/{zh-CN,en,ja,es}.yaml`：按 §9 加 `cliauth.*` 全部 key + `keys.cli_auth_entry`。
7. **绝不手改** `web/*.html`、`web/i18n.*.js`（sitegen 生成物，gitignore）。改完跑 `make build`（sitegen + hash + go build 一条链），`web_verify` 逐页验无 JS 错误/溢出。

### 7.2 页面脚本纪律

- 内容区所有文案走构建期 `{{t}}`；**状态 section 全部预渲染、JS 只切 `hidden`**。运行时 JS 不拼任何面向用户的句子（busy/错误文案也都是预渲染 DOM 显隐）。
- **不要**调 `Anotify.mountLayout`（无侧栏布局）；**要**调一次 `Anotify.mountLoginLangSwitcher()`（原因见 §6 末行）。
- 数据填充只用 `textContent`（设备名/用户名来自后端，防 XSS——partials.js `el()` 或手写都行，禁用 innerHTML）。
- API 封装：401 必须转交 `Anotify.api`（借它的登录守卫）；其余非 2xx 自行解析 `{error, status}`。参考 keys.html `fetchApi` 的写法。
- 回车提交带输入法守卫（`isComposing`/`keyCode===229`），照抄 login.html。
- 切态时焦点管理：每个 section 的标题元素加 `tabindex="-1"`，切换后 `.focus()`（屏幕阅读器可感知状态变化，与 lang-hint 的焦点点管理同款思路）。
- 动效尊重 `prefers-reduced-motion`：confirm 图标脉冲动画包媒体查询禁用（参照 ui.css lang-hint 的处理）。
- 可选增强（P1，不做不阻塞）：confirm 态设备卡副行按 `expiresAt` 显示 mm:ss 倒计时（数字无语言问题）；到期自动切 expired 态。

### 7.3 API 契约（设计假定，须与 worker 的 openapi.yaml 对齐；camelCase，空数组 `[]`）

```
GET /v1/cli-auth/session?s=<sessionId>        # 模式A
GET /v1/cli-auth/session?code=<8位无连字符>    # 模式B
  200 { sessionId, deviceName, status, createdAt, expiresAt }
      status ∈ pending | expired | consumed | denied
  401 未登录（走登录守卫）   404 会话不存在/短码错误

POST /v1/cli-auth/session/{sessionId}/approve   body: { scopes: ["notify:send", ...] }
  200 { status: "approved" }                   409 { status: "expired"|"consumed"|"denied" }

POST /v1/cli-auth/session/{sessionId}/deny
  200 { status: "denied" }                     409 { status: ... }
```

给 worker 的对齐要求：**短码生成字符集必须是 `ABCDEFGHJKMNPQRSTUVWXYZ23456789`**（去 0/O/1/I/L），8 位，与前端过滤集完全一致；`deviceName` 形如「MacBook-Pro (pi agent)」；会话 TTL 10 分钟。

---

## 8. 安全设计说明（为什么长这样）

- **身份条用 accent 浅底而非普通灰条**：手机扫码场景下「手机登录态 ≠ 电脑账号」是最高发的真实事故，它必须仅次于主按钮的视觉权重，不能藏在正文里。
- **拒绝不弹二次确认、批准不默认全勾**：批准是不可逆下发凭证（默认只勾最小权限 notify:send），拒绝完全可逆（重跑脚本即可）——摩擦放在该放的一侧。
- **consumed 态中性灰而非红色**：已领取不是攻击也不是失败，告警色会误导用户以为出事；文案引导去 Keys 页核对即可。
- **denied 态含反钓鱼提示**：拒绝动作的受众分两类——主动拒绝者（要确认已生效）和「这请求不是我发的」警觉者（要安全指引），描述文案同时服务两者。
- **无演示模式**：见 §6，安全写操作页不伪造成功。

---

## 9. 四语言文案 key 表

> YAML 嵌套写法（sitegen 拍平成 `cliauth.entry.heading` 等点 key）。`{name}` 处为 JS 填充的用户名（prefix/suffix 拆分以适配语序）。

### zh-CN（默认语言）

```yaml
keys:
  cli_auth_entry: "CLI 设备授权"

cliauth:
  title: "CLI 设备授权"
  back_home: "返回工作台"
  entry:
    heading: "输入授权码"
    sub: "在终端运行登录脚本后，输入终端显示的 8 位短码完成授权"
    placeholder: "XXXX-XXXX"
    hint: "短码 10 分钟内有效 · 不区分大小写 · 不含 0/O、1/I 等易混淆字符"
    continue: "继续"
    busy: "正在查找会话…"
    not_found: "未找到该短码对应的授权会话，请核对后重试"
  confirm:
    heading: "授权这台设备？"
    sub: "批准后它将获得一个 API Key，以你的账号执行勾选的权限"
    ttl_hint: "授权会话 10 分钟内有效"
    identity_prefix: "以"
    identity_suffix: "的身份授权"
    switch_prefix: "不是"
    switch_suffix: "？退出并切换账号 →"
    scopes_label: "权限范围（至少选择一项）"
    scope_send_label: "发送通知"
    scope_send_desc: "允许 Agent 发送通知"
    scope_receive_label: "接收通知"
    scope_receive_desc: "接收 / 订阅通知"
    scope_read_label: "查看设备"
    scope_read_desc: "查看设备列表"
    scopes_required: "请至少选择一个权限范围"
    approve: "批准授权"
    deny: "拒绝"
    busy: "正在提交…"
  done:
    approved_badge: "已批准"
    approved_title: "授权成功"
    approved_desc: "可以关闭此页面，回到终端查看结果。"
    approved_keys_link: "前往 API Keys 查看 →"
    denied_badge: "已拒绝"
    denied_title: "已拒绝本次授权"
    denied_desc: "终端会收到拒绝结果。如果这不是你的操作，请检查是否有人在尝试登录你的账号。"
    expired_badge: "已过期"
    expired_title: "会话已过期"
    expired_desc: "授权会话 10 分钟内有效。请回到终端重新运行登录脚本，获取新的链接或短码。"
    expired_action: "输入新短码"
    notfound_badge: "未找到"
    notfound_title: "会话不存在或短码错误"
    notfound_desc: "请核对终端显示的短码后重试，或重新运行登录脚本。"
    notfound_action: "重新输入"
    consumed_badge: "已使用"
    consumed_title: "该会话已完成授权"
    consumed_desc: "Key 已下发过，不能重复授权。如需新的 Key，请回到终端重新运行登录脚本。"
    error_title: "无法连接服务器"
    error_desc: "请检查网络连接后重试。"
    retry: "重试"
  trust:
    local: "授权仅在你的服务器上生效"
    ttl: "短码 10 分钟过期"
    revoke: "Key 可随时在 API Keys 页停用"
```

### en

```yaml
keys:
  cli_auth_entry: "CLI Device Authorization"

cliauth:
  title: "CLI Device Authorization"
  back_home: "Back to dashboard"
  entry:
    heading: "Enter authorization code"
    sub: "After running the login script in your terminal, enter the 8-character code shown there to authorize it."
    placeholder: "XXXX-XXXX"
    hint: "Valid for 10 minutes · Case-insensitive · No ambiguous characters like 0/O or 1/I"
    continue: "Continue"
    busy: "Looking up session…"
    not_found: "No authorization session matches this code. Check it and try again."
  confirm:
    heading: "Authorize this device?"
    sub: "Once approved, it will receive an API key that acts as your account with the selected permissions."
    ttl_hint: "Authorization session valid for 10 minutes"
    identity_prefix: "Authorizing as "
    identity_suffix: ""
    switch_prefix: "Not "
    switch_suffix: "? Sign out and switch account →"
    scopes_label: "Permissions (select at least one)"
    scope_send_label: "Send notifications"
    scope_send_desc: "Allow the agent to send notifications"
    scope_receive_label: "Receive notifications"
    scope_receive_desc: "Receive / subscribe to notifications"
    scope_read_label: "View devices"
    scope_read_desc: "View the device list"
    scopes_required: "Select at least one permission"
    approve: "Approve"
    deny: "Deny"
    busy: "Submitting…"
  done:
    approved_badge: "Approved"
    approved_title: "Authorization approved"
    approved_desc: "You can close this page and return to the terminal for the result."
    approved_keys_link: "View in API Keys →"
    denied_badge: "Denied"
    denied_title: "Authorization denied"
    denied_desc: "The terminal will be notified. If this wasn't you, check whether someone is trying to sign in to your account."
    expired_badge: "Expired"
    expired_title: "Session expired"
    expired_desc: "Authorization sessions are valid for 10 minutes. Run the login script again in your terminal to get a new link or code."
    expired_action: "Enter a new code"
    notfound_badge: "Not found"
    notfound_title: "Session not found or incorrect code"
    notfound_desc: "Check the code shown in your terminal and try again, or re-run the login script."
    notfound_action: "Try again"
    consumed_badge: "Already used"
    consumed_title: "This session was already authorized"
    consumed_desc: "A key was already issued for this session. Run the login script again in your terminal to create a new one."
    error_title: "Cannot reach the server"
    error_desc: "Check your connection and try again."
    retry: "Retry"
  trust:
    local: "Authorization only takes effect on your server"
    ttl: "Codes expire in 10 minutes"
    revoke: "Keys can be revoked anytime in API Keys"
```

### ja

```yaml
keys:
  cli_auth_entry: "CLI デバイス認証"

cliauth:
  title: "CLI デバイス認証"
  back_home: "ダッシュボードに戻る"
  entry:
    heading: "認証コードを入力"
    sub: "ターミナルでログインスクリプトを実行後、表示された 8 文字のコードを入力して承認してください"
    placeholder: "XXXX-XXXX"
    hint: "コードは 10 分間有効 · 大文字小文字は区別しません · 0/O・1/I などの紛らわしい文字は含まれません"
    continue: "続ける"
    busy: "セッションを検索中…"
    not_found: "このコードに対応する認証セッションが見つかりません。確認して再入力してください"
  confirm:
    heading: "このデバイスを承認しますか？"
    sub: "承認すると、選択した権限であなたのアカウントとして動作する API キーが発行されます"
    ttl_hint: "認証セッションは 10 分間有効です"
    identity_prefix: ""
    identity_suffix: " として承認します"
    switch_prefix: ""
    switch_suffix: " ではありませんか？ログアウトして切り替え →"
    scopes_label: "権限範囲（少なくとも 1 つ選択）"
    scope_send_label: "通知を送信"
    scope_send_desc: "Agent が通知を送信することを許可"
    scope_receive_label: "通知を受信"
    scope_receive_desc: "通知の受信・購読"
    scope_read_label: "デバイスを表示"
    scope_read_desc: "デバイス一覧の表示"
    scopes_required: "少なくとも 1 つの権限を選択してください"
    approve: "承認する"
    deny: "拒否"
    busy: "送信中…"
  done:
    approved_badge: "承認済み"
    approved_title: "承認しました"
    approved_desc: "このページを閉じて、ターミナルで結果を確認してください。"
    approved_keys_link: "API Keys で確認 →"
    denied_badge: "拒否しました"
    denied_title: "この承認を拒否しました"
    denied_desc: "ターミナルに拒否結果が通知されます。心当たりがない場合は、第三者がアカウントにログインしようとしていないか確認してください。"
    expired_badge: "期限切れ"
    expired_title: "セッションの有効期限が切れました"
    expired_desc: "認証セッションは 10 分間有効です。ターミナルでログインスクリプトを再実行し、新しいリンクまたはコードを取得してください。"
    expired_action: "新しいコードを入力"
    notfound_badge: "見つかりません"
    notfound_title: "セッションが存在しないか、コードが間違っています"
    notfound_desc: "ターミナルに表示されたコードを確認して再試行するか、ログインスクリプトを再実行してください。"
    notfound_action: "再入力"
    consumed_badge: "使用済み"
    consumed_title: "このセッションは承認済みです"
    consumed_desc: "このセッションのキーはすでに発行されています。新しいキーが必要な場合は、ターミナルでログインスクリプトを再実行してください。"
    error_title: "サーバーに接続できません"
    error_desc: "ネットワーク接続を確認して再試行してください。"
    retry: "再試行"
  trust:
    local: "承認はあなたのサーバー上でのみ有効です"
    ttl: "コードは 10 分で失効します"
    revoke: "キーは API Keys ページでいつでも無効化できます"
```

### es

```yaml
keys:
  cli_auth_entry: "Autorización de dispositivo CLI"

cliauth:
  title: "Autorización de dispositivo CLI"
  back_home: "Volver al panel"
  entry:
    heading: "Introduce el código de autorización"
    sub: "Tras ejecutar el script de inicio de sesión en tu terminal, introduce el código de 8 caracteres que aparece allí."
    placeholder: "XXXX-XXXX"
    hint: "Válido durante 10 minutos · No distingue mayúsculas · Sin caracteres ambiguos como 0/O o 1/I"
    continue: "Continuar"
    busy: "Buscando la sesión…"
    not_found: "No se encontró ninguna sesión de autorización para este código. Compruébalo e inténtalo de nuevo."
  confirm:
    heading: "¿Autorizar este dispositivo?"
    sub: "Al aprobarlo, recibirá una clave API que actuará como tu cuenta con los permisos seleccionados."
    ttl_hint: "La sesión de autorización es válida durante 10 minutos"
    identity_prefix: "Autorizando como "
    identity_suffix: ""
    switch_prefix: "¿No eres "
    switch_suffix: "? Cierra sesión y cambia de cuenta →"
    scopes_label: "Permisos (selecciona al menos uno)"
    scope_send_label: "Enviar notificaciones"
    scope_send_desc: "Permite al agente enviar notificaciones"
    scope_receive_label: "Recibir notificaciones"
    scope_receive_desc: "Recibir notificaciones y suscripciones"
    scope_read_label: "Ver dispositivos"
    scope_read_desc: "Ver la lista de dispositivos"
    scopes_required: "Selecciona al menos un permiso"
    approve: "Aprobar"
    deny: "Denegar"
    busy: "Enviando…"
  done:
    approved_badge: "Aprobado"
    approved_title: "Autorización aprobada"
    approved_desc: "Puedes cerrar esta página y volver a la terminal para ver el resultado."
    approved_keys_link: "Ver en API Keys →"
    denied_badge: "Denegado"
    denied_title: "Autorización denegada"
    denied_desc: "La terminal recibirá el resultado. Si no has sido tú, comprueba si alguien está intentando acceder a tu cuenta."
    expired_badge: "Caducado"
    expired_title: "La sesión ha caducado"
    expired_desc: "Las sesiones de autorización son válidas durante 10 minutos. Vuelve a ejecutar el script en tu terminal para obtener un nuevo enlace o código."
    expired_action: "Introducir un código nuevo"
    notfound_badge: "No encontrado"
    notfound_title: "La sesión no existe o el código es incorrecto"
    notfound_desc: "Comprueba el código que aparece en tu terminal e inténtalo de nuevo, o vuelve a ejecutar el script."
    notfound_action: "Reintentar"
    consumed_badge: "Ya utilizado"
    consumed_title: "Esta sesión ya fue autorizada"
    consumed_desc: "Ya se emitió una clave para esta sesión. Vuelve a ejecutar el script en tu terminal si necesitas una nueva."
    error_title: "No se puede conectar con el servidor"
    error_desc: "Comprueba tu conexión e inténtalo de nuevo."
    retry: "Reintentar"
  trust:
    local: "La autorización solo tiene efecto en tu servidor"
    ttl: "Los códigos caducan en 10 minutos"
    revoke: "Las claves se pueden desactivar en cualquier momento en API Keys"
```

---

## 10. 验收自查（对照任务要点）

- [x] 双模式同页切换（§3.1/§3.2）、确认态四要素齐全：设备名 / 身份（防错账号）/ scope 三勾选（默认勾 send、全不勾禁提交）/ 批准+拒绝
- [x] 未登录跳 login 并回跳（沿用 NEXT_MAP，§3.0 + 两个注册点 §7.1.3/7.1.4）
- [x] 五终态 + 网络错误态全部有视觉与文案（§3.3/§4.4）
- [x] 入口方案已拍板并给理由（§2.3，keys 页入口、否掉侧栏项）
- [x] 只用 tokens/现有组件，零硬编码色值；逐区块规格可照做（§4）
- [x] 1280/390 响应式说明（§5）；空态/错误态/降级态专章（§6）
- [x] 实现要点含 web-src 纪律、PAGES/NEXT_MAP 注册、`mountLoginLangSwitcher` 手动调用、输入法守卫、XSS 防护（§7）
- [x] 四语言 key 表全量给出（§9）
