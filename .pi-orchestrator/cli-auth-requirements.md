# 需求文档 · CLI 设备授权登录（Device Authorization Login）

> 版本：v1.0（PM 产出，方案已与最终用户逐条拍板，本文档做产品化与验收标准补全，不重新选型）
> 目标读者：anotify-designer（确认页设计）、anotify-worker（后端实施）、anotify-frontend（前端实现）、anotify-tester（测试把关）、anotify-reviewer（终审）
> 建议落盘：`.pi-orchestrator/cli-auth-requirements.md`

---

## 1. 背景与痛点

### 1.1 现状

当前分发 API Key 的唯一路径：

1. 用户在浏览器登录 Anotify Web 控制台 → 打开 `keys.html`
2. 点「+ 新建 Key」→ 填名称、勾 scope → 生成，明文仅展示一次
3. 用户**手动复制** Key，粘贴到 Agent 运行环境（环境变量 / 配置文件 / 直接贴给 Agent）

### 1.2 痛点（按严重度排序）

| # | 痛点 | 后果 |
|---|------|------|
| P-1 | **Key 明文经过聊天上下文 / 剪贴板**：用户经常直接把 Key 贴进 Agent 对话，或让 Agent 自己读取剪贴板 | Agent（及其背后 LLM 提供商的日志）能看到 Key 明文，违背最小暴露原则；Key 一旦泄露只能手动吊销重建 |
| P-2 | **无浏览器 / 纯 SSH / 服务器场景无法完成授权**：很多 Agent 跑在远端服务器、容器、CI 里，那里没有浏览器，甚至没有显示环境 | 用户必须在另一台有浏览器的机器上建 Key，再通过带外渠道（IM、邮件、笔记软件）搬运到目标机，每一步都是泄露面 |
| P-3 | **多机器分发繁琐**：一个开发者通常有多台跑 Agent 的机器（本机、云主机、NAS、CI） | 每台机器重复「登录 web → 建 Key → 复制 → 粘贴」全流程，Key 管理页迅速堆满语义不清的 Key |
| P-4 | **Key 与机器无对应关系**：手动建 Key 时命名靠自觉 | 三个月后看到「未命名 Key」不知道是哪台机器在用，不敢吊销 |

### 1.3 产品价值

让「Agent 拿到一个可用的上报凭证」从 **人工跨设备搬运** 变成 **一条命令**：Agent 在用户机器上执行登录脚本，用户在**任意一台已登录设备**（含手机）上点一次「批准」，凭证自动落到该机器的 `~/.config/anotify/credentials.json`，**全程 Key 明文不进入 Agent 对话上下文、不经过剪贴板、不经过任何第三方信道**。

这与 Anotify 的核心定位一致：让开发者「跑着 Agent 不用盯终端」；CLI 授权登录补上的是「Agent 连上 Anotify」这第一公里的体验。

---

## 2. 用户故事

### US-1 · 本机有浏览器（最常见）

> 作为跑 Agent 的开发者，我在自己 Mac 上让 Agent 执行登录脚本，脚本自动弹出浏览器授权页，我点「批准」，Agent 的机器上就拿到了 Key。全程我没有复制粘贴过任何密钥。

场景：本地开发机（macOS / Linux 桌面 / Windows WSL 有浏览器可跳）。这是主路径，成功率与体验必须最好。

### US-2 · 远端服务器无浏览器（SSH）

> 作为在 SSH 到云主机上跑 Agent 的开发者，我让 Agent 在远端执行登录脚本，脚本检测到无显示环境，在终端打印一个文本二维码，我用手机扫码进入已登录的授权页点「批准」，远端机器拿到 Key。

场景：云主机 / 家用服务器 / NAS / 容器内。二维码内容 = 授权链接（含 sessionId + userCode），手机须能访问该 Anotify 实例（同一内网或公网 URL）。

### US-3 · 手机也扫不了码 / 二维码渲染不了

> 作为用户，我在一台连二维码都无法使用的环境（如串口终端、极简 TTY）执行登录脚本，终端显示一个 8 位短码（如 `K7QX-3F9M`），我在任意一台已登录的浏览器打开授权页、手动输入短码、确认后批准，目标机器拿到 Key。

