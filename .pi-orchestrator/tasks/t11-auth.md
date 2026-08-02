# 任务 T11 · Passkey 认证 + API Key 中间件

先读根目录 `AGENTS.md` 和 `.pi-orchestrator/TASKS.md` 中 T11 一节。
你在 worktree `wt-auth`（分支 wt-auth）工作，**只改 internal/auth 与相关 store**。

## 目标

实现 `internal/auth`：WebAuthn(Passkey) 注册/登录、会话管理、API Key 签发与校验中间件（argon2id 哈希 + scope）。

## 契约

- store：`store.Open(path)`、ID 生成器（`NewUserID`/`NewSessionID`）
- schema：`users`/`passkeys`/`sessions`/`api_keys` 表已定义于 `internal/store/schema.sql`
- API 契约：`api/openapi.yaml` 的 `/auth/*` 与 `/keys/*`
- 依赖已就位：`github.com/go-webauthn/webauthn v0.17.4`、`golang.org/x/crypto`

## 要实现

1. `internal/auth/webauthn.go`：Passkey 注册/登录
   - 注册：`BeginRegister(username, displayName)` → 返回 creation options + 暂存 challenge；`FinishRegister(...)` 校验 attestation → 建 user + passkey + 会话
   - 登录：`BeginLogin(username)`（支持 conditional/discoverable）→ `FinishLogin(...)` 校验 assertion → 建会话
   - challenge 暂存用内存 map（带过期），单进程够用
   - Relying Party 配置做成可注入（RPDisplayName/RPID/RPOrigins），开发期从环境变量读
2. `internal/auth/session.go`：会话签发/校验/吊销（httpOnly Cookie），`Middleware` 把 userID 注入 request context
3. `internal/auth/apikey.go`：API Key
   - 签发：`CreateKey(userID, name, scopes)` 生成 `ant_<scope>_<random>`，**只存 argon2id 哈希**，明文仅返回一次
   - 校验：`ValidateKey(key)` 解析前缀→查 hash→argon2 比对→返回 (userID, scopes)
   - `RequireScope(scope)` 中间件：从 `Authorization: Bearer` 取 Key 校验，scope 不足返回 403
   - argon2id 参数：time=1 memory=64MB threads=4 keyLen=32（可用 x/crypto/argon2）
4. `internal/auth/auth_test.go`：单测
   - API Key 签发→校验往返、错误 Key 拒绝、scope 判定
   - 会话签发/校验/过期/吊销
   - Passkey 流程可用 webauthn 库的测试手段或对接口做表驱动测试（若真 attestation 难构造，可对内部逻辑单测并在上报说明）

## 约束

- gofmt；错误包装；Key 哈希用 argon2id（不要明文/SHA）
- 环境：`GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto`
- 完成后 commit：`feat(auth): Passkey + API Key 中间件`

## 上报

`DONE T11` + 产出清单 + `go test ./internal/auth/...` 结果 + 遗留风险
