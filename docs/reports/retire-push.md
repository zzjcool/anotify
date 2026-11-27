# 推送域整体退役（V4-D7）

> 决策：**不做「中枢把结果推送到手机」这件事**。日期 2026-11-25，用户决定。
> 落点：`docs/contracts/v4/CONTRACT.md` §0 的 V4-D7 + §1 非目标。

## 为什么不是「隐藏」，而是删除

上一版是 `web/partials.js` 里的 `PUSH_UI_ENABLED = false`：前端藏起来、后端留着，
注释写着「以后翻成 true 即可」。但 v4 换实现时**发送端已经没了**（`40dd61cd`
删掉中枢执行器，连带删了 `push.NewVAPIDSender` 的接线），于是那个开关翻成 true
也发不出任何推送——UI 藏着一个不存在的能力，文档还在承诺「送到所有设备」。

2026-11 的全面审计把这条链拆清楚了（issue #577）：订阅表、VAPID 密钥、公钥端点、
`push.CentralNotifier` 都还在，唯一缺的是「谁触发发送」，而替代路径（节点能力自己发）
在代码里不存在——能力进程的环境变量是闭集，拿不到设备订阅，也拿不到 VAPID 私钥。

所以这次按「要么做回来，要么删干净」处理，用户选择了后者。**不保留 dormant 形态**：
没有开关、没有死代码 Sender、没有「以后翻成 true」的注释。

## 删除清单

### 后端

| 文件 | 处置 |
| --- | --- |
| `internal/push/`（`central.go` / `webpush.go` / `device_filter.go` + 两个测试） | 整包删除 |
| `internal/store/devices.go` + `devices_update_test.go` | 删除；`Device` 类型随之消失 |
| `internal/server/handlers.go` 的 `devicesHandler`（list / upsert / patch / remove / ServeHTTP） | 删除 |
| `internal/server/mux.go` 的 VAPID 装载块、`/v1/vapid-public-key`、`/v1/devices`、`/v1/devices/` | 删除 |
| `internal/store/schema.sql` 的 `devices` 表 + `idx_devices_user` | 删除（v4 baseline 内原地改，迁移引擎仍单块） |
| `internal/store/stats.go` 的 `DeviceCount` / `statsDevicesSQL` / `withDevices` | 删除；`MessageStats` 与 `MessageStatsAll` 不再取 `userID` |
| `internal/store/admin.go` 的用户/系统/TopN 里的设备计数 | 删除 |
| `internal/server/admin_handlers.go` 的 `deviceCount` 字段 | 删除 |
| `internal/server/config.go` 的 `VAPIDPublic` / `VAPIDPriv` / `VAPIDSub` + 校验 | 删除 |
| `cmd/server/main.go` 的 VAPID 启动日志与 `vapid_configured` | 删除；`Validate()` 失败现在是**启动即失败**（此前只 WARN，因为唯一的校验项是 VAPID） |
| `internal/auth/apikey.go` 与 `internal/authn/authn.go` 的 `ScopeNotifyReceive` / `ScopeDevicesRead` | 删除；`scopeLabel` 随之简化 |
| `cmd/devseed/main.go` 的 `RECV_KEY` | 删除（只留 SEND_KEY / SESSION） |
| `scripts/genkeys.go` | 删除（纯 VAPID 密钥生成） |

### 契约与前端

| 位置 | 处置 |
| --- | --- |
| `api/auth.yaml` | 删 `/v1/vapid-public-key`、`/v1/devices`、`/v1/devices/{id}`、`devices` tag、6 个 schema（`VAPIDPublicKey` / `Device` / `DeviceUpsertRequest` / `DevicePatchRequest` / `DeviceResult` / `DeviceList`）、admin 的 `deviceCount`/`enabledDevices`、scope 枚举 → `[notify:send]`。operation 151 → 146 |
| `api/events.yaml` | `Stats` 删 `deviceCount`（含 description 与 `required`） |
| `api/resource.yaml` | 身份域枚举去掉 `devices` |
| `web-src/pages/receivers.html` | 删除整页（含「长连接」tab——那是节点协议，不是用户功能） |
| `web/partials.js` | 删 `PUSH_UI_ENABLED`、`HIDDEN_CAP_NAMES`、`filterCaps`、`pushUIEnabled` 导出、redirect 白名单里的 `receivers.html` |
| `web-src/pages/index.html` | 删「送达率」「接收设备」两张 KPI 卡与其渲染逻辑（含 `data-push-tile` / `data-nonpush-tile` 的隐藏开关） |
| `web-src/pages/keys.html` | 删 `notify:receive` / `devices:read` 两个 scope 勾选项与 chip 映射 |
| `web-src/pages/docs.html` | 删 Web Push 章节、目录项、`/v1/vapid-public-key` 的 API 映射行、`data-push-doc` 开关 |
| `web-src/pages/login.html` | redirect 白名单去掉 `receivers.html` |
| `web-src/locales/*.yaml`（4 份） | 删 `receivers.*`（98 条）、`docs.webpush*`（19 条）、`docs.toc.webpush`、`docs.api.map.vapid`、`keys.create.scope_receive|read_*`、`keys.scope.receive|read`、`index.kpi.rate|devices*`、`uxaudit.res.receivers.*` |

