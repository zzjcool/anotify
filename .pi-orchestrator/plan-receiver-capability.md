# 阶段一实施计划：接收端能力/权限订阅模型改造

> 依据：`receiver-capability-design.md`（pm 设计）+ `context-receiver-capability.md`（scout 侦察）
> 范围：删 status，加 agentState+severity+kind+replyTo；status_filter→eventScope；StatusMatch→ScopeMatch；WS subscribe 加 eventScope；前端全改；插件跨仓库同步。
> 不做：双向回复 / Pet / herdr 桥接（后续阶段）。

## 一、任务拆分

### Task W1 · 后端协议+核心层（worker，worktree）
**改动顺序**（严格按依赖，避免编译断裂）：

1. **broker 层**（`internal/broker/broker.go` + `sqlite.go`）
   - 删 5 个 Status 常量，加 5 个 AgentState 常量 + `IsTerminal()` helper
   - Message struct：删 Status，加 AgentState/Severity/Kind/ReplyTo
   - sqlite.go：Publish 默认值改、INSERT/Replay SELECT/scanMessage 改列名+参数

2. **store 层**（`schema.sql` + `store.go` + `messages.go` + `devices.go` + `admin.go`）
   - schema.sql：messages.status→agent_state（+severity/kind/reply_to 列）；devices.status_filter→event_scope（default 'final'）
   - store.go migrateColumns：加幂等 ADD COLUMN 迁移（新列）；老库旧列留着不引用（见 context §7.1）
   - messages.go：MessageRow struct + InsertMessage/GetMessage/InsertTestMessage SQL+签名
   - devices.go：Device struct StatusFilter→EventScope + 4 处 SQL + scanDevice
   - admin.go：AdminGlobalMessage.Status→AgentState + ListGlobalMessages SQL

3. **route 层**（`internal/route/filter.go`）
   - 删 StatusMatch，加 ScopeMatch（用 broker.IsTerminal）
   - ShouldDeliver：StatusFilter→EventScope、Status→AgentState、调 ScopeMatch

4. **api 层**（`internal/api/notify.go`）
   - NotifyRequest/TestNotifyRequest struct 改字段
   - validStatuses→validAgentStates
   - 加 deriveSeverity() helper（放在 notify.go，API 层派生，不进 broker）
   - ServeHTTP/ServeTestNotify：校验+Message 构造+日志改

5. **ws 层**（`internal/ws/protocol.go` + `handler.go`）
   - protocol.go：Frame 删 Status，加 AgentState/Severity/Kind；加 EventScope（snake_case `event_scope`，与现有帧风格一致）；notificationFrame 改映射；subscribed 帧回显
   - handler.go：session 加 eventScope 字段 + setEventScope/getEventScope（mutex 保护）；handleFrame FrameSubscribe 解析+回显；主循环加 matchScope 过滤；session 初始化默认 "all"

6. **server 层**（`internal/server/handlers.go`）
   - deviceUpsert 默认 EventScope="final"（★ 产品核心诉求，别漏）
   - devicePatchReq：statusFilter→eventScope
   - patch 校验：all|error|success → final|all
   - messageView：Status→AgentState + 加 Severity；toMessageView/getOne 同步

7. **push 层**（`internal/push/dispatcher.go`）
   - pushPayload 加 agentState + severity（sw.js 渲染需要，见 context §7.7）

8. **Go 单测全改**（7 文件，按 context §3.1/§3.2/§4.1 清单逐个改）
   - 注意 §7.3：旧 statusFilter=error 无 1:1 映射，filter_test.go 的 TestStatusMatch 要重写为 TestScopeMatch（测 final/all + terminal 三值），不能简单替换字段名
   - 注意 §7.10：admin_test.go 的 "success"→"done" 是值改不是字段名改

9. **协议契约**（`api/openapi.yaml`）
   - NotifyRequest/TestNotifyRequest：删 status，加 agentState/severity/kind/replyTo
   - Device PATCH：statusFilter→eventScope
   - AdminGlobalMessage：status→agentState

**自测**：`go build ./...` + `go test ./internal/...` 全绿。然后 `make e2e`（但 e2e 此时必然红，因为前端+e2e套件没改——worker 只保证 Go 层绿）。

