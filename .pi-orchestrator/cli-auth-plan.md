# 实施计划 · CLI 设备授权登录（feat/cli-device-login）

> 协调者产出。需求：`.pi-orchestrator/cli-auth-requirements.md`；设计：`.pi-orchestrator/cli-auth-design.md`。
> 本文档是**最终契约裁决**（pm 与 designer 草案有分歧处，以本文为准）+ 任务拆解。

## 0. 契约裁决（最终，worker/frontend/tester 一律以此为准）

### 端点总表

| 端点 | 鉴权 | 说明 |
|---|---|---|
| `POST /v1/cli-auth/sessions` | 匿名，IP 限速 10/min | 建会话。body `{deviceName, scopes[]}` → `{sessionId, secret, userCode, authUrl, pollInterval, expiresAt}` |
| `GET /v1/cli-auth/sessions/{id}/qr.txt` | 匿名，IP 限速 30/min | ASCII 半块字符二维码（内容=authUrl），`text/plain; charset=utf-8` |
| `GET /v1/cli-auth/sessions/{id}/poll?secret=` | 匿名，secret 门控 | 轮询。统一 200 + `{status}`；approved 时**一次性**附带 Key；secret 错 401；不存在 404；快于 pollInterval 429 |
| `GET /v1/cli-auth/sessions/{id}` | **登录 Cookie** | 确认页数据：`{sessionId, deviceName, requestedScopes[], status, createdAt, expiresAt}`；404 统一 `{error:"授权会话不存在或已过期"}` |
| `GET /v1/cli-auth/sessions/by-code?code=XXXXXXXX` | **登录 Cookie**，按用户限速 20/min | 短码 lookup，返回同上；code 大小写不敏感、容忍连字符 |
| `POST /v1/cli-auth/sessions/{id}/approve` | 登录 Cookie | body `{scopes[]}`，必须 ⊆ requestedScopes 且非空（否则 400）；成功 `{status:"approved"}`；终态冲突 409 `{status}` |
| `POST /v1/cli-auth/sessions/{id}/deny` | 登录 Cookie | `{status:"denied"}`；409 同上 |
| `GET /v1/keys/self` | Bearer Key | 自检：`{name, prefix, scopes[]}`；401 |
| `GET /agent-login.sh` | 匿名 | 登录脚本分发，`Cache-Control: no-store`，固定名不指纹 |

### 裁决要点（分歧记录）

1. **页面 lookup 端点需登录**（采纳 designer，否决 pm 的公开只读端点）：防止未登录枚举会话信息。确认页所有 API 走 Cookie；401 由 `Anotify.api` 守卫跳登录。
2. **lookup 必须返回 `requestedScopes`**（designer 草案遗漏）：确认页据此预勾选。
3. **status 枚举 = `pending | approved | consumed | denied | expired`**：lookup 返回 approved 时页面落「已批准」终态（刷新场景）。
4. **authUrl = `{base}/cli-auth.html?s={sessionId}`**，只含 sessionId（否决 pm「URL 同时带 code」：QR 越短越好扫，code 只用于手动输入）。
5. **poll 各结果统一 200 + status 字段**（含 consumed/denied/expired），脚本零依赖好解析；仅 secret 错（401）/不存在（404）/过快（429）用错误码。
6. **一次性领证原子性**：store 层单事务「`UPDATE ... WHERE id=? AND status='approved'` + 插入 api_keys 行」，0 行受影响=已被消费。明文 Key 任何表都不落。
7. **过期惰性迁移**：读路径上 pending/approved 且 now>expiresAt → 置 expired 再返回；expired 不可逆。
8. **脚本源文件放 `web/agent-login.sh`**（手写静态资源，与 partials.js/sw.js 同纪律；不进 web-src/sitegen 管线），hash.mjs 不得指纹它（固定文件名），路由显式 no-store。**否决** Makefile 拷贝进 embed 目录的方案。
9. userCode 字符集 `ABCDEFGHJKMNPQRSTUVWXYZ23456789`（32 字符去歧义），存库大写无连字符，显示 `XXXX-XXXX`。
10. secret = 32B 随机 base64url（~43 字符），库中只存 SHA-256 hex，比对用常量时间。

### 数据模型（store）

