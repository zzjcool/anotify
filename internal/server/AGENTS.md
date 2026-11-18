# server · 模块约定

as_of: 2026-11-16 · area: server · 门禁: `go test ./internal/server/... -count=1`

## 1. 边界

- **负责**：HTTP 路由装配（`mux.go`）、中间件（会话/限流/背压/访问日志）、静态资源与 CDN 缓存分级、
  embed 产物、资源域 handler（`resource_*.go`）、产物上传下载端点。
- **不负责**：业务规则（`store` / `route` / `source`）、执行（`cmd/anode`）、推送（`push`）。
- 判据：**server 只做投影** —— 从 store 拿事实，按 `docs/design/v4/api-conventions.md` 投影成信封，
  不自己发明状态、不在读路径上写库（GC 等后台写者是例外，且必须能被 `WaitBackground` 收尾）。

## 2. 改之前必读

1. `internal/server/mux.go` 的 `NewApp` 装配顺序注释（唯一 mux 写入者）
2. `internal/server/api_deps.go` 文件头（`serverDeps` 是冻结契约 D1）
3. `internal/server/api_envelope.go` 文件头（信封四个顶层键 + `status` 恒存在）

## 3. 硬红线

1. **`mux.go` 是唯一 mux 写入者**：新增资源路由 = 在 `resource_<域>.go` 实现 `register(mux)`，
   再在 `mux.go` 资源域段落加**一行** caller（`mux.go:421-427`）。不手写扁平资源路径，不加旧路径重定向。
2. **装配顺序有语义，不得重排**：store → auth → 保留域 → 背压 → 会话中间件 → 限流 →
   router/hub/source/trigger/lease → 资源域 → 节点协议 → 健康检查/pprof/静态兜底（`mux.go:69-92`）。
   背压**只包写入口**，不包读端点。
3. **鉴权只有一份**：`serverDeps.Session`。各域不许自己再包一层 —— 两份实现迟早对「未登录」给不同状态码。
4. 中枢侧不得 import `os/exec`（`make contract` 的 A5 机检）。执行只发生在节点。
5. 信封纪律：四顶层键 + 可选 `warnings`；`status` 恒存在（无事实 `{}`）；空列表 `[]`；
   错误体统一走 `writeErr`（`handlers.go:21`）/ `writeStatus`（`api_status.go:83`），不要手拼 JSON。
6. 静态资源三分级（`static.go` 的 `cacheClass`）：指纹资源 `immutable` 1 年 / 入口 HTML 短缓存 + ETag /
   API `no-store`。改前端产物后必须 `make fe`，否则 embed 里还是旧指纹。
7. 产物字节只走 `resultspool`：spool 为 nil 时回 503，**绝不 fallback** 到节点/用户目录（A1 禁止）。

## 4. 门禁

```bash
go test ./internal/server/... -count=1
make contract     # 改了 api/*.yaml 或路由必跑：A5 + check_openapi + check_contract + TestOpenAPIParity
make fe           # 改了静态资源/embed 必跑（重新指纹）
```

## 5. 陷阱

- **新增匿名端点必须带限流**：`ratelimit.go` 顶部常量是既有口径（10/min/IP 一档）。无鉴权的入口就是撞库面。
- `statusWriter.WriteHeader` 只认**第一次**（`middleware.go`）：handler 里先写 201 再写 500，
  访问日志记的是 201、响应体却是 500 —— 这类不一致比报错更难查。
- `serverDeps` 加字段 = 改冻结契约：同步 `api_deps.go` 注释 + 所有 `newXxxAPI(` 调用点。
- 关停顺序必须是 cancel ctx → `App.WaitBackground()` → `db.Close()`（见 `mux.go` 的 `WaitBackground` 注释），
  否则后台循环撞 `sql: database is closed`（#259/#340）。
- 保留域读模型（`/v1/stats`、`/v1/orphans`、`/v1/namespaces/{ns}/deployments`）**不套信封**，别顺手「统一」。
- 节点通道不是普通资源：`/v1/node/stream` 的 handler 就是 `ws.Hub` 本身，鉴权在 `hello` 帧里
  （`mux.go` 节点协议段），不在 HTTP 中间件。

## 6. 上下游契约

- 与 `store`：store 给事实，server 只投影；**server 不写 SQL**。
- 与 `route` / `source`：路由规划、源触发都不在这里实现，server 只调用。
- `mux.go` 与 `api/*.yaml` 骨架由 contract owner 会签；跨 area 的装配改动先上报再动手。
