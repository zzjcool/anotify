# UI 黑盒审计（v4 · 2026-11-19）

- 范围：dev 站全部 **23 个页面**（`web-src/pages/*.html` 实测清单，见 §1），带真实播种数据，桌面 1440×900
- 基线：`origin/main` @ `0ce4d115`（dev 容器用该 commit 重建后采集；见 §6「为什么必须重建」）
- 方法：无头 Chromium + 真实 session cookie 逐页打开 → 抓渲染后可见文本 / 全部可交互元素 /
  展开所有 `<details>` → 只读点击主按钮 → 全站链接爬取 → 空命名空间与 404 参数两种空/错态
- 与 [`docs/history/UI-AUDIT-v3.md`](../history/UI-AUDIT-v3.md) 的关系：v3 管**视觉与组件一致性**，
  并把这些开发者旁白「折进了 `<details>`」当作修法；本轮只管**逻辑 / 文案 / 死路**，
  结论是：**折叠不等于删除** —— 折起来的那 6 块仍然是产品 UI 里的实现说明（§3.1）。
- 本轮**不改代码**，只交这一份报告。

## 0. 结论

| 项 | 数 |
| --- | --- |
| 翻过的页面 | 23（另有 5 个带参/异常 URL 探针） |
| 发现问题 | **36** |
| P0（阻断 / 会写错数据） | **0** |
| P1（明显） | **19** |
| P2（轻微） | **17** |
| 死链 | 0（62 个内部 href：61 个 200，1 个 301→`/`） |
| JS 报错 | 0（正常页面） |

**没有 P0 的原因**：主流程（登录 → 总览 → 源 → 事件流 → 链路 → 任务）走得通，没发现白屏或数据丢失。
最接近 P0 的是 B-2「顶栏命名空间与页面命名空间不是同一个」——它可能让用户在「以为在改 A」时把
vars 存进 B（表单自身显示的是 B，所以不是静默写错，定 P1）。

## 1. 页面清单核对

| # | URL（实测） | 结果 |
| --- | --- | --- |
| 1 | `/` | 正常 |
| 2 | `/nodes.html` | 正常 |
| 3 | `/node.html?id=hw` | 正常 |
| 4 | `/namespaces.html` | 正常 |
| 5 | `/namespace.html?name=default` | 正常 |
| 6 | `/capabilities.html` | 正常 |
| 7 | `/capability.html?namespace=default&name=test-audit` | 正常 |
| 8 | `/playbooks.html` | 正常 |
| 9 | `/playbook.html?id=manual-hello` | 正常（参数是 `id`，不是 `slug`） |
| 10 | `/events.html` | 正常 |
| 11 | `/admin.html` | 正常 |
| 12 | `/login.html` | 已登录时客户端跳转→`/`（HTTP 仍是 200）；匿名时正常 |
| 13 | `/cli-auth.html` | 正常 |
| 14 | `/docs.html` | 正常（21 组端点清单） |
| 15 | `/keys.html` | 正常 |
| 16 | `/message.html?id=tsk_…` | 正常（参数是 **task id**，不是 event id） |
| 17 | `/passkey-enroll.html` | 正常 |
| 18 | `/receivers.html` | 正常（整页空态） |
| 19 | `/security.html` | 正常 |
| 20 | `/sources.html` | 正常 |
| 21 | `/source-detail.html?namespace=default&name=test-manual` | 正常 |
| 22 | `/source.html?namespace=default&name=test-manual` | **无入口的重复页**（B-4） |
| 23 | `/trace.html?traceUID=evt_…` | 正常（参数是 **trace UID**，不是 task id） |
| — | `/tasks.html` | **404，该页不存在**（任务详情走 `message.html`） |

## 2. 问题清单

