# 实施计划：Passkey 登录失败文案修复

> 事实源：本文件 + passkey-errmsg-requirements.md + scout-context.md。
> 基于 scout 侦察结论定方案。worktree：`.pi-orchestrator/worktrees/wt-passkey-errmsg`。

## 0. 关键决策（基于 scout 结论）

1. **不引入错误码体系**。前端纯透传 `data.error`，不判错误类型；引入 `code` 字段是新增能力，本次需求只要求「不再误导」，最小改动 = 改服务端文案即可。前端 `login_failed` 模板 `登录失败：{msg}` 会自动显示新文案。
2. **三处「用户不存在」按场景分流**，用 `errors.Is(err, store.ErrNotFound)` 区分「从未注册」vs「其他 DB 错误」：
   - **BeginLogin / FinishLogin（指定用户名登录）**：用户名从未注册 → 保留「用户不存在」（合理，确实用户名错）；其他 DB 错误 → 「登录失败，请稍后重试」。
   - **FinishDiscoverableLogin（可发现登录）**：userHandle 查不到（=凭证孤儿/过期 Passkey）→ **报「Passkey 校验失败，该凭证未关联到任何账户」（不再说用户不存在）** + 打诊断日志 `auth.login.stale_user_handle`。这是用户踩的坑的真正修复点。
3. **防枚举降级**：需求 §5 提到 begin 阶段防枚举。scout 确认前端无指定用户名登录 UI，BeginLogin 只有 e2e/API 走，普通用户不走。**本次不做假 challenge**（复杂度高、收益低、无 UI 触发），留后续。BeginLogin 对未注册用户仍返回「用户不存在」——这在「指定用户名」场景是真实反馈，不算误导。

## 1. 后端改动（anotify-worker）

### 1.1 `internal/auth/webauthn.go`

**BeginLogin（298-316）**：区分 ErrNotFound vs 其他。
```go
user, err := s.db.GetUserByUsername(username)
if err != nil {
    if errors.Is(err, store.ErrNotFound) {
        return nil, errors.New("auth: 用户不存在")  // 保留：用户名从未注册，真实反馈
    }
    return nil, fmt.Errorf("auth: 登录失败，请稍后重试: %w", err)
}
```

**FinishLogin（319-342）**：同 BeginLogin 的分流逻辑（用户名从未注册 → 用户不存在；Passkey 校验失败由 webauthn 库自身报错，已有 `fmt.Errorf("finish login: %w", err)`，保持不变）。

**FinishDiscoverableLogin handler（356-377）**：这是核心修复。
```go
handler := func(rawID, userHandle []byte) (webauthn.User, error) {
    user, err := s.db.GetUserByID(string(userHandle))
    if err != nil {
        // userHandle 失配 = 认证器里残留过期/孤儿 Passkey（用户已被删/重建，username 可能相同但 user ID 变了）
        slog.Warn("discoverable login: stale user handle (orphan passkey)",
            "event", "auth.login.stale_user_handle",
            "user_handle_prefix", userHandlePrefix(userHandle),  // 前 8 字符，不含完整 handle/username
        )
        return nil, errors.New("auth: 该 Passkey 未关联到任何账户，可能已失效，请尝试其他 Passkey 或重新注册")
    }
    return s.loadWebAuthnUser(user)
}
```
新增 helper `userHandlePrefix(b []byte) string`：返回 hex 前 8 字符（如 `userHandlePrefix` 为空时返回 `""`）。

**注意**：webauthn.go 顶部已 `import "log/slog"`（第8行），无需新增 import。`errors` 已导入。需确认 `store` 已导入（第15行有 `github.com/anotify/anotify/internal/store`）✓。

### 1.2 `internal/server/auth_handlers.go`

**不动**。`loginOptions`（117-119）和 `login`（140-145）直接 `writeErr(w, 4xx, err.Error())` 透传，新文案自动到前端。`auth.login.fail` 日志已含 `error: err.Error()`，足够，不重复加日志。

### 1.3 `internal/server/handlers.go`

**不动**。`writeErr` 保持只 `{"error": msg}`，不引入 code 字段（决策0.1）。

## 2. 前端改动（anotify-frontend）

### 2.1 `web-src/pages/login.html`

**不改 catch 逻辑**。`login_failed` 模板 `登录失败：{msg}` 会自动透传新文案。
但可改进：可发现登录失败时，文案「该 Passkey 未关联到任何账户…」较长，确认 `showStatus` 的 error 样式能容纳多行文本（scout 未报溢出问题，验收时 web_verify 确认）。

### 2.2 `web-src/locales/*.yaml`（四语言）

`login.status.login_failed` 模板本身不变（仍 `登录失败：{msg}`），因为新文案是服务端给的。**但需新增一个更友好的 fallback**？——不需要，决策是不引入错误码。保持现状。

**结论：前端本次可能零改动**（scout 确认透传链路无需改）。frontend 仅做 web_verify 验收 + 确认四语言 login 页无回归。

## 3. 测试改动（anotify-tester / worker 一并）

### 3.1 `internal/auth/auth_test.go`

新增：
- `TestFinishDiscoverableLogin_StaleUserHandle`：用一个不存在的 userHandle 模拟孤儿凭证，断言返回错误文案含「未关联到任何账户」或类似，且不报「用户不存在」。
- `TestBeginLogin_UnknownUser`（已存在 423-426，只查 err != nil）：补一条断言文案含「用户不存在」（确认未注册场景文案保留）。
- `TestBeginLogin_DBError`（可选）：mock 或注入 DB 错误，断言返回「登录失败，请稍后重试」。若难 mock 可跳过。

### 3.2 `scripts/e2e/suites/auth_flow.mjs`

- 现有 `webauthnLogin("nonexistent_user_xyz")` 断言 `!== 200`（311-315）：**保持**，状态码断言不受文案影响。
- **不新增** discoverable userHandle 失配 e2e（构造孤儿凭证需真实认证器，e2e 用虚拟 WebAuthn 难造，留单测覆盖即可）。

## 4. 执行顺序

1. worker 改 `internal/auth/webauthn.go`（三处分流 + helper + 日志）+ `internal/auth/auth_test.go` 补单测 → `go test ./internal/auth/...` 绿。
2. frontend 跑 `make fe`（sitegen）+ `web_verify` 登录页无回归 + 确认四语言 login 页渲染。
3. tester 跑 `make e2e` 全绿（含 i18n_coverage 61 断言）。
4. reviewer 终审对照需求 §4 验收。

## 5. 验收对照（需求 §4）

| # | 验收 | 实现点 |
|---|------|--------|
|1|可发现+过期Passkey → 中性提示 + stale_user_handle 日志|webauthn.go handler 364 处分流+日志|
|2|可发现+正常Passkey → 成功|回归，不改|
|3|指定用户名+未注册 → 用户不存在|BeginLogin/FinishLogin 保留 ErrNotFound 分支|
|4|指定用户名+存在但Passkey不对 → Passkey校验失败|webauthn 库自身报错已透传（finish login: %w）|
|5|begin阶段防枚举|降级不做（见决策0.3）|
|6|make e2e 全绿|tester 验证|
|7|四语言无中文残留|i18n_coverage 套件验证|

**验收5降级说明**：scout 确认前端无指定用户名登录 UI，BeginLogin 仅 e2e/API 可达，防枚举收益低，留后续。需求 §5 已预留降级空间。
