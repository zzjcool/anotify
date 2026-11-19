# 白盒审计：页面与信息架构（只读，不改代码）

snapshot-doc: 2026-11-19 白盒审计快照


- 分支：`audit/ui-whitebox`（基线 `origin/main`）
- 方法：读源码（模板 / 内联 JS / locale 值 / `api/*.yaml`），**不碰运行中的站点**
- 覆盖面：`web-src/pages/*.html` 全部 **23 页**、`web-src/layouts/{base,login,focus}.html`、
  共享导航 `web/partials.js`（NAV / mountLayout / apiList / 顶栏菜单）、`web-src/locales/*.yaml` 四语言值、
  `api/{resource,events,agent,auth,ingress}.yaml` 与相关 handler
- 审计目标：「同一件事两处能做」「职责过重」「开发叙述漏进 UI」「默认视图不是用户来这页要干的事」

**一句话结论**：23 页里 **19 页职责清楚**；问题集中在四处 —— ①**源**有三个详情/编辑宿主（3 页写同一份 spec）；
②**能力**的同一份 16 字段表单在列表页和详情页各有一份；③**节点**页同屏两套配置编辑器（表单 + YAML）；
④**开发叙述**没有系统清理，`make dev` / `PUT` / `POST /v1/notify` 这类字面量直接做成了用户可见文案。
另有 2 个 P0 级数据显示错误（安全页假恢复码、管理台全局消息表整表空白）与 1 个 P0 级静默数据丢失（节点 YAML 保存清空 tags）。

> **术语约定**（读下文用）：`改`= 该页能写该实体的字段；`看`= 只读展示；`授权`= 该页决定谁能用（凭证/令牌）。

---

## 1. 职责矩阵

行 = 页面；列 = 实体。`看`/`改`/`授权`；`·` = 该页不涉及。
**加粗** = 该格存在第二个写入口（见 §2）。

| 页面 | 节点 | 命名空间 | 能力 | 源 | 预案 | 事件 | 任务 | 系统配置 | 凭证/授权 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| `index.html` 总览 | 看 | 看 | 看 | 看 | · | 看(8 条预览) | 看(计数) | · | · |
| `events.html` 事件流 | · | 看(筛选) | · | 看(筛选) | · | 看 | 看(计数) | · | · |
| `trace.html` 一次投递 | · | 看 | · | · | · | 看 | 看 | · | · |
| `message.html` 一个任务 | · | 看 | · | · | · | 看 | **改**(取消) | · | · |
| `sources.html` 源列表 | · | 看 | 看(目标) | **改**(增/改/启停/删/跑) | · | · | · | · | · |
| `source.html` 源(旧详情) | · | 看 | 看 | **改**(启停/删/跑) | · | 看(近 10 条) | · | · | · |
| `source-detail.html` 源(详情) | · | 看 | 看(目标/回执) | **改**(编辑弹层/启停/删/跑) | · | 看 | 看(回执) | · | · |
| `capabilities.html` 能力列表 | 看(候选) | 看 | **改**(两种创建) | 看 | · | · | · | · | · |
| `capability.html` 能力详情 | 看(部署状态) | 看 | **改**(编辑/部署/启停/试跑/删) | · | · | 看(留痕) | 看(派发) | · | · |
| `playbooks.html` 预案列表 | · | 看 | 看 | · | **改**(导入/删) | · | · | · | · |
| `playbook.html` 预案详情 | · | 看 | 看 | · | **改**(整包/安装/删) | · | · | · | · |
| `nodes.html` 节点列表 | **改**(登记/轮换令牌) | 看 | 看(已部署计数) | · | · | · | · | · | 授权(令牌) |
| `node.html` 节点详情 | **改**(表单 + YAML + 令牌) | 看(生效值) | 看 | · | · | · | 看 | · | 授权(令牌) |
| `namespaces.html` 命名空间列表 | · | 看 | · | · | · | · | · | · | · |
| `namespace.html` 命名空间详情 | 看(逐节点生效值) | **改**(仅 vars) | · | · | · | · | · | · | · |
| `keys.html` API Keys | · | · | · | · | · | · | · | · | **改** |
| `security.html` 安全与登录 | · | · | · | · | · | · | · | · | **改**(passkey/密码/会话/恢复码) |
| `admin.html` 管理后台 | · | 看(计数) | · | · | · | 看(全站) | 看(全站会话) | **改**(保留期) | **改**(用户角色/禁用) |
| `docs.html` 接入文档 | · | · | · | · | · | 看(协议) | · | · | 看(认证) |
| `login.html` | · | · | · | · | · | · | · | · | 改(登录/注册) |
| `cli-auth.html` | · | · | · | · | · | · | · | · | 改(批准设备) |
| `passkey-enroll.html` | · | · | · | · | · | · | · | · | 改(新增 passkey) |
| `receivers.html` 接收设备 | · | · | · | · | · | · | · | · | 改(设备订阅) |

### 矩阵读出来的三件事

1. **同一格两个「改」**：**源**（3 页）、**能力**（2 页 × 16 字段）。这是本轮最该修的结构问题。
2. **一页改两类以上**：`admin.html`（系统配置 + 用户角色 + 读全站事件/会话）、`node.html`（节点 + 令牌）。
3. **「改」入口唯一且干净**（好消息，别再动）：**命名空间**（只有 `namespace.html` 的 vars 表单；
   `namespace.html:77` 的 YAML 卡刻意只读）、**系统保留期**（只有 `admin.html`，YAML 区只读）、
   **能力启停 / 部署 / 试跑**（各只有 `capability.html` 一处）、**预案安装**（只有 `playbook.html`）。

---

## 2. 重复写入口清单（同一字段/动作能在 ≥2 处改）