| ID | 页面 | 严重度 | 类别 | 原文或操作 | 期望 | 最小修法 |
| --- | --- | --- | --- | --- | --- | --- |
| A-1 | `/nodes.html` | P1 | 文案 | 折叠块标题「**开发者说明：槽位判定与批量写规则**」，展开后：「只有有效 slot 上的 busy=false 是空闲；slot 0 有效。Task 终态不表示进程组已回收；缺 busy/instances 不推断空闲。配置可逐台编辑；批量 API 逐节点原子，允许部分成功，不是 Playbook 的整包 0 写规则。」 | 列表页只说「这台节点能不能用」；槽位判定是实现细节 | 删掉整个 `<details class="nd-more">`（`nodes.html:137-140`）；`slot_note`/`batch_hint` 只保留在 `/docs.html` 的节点章节 |
| A-2 | `/namespaces.html` | P1 | 文案 | 折叠块「命名空间能配什么」→「命名空间维度目前只有默认 vars（**namespace_configs**）与配置生效版本；**schema 里没有**命名空间级配额 —— limits 只有「本机 + 节点」两层…」——**数据库表名和 schema 上了产品界面** | 用户只需要知道「这里能配默认 vars」 | 删掉整块（`zh-CN.yaml:2612 note_body`，四语同步删）；若必须留一句，改成「目前只能配默认 vars，没有配额」 |
| A-3 | `/namespace.html?name=default` | P1 | 文案 | 一屏 4 处实现自述：①「切换会改 URL 的 name 参数（可收藏/分享），并由同一套 **CAS 判据**保证不会把 A 的草稿写进 B。」②「只影响该命名空间的**下一次任务解析**；不写进节点 **applied**。」③ YAML 卡：「**刻意只读**：…再开第二条写入口会让「谁赢了」变得无法判定。」④ YAML 里那段英文注释：「`Writes go through PUT /v1/namespaces/{name}/config (CAS) - this endpoint deliberately has no PUT.`」 | 页面说「这个命名空间的默认变量」，并把写入能力摆在能看见的地方 | 删 ①②；③删「刻意只读…谁赢了」整句，只留「只读」；④删掉服务端 YAML 前导注释常量（`internal/server/resource_namespace_yaml.go:159` 的 `namespaceYAMLPrelude`），YAML 本体保留 |
| A-4 | `/node.html?id=hw` | P1 | 文案 | ①「节点维度：身份与连接 · 授权命名空间 · 配置（YAML）· 物理槽位」（这是在给用户讲页面 IA）②「服务端按当前事实重建的规范化文档：只含 spec 的字段集，不含 status 与本机私有字段（token / server / listen / envPassthrough 不在中枢）。整份贴回即可保存；表单改动会同步成同一份文本。」③「严格解析：未知字段、类型错误、缺 resourceVersion 都会被拒，并指出行号与字段路径」④「这是授权列表，不是副本：生效 = 授权 ∩ 本机申报；只能收紧。」 | 一页只讲清「这台节点：在线吗 / 能跑哪些能力 / 配置在哪改」 | 删 ①②③④ 四段（`node.html:170` 与 YAML 卡说明）；把「生效 = 授权 ∩ 本机申报」压缩成字段旁的一行「实际生效：default」；安装段那句 `vars 非秘密；envPassthrough 仅本机名字名单，禁 PATH、HOME、ANOTIFY_…` 一并删 |
| A-5 | `/capability.html?…&name=test-audit` | P1 | 文案 | ①副标题「身份、健康、定义与脚本；**编辑、部署、删除都在这一页**」②「安装包保存或 install 都不会自动部署；**部署只在这个按钮里发生**。」③「四条条件（**读时派生**）」下表头是 `Implemented / Valid`、`NodesAvailable / Valid` ④「安装来源：`ins_BCOX6QGmmJJir3i4pj`」 | 用户要判断「这个能力现在能不能跑」，不要读实现条件名 | ①删；②删；③「四条条件（读时派生）」→「能不能跑」，`Implemented/NodesAvailable` →「已实现 / 有节点可用」（协议值保留在 `data-*` 属性上，避免 e2e 断言改字面量）；④「安装来源」收进「开发者信息」折叠块 |
| A-6 | `/playbook.html?id=manual-hello` | P1 | 文案 | ①「**只做加法**：安装不改已有路由/源、不删除资源；节点部署需人工操作。」②按钮「**PUT 整体保存模板**」（HTTP 方法当按钮名）③折叠块：「与表单发送同一个 values 对象（不是另一个协议）；仅在点击采用后使用。」「原文来自 **GET includeBody=true** 的 spec.files。保存通过带 CAS 的 PUT 整体替换模板，不自动安装。」 | 用户关心「安装会新增什么、会不会覆盖」 | ①改「安装只新增，不覆盖已有能力/源」；②改「保存模板」；③删这两个折叠块（`zh-CN` 里对应 key 直接删） |
| A-7 | `/admin.html` | P1 | 文案 | ①「**统计口径：全部署（不随顶栏命名空间切换）**」②整块「管理后台能做什么」+ 4 条角色/禁用/会话说明 ③「System YAML（只读）…写入口只有下面那个 **CAS 保存动作**，没有第二条写路径」，YAML 里还有英文「`Writes go through PUT /v1/system/config (CAS) - this endpoint deliberately has no PUT.`」④按钮「以当前 CAS 保存」 | 管理页三件事：用户、会话、保留期 | ①删（顶栏本来就显示「全部署通用」）；②删整块，角色说明留在「降为成员」的确认弹层里；③删后半句并删 `systemYAMLPrelude`（`resource_namespace_yaml.go:163`）；④改「保存」 |
| A-8 | `/message.html?id=tsk_…` | P1 | 文案 | 折叠块「开发者信息」展开后：「等待不消耗执行尝试。非幂等任务不会自动重试；未知接收结果不是安全重跑的依据。」「**租约**是执行授权边界，不保证业务副作用恰好一次；旧尝试与过期租约不能继续提交。」 | 任务详情讲「成功/失败、日志、产物」 | 删这两段；保留「重试」按钮即可 |
| A-9 | `/source-detail.html?…&name=test-manual` | P1 | 文案 | 「**v4 没有独立的更新时间字段**：每次写入以 resourceVersion 递增体现。」 | 只显示「更新时间」或不显示 | 删这句（`zh-CN.yaml:2584 updated_hint`）与它挂着的字段行 |
| A-10 | `/sources.html` | P2 | 文案 | ①折叠块「投递去重与窗口（deliveryId）」→「deliveryId 默认去重30天…**无 replay API**。」②每行直接印「resourceVersion（**CAS 令牌**） rv1:src_…」 | 列表页只需 名字/类型/状态/目标 | ①把这句搬去 `/docs.html` 的事件章节；②CAS 令牌收进行尾「⋯」菜单里的只读信息，不再常驻 |
| A-11 | `/docs.html` | P2 | 文案 | 「端点来自 resource / events / agent / ingress / auth 五份 **v4 spec**；未接入时真实空态，**不展示旧 spec 端点**。」 | 文档页讲怎么调用 | 改「端点清单」并删掉「v4 / 旧 spec」这半句 |
| A-12 | `/security.html` | P2 | 文案 | 开头「它们与「通知发到哪」（接收设备）是**两条独立的轴**」，页尾又整块重复「两条独立的轴 / 认证轴…/ 投递轴…」6 条 | 一段话讲清即可 | 删页尾整块 |
| B-1 | `/node.html?id=hw` | P1 | 逻辑 | **同一份 spec 有两个写入口同屏**：「节点默认配置」表单（按钮「以当前 CAS 保存」→ `PUT /v1/nodes/{name}/config`）+ 下方「编辑 YAML」（按钮「保存这份 YAML」→ `PUT /v1/nodes/{name}/yaml`），二者都能改 `vars` / `limits`；而 `/namespace.html` 明确写「vars 只有一条写路径…再开第二条会让「谁赢了」无法判定」 | 一个字段只有一个改法 | 二选一：YAML 卡降级为**只读 + 复制**（删「保存这份 YAML」「按当前表单重写」两个按钮），或删掉上方 JSON 表单、只留 YAML。推荐前者（改动最小，且保留排障能力） |
| B-2 | `/namespace.html?name=default` | P1 | 逻辑 | **顶栏命名空间 ≠ 页面命名空间**。实测：打开 `?name=default` → 点顶栏「默认 · default」→ 选 `default-2` → 顶栏变「默认 · **default-2**」，页面标题、下拉、表单仍是 **default**，URL 仍是 `?name=default`。同一页面两个「当前命名空间」 | 顶栏切到哪个，页面就看/改哪个 | 进入该页时隐藏顶栏 switcher：给 `mountLayout` 加一个 `scope:false` 选项（`partials.js` 的 `giveUp()` 已有「移除骨架 + 加 `.scope-slot--empty`」的让位逻辑，直接复用），`namespace.html:104` 的调用传它；页面内下拉保持不变 |
| B-3 | `/events.html` | P1 | 逻辑 | 筛选区「命名空间」`select#ev-namespace` 改一次就 `saveScope()`（`events.html:564`）——**它就是顶栏那个 scope**，等于同一状态两个控件；而同一排的「涉及对象 / 源」「Trace UID」「每页数量」必须点「应用筛选」才生效（同一个筛选区一半即时、一半要按钮） | 一排筛选器只有一种生效方式 | 二选一：全即时（删「应用筛选」）；或全手动（`ev-namespace` 的 change 只改 form 值，不 `saveScope`）。推荐「全即时」+ 删按钮 |
| B-4 | `/source.html` | P1 | 逻辑/死路 | 存在**两个源详情页**：`source-detail.html`（只读详情，有「编辑源」）与 `source.html`（完整编辑器），内容/副标题/目标能力/原始 YAML 全部重复，**没有任何页面链到后者的带参形式**；直接打开无参 URL 时同时显示两条互相矛盾的错误：「链接缺少 **metadata.name**。请从源列表打开详情。」+「源不存在，可能已被删除。」 | 一个源只有一页 | 删 `source.html`（源编辑已经是 `sources.html?edit=<name>` 弹层）；若不能删，至少把无参错误收成一条，并把入口从 `playbooks.html` 指向 `source-detail.html` |
| B-5 | `/capability.html?…` | P2 | 逻辑 | 「最近任务：`tsk_BCUdCSNJQCI7aAf2rr`」与「任务总数：**0**　失败：**0**」同屏（计数是 24h 窗口，最近任务却不是），紧接着「节点上的状态 / hw / **当前 digest**」后面**没有值** | 数字要能互相解释 | 「任务总数/失败」标签改成「近 24h 任务 / 近 24h 失败」；「当前 digest」补值，取不到就整行不渲染 |
| B-6 | `/nodes.html` | P1 | 逻辑 | 列表每行直接放「**轮换身份令牌**」按钮（确认弹层原文：「旧令牌立即失效，旧会话也失去新授权。须在本机更新配置。」）——高破坏动作与「查看」同权重 | 列表里不该有会踢掉节点的动作 | 把「轮换身份令牌」移进该行的「⋯」菜单（与 `sources.html` 的「删除源」同级），列表行只留「进入详情」 |
| C-1 | `/` | P1 | 简单 | 已有 5 条事件、三步全部「已完成」，首屏仍常驻「**从 0 到第一条事件**」清单，并以「三步都齐了 —— 去源页面点「立即运行」」结尾 | 已完成的新手引导应消失 | 加一个判断：`events>0` 时整块不渲染（同一条件已用于「三步都齐了」文案，复用它即可） |
| C-2 | `/` | P2 | 简单 | 一屏 7 块：欢迎+「暂无对比」/ 三步引导 / 3 张 KPI / 事件流列表 / 「接入你的 Agent」（含两个复制按钮）/ 一年热力图 / 链路回放图 | 总览首屏 = 今天发生了什么 | 「接入你的 Agent」与三步引导合并成一块且只在首次出现；「暂无对比」在没有对比数据时不渲染（`delta_na`） |
| C-3 | `/node.html?id=hw` | P1 | 简单 | 「当前 namespace 的最近任务」**20 行**全部铺开（同页还有 9 条能力状态、4 个折叠块、3 种配置表示：JSON 表单 / YAML / 只读 YAML） | 详情页默认只给最近 5 条 | 该列表默认 5 行 + 「查看全部 N 条」（链到带 source 过滤的事件流）；20 行改为最多 5 行 |
| C-4 | `/message.html?id=tsk_…` | P2 | 简单 | 三个常驻空块：「没有保留的**环境诊断**。」「没有保留的**执行尝试事实**。」「没有保留的 **emit 事实**。」 | 没数据就不占版面 | 三个块在无数据时整体不渲染（或收进「更多」），只保留「结果摘要 + 日志」 |
| C-5 | `/playbook.html?id=manual-hello` | P2 | 简单 | 「重新体检」出现 **2 次**，「说明」**3 次**，「仅删除清单」标题+按钮 **2 次**，另有「渲染预览 / 试装检查 / 安装」三个同级按钮 | 一屏一个主动作 | 「重新体检」只留页头一个；三个「说明」折成一句 tooltip；危险操作区标题改成「删除这个预案」 |
| C-6 | `/capability.html?…` | P2 | 简单 | 「派出记录 / 这次运行没有派出去」+「剪枝留痕 / 这次运行没有剪枝」两个常驻空块；页头 5 个同级按钮（编辑/部署/停用/试跑/删除）+ 刷新 | 主动作 = 部署或试跑 | 两个空块默认不渲染；「删除」移进「⋯」菜单（`sources.html` 已是这个范式） |
| D-1 | `/` | P1 | 死路 | KPI「**异常任务 2**」副标题「全部命名空间的所有任务」——是 `div`，不可点；全站也没有任务列表页，要定位这 2 个失败任务只能 事件流 → 链路 → 在图上找节点 | 看到数字能点进去看是哪些 | 给这张 KPI 包一层链接：`events.html?status=failed`（或直接列出这 2 个 `tsk_…` 链到 `message.html`） |
| D-2 | `/events.html` | P2 | 死路/空态 | 只有 5 条事件时仍渲染**禁用**的「加载更多」；空命名空间下同时显示「此命名空间与筛选条件下没有事件。」+「已显示 **0** 条唯一事件」+ 禁用按钮 | 没有下一页就不要渲染这个按钮 | `disabled` 时整块 `hidden`（不是 `disabled`） |
| D-3 | `/node.html?id=nope`、`/capability.html?…&name=nope`、`/trace.html?traceUID=…` | P2 | 错误态 | 中文说明后面直接贴原始英文：`NotFound: not found` / `not found` / `trace not found` | 一条人话 | 删掉 `err.reason`/`err.message` 的裸拼接，只保留中文标题 + 「重试 / 返回列表」 |
| D-4 | `/receivers.html` | P2 | 空态 | 连写两句同义：「手机推送暂未开放，上线后这里会显示你的接收设备。」「开放后这里会出现接收设备与送达率。」 | 一句空态 | 删第二句 |
| E-1 | 全站 | P1 | 一致性 | **同一个「重新加载本页数据」有 6 种叫法**：`刷新`（events/capabilities/capability/namespaces/message/trace/source-detail）、`刷新 →`（总览）、`刷新事实`（nodes/node/namespace）、`重新体检`（playbooks/playbook）、`重新加载最新资源`（sources/source）、`加载版本历史`（capability，另一个动作但同造型） | 一个动作一个词 | 统一成「刷新」；`刷新事实`/`重新体检`/`重新加载最新资源` 三处改文案即可（纯 i18n 改动） |
| E-2 | 全站 | P2 | 一致性 | 同一页里「命名空间」与 `namespace` 混用：`中枢授权 namespaces (JSON)`、`仅按该 **namespace** 的下一任务解析`、`当前 **namespace** 的最近任务`、`上下文 namespace` | 面向用户的词只用「命名空间」 | 除 YAML 里必须用字段名外，其余 UI 文案统一「命名空间」（`nodes.node.*` / `node.*` 的 4 个 key） |
| E-3 | 全站 | P2 | 一致性 | 时间有 3 种写法：总览「3 天前」、事件流「2026-11-16T03:18:50Z」、源详情「2026/11/16 11:18:50」、管理后台「16 天前」 | 列表用相对时间、详情用本地时间（同一套） | 事件流改用本地时间格式（与源详情同一个 formatter），相对时间只用于「最近」类列表 |
| E-4 | 全站 | P2 | 一致性 | 每个页面的 `h1` 都是导航上下文而不是页面名：`工作台 · #default`（15 页）、`资源 · 全部署通用`（keys/security）、`管理 · 全部署通用`（admin）；`/receivers.html` 的 h1 只有 `#default`；页面名都在 `h2` | h1 = 页面名 | 把页面名提为 `h1`，上下文串降级为 `div`（`layouts/base.html` 一处改动，`receivers` 顺带修） |
| E-5 | 全站 | P2 | 一致性 | 导航分组语义：`资源` 组里放「API Keys / API 文档 / **安全与登录**」——登录与 Passkey 不是资源；导航叫「总览」，页内叫「工作台」 | 分组名与内容对得上 | 「安全与登录」移到「管理」组（或独立成「账户」）；「总览」或「工作台」二选一 |
| E-6 | `/docs.html` | P2 | 一致性 | 端点分组中英混用且同物异名：`事件`（1 个 GET）与 `事件入口`（1 个 POST）分开；`节点`（13）与 `Node`（4）分开 | 同一资源一组 | 把 `事件入口` 的 POST 并进「事件」，`Node` 的 4 条并进「节点」；分组标题统一中文 |
| F-1 | `/docs.html` | P1 | 事实错误 | 「API 版本 /v1　所有动态接口带版本前缀，**静态资源走 `/static/*`**」——实测 `/static/ui.css` **404**，真实资源是根路径带指纹：`/ui.bb8a8e79.css`、`/tailwind.8211d770.css`、`/i18n.zh-CN.5aa2ca4e.js` | 写对实际路径 | 改「静态资源走根路径并带内容指纹（如 `/ui.<hash>.css`）」或直接删这半句 |
| F-2 | `/cli-auth.html`、`/passkey-enroll.html` | P2 | 文案 | 输入框下「短码 **10 分钟**内有效 · 不区分大小写 · 不含 0/O、1/I 等易混淆字符」，下面 3 条里又写「短码 **10 分钟**过期」 | 有效期只说一次 | 删列表里的「短码 10 分钟过期」一条 |

