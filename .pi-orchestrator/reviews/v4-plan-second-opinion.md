# V4-PLAN-SECOND-OPINION（纯计划，无实现）

冻结基线：docs/design/v4 @ 368b489。本文只给编组/所有权/门禁/冻结接口建议。

## 结论

**不要按模块拆隔离团队。全部 Go 归一个 centurion（单一共享 worktree，阶段串行 + 阶段内多 worker 不重叠包），前端独立一个 team。即 2 个写团队 + 1 个只读 review 带。**

理由：store/route/ws/server 同一 Go module，`go test` 是包级全编译；A 改完 store 的旧数据 shape，B/C 的旧测试（强绑 Spec outputs/handler/9 态 taskassign）立即不可编译，隔离 worktree 里 B 的验收门禁跑不起来 → 只能靠兼容层或假绿灯，两者都被禁止。可编译 freeze 骨架（空实现接口）确实能换并行，但 v4 是行为重写（attemptgrant 计数、lease60/renew20/TERM→5kill、CAS、7/30GC），骨架不承载行为，B 拿到骨架仍要等 A 的真实语义，等于接口写两遍。

## 编组

- T1 Go：centurion，共享 worktree，P1→P4 阶段串行，阶段内多 worker 按包/文件不重叠。
- T2 Web：独立 worktree，P0 冻结 OpenAPI 后全程并行，只写 web/**。
- T3 Review：每阶段末只读评审（正确性/回归、测试覆盖、简洁性），无写权。

## T1 阶段与门禁

- **P1 store 地基**：schema baseline 直接重建（无 migration 块）；artifact 目录 files+render+installs+原子 conflict；clusterNode 三层 vars 合并序；task 单行 attemptgrant 计数；lease 60/renew20/cancel TERM→5 kill；7/30 GC；CAS；status 现算。删 Spec outputs/ports/edge(priority,mapping,literals)/node.namespace_id/9 态 taskassign/单 JSON playbook。
  门禁：`go build ./internal/artifact ./internal/store && go test ./internal/store`
- **P2 route/ws/node**：dispatch 接口、next 版外路由、本地 emit→中枢显式 id 去重；node_protocol.go 单一帧定义，cmd/agent 消费同一 Go 帧。
  门禁：`go build ./internal/route ./internal/ws ./cmd/... && go test ./internal/route ./internal/ws ./cmd/...`
- **P3 server**：mux、ns 前缀路径 + spec/status、HTTP 契约、api/auth/lease 接线；删中枢 executor/notify-phone，保留认证/设备/通知 API。
  门禁：`go build ./... && go test ./internal/server/... ./internal/api/...` + OpenAPI/parity。
- **P4 集成**：devseed/e2e harness 改新 shape、29 个 e2e 重写、`make fe`→fingerprint→embed dist、`make e2e` 全绿。
- **MR 时点**：P3 完成才开 MR。P1/P2 期间 server 包预期编译失败，用显式 expected-fail 清单跟踪；不合并、不弱化、不 skip。

## 主会话必须先冻结的 6 个接口（写进 contract.md）

1. `internal/ws/node_protocol.go`：全部帧类型 + 版本字段，唯一权威；server 只用不另定义。
2. `internal/route/dispatch.go`：方法集、req/resp 类型、错误分类。
3. `internal/artifact/artifact.go`：目录 files/render/installs + 原子 conflict 错误类型。
4. store 公开面：attemptgrant/lease 常量(60/20/TERM5)、GC(7/30) 触发点、CAS 谓词签名、status 由 api 现算（store 只存原始值）、clusterNode vars 三层覆盖顺序。
5. `api/openapi.yaml`：ns 前缀路径、spec/status 形状、显式 id 去重字段、camelCase、空列表 `[]` 非 null。
6. 文件所有权表 + AGENTS/DEVELOPMENT 的 v4 例外条款（baseline 重建合法、禁 migration 块），显式写"v4 例外"，不静默删红线。

## 单一 owner 清单

- T1：`internal/artifact/artifact.go`、`internal/store/{schema,migrate,capability,task,node,graph}`、`internal/route/dispatch.go`、`internal/ws/node_protocol.go`、`server/mux`、OpenAPI/parity、Makefile/devseed/e2e harness。
- T2：`web/partials`、mock、graphs、ui、tokens、tailwind、locales、19 个静态页（capabilities 6611 行）、`scripts/check-classes.mjs`。
- embed dist：只由 P4 单点重建（T2 不碰 embed 产物）。

## 测试红线

只允许退役纯旧 ports/priority/迁移测试，且每条在 contract 里配一个具名的新契约等价覆盖；禁止删除/skip/弱化断言去换绿灯。capabilities 6611 行只按 v4 差异改，不重排重写。

## 不承诺

不恢复 v3 兼容、不多中枢、不做 sandbox；不给"1 小时全量 v4"。时间压缩顺序：先砍 P4 e2e 广度，再砍 P3 非关键端点；P1+P2 是不可压的地基。