| # | 实体/字段 | 入口 A | 入口 B | 入口 C | 等级 |
| --- | --- | --- | --- | --- | --- |
| D1 | 节点 `spec.namespaces` / `vars` / `limits` | `node.html:145-147`（`#node-config-advanced` 三个 textarea）+ `:538` `PUT /config` | `node.html:131-135` YAML 编辑器 + `:351` `PUT /yaml` | — | **P0** |
| D2 | 节点 `spec.tags` | 只能走 YAML（表单没有 tags 字段） | 但表单同步会把 `tags: []` 写进 YAML → 见 F-03 | — | **P0** |
| D3 | 能力 16 个 `spec` 字段 + `metadata.displayName/version` | `capabilities.html:595-731`（三步向导，35 个同 id） | `capability.html:582-707`（编辑弹层，同 35 个 id） | — | **P1** |
| D4 | 能力 `spec.next`（下游） | `capabilities.html:647` `#cap-next` | `capability.html:625` `#cap-next` | `playbook.html:206` 整包原文（`capabilities/*.yaml`） | P1 |
| D5 | 源 `spec.enabled`（启停） | `sources.html:562` 行内 | `source.html:635` | `web/source-editor.js` 的 `f-enabled` | P1 |
| D6 | 源（其余字段） | `sources.html` 弹层（`sourceEditor.open` `:185`） | `source-detail.html` 弹层（`:133`） | `source.html:629` 整页跳到 `sources.html?edit=` | **P1** |
| D7 | 源（删除 / fire / dry-run） | `sources.html:398` / `:123-127` | `source.html:538` / `:115-118`（近乎逐行重复的第二份实现） | — | P1 |
| D8 | 「当前命名空间」 | 侧栏 `#scope-switcher`（`web/partials.js:1370`，写 localStorage、全站生效） | `namespace.html:44` `#config-namespace`（只改本页 `?name=`） | — | P1 |
| D9 | 节点身份令牌轮换 | `nodes.html:307` 行内 | `node.html:549` 详情页 | — | P2 |
| D10 | 给账号加 Passkey | `security.html:677`（本机） | `passkey-enroll.html`（新设备批准） | `login.html:798`（注册时创建） | P2（三路径语义有别，可接受） |
| D11 | 试跑参数 | `capability.html:789` schema 表单 | `capability.html:793` JSON 直填（JSON 优先，`2569`） | — | P2 |
| D12 | 预案安装 `values` | `playbook.html` `valuesForm` | `#pb-values-json`（`:161`）+ `pb-use-json`（`:162`） | — | P2 |
| D13 | 「怎么接入」说明 | `keys.html:399-424` 上报接口卡 | `docs.html:1087-1170` 完整版（卡片自己就链到那里） | `index.html:928` 安装横幅（又是第三套接法） | P2 |
| D14 | 账户安全入口 | 侧栏 NAV（`partials.js:257`） | 侧栏底部用户块（`:1425`） | 顶栏头像菜单（`:1577`） | P2 |
| D15 | 退出登录 | 侧栏用户块（`partials.js:1412`） | 顶栏菜单（`:1585`；注释 `:167-168` 声称「已收进用户块」，与实现相反） | — | P2 |

**已被验证为「不该报」的项**（避免下游重复讨论）：系统保留期只有 `admin.html` 一个写入口
（`grep -rn "/v1/system" web-src/` 只命中 `admin.html:1471/1487/1506`；`api/resource.yaml:1165` 明确无写 YAML 的方法）；
能力 `status.enabled`、部署、试跑各自单入口；命名空间 vars 单入口。

---

## 3. 主表：问题清单

严重度口径：**P0** = 数据显示错误/静默丢数据/安全兜底失效；**P1** = 同一份东西两处维护且已漂移，
或用户照页面做必然失败；**P2** = 冗余、文案、一致性。

### 3.1 安全与账号（`security.html` / `login.html`）

| # | 页面/文件:行 | 级别 | 类别 | 现在是什么 | 为什么不合理 | 最小修法 |
| --- | --- | --- | --- | --- | --- | --- |
| F-01 | `web-src/pages/security.html:2162-2180`（触发 `:2137`） | **P0** | 伪造数据 | `// 演示：本地假码` 生成 8 个 `DEMO…` 码，并 toast「已生成新恢复码，请立即保存」 | `/auth/recovery/status` 一失败就 `recoveryDemo=true`，点「重新生成」**不发请求**，直接渲染假码。用户把假码当兜底 → 真丢设备时锁号。违反本仓「不伪造数据」（同文件 `:1533` 注释） | 删 `2162-2180` 整个 demo 分支与 `:2136-2137` 的赋值；失败一律走 error toast，按钮直接打真接口 |
| F-02 | `security.html:1725,1769,1871,2014,2191,2473,2642,2895,3009,3054` | P1 | 契约不符 | `if (b && b.message) msg = b.message`（10 处） | 服务端错误体是 `{"error":…}`（`internal/server/handlers.go:21-23`、`api/resource.yaml:1899-1911`）→ `.message` 恒空，用户只看到 `HTTP 409`。`login.html:1256` 写法是对的 | 10 处改读 `data.error`（保留 `.message` 兜底） |
| F-03 | `security.html:1132-1135` | P2 | 伪造事实 | `扫码接入` · `刚刚` 静态写死 | 新设备用链接/短码接入时仍写「扫码接入」；时间恒为「刚刚」；JS 从不更新这两个节点 → `via_url`/`via_code` 词条成死键 | 由 watch 数据填真实渠道；无数据不显示 |
| F-04 | `security.html:2981-2998`（按钮 `:1157-1165`） | P1 | 危险动作确认 | 批准新设备一次点击直接 POST | 「批准」= 给账号加一个永久登录凭证，却无二次确认；**吊销一个会话**反而要走 `confirmDanger`（`:1992`）。风险与摩擦方向相反 | 批准前过 `confirmDanger`，正文写「该设备今后可长期登录此账号」 |
| F-05 | `security.html:896-949` | P2 | 可删/职责 | 「两条独立的轴」概念说明卡（54 行） | 纯概念解释，占账户安全页正文一大块；用户来这页是管凭证 | 删整块，概念进 `docs.html` |
| F-06 | `security.html:611-659,1328-1345,1360` | P2 | 重复实现 | 页内 `.toast`（CSS + 元素 + 函数 + 监听）全套 | 与 `Anotify.toast`（`web/partials.js:2473`）重复；`611-612` 注释自认是跨页欠账 | 删页内副本，改调 `Anotify.toast` |
| F-07 | `security.html:951-1258` + `2558-3148` | P2 | 职责过重 | 新设备授权弹层 + ~591 行 JS | 899/3158 = **28%** 的行数是跨设备流程，账户安全页成了两个流程的宿主 | 弹层与 JS 抽成独立文件，本页只留 `:682` 入口按钮 |
| F-08 | `login.html:594` | P2 | 文案/结构 | 品牌区写「点击下方按钮…无需输入用户名」 | 左栏应放产品价值，却放了操作说明；与 `:672-674` 副标题「Passkey 一键登录 · 也可选用密码」同义重复 | 左栏换价值句，操作说明只留按钮上方 |
| F-09 | `login.html:1379-1394` | P2 | 默认动作 | 首访自动 `switchTab("signup")` | 「登录页」的默认动作不是登录；换浏览器/清 storage 的回访用户先看到注册（`:1366-1373` 注释已承认） | 保留首跑提示，但不要自动切 tab |

