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
