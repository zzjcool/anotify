# 冻结契约 · 任务日志：节点为真相，控制台按需取

> 唯一事实源。日期 2026-11-06。用户拍板：「A：节点是真相，控制台点开时中枢向节点按需取」。

## 0. 为什么改

上一轮做的是「agent 把 stdout/stderr **上传**到中枢 `task_logs`」。用户明确：**日志只存在节点上**，控制台要看时再去取。

现状（已就绪，不要重做）：

- 节点档案：`<data-dir>/runs/<trace>/<capability>@<version>/<seq>-a<attempt>/` 内有 `input.json` / `stdout.log` / `stderr.log` / `meta.json`（完整、8 MiB 单流上限、0700/0600、7 天 + 2000 份 GC）
- 中枢已有 `GET /v1/tasks/{taskId}/logs?namespaceId=…` → `{stdout,stderr,truncated,bytes}`（trace 页在用）
- 中枢**没有**任何向节点取数的通道（agent 无入站监听，帧只有 welcome/assign/deploy/cancel/error/upgrade/emit_ack）

## 1. 新帧（WS）

```
中枢 → 节点 : logs_request  {type,task_id,request_id}
节点 → 中枢 : logs_response {type,task_id,request_id,stdout,stderr,truncated,bytes,error}
```

- `request_id` 由中枢生成（`lreq_<base62>`），节点原样回带；中枢按它配对，5 秒超时。
- 节点找不到档案（被 GC / 老任务 / 不是这台节点跑的）→ `error:"no_local_archive"`（**不是**空字符串），中枢翻成 `200 {"stdout":"","stderr":"","bytes":0,"available":false,"reason":"no_local_archive"}`。
- 节点回包**单次上限 1 MiB**（帧硬上限之内）；超出时截断并在 `truncated=true`（档案本身仍完整）。优先给 stderr 尾部 + stdout 尾部（各 512 KiB）。
- 帧里不带 params（那属于 `input.json`，控制台暂不展示；避免把可能敏感的入参走过网络）。

## 2. 中枢端点（形状不变，语义变）

```
GET /v1/tasks/{taskId}/logs?namespaceId=…
→ 200 {"stdout":"…","stderr":"…","truncated":false,"bytes":1234,"available":true}
→ 200 {"stdout":"","stderr":"","bytes":0,"available":false,"reason":"node_offline"|"no_local_archive"|"timeout"}
→ 404 任务不存在 / 跨命名空间
```

- 任务存在但节点离线 → **200 + available:false**（前端已有降级分支；不要把"节点不在"做成 5xx）
- 新增字段 `available` / `reason`；`bytes` 保留（0 与"没取到"用 available 区分）
- 每次请求都向节点取（**不落库、不缓存**：日志只存在节点上）

## 3. 停止上传

- 删除 agent 侧的日志上传（`cmd/agent/logs.go` 的上传路径 + `client.go` 的 `uploadLogs` 调用）；`outcome.Stdout/Stderr/Truncated/Bytes` 保留（summary 与档案还要用）
- 删除 `POST /v1/node/tasks/{taskId}/logs` 与 `internal/store/task_logs.go` 的写入路径
- **`task_logs` 表保留**（`migration 4` 已发布，删表要 migration 6；本轮不动 schema，只停止写入）——在表注释里写明「已停用，日志只在节点」
- `GET /v1/tasks/{id}/logs` 的读实现改为「从节点取」

## 4. 节点侧

- 档案路径已有，但**按 task_id 查不到**（路径按 trace/能力/序号组织）。新增旁路索引：
  `<data-dir>/runs/.tasks/<task_id>` → 一行文本：相对路径（相对 `runs/`）。写档案时一并写（原子：先写档案，再写索引；两者不一致时以档案为准，索引缺失就扫一遍 meta.json 兜底）。
- 收到 `logs_request` → 查索引 → 读 `stdout.log` / `stderr.log` 尾部 → 回包。**不阻塞**任务执行（读文件很快，超时 2 秒）。
- GC 删目录时**同时删索引**（否则会指到不存在的档案）。

## 5. 界面

trace 页日志面板保持不变（懒加载、失败默认展开）。新增两种降级文案（四语言）：
- `available:false, reason:node_offline` → 「节点离线，日志留在节点上，连上后可再看」
- `reason:no_local_archive` → 「节点上没有这次执行的档案（可能已过保留期）」

## 6. 分组

| 组 | 范围 |
|---|---|
| **12（Go，单组做完）** | `internal/ws`（帧 + 请求/响应配对）、`internal/server`（端点改按需取 + 删上传端点）、`internal/store`（删写入路径，表保留 + 注释）、`cmd/agent`（索引 + 帧处理 + 删上传）、`api/openapi.yaml` |
| 协调者 | 前端降级文案与 e2e（若 12 留着不做，我补） |

## 7. 门禁

gofmt / vet / build / `go test ./...` / `make contract`；节点侧新增用例必须覆盖：命中、未命中、节点离线、超时、1 MiB 截断。
