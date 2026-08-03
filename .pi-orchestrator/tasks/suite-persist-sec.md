# 任务：编写两个 E2E 套件 `persistence.mjs` 和 `security.mjs`

先读 `/Users/zheng/code/anotify/.pi-orchestrator/tasks/e2e-suites-common.md`（公共约定）。

## 套件 1：`scripts/e2e/suites/persistence.mjs`（重启持久化）

覆盖 case：

1. 起服务，seed 用户，用 sendKey 上报 3 条通知，创建设备（session POST /v1/devices），创建 Key
2. `server.stop()` 但**保留 dbPath**（注意：harness 的 stop 会删临时目录——你需要不用默认 tmp，或改为：startServer 后记录 dbPath，手动 kill 进程不删目录，再用同 dbPath 重启。可复制 harness 逻辑或用 extraEnv 指定 DB 到固定路径）
   - 实现建议：不依赖 server.stop 删目录；改成 spawn 两次 startServer 指向同一个你自建的固定 DB 路径（如 mkdtemp 后自己管理生命周期，测试结束才清理）
3. 用同一 DB 重启服务 → GET /v1/notifications（带 session）→ 之前的 3 条消息仍在（Replay 到）
4. GET /v1/devices → 设备仍在
5. 用之前的 sendKey 再上报 → 仍 200（Key 持久化）
6. seq 连续：重启后再上报，新消息 seq 接续（不会从 1 重来导致冲突）

## 套件 2：`scripts/e2e/suites/security.mjs`（安全矩阵）

覆盖 case：

1. scope 越权：recv Key POST /v1/notify → 403；send Key 连 /v1/stream → 403/拒
2. Key 篡改：把合法 sendKey 改几个字符 → 401；改前缀（ant_send→ant_live）→ 401
3. 无 Authorization 头访问 /v1/notify → 401；非 Bearer scheme（Basic xxx）→ 401
4. 未登录访问 /v1/devices /v1/keys /v1/notifications → 各 401
5. Key 哈希不可逆：seed 后用 sqlite 查 api_keys.key_hash（可用 `go run ./cmd/devseed` 之外——直接读 DB 文件验证 key_hash 是 argon2 PHC 格式以 `$argon2id$` 开头，且**不等于**明文 Key，不含明文子串）。可用 node 读文件二进制搜明文 Key 是否出现（不应出现）
6. 会话 Cookie 属性：注册/devseed 建会话后，响应 Set-Cookie 的 anotify_session 含 HttpOnly
7. SQL 注入：username 用 `admin' OR '1'='1` 之类注册/查询 → 不报错、不越权、正常 400/404
8. 路径穿越：GET /../etc/passwd 或 /%2e%2e/ → 403/404，不读出站外文件

两个套件都自测跑通（exit 0）后上报。发现产品 bug（如 Key 明文落盘、SQL 注入成功、路径穿越可读）要**明确上报**，不要改断言迁就。
