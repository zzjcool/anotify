# 管理后台「全局消息」整表空白：页面读的是旧通知实体字段，端点早就是事件流（#456）

snapshot-doc: 2026-11-19 本机实测快照（下文数字、截图、门禁输出都是这次运行的定格；每处都附了复跑命令，复跑以新输出为准）

- 基线：`origin/main` = `370fcd69`；分支 `pi-subagent/host-workspace/anotify-456-r3-4d9e8d2f`（独立 worktree）
- 结论：**真 bug**（前端字段名与契约不符），不是「后端少发字段」也不是「空态没设计」
- 断言：既有断言一条没改、没放宽；新增单测自带变异自证（把字段名改回旧名必须红）

## 1. 现象与根因

现象（issue #456）：管理台「全局消息」整表空白；表头三列读 `m.title` / `m.username` / `m.agentState`，
第三列还会渲染出一枚字面 `undefined` 徽章。

根因只有一条：**页面按旧「通知」实体取字段，而这个端点的数据源早已是 events。**

- 端点：`GET /v1/admin/messages`（`api/auth.yaml`）—— 响应键名沿用历史的 `messages`，
  但行是**事件**（`AdminEvent`：`id` / `namespaceId` / `source` / `type` / `traceId` / `deliveryId` /
  `createdAt`，见 `internal/store/admin.go` 的 `AdminGlobalEvent`）。`title` / `username` /
  `agentState` 这三个名字服务端一个都不发。
- 页面：`web-src/pages/admin.html` 的 `loadMessages()` 与表头。读不到 → `m.title` 为 `undefined`
  （`el()` 的 `text` 落成字符串 `"undefined"` 或空）、`statusBadgeEl(m.agentState)` 把 `undefined`
  印成徽章文本 → 整表看起来是空的。

> 注：issue 里写「按 `api/resource.yaml` 的真实字段渲染」，实际这份端点在 `api/auth.yaml:1574`
> （`/v1/admin/messages`），`AdminEvent` schema 在同文件 `components.schemas`。本报告按真实位置写。

第二个真问题：**空态早就存在，但它描述的是旧数据源**。`#messages-empty` 的副标题原文是
「Agent 上报的通知会保留在这里」——事实是「进来的事件」。有数据时三列全空（看起来像一张空白表），
无数据时文案说的又不是这一页在展示的东西。

## 2. 改法

字段映射只放在 model 层一处，页面只负责把值塞进 `td` —— 这样「字段名」只有一份可被单测盯住的实现。

| 文件 | 改动 |
| --- | --- |
| `web/partials.js` | 新增 `modelAdminEvent(row)`（`Anotify.model.adminEvent`）：读事件的 7 个扁平字段，缺失归一成 `""`/`null`；`createdAt` 保持 Unix **秒**（页面 `fmtTime(unix)` 自己乘 1000） |
| `web-src/pages/admin.html` | 表头换成 事件 / 来源 / 命名空间 / 时间；`loadMessages()` 改走 `Anotify.model.adminEvent(row)`，事件名渲染成 `.badge badge-neutral`；删掉只服务旧字段的 `statusBadgeEl()`（它唯一的作用是把 `undefined` 画成徽章） |
| `web-src/locales/{zh-CN,en,ja,es}.yaml` | `admin.messages` 词条 `title/user/agent_state` → `type/source/namespace`；`admin.messages.empty` → 暂无事件；`admin.messagesEmptySub` 改成事件语义（4 语言同步，键集合保持一致） |
| `web/mock.js` | `/v1/admin/messages` 原来恒返 `{messages: []}`：预览/前端联调**永远看不到数据态**，这正是 bug 能带病上线的原因之一。改成返回两条真实形状的事件（`github/pr_opened`、`cron/daily_report`） |
| `Makefile` | 新增 `test-frontend` 目标并在 `ci` 里调用（不接线的单测等于没有单测，见下） |