## 3. 系统性根因（19 条 P1 里 12 条来自下面两类）

> 3.1 占 9 条 P1（A-1…A-9），3.3 占 3 条（B-1/B-2/B-3）；3.2 是这两类共用的「只改词」改法。

### 3.1 「折起来」被当成了「删掉」

v3 的修法是把开发者旁白塞进 `<details>`（`uxaudit.res.nodes.note_summary: '开发者说明：…'`）。
本轮展开所有 `<details>` 后，6 个页面仍能读到实现规则，其中 **A-2 直接把 `namespace_configs` 表名和
「schema 里没有」写进了命名空间列表页**。

> 判定标准建议：**用户能不能据此改变操作？** 不能 → 不进 UI（进 `docs/`）。
> 「开发者信息/开发者说明」这个标题本身就是承认「这段不该在这」——它就是待删标记。

### 3.2 内部机制词当了按钮名和字段名

`以当前 CAS 保存`、`明确重载最新配置`、`PUT 整体保存模板`、`四条条件（读时派生）`、`resourceVersion（CAS 令牌）`、
`applied / 中枢`。这些不是用户的心智模型。最小改法不是重写逻辑，而是**只改词**：
`保存` / `重新加载` / `保存模板` / `能不能跑` / `版本令牌`。

### 3.3 同一个状态有两个控件