### 3.2 管理后台与凭据（`admin.html` / `keys.html` / `cli-auth.html`）

| # | 页面/文件:行 | 级别 | 类别 | 现在是什么 | 为什么不合理 | 最小修法 |
| --- | --- | --- | --- | --- | --- | --- |
| F-10 | `admin.html:502-550` + `:1211-1227` | **P0** | 数据显示错 | 表头「标题/用户/运行状态」，行读 `m.title` / `m.username` / `m.agentState` | 后端 `GET /v1/admin/messages` 返回的是**事件** `{id,namespaceId,source,type,traceId,deliveryId,createdAt}`（`internal/store/admin.go:274-282`，`admin_handlers.go:347` 透传）→ 三列全空、状态徽章落 `undefined`。整张「全局消息」表是空的 | 改读 `source`/`type`/`createdAt`，表头换「源/类型/时间」；顺手把 tab 名从「全局消息」改「全站事件」 |
| F-11 | `admin.html:604-631` | P2 | 可删 | 「管理台说明」三条 bullet | 复述紧邻按钮的语义 | 删整块 |
| F-12 | `admin.html:635-642` | P2 | 可删 | 「Top 活跃用户」 | 用户表已有「消息」列（`:1045-1049`），同一事实两处 | 删，或改成用户表列头可排序 |
| F-13 | `admin.html:655-658` + `:1475` | P2 | 内部字段上表面 | 「开发者信息」印 `resourceVersion=` | CAS 令牌不该出现在运维视野里；源/能力详情都把它藏进折叠区或不显示 | 删这两处 |
| F-14 | `admin.html:1339` 与 `:1519-1521`/`:678` | P2 | 重复错误路径 | 同页两张 403 卡 | 两个 IIFE 各自鉴权，同一事实两条渲染路径 | 只留页级 403 |
| F-15 | `admin.html:652-675` + `:1446` | P2 | 键名错位 | 系统保留期区用 `nodes.v4.*` 文案键 | 区块已迁出节点页（#436），改文案会误伤 `node.html` | 迁到 `admin.system.*` |
| F-16 | `keys.html:1127`、`:1006-1010` | P1 | 契约不符 | `data.message \|\| "HTTP"+status` | 契约错误体是 `{error}` → 用户只看到 `HTTP 400`；`:1005` 注释还写着「别让用户只看到 HTTP 400」 | 两处改 `data.error` |
| F-17 | `keys.html:399-424` | P2 | 可删 | 「上报接口」卡（= 副标题 `:373-375`；完整版在 `docs.html:1087-1170`，卡片自己就链过去） | 三处讲同一件事（见 D13） | 删卡片，留一行链接 |
| F-18 | `keys.html:511-545` | P2 | 可删 | 「Key 的安全实践」4 条 | 与创建弹层里的 `keys.save.warning` 同义 | 压成 1 条或移 docs |
| F-19 | `keys.html:377` | P2 | 职责 | 页级第二动作「CLI 设备授权」 | Key 管理与设备授权是两个任务 | 降级为页脚链接 |
| F-20 | `cli-auth.html:878-887` | P2 | 重复文案 | 信任脚注 3 条 | 与 `:1037` entry hint、`:1044` ttl_hint 同义 | 留一处 |

### 3.3 节点与命名空间（`nodes.html` / `node.html` / `namespaces.html` / `namespace.html`）

| # | 页面/文件:行 | 级别 | 类别 | 现在是什么 | 为什么不合理 | 最小修法 |
| --- | --- | --- | --- | --- | --- | --- |
| F-21 | `node.html:313`（+ `:316` 同步、`:342` 重置） | **P0** | 静默丢数据 | `spec: { …, tags: config.tags \|\| [] }` | `GET /v1/nodes/{name}/config` 的 wrapper **没有 tags 字段**（`internal/server/resource_node.go:218-225`）→ `config.tags` 恒 `undefined` → `[]`。用户改了任一配置字段（`:364` 触发 `syncYAMLFromForm`）或点「YAML 重置」（`:342`）后，YAML 里变成 `tags: []`；保存即被 `store` 当成「显式清空」（`internal/store/resource_cas.go:295`）。有 tags 的节点会被静默清空 | 从 `/yaml` 或 `GET /v1/nodes/{name}` 取 tags 后再序列化；或干脆在 `draftDoc` 里省略 tags（`nil` = 不动） |
| F-22 | `node.html:141-150` vs `:131-135` | **P0** | 双重写入口 | 表单 `PUT /config` 与 YAML `PUT /yaml` 写同一组 `spec.namespaces/vars/limits` | 同页两条 CAS 写路径，谁赢无法判定；`namespace.html:77` 的文案还专门写了「再开第二条写入口会让谁赢了无法判定」——节点页正是反例 | 删表单那套（`#node-config-advanced` 三个 textarea + `#node-config-save`），只留 YAML；或反之，只留表单 |
| F-23 | `web-src/locales/zh-CN.yaml:1744-1745` | P1 | 文案失真 | 标题「节点与配置」、副题「逐台改配置」 | `nodes.html` 已无任何配置编辑（只剩登记 + 轮换令牌），标题承诺了不存在的能力 | 改「节点」/「登记 · 在线状态 · 令牌轮换」 |
| F-24 | `zh-CN.yaml:2602` | P1 | 文案失真 | 「这里管理默认 vars」 | `namespaces.html` 只有链接行（`:61`），没有 vars 编辑器 | 删这半句，或改为「点进某个命名空间改 vars」 |
| F-25 | `node.html:480-481` vs `:487-492` | P1 | 注释与行为相反 | 注释「不再读 URL 的 `?namespace=`」，实际走 `A.readScope()`（`web/partials.js:1936` 会采纳 URL 参数） | `nodes.html:303` 仍在拼 `&namespace=`，且缺省写死 `default` | 删 `nodes.html:303` 的 `&namespace=`，改注释 |
| F-26 | `namespace.html:42-46` | P1 | 双状态 | 页内 `select#config-namespace` 改 URL 的 `name`；侧栏 `switchScope` 改 localStorage | 同一屏两套「当前命名空间」，互不同步；其它 22 页只认侧栏 | 删页内 scope 卡，`?name=` 只作深链入口 |
| F-27 | `node.html:165` | P1 | 错位说明 | `ns_hint`「仅按该 namespace 的下一任务解析；不写进节点 applied」 | 讲的是 vars 解析规则，却挂在「授权/生效命名空间」卡下 | 删该行，或挪到 vars 摘要旁 |
| F-28 | `nodes.html:307` | P2 | 双入口 | 行内「轮换身份令牌」 | 与 `node.html:549` 同一破坏性动作，且结果复用 `:216-224` 的登记面板 | 删行内按钮，保留详情页 |
| F-29 | `nodes.html:310` | P2 | 空态 | 纯文本空态 | 无动作按钮；`node.html:461-468` 的空态有按钮，两页不一致 | 复用带「登记节点」的 emptyState |
| F-30 | `namespaces.html:93` | P2 | 空态 | 「还没有命名空间。」 | 无下一步动作，且全站无创建入口 | 补一句创建方式（API/CLI），别只报空 |
| F-31 | `node.html:170` 与 `nodes.html:137-139` | P2 | 重复旁白 | 同一段「开发者说明：槽位判定与批量写规则」 | 同一份解释两处维护 | 列表页删掉 |
| F-32 | `zh-CN.yaml:1761,1797,1810,1814,1826,2612,2620,2636,2638` | P2 | 开发叙述 | 出现表名 `namespace_configs`、`schema 里没有`、`slot 0 有效`、`Task 终态`、`谁赢了无法判定`、`envPassthrough` | 实现细节进界面（9 处） | 删或改写为用户语言 |