场景：兜底路径。短码去歧义字符集（无 0/O、1/I/L 等），8 位 + 中划线，手动输入成本低、不可遍历（登录 + 限速双重防线）。

### US-4 · 用户收紧授权范围

> 作为安全敏感的用户，我在确认页看到脚本申请了 `notify:send`，页面还提供 `notify:receive` / `devices:read` 可选。我默认只批 `notify:send`，或者反选掉某个 scope 再批准。最终签发的 Key 只有我在页面上勾选的那些 scope。

场景：用户有最终决定权，脚本申请的 scope 只是「默认值」。

### US-5 · 用户在确认页核实身份与设备

> 作为用户，我在批准前能在确认页清楚看到：「以 zheng 的身份授权」、设备名 `agent:my-macbook`，防止我在错误账号下批准、或分不清是哪台机器在请求。

### US-6 · 用户事后管理

> 作为用户，我事后在 keys.html 看到这台机器自动建的 Key（名称 `agent:<hostname>`，前缀、scope、最近使用时间清晰），可以随时停用 / 吊销，和手动建的 Key 体验一致。

### US-7 · Agent 拿与服务等版本的脚本

> 作为自托管用户，我从自己的 Anotify 实例用一条 curl 拿到登录脚本（`GET /agent-login.sh`），脚本与我部署的服务版本完全一致，不需要额外装 CLI 工具或去 GitHub 下载。

---

## 3. 功能范围

### 3.1 做（In Scope）

#### A. 服务端

1. **授权会话生命周期 API**
   - `POST` 建会话：脚本携带设备名（hostname）、申请 scope 列表 → 返回 `sessionId` / `secret` / `userCode` / `authUrl` / `pollInterval` / `expiresAt`（TTL 10 分钟）
   - `GET .../qr.txt`：返回 ASCII 文本二维码（内容 = `authUrl`），供脚本直接打印到终端
   - `GET` 轮询：脚本带 `sessionId` + `secret`（query 参数）轮询；状态机：`pending` → `approved` →（首次有效 poll 时事务内建 Key、明文一次性下发）→ `consumed`；另有 `denied` / `expired`
   - 确认页相关端点：按 sessionId 或 userCode 查询会话详情（设备名、申请 scope）供确认页渲染；批准 / 拒绝端点（需登录会话）
2. **授权确认页**（新前端页面，走现有 sitegen 体系：web-src/pages + locales 双语）
   - 三条入口汇入同一个确认页（见 3.1-B）
   - 显示：设备名、当前登录身份（「以 xxx 的身份授权」）、scope 勾选框（默认勾脚本申请的项，每项带说明文案）、批准 / 拒绝按钮
   - 未登录访问 → 跳登录页 → 登录成功回跳确认页（**需把确认页页名加入 login.html 的 `NEXT_MAP` 白名单**，这是现有 open-redirect 防线，必须复用不得绕过）
3. **Key 签发复用现有机制**：沿用 `internal/auth.KeyManager.CreateKey`，`ant_*` 不透明 Key、永不过期、argon2id 只存哈希、keys.html 可见可吊销、自动命名 `agent:<hostname>`。**明文不落库**（仅首次有效 poll 的响应体中出现一次）
4. **脚本分发**：单二进制 embed 并 serve 登录脚本 `GET /agent-login.sh`（内容类型 `text/x-sh` 或 `text/plain`，no-store 缓存策略，与二进制同版本）
5. **安全防线**
   - 建会话按 IP 限速（现状服务端无限速组件，需新建，注意单二进制约束：内存计数器即可，不引入 Redis）
   - 短码 lookup 必须登录 + 按用户限速（防遍历 8 位码空间）
   - 会话 TTL 10 分钟，过期自动转 `expired` 终态
   - `secret` 仅在建会话响应中返回一次，脚本保存在内存，**不出现在任何 URL / 二维码 / 短码 / 日志中**

#### B. 登录脚本 `scripts/login.sh`（POSIX sh + curl，零依赖）

