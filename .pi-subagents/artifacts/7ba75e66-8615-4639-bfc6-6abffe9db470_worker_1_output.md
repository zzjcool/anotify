DONE T11

# T11 · Passkey 认证 + API Key 中间件 — 完成报告

worktree: `wt-auth`（分支 wt-auth），commit `6ea9ae5`
注：任务提示里给的 `context.md` / `plan.md` 不存在，以任务卡 `.pi-orchestrator/tasks/t11-auth.md` 为准实施。

## 产出文件

实现（worktree 根相对）：

- `internal/auth/webauthn.go` — WebAuthn 注册/登录。注册：BeginRegister（用户名查重 + 生成 creation options + 暂存 challenge）/ FinishRegister（校验 attestation → 建 user + passkey + 会话）。登录：BeginLogin / FinishLogin（指定用户名）+ BeginDiscoverableLogin / FinishDiscoverableLogin（conditional / passkey，按 userHandle 定位用户）。challenge 用带 TTL 的内存 map（一次性取用、过期拒绝、命名空间隔离）。RP 配置可注入（RPDisplayName/RPID/RPOrigins/SessionTTL/SecureCookie）。登录成功回写 sign_count 防重放。
- `internal/auth/session.go` — 会话签发/校验（含过期判定）/吊销，httpOnly Cookie（Secure/SameSite=Lax），`Middleware` 校验 Cookie 并把 userID 注入 request context（`UserIDFromContext`）。
- `internal/auth/apikey.go` — API Key：`CreateKey` 生成 `ant_<label>_<random>`（label=send/recv/full/key），**只存 argon2id PHC 哈希**，明文仅返回一次；`ValidateKey` 前缀定位 + argon2id 常量时间比对；`RequireScope(scope)` 中间件（无 key 401 / 无效 401 / scope 不足 403）。argon2id 参数 time=1 memory=64MB threads=4 keyLen=32（按任务卡）。
- `internal/auth/credid.go` — credential id 的 base64url 编解码（表主键用字符串）。
- `internal/store/auth.go` — users/passkeys/sessions/api_keys 四表的数据访问（严格按 schema.sql 字段），定义 `store.ErrNotFound`。

测试：

- `internal/auth/auth_test.go` — 18 个用例
- `internal/store/auth_test.go` — 4 个用例

## 自测命令与结果

```
go test ./internal/...          → ok auth 0.645s / ok store 0.726s (22 tests PASS)
go test ./internal/auth/... -race → ok 3.464s
go vet ./internal/...           → 干净
gofmt -l internal/              → 干净（无输出）
go build ./...                  → 通过
```

关键覆盖：

- API Key：签发→校验往返（userID/scope 正确）、argon2id PHC 格式（非明文）、scope→前缀标签映射（send/recv/full/key）、篡改 key 拒绝、非法前缀拒绝、不存在 key 拒绝、已停用 key 拒绝、未知 scope 拒绝、空 scope 拒绝
- RequireScope 中间件：有效 key 放行并注入 userID、缺 Authorization 401、非 Bearer 401、错误 key 401、scope 不足 403
- 会话：签发→校验→吊销生命周期、过期判定、中间件（无 Cookie 401 / 有效 200+注入 userID / 无效 401）
- challenge：一次性取用、过期拒绝、reg:/login: 命名空间隔离
- 注册前置：用户名重复拒绝、BeginRegister 返回 options 且暂存 challenge、BeginLogin 未知用户拒绝
- credid base64url 往返、非法串报错
- store：四表 CRUD、用户名唯一约束、ErrNotFound、级联字段（transports/scopes JSON、enabled 布尔、last_used 可空）

## 遗留风险

1. **真实 WebAuthn attestation/assertion 未做端到端单测**：注册/登录的 Finish* 需要真实认证器（平台/安全密钥）产生有效签名，无法在多平台 CI 单测中伪造。已对 challenge 存取、用户查重、凭证存取、会话建立等周边逻辑做了完整单测；真实链路留给集成期桌面 Chrome（T36）与 iOS 真机（T40）验证。
2. **challenge 存于内存 map**：单进程够用（任务卡明确允许）；若未来多副本部署需换共享存储（如 Redis），届时随 Broker 一起演进。
3. **会话/Key 无后台过期清理任务**：过期会话在使用时被动拒绝，但未主动 DELETE；量大后建议加一个定期清理（可与 broker 的过期清理放一起，属 T10/集成范围，不阻塞本任务）。
4. **argon2id 校验在每次 API 调用执行**（64MB/1轮）：单次约几十 ms，对 Agent 上报频率完全可接受；若未来有高并发上报，可加一层带 TTL 的校验缓存（当前不需要，避免过早优化）。
5. **`store.NewEventID()` 复用为 api_keys.id**：ID 生成器目前只保证唯一 + 时间有序，前缀语义（evt_）对 api_keys 略不准；功能无影响，若在意可在后续给 store 增 `NewKeyID()`。