### 3.4 源（`sources.html` / `source.html` / `source-detail.html`）

| # | 页面/文件:行 | 级别 | 类别 | 现在是什么 | 为什么不合理 | 最小修法 |
| --- | --- | --- | --- | --- | --- | --- |
| F-33 | `source.html`（753 行）vs `source-detail.html`（631 行） | **P1** | 同职责两页 | 两页都是「某个源的详情」，两套实现（214 行 JS 结构重合，已漂移） | `source.html` 唯一入口是 `playbook.html:290`；`sources.html:543` 走的是 `source-detail.html`。用户从不同路点进同一个源会看到两张不同的页 | 删 `source.html`：`playbook.html:290` 改指 `source-detail.html`；把下游配置图（`A.loadSourceTopology` 已在 `partials.js:2120+`）并进 detail；同步 `partials.js:2213/2920` 白名单与 e2e |
| F-34 | `source.html:735` | P1 | 数据错 | 取 `nsList("events")`（`limit=500`）后**客户端**按源名过滤，再取 10 条 | 源比最近 500 条事件更老/更安静 → 误报「还没有事件」；`source-detail.html:419-420` 用的是服务端 `?source=&limit=20` | 改走服务端过滤 |
| F-35 | `sources.html:405-427`、`source.html:545-566` | P1 | 功能坏 | 删除后「撤销」用快照 `POST spec: snapshot.spec`（GET 读投影） | 读投影含 `config.hookUrl/secretSet`，真服务会 400 `unknown_field`（`internal/server/resource_source.go:257`、`:813-845`）；webhook 的 token/secret 只写不读，也无法恢复。`web/mock.js:4441-4443` 静默 strip，所以 e2e 一直是绿的 | 撤销改为「重新打开编辑器新建（剥掉投影字段）」，或直接去掉 undo |
| F-36 | `sources.html:580-581`、`source.html:446-447` | P2 | 死字段 | 「最近错误」「安装来源」 | `lastError` 恒 `null`（`api/resource.yaml:2540-2543`、`resource_source.go:621`）；`installedFrom` 不在 `SourceStatus` 里 | 删这两行 |
| F-37 | `sources.html:572` | P2 | 内部字段上首屏 | 列表行显示 `resourceVersion（CAS 令牌）` | 另两页都把它藏进「开发者信息」（`source.html:644-648`、`source-detail.html:502`）→ 三页三口径 | 换成「创建时间/目标数」，rv 进折叠 |
| F-38 | `source-detail.html:504`、`source.html:706` | P2 | 开发叙述 | 「v4 没有独立的更新时间字段…」「图只展示 v4 引用…」 | 给终端用户讲内部设计与字段缺席 | 删 hint，或改「写入序号」 |
| F-39 | `sources.html:80-83` `#dedup-note` | P2 | 重复说明 | 去重说明 | 与 `source-editor.js` 的 fire 回执、webhook 段同文（三处） | 留一处 |
| F-40 | `sources.html:471-483` | P2 | 错误扩散 | `?edit=<不存在>` 失败后把**整个列表**替换成 loadError | 一次深链失败让用户失去列表 | 只 toast，不动列表 |

### 3.5 能力与预案（`capabilities.html` / `capability.html` / `playbooks.html` / `playbook.html`）

