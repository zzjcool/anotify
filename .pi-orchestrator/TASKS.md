# Anotify · 实施任务台账（唯一事实源）

> 协调者：主 Agent（pi）。规则：子 Agent 报完成 ≠ 完成；**只有我独立验证通过才把任务标 ✅**。
> 状态：⬜待办 🟦进行中 ✅已完成 ❌返工

## 环境约定（所有子 Agent 必读）

- Go 拉依赖：`GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto`（直连、官方校验、自动补 go1.25）
- 项目非 git 时由协调者统一 git init；各子 Agent 在自己的 worktree 工作，提交到各自分支
- 完成后上报格式：`DONE <任务ID> | 产出文件清单 | 自测命令与结果`

---

## 阶段 0 · 地基（串行）

### [T01] 仓库骨架 + Go module + Makefile

- 状态：✅（git init 已做）
- 依赖：无
- 产出：go.mod / 目录骨架 / Makefile / AGENTS.md
- 验收：`go build ./...` 通过

### [T02] SQLite schema + migration

- 状态：⬜
- 依赖：T01
- 产出：`internal/store/schema.sql` + migration 逻辑
- 表：messages / consumer_offsets / deliveries / users / devices / api_keys / sessions / passkeys
- 验收：migration 单测通过

### [T03] Broker 接口定义（不含实现）

- 状态：⬜
- 依赖：T01
- 产出：`internal/broker/broker.go`（Message 结构体 + Broker 接口）
- 验收：编译通过

### [T04] API 契约 OpenAPI 草稿