`deliveryId` 刻意保留空串而不是 `—`：外部投递幂等键**可空**（人工/内部事件没有投递键），
空串本身是事实，展示层自己决定画什么。表格里没有展示 `traceId` / `deliveryId`（四列放不下），
但模型层保留它们，`AdminEvent` 的 7 个字段一个不丢。

## 3. 测试：给一条真实形状的事件 → 三列都有值；变异必须红

新增 `scripts/admin-messages.test.mjs`（`node --test`，纯 stdlib + `node:vm`）。它不是「读源码断言字符串」：
把 `web/partials.js` 真的读进 vm 跑起来，喂一条真实形状的事件行再断言行值。

事件形状照 `api/auth.yaml · AdminEvent` 的 required 全 7 个字段，取值照
`internal/server/admin_test.go` 里真写进库的那条（`github` / `pr_opened` / `trace_admin_test`）：

```js
const REAL_EVENT = {
  id: "evt_01J8ZQ4W6T9K3M5N7P1R2S4V6X",
  namespaceId: "ns_01J8ZQ4W6T9K3M5N7P1R2S4V6X",
  source: "github", type: "pr_opened",
  traceId: "trace_admin_test", deliveryId: "gh-delivery-88213",
  createdAt: 1763467200,           // Unix 秒
};
```

三层判据（缺一层就等于没查）：

1. **行为**：`model.adminEvent(REAL_EVENT)` 的 `type` / `source` / `namespaceId` / `createdAt` 逐字等于输入值，
   且三列都不是 `""`、不是占位符 `—`；
2. **变异自证**：把 model 里的字段名改回旧名（`type→title`、`source→username`、`namespaceId→agentState`）
   重新加载同一份源码，第 1 层的断言必须**变红**；
3. **接线**：页面 `loadMessages()` 真的调用 `Anotify.model.adminEvent()`、不再出现 `m.title`/`m.username`/
   `m.agentState`、空列表走 `showEmpty(st)`；表头引用的是 `admin.messages.type/source/namespace`。
   只有 1+2 的话，页面可以继续读旧名字而单测全绿 —— 这一层就是 2026-11-16「窄任务绕过模块约定」那个教训的同类。

跑法：`node --test scripts/admin-messages.test.mjs`（= `make test-frontend`，已接进 `make ci`）。

**为什么必须接进 ci**：这一页此前的所有守卫都是绿的 —— `check-classes`（类名）、`check-i18n-keys`（键存在）、
`check-wire-fields`（只扫 `spec.`/`status.` 读取点）、e2e `admin_flow`（只看端点 200 + `messages` 是数组）。
没有一个守卫会问「这页读的字段名服务端到底发不发」。唯一会给红的，是把一条事件真的送进渲染路径。

### 双向变异实测（不是自证脚本里演的，是真的改源码跑）

| 变异 | 结果 |
| --- | --- |
| `web/partials.js` 里 `r.type/r.source/r.namespaceId` → `r.title/r.username/r.agentState` | ✖ 2 failed / 3 passed：`事件列必须是事件的 type` / `'' !== 'pr_opened'`（变异自证那条也同时红：它反过来证明自证没在空转） |
| `web-src/pages/admin.html` 里三列改回 `row.title` / `row.username` | ✖ 1 failed：`loadMessages() 仍在读旧通知字段 .username（服务端不发 → 空白/undefined）` |

两处源码已按变异前的样子还原（`git diff` 只留本次正式改动）。

## 4. 浏览器级验证（真 Chrome + mock 后端）

`NO_TUNNEL=1 MOCK_PORT=7799 make mock-local` 起静态站，用 playwright-core 驱动 headless Chrome 打开
`/admin.html` → 点「全局消息」tab，取真实 DOM：