| # | 页面/文件:行 | 级别 | 类别 | 现在是什么 | 为什么不合理 | 最小修法 |
| --- | --- | --- | --- | --- | --- | --- |
| F-41 | `capabilities.html:595-731` vs `capability.html:582-707` | **P1** | 重复实现 | 同一份能力表单两页各一份（35 个同 id，9 个函数各 2 份） | 已实际分叉：向导用 `caps.form.version_hint`（`zh:1512`），编辑器用 `version_edit_hint`（`zh:1513`）。改一处必漏一处 | 抽成 `web/` 下一份手写 JS 表单（无构建，靠 `<script defer>` 共用）；做不到就砍掉向导里的 2 个 `<details class="cap-adv">`（`:652`/`:690`） |
| F-42 | `capability.html`（2838 行） | P1 | 职责过重 | 10 个只读区块 + 4 个弹层 + 1 个启停 | 是同类详情页（`source.html` 753 行）的 3.8 倍；但删任一块都丢真实字段 → 该**合并**不该删 | 合并同一份 spec 的三视图：脚本正文 `:1629` + 原始 YAML `:1637` + 版本历史 `:1642` → 一个区块加两个 tab |
| F-43 | `capability.html:1476` / `:1576-1580` / `:1516`；`capabilities.html:1134` | P2 | 信息重复 | 「下游」在一页出现 3 次、全集群 4 次 | 声明值与解析结果本可左右并排读完，现在要滚三屏拼 | `spec.next` 与 `resolved` 合成一块（左声明 → 右解析态） |
| F-44 | `capability.html:1430` 与 `:729` | P2 | 重复文案 | 同一 key `caps.deploy.hint` 同屏渲染两次 | — | 删动作条那条（`:1430`） |
| F-45 | `capability.html:533` | P2 | 自述结构 | 副标题「身份、健康、定义与脚本；编辑、部署、删除都在这一页」 | 页面自述自己的目录，且与代码 fallback「写操作都在这一页」不一致 | 删副标题 |
| F-46 | `playbooks.html:254/260`、`playbook.html:133`（locales `zh:2282/2288/2296`） | P1 | 术语 | 同一对象 4 个叫法：预案 / 安装包 / 文件包 / 包名（slug） | 用户无法判断「预案 = 安装包」是否同一物 | 统一「预案」；其余三别名全文替换 |
| F-47 | `zh-CN.yaml:2329`（渲染于 `playbook.html:233`） | **P1** | 按钮文字 = 开发叙述 | 按钮写「PUT 整体保存模板」 | 用户点按钮不该看到 HTTP 动词 | 改「保存模板」 |
| F-48 | `zh-CN.yaml:2321`（`playbook.html:197`） | P1 | 开发叙述 | 「原文来自 GET includeBody=true 的 spec.files…带 CAS 的 PUT 整体替换」 | 查询参数 + spec 路径 + CAS 全进用户提示 | 改「原文来自包内文件；保存会整体替换」 |
| F-49 | `zh-CN.yaml:2298`、`:2312`、`:2317`、`:2318`、`:2319` | P2 | 开发叙述 | 「用 PUT 整体更新模板」「（不是另一个协议）」「按 valuesDigest 折叠」「attempts 是尝试次数…不是所有权」 | 机制说明当用户文案 | 逐句改用户语言，或删 |
| F-50 | `playbooks.html:437-453` | P2 | 跨页不一致 | 卡片成员项是纯 `span`，不可点 | `playbook.html:273-284` 同数据可点 → 列表页断链 | `items()` 复用 `resourceLink` 同款 `<a>` |
| F-51 | `capability.html:789` vs `:793` | P2 | 双输入 | 试跑参数同屏两个入口 | 覆盖规则靠文案 `params_json_hint`（`zh:1586`）解释 | 填了 JSON 时禁用上方表单（互斥态） |

### 3.6 事件、链路、任务、接收端

| # | 页面/文件:行 | 级别 | 类别 | 现在是什么 | 为什么不合理 | 最小修法 |
| --- | --- | --- | --- | --- | --- | --- |
| F-52 | `receivers.html` 整页（1441 行） | **P1** | 不可达页面 | 顶层页，**应用内零入口**（全仓只命中 `partials.js:2205` 白名单、`login.html:1267` NEXT_MAP） | `sitegen` 按目录发现页面（`internal/sitegen/sitegen.go:238`）→ 四语言照常发布。页面自陈「不在侧栏 NAV 里」（`:652-657`），首屏只有「推送暂未开放」（`:835-849`） | 二选一：①sitegen 跳过该页；②`security.html` 给一个受 `PUSH_UI_ENABLED` 门控的入口。现状两者都没做 |
| F-53 | `receivers.html:543-578`、`:555`、`:568` | P1 | 假数据/死 UI | 「长连接」tab：`#ws-list` 恒空、`#ws-live-count` 恒 0；命令写死 `wss://api.anotify.dev/v1/node/stream` + `ant_recv_…` | 那是**节点** WS 路径（`api/agent.yaml:96-126`，nodeToken），拿接收端 key 连必然失败；计数没有任何数据源 | 删整个 tab + `:786-797` 的 ws 分支 |
| F-54 | `zh-CN.yaml:1268`（`events.empty`） | **P1** | 文案教死路由 | 「还没有事件。用 `POST /v1/events` 提交第一个。」 | 该路径 v4 已下线并**有测试钉住 404**（`internal/server/api_integration_test.go:211-212` → `:219-225` 断言 404） | 改「还没有事件。Agent 或外部系统上报后会出现在这里。」 |
| F-55 | `docs.html:852` ← `zh-CN.yaml:905`（四语同） | **P1** | 文档教死端点 | 接入第 2 步：「携带 Key `POST /v1/notify`」 | `api/ingress.yaml` 已无 `/v1/notify`（0 命中），`internal/server` 无该路由 → 照文档做必 404；同页 `:864` 的 curl 又打 `/v1/namespaces/{namespace}/events`，文档自相矛盾 | `docs.agent.step2.desc` 改 `POST /v1/namespaces/{namespace}/events`，四份 locale 同改 |
| F-56 | `events.html:414-416` + `:375-381` | P1 | 死分支/口径 | 有 `origin=test` 的「手动试跑」徽章代码，但页面从不传 `origin` | 服务端默认排除 `origin=test`（`internal/store/event.go:526-549`）→ 试跑的事件永远不出现在事件流，徽章是死分支 | 筛选栏加「全部/投递/试跑」并传参；或删徽章并在副标题写明「试跑在链路页看」 |
| F-57 | `zh-CN.yaml:881` vs `:1880` | P1 | 自相矛盾 | 一处说「你可以稍后重放」，另一处说「无 replay API」 | 全站无重投入口，API 也没有（`api/events.yaml` 全只读） | `:881` 改「用新的 deliveryId 重新投递」 |
| F-58 | `events.html:274` | P2 | 文案 | 副标题是一句去重策略长句 | 页面第一句应是「这是什么页」；`events.subtitle`（`zh:275`）已经有了 | 改用 `events.subtitle` |
| F-59 | `trace.html:419-420` | P2 | 重复渲染 | 同一 `reason` 显示两次（chip + 文本） | `:423-424` 注释自称不再重复 | 删 `:420` |
| F-60 | `trace.html:442` | P2 | 文案 | 空态带「筛选」字样 | 该页没有筛选器 | 换 trace 自己的空态文案 |
| F-61 | `trace.html:219-225` | P2 | 导航 | 成功态页头无返回入口 | 返回只在错误/空态给（`:310`/`:406`/`:499`） | 页头加面包屑 |
| F-62 | `message.html:478-486` | P2 | 本地化 | 诊断卡 9 个标签 8 个是协议原名 | 首屏像协议 dump | 补 i18n 标签（折叠区可保留原名） |
| F-63 | `message.html:512-513` | P2 | 常驻噪音 | 两条与任务状态无关的免责长句 | 每页固定噪音 | 收进折叠，或按字段有值才出现 |
| F-64 | `events.html:324` | P2 | 调试残留 | 每页数量选项里有 `2` | 无用户意义（e2e 用 50） | 删该 option |
| F-65 | `zh-CN.yaml:336`（`message.missing_id_hint`） | P2 | 开发叙述 | 「深链需要带上任务 id，例如 `message.html?id=tsk_xxx`」 | 文件名与前缀是开发词汇 | 改「这个链接缺少任务标识，请从事件流重新进入」 |