### 文档与脚本

| 位置 | 处置 |
| --- | --- |
| `README.md` / `README.en.md` | 产品首段重写（不再承诺推送到设备），架构图去掉 Web Push，快速开始去掉密钥生成步骤与 VAPID，API 表与环境变量表删推送行，文件结构删 `push/` |
| `DEVELOPMENT.md` | 项目定位、技术栈表、架构图、目录树、密钥生成命令、两条推送专属陷阱（VAPID mailto 前缀、PATCH /v1/devices）删除 |
| `IOS_TESTING.md` | 删除（整篇是 iOS 真机推送验证清单） |
| `docs/contracts/receiver-capability-design.md` | 删除（推送域的设计草案，依据代码已全删且从未生效） |
| `E2E_TESTING.md` | 删「桌面推送」行与「唯一人工环节」（不再有人工环节） |
| `docs/dev-docker.md` | 第 7 节从「功能开关」改写为「推送域已退役」 |
| `docs/lint-guards.md` | ORDER BY 清单删 `devices.go` |
| `docs/contracts/v4/CONTRACT.md` | 新增 V4-D7；§1 非目标改写（原文写着「设备/VAPID/推送不得误删」，现按 D7 反转）；§5 措辞同步 |
| `Makefile` | 删 `keys` 目标与 `_dev-env-keys`；`run` / `docker-run` / `dev-env` 去掉 VAPID |
| `.env.example` / `.env.compose.example` / `docker-compose*.yml` / `Dockerfile` | 删 `ANOTIFY_VAPID_*` |
| `scripts/push_e2e.mjs` | 删除 |
| `scripts/e2e/**`、`scripts/perf/**`、`scripts/*.sh` | 删 `receivers` 页用例与 `RECV_KEY`/`ANOTIFY_VAPID` 引用；F3 视图矩阵 336 → 352（22 页 × 4 语言 × 2 主题 × 2 视口，按 `f3-ui-helper.mjs` 的真实公式；删除 `receivers` 后 `PAGES` 23 → 22）。同批把 `pageBatches` 的冻结分片数改为按 `PAGES.length` 派生 |

## 保留了什么，为什么

| 保留项 | 理由 |
| --- | --- |
| `notify:send` scope | 它护的是事件入口 `POST /v1/namespaces/{ns}/events`，与推送无关。名字里的 notify 是历史包袱，改名会让已签发的 Key 失效 |
| `devices`/`cli_auth_sessions` 相关的**设备授权** | 另一义：CLI `curl \| sh` 登录与 Passkey 补建。`devices` 表删了，但 device authorization 的会话表保留 |
| `internal/store.Stats` 的 `delivered` / `deliveryTotal` | 任务口径（终态任务里 succeeded 的占比），与推送无关；只是首页不再展示 |
| `push.go` 之类的 git 历史 | 恢复时要看得到为什么删、当时怎么接线 |

## 验证

```bash
go build ./...                                    # 全仓编译
go vet ./...                                      # 含测试文件
go test ./internal/store/ ./internal/server/ ./internal/auth/ ./internal/api/ -count=1
make fe                                           # 死类守卫 + i18n 键守卫 + sitegen
node --test scripts/v4-frontend.test.mjs          # 前端 canonical 层单测
make contract                                     # 契约静态断言（含 OpenAPI parity 冻结点）
grep -rn 'vapid\|receivers\|notify:receive\|devices:read' --include='*.go' --include='*.yaml' --include='*.mjs' --include='*.html' . | grep -v docs/history | grep -v docs/reports
```

parity 门禁的三个 ratchet 数字同步下调（`parityPairsFloor` 225→224、`bareMountFloor`
27→24、`bareOpFloor` 39→34、`specSchemaFloor` 265→264、`muxPatternCount` 140→137、
operation 冻结值 151→146），每一条都在测试文件里留了「为什么减」的注释。

## 如果要恢复

必须**新开决策**，不能靠改开关：订阅存储、VAPID 装载、公钥端点、发送端、鉴权 scope、
前端页面都要重新设计。审计 issue #577 里有两条可选路线（中枢内置投递 vs 节点侧投递）
与各自的成本估算，可以当起点。

## 补充清扫（issue #577 第二轮）

本次退役把后端、契约与主路径页面都删了，但审计 issue #577 列出的承诺有几处没被带走，
仍对用户可见。它们与上面的决定同源，处置方式也一致：删掉承诺，不保留开关。

