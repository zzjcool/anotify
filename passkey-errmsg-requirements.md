# 修复：Passkey 登录失败文案误导（用户名一样却提示「用户不存在」）

> 协调者定（改动边界清晰，无需起独立 pm agent）。事实源：本文件 + 下述 scout 侦察 context。
> worktree：`.pi-orchestrator/worktrees/wt-passkey-errmsg`，分支 `fix/passkey-login-error-message`（off main cb7ae34）。

## 1. 背景与现象

用户报告：登录时使用了一个清理之前的「过期 Passkey」（认证器里残留的 resident credential，
对应用户已被删除/重建，但 username 相同），系统提示「用户不存在」，而不是提示「Passkey 不对」。

根因（已由协调者在 `internal/auth/webauthn.go` 确认）：

1. **登录失败文案统一是「用户不存在」**，三处：
   - `BeginLogin`：`GetUserByUsername` 失败 → `errors.New("auth: 用户不存在")`
   - `FinishLogin`：`GetUserByUsername` 失败 → `errors.New("auth: 用户不存在")`
   - `FinishDiscoverableLogin` 的 handler：`GetUserByID(userHandle)` 失败 → `errors.New("auth: 用户不存在")`

2. **可发现登录按 userHandle（内部 user ID）查用户**，不是按 username。
   Passkey 在认证器里存的是创建那一刻的 `store.NewUserID()`。用户被删/重建后 username 相同但 user ID 变了，
   认证器里的旧 Passkey 带着「已不存在的 userHandle」回来 → `GetUserByID` 查不到 → 报「用户不存在」。
   **文案掩盖了真正失败原因（凭证孤儿），误导用户以为是「没注册过」。**

3. **信息泄露**：`BeginLogin` 对「用户不存在」直接报明文，承认 username 是否在库——可被枚举用户名。
   标准 WebAuthn 实现通常对未知用户返回假 challenge 以防枚举。

## 2. 目标

- **不再误导**：Passkey 校验失败（含凭证孤儿/userHandle 失配）时，提示「Passkey 校验失败/凭证无效」，
  不再说「用户不存在」——除非确实是「指定用户名登录」且该用户名从未注册（这种情况提示「用户不存在」是合理的）。
- **可发现登录诊断**：userHandle 失配时打结构化日志（`event: auth.login.stale_user_handle`），
  方便排查「认证器里残留过期 Passkey」——这正是用户踩的坑。
- **减少枚举**（次级，不破坏现有契约）：指定用户名登录 begin 阶段，对未知用户不再明文泄露「用户不存在」到前端，
  改为返回中性错误（具体见 §3，需 scout 确认前端是否能配合）。

## 3. 范围与边界

### 改
- `internal/auth/webauthn.go`：三处登录失败文案 + discoverable login 加诊断日志。
- `internal/server/auth_handlers.go`：登录失败响应文案（如需前端区分，用错误码而非裸文案）。
- 前端 `web/login.html`（+ i18n）：登录失败的提示文案，按错误码显示对应中性文案。
- 相关测试：`internal/auth/auth_test.go`、`internal/server/passkeys_test.go` 或 e2e 登录套件
  （scout 确认现有覆盖后补/调整）。

### 不改（明确排除）
- 不改 Passkey 注册流程文案。
- 不改「管理 Passkey」页（删凭证）逻辑——孤儿 Passkey 的清理仍需用户手动在系统删，
  仅在登录失败时给出更有用的提示。后续可单独立项做「检测孤儿凭证」功能，本次不做。
- 不改用户表结构、不改 user ID 生成方式。

## 4. 验收标准

1. **可发现登录 + 过期 Passkey**：用一个已删除用户残留的 Passkey 走可发现登录，
   - 前端提示为「Passkey 校验失败 / 凭证无效」（中性，不提「用户不存在」）；
   - 服务端日志含 `event: auth.login.stale_user_handle`（slog Warn 级，含 userHandle 前 8 字符，**不含完整 handle 也不含 username**，避免日志泄露）。
2. **可发现登录 + 正常 Passkey**：登录成功，行为不变（回归）。
3. **指定用户名登录 + 用户名从未注册**：提示「用户不存在」（合理，保留——这种情况确实是用户名错）。
4. **指定用户名登录 + 用户存在但 Passkey 不对**：提示「Passkey 校验失败」，不说「用户不存在」。
5. **指定用户名登录 begin（用户名从未注册）**：不再明文把「用户不存在」回给前端 begin 阶段
   （具体方案见 plan，优先返回中性文案 + 前端仍能走完 begin/finish 流程不崩）。
6. `make e2e` 全绿（含 i18n_coverage 61 断言不回归、登录相关套件不回归）。
7. 四语言（zh/en/ja/es）文案齐全，无中文残留。

## 5. 风险

- **begin 阶段防枚举的复杂度**：若 begin 对未知用户返回假 challenge，需保证 finish 阶段也能优雅失败。
  若复杂度高，本次可降级为「begin 仍报用户不存在，但 finish 阶段统一中性文案」——先保证不误导，防枚举留后续。
  （由 plan 阶段决定，pm 不预设实现。）
- **前端错误码体系**：若前端目前靠匹配文案字符串判断错误类型，改文案会破坏前端逻辑。
  需 scout 确认前端登录失败处理方式，决定是引入错误码还是仅改文案。