### 3.7 总览、导航、文档（IA 层）

| # | 页面/文件:行 | 级别 | 类别 | 现在是什么 | 为什么不合理 | 最小修法 |
| --- | --- | --- | --- | --- | --- | --- |
| F-66 | `index.html:531-539` | P1 | 默认视图 | 三步引导卡「从 0 到第一条事件」只在 `≤639px` 且 `data-complete=1` 时收起 | 桌面（1440）已完成用户**永久**看到它占首屏第一张卡；真正的事件流被推到第 3 屏 | 把 `:534-539` 两条规则移出 media query；或 `data-complete=1` 时整块 `hidden` |
| F-67 | `index.html:928`（`#quickstart`） | P1 | 默认视图 | 接入横幅无任何收起条件 | 老用户每次打开首页都看到安装命令 | 复用 `#first-event[data-complete=1]` 判据，降为一行 |
| F-68 | `index.html:978-1029` | P2 | 可删 | 53 周热力图独占一行，格子**不可点** | 与 KPI 同源（`/v1/stats?daily`），却不回答「今天有没有出事」 | 下沉到 `#latest-flow` 之后，或删 |
| F-69 | `index.html:557-558` 与 `:970` | P2 | 重复入口 | 同屏两个「查看文档 →」，同 href `docs.html#agent` | 同一动作两个按钮 | 删页头那个 |
| F-70 | `index.html:826` vs `zh-CN.yaml:274` | P2 | 重复入口 | 首页卡片标题与事件流页标题逐字都是「事件流」 | 首页只是 8 条预览（`/v1/console/events?limit=8`），与 events.html 同源同名 | 首页标题改 `events.recent`「最近事件」（词条已存在 `zh:1266`） |
| F-71 | `web/partials.js:1541` | P1 | 死跳转 | 顶栏铃铛 `location.href = "index.html#recent"` | 全仓无 `id="recent"`（grep 零命中）→ 23 页共用一个必然无效的跳转 | 改指向 `index.html#events-feed`；`:1550-1553` 常亮红点直接删（无未读数据源） |
| F-72 | `web/partials.js:1189` | P2 | 占位 | `mountLayout` 的默认 `username` 写死成一个具体名字 | 22 页调 `mountLayout` 都不传 username（只有 index 传）；`/v1/auth/me` 慢时侧栏与头像显示别人的名字首字母 | 默认改空串，由 `loadMe` 填 |
| F-73 | `docs.html:1316-1326`（+ `zh:832-838`、TOC `:826-828`/`:1348-1350`） | P1 | 开发叙述 | 「设计取舍说明：为什么界面这样显示」，lede 原话「写给维护者与排障者」 | UI 里明说这是给维护者看的，还占 TOC 1/8 | 删整节 + 两处 TOC + 词条 |
| F-74 | `docs.html:1003`（`zh:838` 附近） | P2 | 开发叙述 | 「端点来自 … 五份 v4 spec」无条件渲染在正文首行 | 讲构建器输入，用户无从理解 | 改「下列端点由服务端的 OpenAPI 定义在构建期生成」，或整行删 |
| F-75 | `docs.html:815`（`zh:815`） | P2 | 文案 | 副标题「Agent 上报 · Web Push · WebSocket」 | 推送当前关闭（`PUSH_UI_ENABLED=false`，`web/partials.js:2762`），正文明明隐藏了 webpush 段 | 删 `Web Push · ` |
| F-76 | `docs.html:886`（详情 `875-901`） | P2 | 开发叙述 | 「一条通知的旅程」整段讲 APNs/FCM/SW 投递链路 | 推送未开放；v4 走 targets | 删该 `<details>`，或补「推送开放后生效」 |
| F-77 | `zh-CN.yaml:541`、`:693`、`:803`、`:1112` | **P1** | 喊话开发者 | 四处错误态写「本地可运行 `make dev` 起服务」 | 把内部开发命令做成了面向终端用户的错误提示（四语同病） | 统一改「请确认服务已启动后重试」 |
| F-78 | `zh-CN.yaml:835`（`docs.design.lede`） | P1 | 喊话开发者 | 「下面这些是写给维护者与排障者的说明」 | 同上（四语同病） | 随 F-73 一起删 |
| F-79 | `zh-CN.yaml:1669` | P2 | 喊话开发者 | 「中枢没有配置 `ANOTIFY_AGENT_BIN`…」 | 环境变量名给用户看；可达性待确认（动态 key） | 改「自升级未启用，请在节点上运行 `anotify-agent upgrade`」 |
| F-80 | `zh-CN.yaml:2638`（`namespace.v4.yaml_hint`） | P1 | 开发叙述 | 「…apiVersion / kind / metadata / spec.vars…PUT /v1/namespaces/{name}/config…」 | 字段路径 + 写入口 + 「谁赢了」的设计自辩全进用户 hint | 改「这份文档只读；下方表单是唯一的修改入口」 |
| F-81 | `zh-CN.yaml:1447`（`caps.form.env_hint`） | P2 | 开发叙述 | 「`vars.*` 必须同时写进 `requires.values`」 | 字段路径当说明；ja 同串还混用 `ハブ`/`中枢` | 改用户语言 |
| F-82 | `zh-CN.yaml:320` vs `:331-332` | P2 | 术语 | `message.title`「通知详情」↔ `message.error.*`「消息不存在」 | 同一页两个名字（且是 v3 残留键） | 删旧键组 |
| F-83 | `zh-CN.yaml:1636`、`:2261` | P2 | 开发叙述 | 「目标是占位实现（noop）」/「noop 占位」 | `noop` 未中文化 | 改「空操作实现（不执行任何动作）」 |