```
数据态（mock 两条事件）：表头 = ["事件","来源","命名空间","时间"]
  首行 = ["pr_opened","github","ns_mock_default","2 分钟前"]   第 2 行 = ["daily_report","cron","ns_mock_default","1 小时前"]
  底栏计数 = 2    #messages-body 可见 / #messages-empty 隐藏   控制台错误 = 0
空态（把 mock 的 messages 临时改成 []）：
  #messages-body 隐藏 / #messages-empty 可见，标题「暂无事件」+ 副标题「源产生的事件会逐条出现在这里」+ 重试按钮
  底栏计数 = 0   控制台错误 = 0
```

截图：`/tmp/admin-messages-data.png`、`/tmp/admin-messages-empty.png`（本机临时产物，不入库）。
两态都是**明确可分辨**的：有数据就是四列有值，没数据就是一块带图标/标题/副标题/重试的空态，
不再是「一张只有表头的空白表」。

## 5. 门禁

`flock /tmp/anotify-ci.lock -c "make ci"` → **EXIT=0**（首次跑曾卡在本机没装 staticcheck：那是环境坑，不是代码，见下）。
本次输出里的关键数字：

- 契约：`✅ 契约静态断言（A2/A4/A5/A6/A7 + openapi 一致性 + 字段级核对 + anode 可编译）`
- 前端：`check-wire-fields 234 个 schema / 724 个已知字段`、`check-i18n-keys 4 份 locale 键集合一致 / 2189 个引用点`、
  `check-ui-copy 9284 条值 / 20 条规则`、`tailwind 955 个源码类名`、`sitegen 4 语言 × 23 页`
- 新单测（本仓唯一跑 `node --test` 的前端用例）：`tests 5 / pass 5 / fail 0`
- Go 单测（`make test` = `go test ./... -count=1 -shuffle=on`）：`ok` 包 **41** 个，`FAIL` **0** 处
- 基准冒烟：`BenchmarkDispatchChain-8 1 2402508 ns/op`（只证「编译得过 + 能派出任务」）
- 其它：临时目录零残留（隔离 TMPDIR 新增 0）、harness 自测通过、`EXIT=0`

复跑同一条命令即可。两个本机环境坑（不是本次改动引入，但会让门禁假红，记在这里）：

1. `node` / `go` 不在默认 `PATH` 上，得先：
   `export PATH=/usr/local/go/bin:$HOME/.nvm/versions/node/v24.13.1/bin:$PATH`
2. 首次跑会 `make tools` 装 staticcheck，而 `GOPROXY=direct` 下 `honnef.co` 偶发 `unexpected EOF`。
   遇到就从机器上已装的同版本二进制拷一份（`cp <某个已有>.bin/staticcheck .bin/staticcheck`，版本对得上 `2025.1.1 (0.6.1)`）。
   不这么做得先把 `make lint-go` 修绿，否则整条门禁卡在「工具装不上」而不是代码上。
3. 多个 worktree 并行跑 `make ci` 时会抢 `flock`（正确行为）：第二个会原地等锁，不是卡死。

## 6. 刻意没做 / 遗留

- **没有**改 tab 名（`admin.tab.messages` 仍叫「全局消息」）与 KPI 卡片「消息总数」：那是 IA/文案决策，
  与 #456 的渲染 bug 分开；改了会牵动 e2e 的文案断言。
- **没有**在表里展示 `traceId` / `deliveryId`：四列已经够用，多挂一列是另一个产品决策；
  模型层已保留这两个字段，将来要加链接（`trace.html?…`）不必回到字段名这层。
- 老库不可用时的行为未变（`store.Open` 报错，不迁移、不删数据）——本次没有 schema 变更。
- `scripts/check-wire-fields.mjs` 只扫 `spec.`/`st.`/`status.` 前缀的读取点，扁平行（`r.type`）不在它的
  检查面内；本次靠 `scripts/admin-messages.test.mjs` 兜住。要把它扩成「扁平行也按 schema 校验」是独立的
  守卫改造（会牵到别的页面），不在这一条 issue 的范围里。