```sql
CREATE TABLE IF NOT EXISTS cli_auth_sessions (
  id               TEXT PRIMARY KEY,            -- "cas_" + base64url(16B)
  secret_hash      TEXT NOT NULL,               -- sha256 hex
  user_code        TEXT NOT NULL UNIQUE,        -- 8 字符大写无连字符
  device_name      TEXT NOT NULL,               -- ≤64 字符
  scopes_requested TEXT NOT NULL,               -- JSON 数组
  scopes_granted   TEXT,                        -- JSON 数组，批准前 NULL
  status           TEXT NOT NULL,               -- pending/approved/consumed/denied/expired
  user_id          TEXT,                        -- 批准前 NULL
  key_id           TEXT,                        -- 领证前 NULL
  created_at       INTEGER NOT NULL,
  expires_at       INTEGER NOT NULL,            -- createdAt + 600
  consumed_at      INTEGER
);
```

`schema.sql` 加表（CREATE TABLE IF NOT EXISTS 天然幂等，老库自动补）；往返一致性单测必配。

### 关键复用点

- `internal/auth/apikey.go`：`KeyManager.CreateKey` 校验 scope 合法性的逻辑抽到包内小函数供 CliAuthManager 复用；同包可直接用 `hashKey`/`scopeLabel` 构建 key 记录。
- `SessionManager.Middleware`（sessMW）：页面侧端点套它。
- Key 自动命名 `agent:<deviceName>`；label 逻辑自动产 `ant_send_` 前缀。
- 限速器：`internal/server` 新增 ~60 行内存固定窗口限速器（key→count/windowStart，mutex，惰性清理），不引外部组件。
- QR：新依赖 `github.com/skip2/go-qrcode`（纯 Go），`qrcode.New(url, qrcode.Low)` 取 Bitmap 自渲半块字符（▀▄█ + 空格，含 2 模块 quiet zone），80 列终端可完整显示。
- mux 用 Go 1.22+ 模式（`"POST /v1/cli-auth/sessions"`、`"GET /v1/cli-auth/sessions/{id}/poll"`），`by-code` 字面量优先于 `{id}` 通配，无冲突。

## 1. 任务拆解与编排

| 任务 | Agent | 内容 | 依赖 |
|---|---|---|---|
| T1 | worker | store（表+CRUD+迁移+round-trip 测试）+ `internal/auth/cliauth.go` CliAuthManager（建会话/lookup/approve/deny/poll 消费式领证/惰性过期/短码生成重试/惰性清理）+ 单测 | — |
| T2 | frontend | `web-src/layouts/focus.html` + `web-src/pages/cli-auth.html` + 四语言 locales + `partials.js` PAGES + `login.html` NEXT_MAP + `keys.html` 入口 + `make fe` + web_verify | 契约（本文档 §0），与 T1 并行 |
| T3 | worker | server 层：handlers+mux 路由+限速器+qr.txt（go-qrcode 依赖）+`/v1/keys/self`+`/agent-login.sh` 路由+openapi.yaml+handler 测试 | T1 |
| T4 | worker | `web/agent-login.sh` 脚本（POSIX sh+curl 零依赖：幂等自检/建会话/三入口输出/轮询/0600 原子落盘/.bak 备份/退出码 0,1,2,3,4/stdout 无 Key）+ hash.mjs 不指纹确认 | T3（路由先有） |
| T5 | tester | e2e 套件 `scripts/e2e/suites/cli_auth.mjs`（先写套件+单跑，全量 e2e 放最后留足预算）+ web_verify 复核 | T1–T4 |
| T6 | reviewer | 终审（收敛范围、先写报告再深挖；对照 requirements/design/plan） | T5 |

**纪律**：各实现 agent 不执行 git commit（协调者分阶段统一提交，防并行索引竞争）；worker 测试范围自限（T1 只跑 `./internal/store ./internal/auth`，T3 跑 `./internal/...`）；frontend 只跑 `make fe` 不跑 `make build`（避免与 worker 的 go 编译互踩）。

## 2. 验收锚点

以 requirements §4 的 AC-01…AC-36 为准；安全不变量（§3.3：sessionId/userCode 只有批准权，secret 才有领证权）为最高裁决原则。完成定义：`make e2e` 全绿 + reviewer APPROVE。