1. 建会话并解析响应（`sessionId` / `secret` / `userCode` / `authUrl` / `pollInterval` / `expiresAt`）
2. **三种授权入口同时给出，用户任选其一**：
   - a. 检测到本地有可用浏览器（且非 SSH / 无显示环境）→ 自动 `open`（macOS）/ `xdg-open`（Linux）打开 `authUrl`
   - b. 拉取并打印 ASCII 文本二维码（`GET .../qr.txt`）
   - c. 打印 8 位短码 `XXXX-XXXX` 与手动输码入口地址
   - 三者**同时展示**（如：尝试 open 的同时仍打印二维码与短码），用户用哪条都行
3. 带 `secret` 按 `pollInterval` 轮询，直到 `consumed`（拿到 Key）/ `denied` / `expired` / 网络错误重试上限
4. 拿到 Key 后写入 `~/.config/anotify/credentials.json`：
   - 目录不存在则创建（0700）
   - 文件权限 0600
   - **原子写入**（先写临时文件再 rename），防 Ctrl-C 留下半截 JSON
   - 已存在同名凭证时的行为：覆盖前先备份或直接原子覆盖（见开放问题 Q-3）
5. **stdout / stderr 永不打印 Key 明文**：只打状态行（「等待批准…」「已批准，正在领取凭证…」「凭证已写入 ~/.config/anotify/credentials.json」「已拒绝 / 已过期」）
6. 依赖仅 POSIX sh + curl（JSON 解析用 shell 文本处理，**不得依赖 jq / python / node**——零依赖是本方案的硬约束）

### 3.2 不做（Out of Scope · 明确边界）

| # | 不做项 | 理由 |
|---|--------|------|
| N-1 | **不做插件 / 编辑器集成 / 各 Agent 框架专属适配** | 只交付 POSIX 脚本这一条通用路径；插件生态超出单二进制自托管定位，属于社区后话 |
| N-2 | **不做 Key 过期时间 / 自动轮换** | 沿用现有 `ant_*` 不透明 Key 模型（永不过期 + 可吊销）。给 Agent 的 Key 加过期会导致长跑 Agent 静默失联，违背「可靠推送」核心承诺 |
| N-3 | **不用 JWT 替代 ant_* Key** | 永不过期 + 不可吊销是致命组合；单二进制架构用不上 JWT 的无状态校验优势，反而引入密钥轮换复杂度 |
| N-4 | **不做 loopback 回跳**（脚本起本地 HTTP 端口等浏览器回调） | 增加端口冲突 / 防火墙 / 容器网络复杂度；轮询已足够可靠，OAuth device flow 主流实现（GitHub/AWS SSO）同样用轮询 |
| N-5 | **不做二维码图片（PNG/SVG）渲染与内嵌二维码库** | ASCII 文本二维码由服务端渲染（Go 生成纯文本），脚本零依赖；不引入前端二维码 JS 库作为授权链路必需品 |
| N-6 | **不做授权页的 WebAuthn 免登录批准**（即在确认页再验一次 Passkey 才放行） | 会话 Cookie 已是强认证（Passkey 登录）；加二次验证属于可后续叠加的增强，不进本期 |
| N-7 | **不做多凭证 profile 管理**（credentials.json 里多套配置切换） | 单凭证文件覆盖 90% 场景；多 profile 留给后续 |
| N-8 | **不做脚本自动更新 / 版本协商** | 脚本由服务端同版本 serve，天然一致；不做自更新逻辑（安全隐患大于收益） |
| N-9 | **不在 keys.html 增加「授权会话管理」UI**（如查看进行中的授权会话列表） | 会话 TTL 仅 10 分钟且一次性，管理价值低；keys.html 只看到最终产出的 Key 即可 |
| N-10 | **不做授权链接的一次性使用限制**（authUrl 可被多人打开看到确认页，但批准动作需登录，且领 Key 必须 secret） | 安全模型已保证 URL 泄露最坏结果只是别人替你点批准，领不走 Key；加一次性限制会破坏「三入口汇入同一会话」的体验 |

### 3.3 安全模型核心不变量（所有验收不得破坏）