### Task F1 · 前端全改（frontend，worktree，与 W1 并行）
依赖 scout context §5。改 `web-src/`，跑 `make fe` 重新生成。

1. **receivers.html**：FILTER_LABEL→SCOPE_LABEL（final/all）；demo device statusFilter→eventScope（新设备默认 final）；updateDevice 调用改
2. **index.html**：tn-status→tn-agent-state（5 option）；STATUS_META→AGENT_STATE_META（5 值+severity 着色）；filter 逻辑改（建议 all/terminal/working 三档）；test-notify 字段改
3. **message.html**：STATUS_META→AGENT_STATE_META；渲染逻辑改
4. **admin.html**：statusBadgeEl map 改 agentState 5 值；i18n key 改
5. **docs.html**：curl 示例 status→agentState；投递规则 statusMatch→scopeMatch
6. **locales/*.yaml**（4 文件 zh-CN/en/es/ja）：按 context §5.6 改 key（common.status.*→common.agentState.*，加 severity.*，receivers.filter→receivers.scope，common.field.status→agent_state）
7. **ui.css**：CSS 类名保留（通用样式），JS 映射表改即可（context §5.7 建议）

**自测**：`make fe`（sitegen+hash）成功 + `web_verify` 逐页无 JS 错误。

### Task T1 · E2E 套件改（tester，依赖 W1+F1 合并后）
依赖 scout context §4.2。7 套件 + ws_test.mjs。

1. **routing.mjs**：★ 重写测试矩阵（§7.3 语义变了——final 放行 done+interrupted+error 三终态，非旧 error/success 单值过滤）。重新设计用例：done 广播+final 放行、working→final 拦截/all 放行、blocked→同 working、error→final 放行、interrupted→final 放行
2. **api_contract.mjs**：status→agentState（值 success→done, info→working）；statusFilter→eventScope（error→final, success→final, all→all）；坏值 400
3. **ws_protocol.mjs**：status→agentState；加 eventScope subscribe 帧测试（final 过滤中间态）
4. **security/edge_cases/persistence/push_e2e/frontend.mjs**：status:"success"→agentState:"done" 批量替换
5. **ws_test.mjs**：同上

**门禁**：`make e2e` 全绿（57s, 968 断言）。

### Task R1 · 终审（reviewer，依赖 W1+F1+T1）
对照 design.md + requirements 验收标准审 diff。重点：
- agentState 5 值 + severity 派生正确性
- eventScope 默认 final（push）是否所有路径都对了
- ScopeMatch 逻辑 + IsTerminal 一致
- 迁移幂等性
- 测试断言没迁就 bug（§7.3 语义重写是否到位）

## 二、执行顺序与 worktree

```
W1(worker, wt-receiver-backend)  ┐
                                  ├─ 并行（前后端改不同目录，主工作区协作也可，但用 worktree 更干净）
F1(frontend, wt-receiver-frontend)┘
        ↓ 合并到主工作区
T1(tester, 主工作区) ── 跑 make e2e
        ↓
R1(reviewer, 主工作区)
        ↓
协调者：make e2e 终验 + 跨仓库同步 anotify-plugins（cross-repo skill）
```

- W1/F1 用 worktree 隔离（代码改动）；编排文档（context/plan/design）已在主工作区，不进 worktree。
- T1/R1 在主工作区（W1/F1 合并后），避免 worktree 清理丢测试产物。
- 派发契约：W1/F1 async + timeoutMs≥3600000 + output 落盘；T1 同（跑全量 e2e）；R1 async + timeoutMs≥2400000。

## 三、验收标准（来自 design §9）

1. NotifyRequest/Message/schema：status 删除，agentState+severity(+kind/replyTo占位) 就位
2. Device：status_filter 删除，eventScope(final|all)，push 默认 final
3. route：ScopeMatch 替代 StatusMatch；WS subscribe 支持 eventScope
4. 前端：receivers 页 + 通知列表 + admin + docs 全部按新模型渲染，无旧 status 残留
5. `make e2e` 全绿
6. 插件仓库同步（跨仓库，单独走）

## 四、开放问题（已拍板，不再讨论）

Q1-Q6 全按 pm 建议：删 status / 5 值含 interrupted / 废弃只收 error / final+all 两档 / WS 加 eventScope / severity 命名 / platform 保留。