- B-2：`namespace.html` 页内下拉 vs 顶栏 scope（**两者不同步**）
- B-3：`events.html` 的命名空间 select **就是**顶栏 scope（两者同步，但是两个控件）
- B-1：`node.html` 的表单与 YAML 两个写入口

三者都源于「顶栏 scope 是全局单例，但页面又各自需要一个命名空间选择器」。建议定一条规则：
**页面主体对象的命名空间用页面内控件；顶栏 scope 只出现在「跨命名空间聚合」的页面（总览/事件流）**，
其余页面进入时隐藏顶栏 switcher。

## 4. Top 5（按用户感知影响排序）

| # | 问题 | 为什么排前面 | 改动量 |
| --- | --- | --- | --- |
| 1 | **B-2 顶栏命名空间与页面不一致** | 用户以为自己切到了 `default-2`，实际在改 `default` 的 vars —— 唯一可能写错配置的一条 | 中（一个页面 + 顶栏显隐） |
| 2 | **A-1/A-2/A-9 开发者叙述（尤其 A-2 露出数据库表名）** | 正是用户这次点名的那类问题；`namespace_configs` 是外行也能看出「这是开发日志」 | 小（纯删 i18n 文案，四语同步） |
| 3 | **D-1「异常任务 2」不可点** | KPI 在首屏最显眼处报「有 2 个失败」，却没有任何路径去看它们 —— 用户第一反应是「点了没用」 | 小（包一层链接） |
| 4 | **C-1 已完成的新手引导常驻** | 老用户每次打开总览都被问一遍「从 0 到第一条事件」，且三步全「已完成」，自相矛盾 | 小（一个 `events>0` 判断） |
| 5 | **E-1 刷新有 6 种叫法 / B-1 节点页两个写入口** | 前者是「同一件事长六个样」的最直观例子；后者是「同一件事两处都能改」的最直观例子，正好对应用户原话的两个判据 | 小（改词）+ 中（删一个保存按钮） |