> **`sessionId` / `userCode` 只授予「批准权」；`secret` 才授予「领 Key 权」。**

- `authUrl`（含 sessionId + userCode）、二维码、短码，三者任何一个泄露，攻击者能做的最坏事情 = 替真实用户点「批准」（而批准本身还需要攻击者有该实例的登录账号），**无法领走 Key**
- 反之，只有 `secret`（脚本内存中）才能从轮询端点取到 Key 明文
- Key 明文全链路只出现两次：服务端首次有效 poll 的响应体 + 落盘 credentials.json（0600）。**不出现**在：URL、二维码、短码、终端输出、日志、数据库

---

## 4. 验收标准（Given / When / Then）

> 说明：`<sid>`=sessionId，`<sec>`=secret，`<code>`=userCode。所有 API 测试用 curl 可重现；脚本行为用真实执行验证。验收标准编号供 tester / reviewer 引用。

### 4.1 建会话

**AC-01 · 建会话成功**
- **Given** Anotify 服务正常运行
- **When** 脚本 `POST` 建会话（携带设备名 `my-macbook`、scopes `["notify:send"]`）
- **Then** 响应 200，JSON 含 `<sid>`（非空唯一）、`<sec>`（高强度随机，≥128 bit 熵）、`<code>`（8 位、去歧义字符集、形如 `XXXX-XXXX`）、`authUrl`（绝对 URL，含 `<sid>` 与 `<code>`，**不含 `<sec>`**）、`pollInterval`（正整数秒）、`expiresAt`（≈ 建会话时间 + 600 秒，误差 ≤ 5 秒）

**AC-02 · 响应中 secret 不进入 URL 族**
- **Given** AC-01 的响应
- **When** 检查 `authUrl`、二维码文本内容、短码
- **Then** 三者均不含 `<sec>`，也不含 `<sec>` 的任何可推导片段

**AC-03 · 建会话参数校验**
- **When** 脚本提交空 scopes、未知 scope（如 `admin:*`）、超长设备名（>64 字符）
- **Then** 分别返回 4xx 且错误信息明确；不建会话（DB 无记录）

**AC-04 · 建会话按 IP 限速**
- **When** 同一源 IP 在限速窗口内（如 1 分钟内）连续建会话超过阈值（如 10 次）
- **Then** 超限请求返回 429；窗口过后恢复 200
- **And** 限速不依赖外部组件（无 Redis），重启服务后限速器状态可重置（可接受）

### 4.2 三入口汇合

**AC-05 · 浏览器直开入口**
- **Given** 已建会话，且用户已在该浏览器登录 Anotify
- **When** 浏览器打开 `authUrl`
- **Then** 进入确认页，显示：设备名 `agent:my-macbook`、`以 <当前登录用户名> 的身份授权`、scope 勾选区（`notify:send` 已默认勾选且带说明文案；`notify:receive` / `devices:read` 未勾选且带说明文案）、批准与拒绝按钮

**AC-06 · 二维码入口**
- **Given** 已建会话
- **When** 脚本 `GET .../qr.txt`
- **Then** 响应 200 `text/plain`，内容为可扫描的 ASCII/Unicode 文本二维码；扫码解码结果 == `authUrl`（逐字符一致）；手机扫码后打开的页面与 AC-05 相同

**AC-07 · 短码入口**
- **Given** 已建会话，用户在任意已登录浏览器打开授权页（不带参数的入口地址）
- **When** 在输入框键入 `<code>`（大小写不敏感、容忍用户输入的中划线/空格变体）
- **Then** 页面定位到同一会话，进入与 AC-05 相同的确认页（同一 `<sid>`）

**AC-08 · 三入口指向同一会话**
- **Given** 一次建会话
- **When** 先后通过 authUrl、二维码解码链接、短码三种方式打开确认页
- **Then** 三个页面展示的设备名、申请 scope 完全一致；在其中任一处点「批准」，其余入口随后刷新均显示「已批准」终态