- 状态：⬜
- 依赖：T01
- 产出：`api/openapi.yaml`（/v1/notify、auth/*、devices、keys、notifications、stream）
- 验收：契约评审通过

### [T05] 前端公共层约定

- 状态：⬜
- 依赖：T01
- 产出：`web/` 布局壳约定 + tokens.css 引用规范 + 路由约定文档
- 验收：—

### [T06] 指纹脚本（content-hash + 引用改写 + manifest）

- 状态：⬜
- 依赖：T01
- 产出：`scripts/hash.mjs`（扫描 web 资源→hash 改名→改写 HTML 引用→生成 manifest.json）
- 验收：对样例文件产出哈希改名正确

---

## 阶段 1 · 并行盖楼（worktree 隔离）

### [T10] SQLiteBroker 实现

- 状态：✅（协调者独立验证：go vet 净 / broker 7 tests PASS）
- 依赖：T02 T03
- worktree：wt-store
- 产出：`internal/store/*` `internal/broker/sqlite.go`（Publish/Subscribe/Ack/Replay + 进程内广播 + DB 回放 + 过期清理）
- 验收：`go test ./internal/...` 全绿

### [T11] Passkey 认证 + API Key 中间件

- 状态：✅（协调者独立验证：go vet 净 / auth+store 22 tests PASS，-race 通过）
- 依赖：T02 T04
- worktree：wt-auth
- 产出：`internal/auth/*`（WebAuthn 注册/登录、会话；API Key 签发/argon2 校验/scope）
- 验收：auth 单测 + 契约测试

### [T12] /v1/notify 上报 + 路由 + 双派发器

- 状态：✅（协调者独立验证：go vet 净 / api+push+ws 全 PASS）
- 依赖：T02 T03 T04
- worktree：wt-notify
- 产出：`internal/api/notify.go` `internal/ws/*` `internal/push/*`（标签路由规则 + status 过滤 + WS 派发 + WebPush 派发）
- 验收：notify 单测 + 两消费者路由测试

### [T13] 前端核心页：login + 总览 + 通知接收(Receivers 双 tab)

- 状态：✅（协调者 web_verify 三页：无 JS 错误/溢出，后端 404 降级正确，视觉还原到位）
- 依赖：T05
- worktree：wt-fecore
- 产出：`web/login.html` `web/index.html` `web/receivers.html`
- 验收：web_verify 逐页通过

### [T14] 前端管理页：API Keys + 安全与登录(Security) + 接入文档

- 状态：✅（返工后协调者复验：三页无裸 hex、字体引用正确、无 404 失败请求、无 JS 错误/溢出）
- 遗留：docs.html 移动端参数表轻微溢出（基线遗留问题）→ 转入 T22 移动端适配任务统一处理
- 依赖：T05
- worktree：wt-feadmin
- 产出：`web/keys.html` `web/security.html` `web/docs.html`
- 验收：web_verify 逐页通过

---

## 阶段 2 · 集成（串行）

### [T20] 合并 5 个 worktree + 前后端连调

- 状态：✅（4 后端 worktree 干净合并；解 import 循环/store MessageRow 重构/契约适配；全量 go test 通过）
- 依赖：T10-T14 全 ✅
- 验收：合并后 `go build ./...` + 前端引用正确

### [T21] 指纹 + go:embed + 单二进制冒烟

- 状态：✅（make build 指纹+embed；单二进制起服务，embed 首页 200，缓存分级正确）
- 依赖：T20 T06
- 验收：`./anotify` 起服务，首页可开

---

## 阶段 3 · 并行验证

### [T30] 单元测试全量 `go test ./...`

- 状态：✅（api/auth/broker/push/server/store/ws 全部 ok）

### [T31] 集成测试：注册→订阅→POST /v1/notify→断言 WS 帧 + delivery

- 状态：✅（播种用户+Key+设备；上报→matched=1+投递预览；WS hello→notification→ack 全过；Replay 持久化+断线续传正确）

### [T32] API 契约矩阵（Key/scope/错误码/标签路由/status 过滤）

- 状态：✅（无 Key 401/错误 Key 401/scope 不足 403/deviceTags 路由/catch-all 全验证）

### [T33] 前端渲染 web_verify（console/JS错误/溢出/截图）

- 状态：✅（六页 web_verify 全过：无 JS 错误/溢出/失败请求，侧栏视觉统一）

### [T34] CDN 缓存头验证（哈希 immutable / index ETag / v1 no-store）

- 状态：✅（classify 单测 + 实服务 index max-age=60 / v1 no-store 验证）

### [T35] Docker build 单二进制镜像 + run 起服务跑集成脚本

- 状态：✅（镜像 20.5MB；容器起服务 embed 前端/缓存分级/鉴权全正常）

### [T36] 桌面 Chrome Web Push 端到端

- 状态：✅（本地持久化 Chrome 跑通：真实 FCM endpoint 订阅 → 设备上报 → notify matched=1 → deliveries status=sent。注：trycloudflare 被公司 DNS 污染，桌面用 localhost 安全上下文验证；iOS 真机不受影响走 T40）

---

## 阶段 3.5 · 固化 E2E 测试体系（用户要求：测试不推给人工）

### [T50] E2E 测试体系（make e2e 固化门禁）

- 状态：✅（9 套件 224 断言 + Go 单测全绿，连跑 2 次稳定）
- 关键突破：Playwright CDP 虚拟认证器 → Passkey 全流程无头自动化（无需真人）
- 套件：api_contract(48)/auth_flow(15)/ws_protocol(31)/routing(23)/persistence(15)/security(22)/edge_cases(18)/frontend(45)/push_e2e(7)
- 唯一人工：iOS 真机 APNs（T40）

### [T51] 测试体系抓到的真实 bug（已全部修复）

- ✅ PATCH /v1/devices 不落库（store.UpdateDevice）——路由功能生产失效
- ✅ /v1/notifications 返回 PascalCase（broker.Message json tag）——契约不一致
- ✅ keys/security 路由守卫失效（fetchApi 无 401 处理）
- ✅ index.html normalize 字段不匹配

## 阶段 4 · 真机（交给用户）

### [T40] iOS Safari 添加到主屏幕全链路

- 状态：⬜ 依赖：T21 + Cloudflare Tunnel 临时域名
- 交付：操作清单 + 公网地址