| 位置 | 原状态 | 处置 |
| --- | --- | --- |
| `web-src/pages/cli-auth.html` | CLI 授权确认页仍列出 `notify:receive` / `devices:read` 两个复选框，勾了也过不去（早已不在 scope 白名单） | 删掉这两块勾选项，对应词条从四份 locale 一并移除 |
| `web-src/locales/*.yaml` 的 `docs.agent.*` | 「Agent 完成任务后，把通知自动送到你的所有设备」「一行 curl 让通知送到所有设备」 | 改为如实描述：完成事件进入事件流，沿源的下游表派发给能力 |
| `web-src/pages/docs.html` 的旅程插图 | 图里是 WebPush 服务、APNs/FCM、Service Worker 弹窗 | 换成事件 → 计划 → 派发 → 节点执行 → emit → trace 的真实链路 |
| `web-src/locales/*.yaml` 的 `index.quickstart.*` | 首页横幅推荐 `pi install npm:pi-anotify`，该插件打的是已删除的 `/v1/notify` | 换成本实例分发的登录命令 `curl -fsSL <base>/agent-login.sh \| ANOTIFY_BASE_URL=<base> sh`（`<base>` 由 `index.html` 用 `location.origin` 填实，路径就是 `GET /agent-login.sh`）；步骤说明改为登录授权 → 建源 → 上报事件 |
| `web-src/locales/*.yaml` 的 `login.ios_tip` | 以「要接收推送通知」为由引导添加到主屏幕 | 保留添加到主屏幕的引导，去掉推送这个理由 |
| `web-src/locales/*.yaml` 的 `keys.create.*` 与 `docs.auth.*` | 「上报与接收分开」「用于接收端长连接」「总览 / 设备 / Keys 管理」 | 改为只说 Key 的实际上限：只能上报事件；设备页已不存在 |
| `web/manifest.webmanifest` | 描述是「Agent 完成即通知 · 多端推送」 | 改为事件进入中枢、能力链在节点上执行 |
| `.env.dev.example` | 三个 `ANOTIFY_VAPID_*` 标成「必填」，服务端从不读取 | 整块删除 |

同批修掉接入示例本身的两处失真：示例的 `source` 用的是 `on-push`，而源必须先存在，
照抄会得到 `source not found`；参数表与示例里的 `idemKey` / `traceId` 在 v4 投递体里不存在
（`idemKey` 会被静默忽略）。现在示例用 `agent` 这个源名，步骤里先建源、再建 Key、然后上报，
字段改成 v4 真正的 `id`。`docs.events.params.source` 也改回「源名」这个正确含义。

处置边界：**不恢复任何发送路径**。上面每一条都是「删承诺」，不是「做回来」。
决策本身未变（仍是 V4-D7），因此契约文档不需要新增条目——落下的只是实现。

## 补充清扫第二轮（#577 review r2）

上一轮的清扫本身带了两个「照样抄不了」的缺陷，review 打回后逐条修掉：

| 位置 | 问题 | 处置 |
| --- | --- | --- |
| `web-src/locales/*.yaml` 的 `index.quickstart.cmd_install` | 推荐命令写成 `sh web/agent-login.sh`：这是**仓内路径**，部署版首页没有这个文件（对外只有 `GET /agent-login.sh`）；且脚本要 `ANOTIFY_BASE_URL` 或首个位置参数，缺了就退出 1 —— 照抄必失败 | 命令改为 `curl -fsSL {base}/agent-login.sh \| ANOTIFY_BASE_URL={base} sh`，`{base}` 由 `index.html` 用 `location.origin` 填实（可见文字与「复制」按钮同时填，同 `nodes.html` 的安装命令） |
| `web-src/locales/*.yaml` 的 `ui.index.quickstart_hint` | 「一句话让 Agent **自动装好**」——这条命令只写一个凭证，不安装任何东西 | 改为「一条命令拿到本机上报凭证」（en/ja/es 同步） |
| `web-src/pages/docs.html` 的 `#agent` | 首页推荐的命令在文档里查不到，`ANOTIFY_BASE_URL` 在全仓用户可见文档里一次都没出现 | 新增「也可以用脚本代建 Key」块：完整命令 + `ANOTIFY_BASE_URL` 含义 + 凭证落盘路径 |
| `README.md` / `README.en.md` / `docs.html#events` 的示例 | `"source":"github"`：没有这一步的建源说明，照抄就是 `404 source not found`（#577 的原始症状之一） | 示例改用文档里让读者创建的 `agent` 源，并在示例旁写明「源必须先存在」；README 的 `idemKey` 上一轮漏改，这次一并换成 v4 真正的 `id` |
| `web-src/pages/docs.html` 的 `mountLayout.subtitle` 兜底串 | i18n 字典缺失时顶栏会显示「Agent 上报 · Web Push · WebSocket」——退役的推送域仍在兜底文案里 | 兜底串与 `docs.subtitle` 对齐（事件流 · 任务链） |

守卫：`scripts/cli_auth_smoke.sh` 新增一步——**把首页那条命令按显示文本照跑**（`{base}` 换成
真实 origin），断言它进到「正在向 … 发起设备授权」而不是用法错误；`scripts/e2e/suites/cli_auth.mjs`
新增 AC-36：首页横幅的可见文字与「复制」按钮的 `data-copy-text` 必须逐字相同、都含真实 origin 与
`/agent-login.sh`，且不得回落到仓内 `web/` 路径。