**AC-09 · 未登录先跳登录再回跳**
- **Given** 已建会话，浏览器**未登录**
- **When** 打开 `authUrl`（或短码入口提交短码）
- **Then** 302/跳转到登录页且 `?next=` 指向确认页（页名在 login.html `NEXT_MAP` 白名单内）；完成 Passkey 登录后自动回跳确认页并正确显示会话详情；**不允许**通过篡改 `?next=` 跳到白名单外地址

**AC-10 · 短码 lookup 需登录 + 限速**
- **When** 未登录请求短码 lookup 端点 → 401；同一登录用户在窗口内连续查询超过阈值（如 20 次/分钟，含错误码）→ 429
- **And** 查询不存在的短码与查询到存在短码的响应**耗时与状态码无可区分差异**以外的信息泄露（错误文案统一为「授权码无效或已过期」，不区分「不存在」与「已过期」）

### 4.3 批准 / 拒绝与 scope 减量

**AC-11 · 批准（默认 scope）**
- **Given** 确认页已打开，`notify:send` 默认勾选
- **When** 用户不改动勾选直接点「批准」
- **Then** 会话转 `approved`；随后脚本首次有效 poll 返回 200 且 body 含一次性 Key 明文，该 Key 的 scope == `["notify:send"]`（用该 Key 调 `POST /v1/notify` 成功；调 `GET /v1/stream` 或 `GET /v1/devices` 被拒 403）

**AC-12 · scope 勾选减量生效**
- **Given** 脚本申请 scopes `["notify:send","notify:receive"]`，确认页两项均默认勾选
- **When** 用户**反选** `notify:receive`，仅保留 `notify:send` 后批准
- **Then** 最终签发 Key 的 scope 只含 `notify:send`（`GET /v1/stream` 用该 Key 被拒 403）；keys.html 中该 Key 显示的 scope 与实际一致

**AC-13 · 不允许勾出脚本未申请的 scope**
- **When** 确认页提交的勾选集合包含脚本建会话时未申请的 scope（构造请求绕过前端勾选框）
- **Then** 服务端拒绝（4xx）或静默剔除未申请项；最终 Key 的 scope ⊆ 脚本申请集合 ∩ 用户勾选集合

**AC-14 · 全不勾不能批准**
- **When** 用户反选所有 scope 后点批准
- **Then** 前端阻止提交并提示「至少保留一个权限」；若构造请求绕过，服务端返回 4xx 且不建 Key

**AC-15 · 拒绝**
- **When** 用户点「拒绝」
- **Then** 会话转 `denied`；脚本下一次 poll 收到明确拒绝信号（非超时），打印「已被拒绝」类状态行并以非 0 退出码退出；不建 Key（keys.html 无新增）

**AC-16 · 重复批准/拒绝幂等**
- **When** 会话已 `approved`/`denied`/`consumed`/`expired` 后再次点批准或拒绝
- **Then** 返回明确终态提示（如 409 或 200 + 终态文案），不改变既有终态，不重复建 Key

### 4.4 一次性领取（安全模型核心）

**AC-17 · 首次有效 poll 建 Key、明文一次性下发**
- **Given** 会话已被批准（`approved`），脚本尚未 poll 到结果
- **When** 脚本带 `<sid>` + `<sec>` 发起 poll
- **Then** 响应 200 含 Key 明文；服务端在同一事务内完成：创建 Key 记录（名称 `agent:my-macbook`、argon2id 哈希入库、明文**不落库**）、会话置 `consumed`；DB 中任何表查询不到 Key 明文（用明文全文 grep SQLite 文件无命中）

**AC-18 · 第二次 poll 不再发 Key**
- **Given** AC-17 已完成（会话 `consumed`）
- **When** 用相同 `<sid>` + `<sec>` 再次 poll
- **Then** 响应不含 Key 明文（返回终态信息，如 `{"status":"consumed"}` 或 410）；DB 中 Key 记录数不增加

**AC-19 · 错误 secret 领不到 Key**
- **Given** 会话已批准
- **When** 用真实 `<sid>` + 伪造 `<sec>` poll
- **Then** 401/403，不含 Key；且**不消费**会话（随后用正确 `<sec>` poll 仍能拿到 Key）
- **And** secret 比对使用常量时间比较（代码评审项，不列运行时测试）