## 5. 证据与复现

### 5.1 环境

```bash
# dev 站：容器 anotify-dev（127.0.0.1:8080），数据卷 anotify-dev-data（未清空）
cd ~/code/anotify && make dev-seed          # 幂等，打印 SESSION=…
curl -s localhost:8080/health               # {"ok":true}
```

> 采集用的是该实例 `sessions` 表里的活跃行（账号 `zzjcool`）。该实例里 `demo` 账号当时**没有**会话行，
> 所以没有用 `make dev-seed` 打印的 SESSION —— 这一步值得单独查一次 devseed 的会话归属。

### 5.2 采集脚本（只读；展开所有 details，不点写操作）

```js
// /tmp/uiaudit/shoot.mjs（节选）
const ctx = await browser.newContext({ locale: "zh-CN", viewport: { width: 1440, height: 900 } });
await ctx.addCookies([{ name: "anotify_session", value: cookieVal, domain: "127.0.0.1", path: "/" }]);
page.on("console", m => m.type() === "error" && errors.push(m.text()));
page.on("response", r => r.status() >= 400 && bad.push(`${r.status()} ${r.url()}`));
await page.goto(BASE + p, { waitUntil: "domcontentloaded" });
await page.waitForTimeout(2500);                       // 等客户端 fetch 落地
await page.evaluate(() => document.querySelectorAll("details").forEach(d => d.open = true));
const text = await page.evaluate(() => document.body.innerText);
```