---

## 4. 每页四问四答（简表）

问题：① 只干一件事吗（一句话职责）｜② 多余可删的区块/按钮/说明｜③ 默认视图是不是用户来这页最想干的｜④ 空态/错误态怎么处理。

| 页面 | ① 一句话职责 | ② 多余 | ③ 默认视图 | ④ 空/错态 |
| --- | --- | --- | --- | --- |
| `index.html` | 跨命名空间看「现在有没有出事 + 最近发生了什么」 | 三步引导卡（桌面不收起）、接入横幅常驻、热力图、页头第二个文档按钮 | ⚠️ 一半：首屏黄金位给已完成用户永远走完的引导，「异常任务」只是个不可点的数字 | ✅ 齐：KPI 退回 `–`（不画 0）、摘要行、事件流/链路各自错误节点 |
| `events.html` | 最近进来了哪些事件（时间倒序） | 副标题去重长句、option `2`、重复骨架两份 | ⚠️ 默认 20 条无过滤是对的，但试跑/剪枝被服务端默认藏起且无开关 | ✅ 空态/错误有文案；过滤无结果**无**「清除筛选」 |
| `trace.html` | 一次投递的全景：图 + 任务 + 剪枝 + 事件 | 重复 reason、借来的空态文案 | ✅ 首屏即图 | ✅ 三态分级细；成功态无返回入口 |
| `message.html` | 一个任务的事实：相位/尝试/结果/日志 | 协议原名标签 8 个、两条常驻免责句 | ✅ | ✅ 404/缺参/失败三分支，不画乐观成功 |
| `sources.html` | 源列表 + 全部写动作 | 去重说明三处同文、`lastError`/`installedFrom` 死行、rv 上首屏 | ✅ 列表 + 行内动作（未塞表单） | ✅ 空态/搜索无结果/409 都有；`?edit=` 失败会吞掉整个列表 |
| `source.html` | 单源只读拓扑（+ 页头动作） | 整页（建议并入 detail，见 F-33） | ⚠️ 同职责双页 | ⚠️ 缺 name 时描述用 notfound；事件卡会误报空 |
| `source-detail.html` | 单源只读详情（就地编辑） | v4 设计自辩 hint | ✅ | ✅ |
| `capabilities.html` | 命名空间内能力清单 + 两种建能力入口 | 向导与详情页重复的完整表单 | ✅ | ✅ 全仓最好的一档（骨架→失败重试→空态→搜索无结果带「清空搜索」） |
| `capability.html` | 单能力全事实面 + 全部写操作 | 三种 spec 视图可并、下游重复 3 次、同句 hint 渲染两次 | ✅ 身份+状态在前 | ✅ 每块独立失败 + 独立重试，一块坏不连坐 |
| `playbooks.html` | 预案清单 + 导入整包 | 成员项不可点（与详情页不一致） | ✅ | ✅ |
| `playbook.html` | 预案体检 + 安装 + 整包源码 | 机制说明当文案（PUT/CAS/valuesDigest） | ⚠️ 一页三种身份（只读体检 / 源码编辑 / 安装台） | ✅ 徽章双路收敛（`:480-491` 记录了 404 后永久 loading 的旧坑） |
| `nodes.html` | 列节点 + 登记 + 令牌轮换 | 重复旁白、行内轮换、搜索只过滤已加载 20 条 | ✅ | ⚠️ 空态无动作按钮；搜索未加载部分显示「没有匹配项」（假阴性） |
| `node.html` | 单节点：事实/动作/配置/槽位/部署/任务 | 两套配置编辑器（P0）、`ns_hint` 错位、整卡静态 curl | ⚠️ 首屏空白（`:109-110` 后才渲染） | ⚠️ 无 scope 时两张卡空白无解释（`:488` 直接 return） |
| `namespaces.html` | 命名空间行列表（只读） | 副标题声称管 vars、含表名的 note | ✅ | ⚠️ 空态无下一步 |
| `namespace.html` | 单 ns：vars 编辑 + 逐节点生效值 | 页内第二个「当前 ns」选择器 | ✅ | ⚠️ 加载期不写 loading（首屏空白）；错态无返回列表 |
| `keys.html` | 管我的 API Key | 上报接口卡、安全实践 4 条、页级第二动作 | ✅ 主动作唯一 | ✅ 错误码文案坏（F-16） |
| `security.html` | 管理当前账号的登录方式本身 | 概念卡、页内 toast、新设备授权弹层（28% 行数） | ✅ | ✅ 空态/错误态分离（`fetchApiResult` + `errorCard`） |
| `admin.html` | 部署级运维台（5 件事） | 管理台说明、Top 用户、开发者信息、第二张 403 | ❌ 主动作不唯一 | ✅ 骨架/错误/403 都有，但同页两条 403 路径 |
| `docs.html` | 给「用 API 接进来的人」查投递/认证/节点协议 | 「设计取舍说明」整节、五份 spec 自述、WebPush 段 | ⚠️ 接入第 2 步打的是死端点（F-55） | ✅ 无请求；唯一空态是 OpenAPI 未生成 |
| `login.html` | 匿名入口 | 品牌栏操作说明；5 个认证动作平铺 | ⚠️ 首访自动切到注册 | ✅ 探活/4xx/不支持 WebAuthn 三类分明 |
| `cli-auth.html` | 浏览器里批准/拒绝终端发起的设备授权 | 信任脚注 3 条 | ✅ 主动作唯一 | ✅ 8 态 + 终态 |
| `passkey-enroll.html` | 新设备把本机变成该账号的 Passkey | 无（6 个预渲染状态是必要的） | ✅ | ✅ 短码不存在就地报错 |
| `receivers.html` | 我的推送设备与接收通道 | 长连接 tab 整块（假数据）、三个恒 0 计数 | ❌ 首屏只剩「推送暂未开放」 | ⚠️ 推送分支诚实，长连接分支不诚实 |

---

## 5. 可删清单（逐条给选择器/文案 + 位置）

**整块 / 整页**