**AC-20 · 仅有 sessionId/userCode 无法领 Key（不变量验收）**
- **When** 仅用 `<sid>`（无 `<sec>`）、或仅用 `<code>`，对轮询/领取端点发起请求（所有合理参数组合）
- **Then** 一律拿不到 Key 明文（401/403/4xx）；模拟「authUrl 泄露给攻击者」场景：攻击者打开确认页甚至完成批准，仍无任何端点可凭 `<sid>`/`<code>` 取到 Key

### 4.5 凭据文件与脚本输出

**AC-21 · 凭据文件落盘与权限**
- **Given** 脚本成功领取 Key
- **When** 检查 `~/.config/anotify/credentials.json`
- **Then** 文件存在；权限位恰为 `0600`（`stat -f %Lp` / `stat -c %a` 验证）；所在目录 `~/.config/anotify` 权限为 `0700`；内容为合法 JSON，含 Key 字段（字段名以实现为准，如 `apiKey`）及必要元数据（如 server URL、创建时间）

**AC-22 · 原子写入**
- **Given** 脚本运行中
- **When** 在写凭据文件瞬间发送 SIGINT（可通过故障注入/代码评审验证）
- **Then** 不存在内容截断的 credentials.json（要么旧版完整、要么新版完整）；目录中无残留临时文件（脚本正常退出路径下）

**AC-23 · stdout/stderr 无 Key 明文**
- **Given** 完整跑一次授权流程（批准 → 领取 → 落盘）
- **When** 捕获脚本全过程 stdout + stderr（含 `-v`/调试模式若有）
- **Then** 输出中不含 Key 明文，也不含 `<sec>`；仅含状态行（等待中/已批准/已写入路径/错误原因）；退出码：成功 0、拒绝/过期/超时/错误均非 0 且可区分（文档化各退出码含义）

**AC-24 · 脚本零依赖**
- **Given** 最小环境（如 `debian:stable-slim` 容器，仅 `sh` + `curl`，无 jq/python/node/bash）
- **When** 用 `sh login.sh`（显式以 POSIX sh 解释执行）跑通全流程
- **Then** 成功拿到凭据文件；`dash` 与 `busybox sh` 下同样通过（至少其一，最好两者）

**AC-25 · 脚本与服务等版本分发**
- **When** `curl -sS https://<实例>/agent-login.sh`
- **Then** 200，`Content-Type: text/x-sh` 或 `text/plain`；响应头 `Cache-Control: no-store`；脚本内容与构建时 embed 的版本一致；该端点无需认证（脚本是公开分发物，不含任何机密）

**AC-26 · 无浏览器环境检测**
- **Given** SSH 会话（`SSH_TTY`/`SSH_CONNECTION` 已设置）或无 `DISPLAY`/`WAYLAND_DISPLAY` 的 Linux
- **When** 执行脚本
- **Then** 不尝试 `open`/`xdg-open`（或尝试失败静默降级），终端完整打印二维码 + 短码 + 手动入口地址；不因打开浏览器失败而中断轮询

**AC-27 · 有浏览器环境自动打开且不阻断**
- **Given** macOS 本地终端
- **When** 执行脚本
- **Then** 自动调用 `open <authUrl>`；**同时**仍打印二维码与短码（三条入口同屏给出）；轮询正常进行

### 4.6 TTL 与终态

**AC-28 · 会话 TTL 10 分钟**
- **Given** 建会话后不进行任何操作
- **When** 时间超过 `expiresAt`
- **Then** 确认页访问显示「已过期」；批准/拒绝操作被拒；脚本 poll 收到 `expired` 并打印明确状态行、以非 0 退出；过期会话**不可复活**（再次批准返回错误）

**AC-29 · 到期边界**
- **When** 恰在过期前后发起批准（用可注入时钟或缩短 TTL 的测试构建验证，如 TTL=5s 的配置）
- **Then** 过期前批准成功 → 正常领证；过期后批准被拒 → 脚本收到 expired。边界判定以服务端时间为准