链接爬取：把每页渲染出的 `a[href]` 汇总（62 个唯一内部链接）逐个 `curl -o /dev/null -w %{http_code}`：
**61 个 200 + 1 个 301**（`index.html` → `/`），没有 404。

### 5.3 逐条关键片段

**A-1 / A-2（折叠块里的原文，展开后）**

```text
nodes.html   开发者说明：槽位判定与批量写规则
             只有有效 slot 上的 busy=false 是空闲；slot 0 有效。Task 终态不表示进程组已回收；
             缺 busy/instances 不推断空闲。 配置可逐台编辑；批量 API 逐节点原子，允许部分成功，
             不是 Playbook 的整包 0 写规则。

namespaces.html  命名空间能配什么
             命名空间维度目前只有默认 vars（namespace_configs）与配置生效版本；schema 里没有
             命名空间级配额 —— limits 只有「本机 + 节点」两层，节点授权决定这个节点能被哪些
             命名空间使用。
```

来源：`web-src/locales/zh-CN.yaml:1772 slot_note`、`:1810 batch_hint`、`:2612 note_body`
（`en.yaml` 有逐字对应的英文版，说明是同一批文案）。

**A-9（v4 字样留在产品 UI）**

```text
web-src/locales/zh-CN.yaml:2584  updated_hint: 'v4 没有独立的更新时间字段：每次写入以 resourceVersion 递增体现。'
web-src/pages/source-detail.html:504  basic.append(txt("p","field-hint", t("source_detail.updated_hint", …)))
```

