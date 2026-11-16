# 目标
把已完成的 v4 store/runtime 接缝装配成可用的 HTTP 资源 API，同时完整保留独立身份与通知域。当前工作是 G3a，不重做 G1、Node、S1、G2-R1 或 E1。

## 基线与前置
- 以协调者提供的 **E1 验收后集成基线** 开工；本地核心交付树为 `.pi-subagents/runs/r-57adb51b/worktrees/v4-core-delivery/.pi-subagents/runs/r-01be4aa7/worktrees/core-g2-resume`，不要从旧 main/PR #329 重建已经完成的实现。
- 冻结依据：`docs/design/v4/{decisions,capability,source,anode,playbook,config,api-conventions}.md`；`.pi-orchestrator/v4/G3-HTTP-SPEC-RESUME-HANDOFF.md` D1/D2、W1–W6、S1、E1。

## 实施范围 / 文件所有权
- `internal/server/**`：资源 handlers、mux 装配、投影与对应测试。
- `internal/api/**`；`internal/auth/**` 仅装配与旧中央内置能力期望移除；`internal/authn/**` 仅用户 namespace / Node 鉴权接线。
- 共享 `internal/server/mux.go` 由本 issue 首先持有；产物传输 issue 在本 issue 完成后串行添加路由。
- 不改 store/runtime/protocol/Node、Makefile、seed、前端、五份 OpenAPI 或 Go 依赖；需要新增底层入口必须先提交具体接口缺口。

## 必须实现
1. Resource API 使用 `/v1/namespaces/{namespace}/...` 的真实 name/namespace 身份与轻量信封；`status` 只读派生，JSON camelCase，空列表返回 `[]`。
2. Capability/Source/Node/Namespace/Playbook 的 CRUD、配置与动作消费现有真实接口。更新使用绑定 UID/revision 的 `metadata.resourceVersion`；DELETE 仅使用 query `resourceVersion`；缺失 400、冲突 409 并给当前版本，拒绝 ABA/跨 namespace。
3. Playbook files/source 互斥，安装 insert-only，冲突整包 0 写，不自动部署；Node 注册后发 token，明文仅一次，敏感字段 writeOnly。
4. Task/Event/Trace/列表 cursor/emissions 投影与取消、部署、fire/dryRun 等动作必须接真实 runtime，不能复制旧 DTO。未解析 `downstream_missing` 为真实 failed/attempt0 Task，未知 version/digest 省略，不造 Node 过程事实。
5. 保留 auth/password/passkey/recovery/CLI key/session/admin/devices/VAPID/broker/backpressure/独立 POST notify/push。旧 `notify.phone` 中央 Capability 和 executor 刻意退役，不能恢复，也不能借机删除独立推送。

## 验收标准
- [ ] 逐项 inventory 原 server/API 测试；迁移安全意图，退役项具名说明，禁止整文件清空认证/权限/CAS/delete/secret 断言。
- [ ] 真实 httptest 覆盖信封/只读 status/CAS/ABA/跨域隔离、列表 cursor、Task 零 attempt 失败、token one-shot、安装冲突 rollback、fire 与 preview 0 写。
- [ ] 错误响应与原独立 auth 契约一致；匿名 `/v1/auth/me` 不套 Resource，敏感值不进入 response/log。
- [ ] `gofmt -l internal/server internal/api internal/auth internal/authn` 无输出，`git diff --check` 通过。
- [ ] `GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto go vet ./internal/server/... ./internal/api/... ./internal/auth/... ./internal/authn/...` 通过。
- [ ] 相同包 `go test ... -count=1` 通过；底层 store/runtime/protocol 只读回归通过。

## 完成定义
交付源码 commit、公共路由/鉴权/DTO 清单、具名旧→新测试映射、真实命令/exit/full log，独立 fresh review 无 critical/major。G4 旧 CLI/seed 尚未接线造成的全仓编译阻塞必须列具体 owner，不加 v3 兼容层掩盖。本 issue 不声称全 v4 发布完成。