**AC-30 · 终态集合与互斥**
- **Then** 会话状态机恰好为 `pending → approved → consumed`，旁路终态 `denied` / `expired`；任何状态迁移非法组合（如 `denied → approved`、`consumed → approved`、`expired → approved`）均被服务端拒绝（针对每个非法迁移至少一条测试）

**AC-31 · poll 节奏遵守 pollInterval**
- **When** 观察脚本轮询行为
- **Then** 相邻 poll 间隔 ≥ 服务端返回的 `pollInterval`（秒）− 合理时钟误差；服务端对同一 `<sid>` 的高频 poll（明显快于 pollInterval）有保护（429 或忽略），不因此泄露状态或崩溃

### 4.7 与现有产品的一致性

**AC-32 · 自动建的 Key 在 keys.html 可见可管理**
- **Given** 通过 CLI 授权流程建出 Key
- **When** 打开 keys.html
- **Then** 列表出现该 Key：名称 `agent:my-macbook`、前缀 `ant_send_…`（或对应 label）、scope 徽章正确、状态「启用」、「最近使用」在首次调用后更新；点「停用」后该 Key 调 `POST /v1/notify` 返回 401

**AC-33 · 空列表与既有行为不回归**
- **Then** `GET /v1/keys` 在无 Key 时仍返回 `[]` 而非 `null`（既有契约）；既有 e2e 套件（auth_flow / api_contract / security）全绿，无回归

**AC-34 · 限速不误伤正常流程**
- **Given** 默认限速配置
- **When** 单个用户正常完成一次完整授权（建会话 1 次 + 二维码 1 次 + 短码 lookup ≤ 数次 + poll 按 pollInterval）
- **Then** 全程无任何 429

**AC-35 · i18n 与设计风格**
- **Then** 确认页为 sitegen 体系页面（web-src/pages 源 + locales 双语，zh 根路径 + /en/）；颜色仅用 tokens.css 变量（无硬编码色值）；页面在 375px 移动宽度下无横向溢出（手机扫码后的主要使用场景）；web_verify 无 JS 错误

### 4.8 回归门禁

**AC-36 · 固化门禁**
- **Then** 新增 CLI 授权 E2E 套件（至少覆盖：建会话→批准→领证→Key 可用、拒绝、过期、一次性领取、scope 减量、stdout 无 Key 明文扫描）纳入 `make e2e`；全量 `make e2e` 绿才算完成

---

## 5. 优先级

| 项 | 优先级 | 说明 |
|---|--------|------|
| 建会话 API + 轮询 + 一次性领证（事务建 Key） | **P0** | 安全模型核心，缺此整体不成立 |
| 确认页（三入口汇入、身份显示、scope 勾选、批准/拒绝、未登录回跳） | **P0** | 唯一授权界面 |
| 登录脚本（三入口输出、轮询、凭据落盘 0600 原子写、stdout 无 Key） | **P0** | 用户触达的唯一客户端 |
| TTL 10 分钟 + 状态机终态保护 | **P0** | 安全不变量的一部分 |
| 建会话 IP 限速 + 短码 lookup 登录+限速 | **P0** | 防遍历/防滥用，与短码方案共存亡 |
| 文本二维码端点（qr.txt） | **P1** | SSH/服务器场景的关键入口，但有短码兜底，可降级 |
| keys.html 无需改动即可展示自动建的 Key（验证项） | **P1** | 大概率零改动，验收确认即可 |
| 文档：README/docs.html 增加「Agent 快速接入」一节（一条 curl 命令） | **P1** | 功能可发现性 |
| 英文二维码美化、终端彩色状态行 | **P2** | 锦上添花 |
| 授权会话审计日志（谁批准了哪台机器，security.html 展示） | **P2** | 后续迭代候选，本期可只留 DB 字段 |

---

## 6. 开放问题（需拍板，按建议默认值可先开工）