**B-2（顶栏切换后页面不动）**

```text
打开 /namespace.html?name=default
点击顶栏「默认 · default」→ 选「default-2」
结果：顶栏 = 「默认 · default-2」；页面标题 = default；页内下拉 = default；
      URL 仍 = /namespace.html?name=default；表单仍写着 default 的 vars
```

**B-3（命名空间 select 就是顶栏 scope）**

```text
web-src/pages/events.html:564
  node("ev-namespace").addEventListener("change", () => { saveScope({ namespace: … }); });
其余筛选项走 filters = { source, traceId, limit } + 「应用筛选」按钮（:500）
```

**C-1（新手引导在已有数据时仍常驻）**

```text
下午好，zzjcool
累计 5 条事件，今天 0 条。
从 0 到第一条事件
三步都齐了，节点上就能发出第一条事件。
1 建一个源 … 已完成   2 建一个能力 … 已完成   3 登记一个节点 … 已完成
三步都齐了 —— 去源页面点「立即运行」，就会写出第一条事件。  去源页面 →
```

**D-1（KPI 不可点）**

```html
<!-- web-src/pages/index.html:798 -->
<div class="stat-value" id="kpi-failed">–</div>
<span id="kpi-failed-sub">全部命名空间的所有任务</span>   <!-- 无 <a> 包裹，无 onclick -->
```

