# DONE persistence.mjs + security.mjs

## 产出文件

- `scripts/e2e/suites/persistence.mjs`（新增，4424 字节）
- `scripts/e2e/suites/security.mjs`（新增，5987 字节）

## 覆盖 case 清单

### persistence（重启持久化，15 断言）

1. 重启前上报 3 条通知 → 各 200
2. 重启前创建设备 → 200 且返回 id
3. 重启前创建 Key → 200
4. 重启前消息数=3
5. **重启后消息仍在**（count=3）且内容一致
6. **重启后设备仍在**且名称一致
7. **重启后旧 Key 仍可用**（上报 200）
8. **seq 连续**：重启后新消息 seq = maxSeqBefore+1（不从 1 重置）
9. 重启后会话仍有效

实现要点：自建固定 DB 目录（绕开 harness stop 删临时目录的行为），两次 startServer 传同一 `ANOTIFY_DB`，进程级 kill 后重启，验证 SQLite 持久化。

### security（安全矩阵，22 断言）

1. scope 越权：recv Key 上报→403；send Key 连 /v1/stream 握手被拒（收不到 hello）
2. Key 篡改：改尾部字符→401；改前缀(ant_send→ant_live)→401
3. 无 Authorization 头→401；Basic scheme→401
4. 未登录 GET /v1/devices、/v1/keys、/v1/notifications→各 401
5. **Key 哈希不可逆**：DB（含 WAL 文件）二进制搜不到明文 send/recv Key；含 `$argon2id$` PHC 标记
6. SQL 注入：恶意 username 注册 options 不 500；`'; DROP TABLE users;--` 作 title 上报正常；注入后会话仍可用（表未被破坏）；伪造 session→401
7. 会话 Cookie：Set-Cookie 含 HttpOnly 且指定 anotify_session
8. 路径穿越：4 种 payload（/../etc/passwd、/%2e%2e/、/..%2f、/static/../../）→ 均 404 不泄露文件

## 自测命令与结果

```
ANOTIFY_VAPID_PUBLIC_KEY=... ANOTIFY_VAPID_PRIVATE_KEY=... \
  node scripts/e2e/suites/persistence.mjs  → PASS (15 通过 / 0 失败, exit 0) ×2 连跑
  node scripts/e2e/suites/security.mjs     → PASS (22 通过 / 0 失败, exit 0) ×2 连跑
```

## 发现的产品 bug

**无阻断性产品 bug**（Key 明文未落盘、SQL 注入被参数化阻隔、路径穿越被正确拦截、会话/scope/越权全部正确）。

## 调试中发现并修正的测试自身问题（非产品 bug）

1. persistence：`/v1/notifications` 直接序列化 `broker.Message`（Go 无 json tag），返回 **PascalCase** 字段（`Title`/`Seq` 而非 `title`/`seq`）。断言已适配。
2. security：SQLite WAL 模式下 key_hash 起初还在 `-wal` 文件未 checkpoint 到主 DB，改为搜 db 目录全部相关文件后断言通过。
3. security：logout 吊销会话的断言原放在"SQL注入后会话可用"之前导致后者 401，已调整执行顺序。

## 遗留风险 / 给协调者的建议

- **API 契约不一致（建议跟进，非本任务范围）**：`/v1/notifications` 返回 PascalCase 字段，而其余端点（如 devices 的 `store.Device` 有 json tag）返回 camelCase。前端 index.html 已被并行 worker 做了 PascalCase/camelCase 双兼容。建议后续给 `broker.Message` 统一加 json tag（camelCase），让 API 契约一致并更新 api/openapi.yaml。
- web/index.html、web/keys.html 存在其他并行 worker 的未提交改动（与本任务无关，未触碰、未暂存）。
- 两套件均未 git commit（按惯例由协调者统一提交）。

```
DONE persistence.mjs + security.mjs
```