1. `security.html:896-949` 「两条独立的轴」概念卡（+ `security.concept.*` 9 条词条）。
2. `security.html:2162-2180` 假恢复码分支（P0，必须先删）。
3. `docs.html:1316-1326` `#design-notes` 整节 + TOC 两处（`:826-828`、`:1348-1350`）+ `docs.design.*` 3 条词条。
4. `receivers.html:525-579` 长连接 tab、`:786-797` ws 分支、`:420`/`:568` 计数（待产品确认后连页一起停发）。
5. `admin.html:604-631` 管理台说明；`:635-642` Top 活跃用户。
6. `capability.html:1637` 原始 YAML 区块（并进版本历史，不丢数据）。
7. `index.html:978-1029` 热力图（或下沉）。
8. `node.html:141-150` 表单配置那套编辑器（若采用「只留 YAML」方案）。
9. `namespace.html:42-46` 页内命名空间 scope 卡。
10. `sources.html:80-83` `#dedup-note`。

**单个按钮/行/项**

11. `index.html:557-558` 页头「查看文档 →」（`:970` 已有）。
12. `web/partials.js:1550-1553` 铃铛红点；`:1577` 顶栏菜单「账户安全」项；`:1585` 顶栏菜单「退出登录」项（`:1412` 已有）。
13. `nodes.html:307` 行内「轮换身份令牌」；`:137-139` 开发者说明块（`node.html:170` 有同段）。
14. `node.html:172` 静态 curl 卡（`nodes.html:117-133` 是真值版）；`:165` `ns_hint`。
15. `sources.html:580-581`、`source.html:446-447` 死字段两行。
16. `sources.html:572` 列表行 rv。
17. `events.html:324` option `2`；`:416` 试跑徽章（若不加 origin 筛选）；`:338-350` 静态骨架第二份。
18. `trace.html:420` 重复 reason。
19. `message.html:512-513` 两条免责句。
20. `keys.html:399-424` 上报接口卡；`:511-545` 安全实践 4 条中的 3 条。
21. `cli-auth.html:878-887` 信任脚注 3 条中的 2 条。
22. `playbook.html:183/197/233` 三处 PUT/CAS/includeBody 措辞。

**死词条（四语言同删；`check-i18n-keys` 只查键存在，不查引用）**

23. `common.nav.receivers`（`zh:71`）、`docs.toc.notify`（`:826`）、`docs.toc.stream`（`:828`）、`docs.endpoints.empty_*`（`:842-843`）、`events.empty`（`:1268`，改完 F-54 后再决定去留）。
24. `srcpage:` 32 条（`zh:2142-2174`）+ 旧 `sources.*` 约 49 条（`zh:1926-2076`，全仓 0 引用）。
25. `namespace.pending/applied/context/effective_vars/ns_config` 5 条（`zh:1786-1796`）。

> 注：`check-i18n-keys.mjs` 当前报 **770 / 2328 键无引用**（仅 WARN）。这不是要一次清完 770 条，而是说明**过时措辞能长期存活**（F-82 就是这么来的）；先清本表这 40 余条。

---

## 6. Top 5（按用户感知影响排序）

| 序 | 问题 | 位置 | 为什么排这个位置 |
| --- | --- | --- | --- |
| 1 | **安全页发假恢复码**：恢复码状态接口失败后，点「重新生成」不发请求，直接渲染 8 个 `DEMO…` 码并提示「已生成，请立即保存」 | `security.html:2162-2180` | 唯一会导致**账号永久锁死**的问题。用户以为有兜底，真丢设备时才发现码是假的。测试/演示期的坑，上线即事故 |
| 2 | **管理台「全局消息」整表空白**：表头 3 列（标题/用户/运行状态）读的字段后端根本不返回 | `admin.html:502-550`、`:1211-1227` | 管理员打开就看见一张空表 + `undefined` 徽章，会立刻怀疑系统坏了。修法只是换 3 个字段名 |
| 3 | **接入文档第 2 步教你打一个已删除的端点**：`POST /v1/notify` | `docs.html:852` ← `zh-CN.yaml:905` | 新用户接入必经之路，照做 404，然后会怀疑产品而不是文档。同类还有 `events.empty` 教 `POST /v1/events`（测试钉住 404） |
| 4 | **节点 YAML 保存静默清空 tags**：表单同步把 `tags: []` 写进 YAML（`/config` 不返回 tags） | `node.html:313`、`:316`、`:342` | 无提示的数据丢失，且只有用 CLI/API 写过 tags 的节点会中招 —— 正是最不该得罪的那批用户 |
| 5 | **开发叙述仍在产品界面上**：`make dev` ×4、按钮「PUT 整体保存模板」、「写给维护者与排障者」、`POST /v1/events` | `zh-CN.yaml:541/693/803/1112/2329/835` | 这正是本次审计的起点（节点页那段「已迁到…」已被 `9ec8afff` 删掉，但同类还有 20+ 处）。逐条改成用户语言，成本低、感知强 |

---

## 7. 已验证 / 待验证

**本轮由协调者逐行复核过的**（`file:line` + 原文 grep 命中）：F-01、F-10、F-16、F-21、F-22、F-23、
F-52、F-54、F-55、F-66、F-71、F-72、F-73、F-77、F-80、F-47，以及 D1/D2/D3/D5/D8/D9/D14/D15 与
「系统保留期单入口」的结论。

**子审计给出行号但本轮未逐行复核**（改之前建议顺序扫一眼）：F-02、F-03、F-04、F-17~F-20、F-24~F-51、
F-53、F-56~F-65、F-67~F-70、F-74~F-76、F-78~F-83。

**需要产品/契约定夺，不属于「改文案」**：

1. `source.html` 是否退场 —— 唯一入口是 `playbook.html:290`，但 e2e `scripts/e2e/suites/events_paging.mjs:680` 直接 `goto` 它并断言 4 个图脚本，删页要同步改 e2e。
2. `receivers.html` 是否停止发布 —— 取决于 `PUSH_UI_ENABLED`（`web/partials.js:2762`）的开闸计划。
3. 首页三步卡与接入横幅的收起判据 —— 现在只对 `≤639px` 生效，桌面永久驻留。
4. `docs.html` 的 WebPush 段在推送开放后是否与 v4 `targets` 语义冲突（不在本次范围）。

**本次未做（避免越界）**：没有启动站点、没有跑 e2e、没有改动任何代码或 locale。
E2E 依赖的锚点（`.card[data-uid]`、`#ev-list article`、`#node-yaml-text`、`#namespace-vars` 等）在改动前需跑
`./scripts/e2e/run_all.sh --serial frontend`。