| # | 问题 | 建议默认 | 影响面 |
|---|------|---------|--------|
| Q-1 | 确认页页名与路由：`device.html` / `authorize.html` / `connect.html`？ | `device.html`，入口 `device.html?sid=..&code=..`，短码入口 `device.html` | designer/frontend；login.html NEXT_MAP 需加白名单 |
| Q-2 | 限速具体阈值（建会话/IP、短码 lookup/用户、poll/会话） | 建会话 10 次/分钟/IP；短码 lookup 20 次/分钟/用户；poll 不快于 pollInterval×0.8 | worker；阈值写进配置还是常量 |
| Q-3 | credentials.json 已存在时：覆盖 or 报错退出 or 备份旧文件？ | 原子覆盖 + 旧文件备份为 `credentials.json.bak` | 脚本行为；需在 AC-21/22 补一条用例 |
| Q-4 | 设备名来源：仅 hostname，还是允许脚本参数/环境变量覆盖（如 `ANOTIFY_DEVICE_NAME`）？ | 默认 `agent:<hostname>`，支持环境变量覆盖 | 脚本 + 建会话 API 参数 |
| Q-5 | 用户批准时是否可改设备名（确认页可编辑名称）？ | 本期不可改，保持 `agent:<hostname>`，改名去 keys.html（若支持） | 范围控制 |
| Q-6 | `GET /agent-login.sh` 路径名是否最终（也可 `/v1/agent-login.sh` 或 `/static/`）？ | 根路径 `/agent-login.sh`，短、好记、适合命令行传播 | server 路由；README 传播文案 |
| Q-7 | qr.txt 的二维码容错级别与尺寸（终端宽度 80 列约束） | 容错 L 级、模块 1 字符，确保 80 列终端完整显示；authUrl 尽量短（sid 用短 ID） | worker；sid 长度决策 |
| Q-8 | 会话记录过期后的清理策略（DB 膨胀） | 每次建会话时顺手删除 `expiresAt < now-1h` 的记录（惰性清理），不引入后台任务 | store 层 |
| Q-9 | 拒绝/过期后脚本退出码分配 | 0=成功，1=通用错误，2=拒绝，3=过期，4=超时/网络 | 脚本文档化 |
| Q-10 | 是否允许同一用户对同一 hostname 重复授权（产生多个 Key）？ | 允许（keys.html 自行管理），不做去重——去重会引入「旧 Key 是否还有效」的歧义 | 产品行为确认 |

---

## 7. 与现状的集成点清单（给 worker/frontend 的锚点）

1. **Key 签发**：复用 `internal/auth.KeyManager.CreateKey(userID, name, scopes)`（返回明文一次 + 记录）；label 逻辑自动产出 `ant_send_` 等前缀；**注意现有 `CreateKey` 校验 scope 合法性，授权流程提交的「脚本申请 ∩ 用户勾选」集合要在服务端重新校验**
2. **登录回跳**：`web-src/pages/login.html` 的 `NEXT_MAP` 白名单需新增确认页页名（open-redirect 防线，禁止绕过）
3. **路由装配**：`internal/server/mux.go` 新增授权会话相关路由（建会话/二维码可匿名；poll 需 secret；确认页数据/批准/拒绝/短码 lookup 需 sessMW 登录态）；静态分发 `/agent-login.sh` 走 embed + no-store（参照 `/v1/*` 的缓存策略，不走哈希指纹——文件名必须固定）
4. **前端**：确认页走 sitegen（`web-src/pages/` + `locales/{zh-CN,en}.yaml`），scope 勾选 UI 可复用 keys.html 新建弹层的卡片样式与 SCOPE_META 说明文案
5. **store**：新增授权会话表（sessionId/secretHash/userCode/userID(批准后回填)/deviceName/scopesRequested/scopesGranted/status/expiresAt/createdAt/consumedAt）；`store.Open` 加幂等建表；secret 建议只存哈希（与 Key 同原则）
6. **测试**：新增 `scripts/e2e/suites/cli_auth.mjs`；限速与 TTL 用可注入配置缩短窗口测试，勿用真实 10 分钟等待

---

*PM：anotify-pm · 方案已定，本文档锁定边界与验收；实现层遇冲突以「安全模型核心不变量」（§3.3）与「单二进制零依赖」为最高裁决原则。*