**F-1（/static/* 不存在）**

```console
$ curl -s -o /dev/null -w '%{http_code}\n' localhost:8080/static/ui.css
404
$ curl -s localhost:8080/ | grep -o 'href="/[a-z]*\.[a-f0-9]*\.css"'
href="/fonts/fonts.f07f222a.css"  href="/tailwind.8211d770.css"
href="/tokens.4a03d362.css"       href="/ui.bb8a8e79.css"
```

**E-1（同一动作的六种叫法，逐页实测）**

```text
index 刷新 →      nodes/node/namespace 刷新事实     playbooks/playbook 重新体检
sources/source 重新加载最新资源    events/capabilities/… 刷新
```

**B-4（无入口的重复页 + 两条矛盾错误）**

```text
/source.html（无参）：
  链接缺少 metadata.name。请从源列表打开详情。
  源不存在，可能已被删除。
/source.html?namespace=default&name=test-manual：
  渲染出与 /source-detail.html 完全相同的「配置与状态 / 目标能力 / 原始 YAML」，
  且页内重复三个小标题：配置与状态 ×2、目标能力 targets ×2、查看原始 JSON ×2
全站 href 扫描：没有任何页面链接到 source.html（都是 source-detail.html）
```

## 6. 边界说明

1. **dev 容器原本是旧构建**：审计开始时容器由 `dfadbab5` 构建，比 `origin/main` 少 9 个提交
   （其中仅 1 个动 UI），包括 PR #447「删掉节点页那段『命名空间与系统配置不在这页』」。用 `origin/main`（`0ce4d115`）
   重建后重新采集，该段**已确认消失**（`nodes.html` 渲染文本 416B → 308B），故不计入本报告。
   残留的同类问题是 A-1（同一页的「开发者说明」折叠块）。重建用的命令（在 `origin/main` 的 worktree 里）：

   ```bash
   cp ~/code/anotify/.env.dev .            # 只含 VAPID/RP 等本地配置，不入库
   docker compose --env-file .env.dev -f docker-compose.dev.yml build anotify
   docker compose --env-file .env.dev -f docker-compose.dev.yml up -d --force-recreate anotify
   ```

   > `make dev-seed` 会 `--build` **tools** 镜像，但不会重建 `anotify` 服务镜像；
   > `Makefile` 里 `dev-reset` 的注释（「`up -d --build` 在容器已存在时可能既不重建镜像也不重建容器」）就是这个问题。
   > 不显式 `build + --force-recreate`，会拿旧界面审出已修的问题。
2. **不报「功能不全」**：没有任务列表页、没有批量操作、没有移动端专用视图 —— 这些是新需求，不是审计发现。
   本报告只报「已有的东西不合理」；D-1 报的是既有 KPI 不可点，不是「缺少任务页」。
3. **不报纯视觉**：颜色/间距/图标/动效归 v3，见 `docs/history/UI-AUDIT-v3.md`。
4. **数据里的 20 个 `dev-recv`/`dev-send` 旧前缀 Key** 是测试残留（新 Key 前缀是 `ant_live_`/`ant_recv_`），
   不是 UI 缺陷，未计入。
5. **写操作未执行**：只打开并取消「轮换身份令牌 / 试跑 / 部署 / 新建 *」的弹层，没有提交，
   所以「部署后状态是否正确」不在本次结论内。
