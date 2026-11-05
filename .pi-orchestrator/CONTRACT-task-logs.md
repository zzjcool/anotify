# 冻结契约 · 任务日志（节点 → 中枢 → 控制台）

> 唯一事实源。日期 2026-11-05。

## 0. 为什么

现场：`test1@2` 任务 failed，控制台只显示

```
exit status 2: capability: test1@2
```

真实原因在**子进程的 stderr** 里：

```
flag provided but not defined: -to
```

节点上有一份旧的 `/usr/local/bin/anotify-agent` 抢了 PATH，脚本里的
`emit --to` 被旧二进制当成未知参数。这条信息价值极高，却**一行都没留下来**——
`result` 帧只带 stdout 第一行（summary）和 `exit status N`。

目标：**任务跑坏了，控制台能看到为什么**，不用登录机器翻 journald。

## 1. 采集（agent 侧）

- 每个任务的 stdout / stderr 各自捕获，**各截断到 32 KiB**（合计 64 KiB）。
- 截断时保留**尾部**（错误通常在最后），并在末尾追加一行标记：
  `\n…（日志已截断：共 N 字节，只保留最后 32768 字节）`
- 二进制内容不安全：先按 UTF-8 校验，非法字节替换为 `?`（JSON 必须能编）。
- 捕获的是**子进程**的输出；agent 自己的日志（`slog`）不混进来。

为什么截断而不是全量：帧软上限 1 MiB，一次 20 MB 的构建日志不该把心跳挤掉；
32 KiB 尾部覆盖了绝大多数"为什么炸了"。

## 2. 传输（不回传整帧，落 HTTP）

**不走 WS 帧**——理由同上（帧堵塞会让节点掉线）。改为：

```
POST /v1/node/tasks/{taskId}/logs      （node token 鉴权）
Content-Type: application/json
{"stdout":"…","stderr":"…","truncated":true,"bytes":1048576}

→ 204 No Content
→ 404 任务不存在 / 不属于这个节点
```

- agent 在**发 result 帧之前**上传（失败只记 WARN，**不影响任务成败**：日志丢了
  是可惜，但不能因为日志上传失败把跑成功的任务判成失败）。
- 幂等：同一 task 重复上传直接覆盖（重派/重启补传都安全）。
- 上限：请求体 256 KiB（服务端硬拒 413）；超出说明 agent 侧截断逻辑坏了。

## 3. 存储

`+migration 4`：

```sql
CREATE TABLE IF NOT EXISTS task_logs (
    task_id      TEXT PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE,
    namespace_id TEXT NOT NULL,
    stdout       TEXT NOT NULL DEFAULT '',
    stderr       TEXT NOT NULL DEFAULT '',
    truncated    INTEGER NOT NULL DEFAULT 0,
    bytes        INTEGER NOT NULL DEFAULT 0,
    created_at   INTEGER NOT NULL
);
```

跟随任务生命周期（`ON DELETE CASCADE`）：任务被 retention 清掉时日志一起走。

## 4. 读（控制台）

```
GET /v1/tasks/{taskId}/logs?namespaceId=…      （会话鉴权 + 命名空间隔离）
→ 200 {"stdout":"…","stderr":"…","truncated":false,"bytes":1234}
→ 200 {"stdout":"","stderr":"","truncated":false,"bytes":0}   ← 没有日志（老任务/未上传）
→ 404 任务不存在 / 不在这个命名空间
```

**永远 200 而不是 404**（当任务存在但没有日志时）："没日志"是合法状态，
把它做成 404 会让前端在正常路径上处理错误。

## 5. 控制台 UI

trace 页每个任务卡：

- 失败任务**默认展开**日志（这是最需要它的时候），成功任务默认折叠
- 一个「日志」按钮/折叠条：`stdout` 与 `stderr` 分两块（等宽字体、可滚动、
  自动 `overflow-x`），空的块不显示，两块都空时显示「没有输出」
- 截断时显示一行提示（不得让人误以为看全了）
- 四语言；颜色只用 tokens（stderr 用 danger 色系）

## 6. 分组

| 组 | 范围 |
|---|---|
| **E（Go）** | `cmd/agent`（捕获 + 上传 + PATH 打头）、`internal/ws`（result 帧与会话）、`internal/store`（migration 4 + 读写）、`internal/server`（上传端点 + 读取端点）、`api/openapi.yaml` |
| **F（前端）** | `web-src/pages/trace.html`（日志面板）、i18n 四语言、`web/mock.js`、`scripts/e2e/suites/frontend.mjs` |
| 协调者 | e2e（trace/real_backend 断言日志真的落库并可读）、dev 部署与现场复核 |

## 7. 门禁

E：gofmt / go vet / go build / go test ./... / make contract（openapi 必须同步）。
F：check-classes / check-tailwind-css / make fe / e2e frontend。
