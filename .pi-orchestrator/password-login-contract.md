# 密码登录（第二套登录模型）· 冻结契约

> 协调者冻结。任何实现细节与此处冲突时，**以本文件为准**；确有必要变更先上报协调者，
> 不得单方面改 API / DOM / i18n key。

## 0. 目标与非目标

**目标**：在 Passkey 之外增加「用户名 + 密码」登录，形成两套并存的登录模型：
- 登录页：Passkey 一键登录（保持主路径）＋「用密码登录」表单。
- 安全页：设置 / 修改 / 移除密码。
- 后端：密码登录端点、密码管理端点、argon2id 存储、登录限速。
- 不变量：**每个账号至少保留一种登录方式**（≥1 Passkey 或已设置密码）。
  - 删除最后一个 Passkey：仅当已设置密码时允许（否则 409）。
  - 移除密码：仅当有 ≥1 Passkey 时允许（否则 409）。

**非目标**（本轮不做）：
- 用密码「注册」新账号（注册仍是 Passkey ceremony；密码在安全页设置）。
- 忘记密码的邮件/管理员重置流程（已有恢复码兜底）。
- 修改密码后强制吊销其它会话。
- 2FA / TOTP。

## 1. 数据库（migration 8）

`internal/store/schema.sql` **末尾追加**（baseline 不动），并把 `internal/store/migrate.go`
的 `headVersion` 从 7 改成 8：

```sql
-- +migration 8 user_password

-- 密码登录（第二套登录模型，与 Passkey 并存）。空串 = 未设置密码（纯 Passkey 账号）。
-- 只存 argon2id PHC 哈希（复用 auth.hashKey/verifyKey，与 api_keys / recovery_codes 同一实现），
-- 明文永不落库、不落日志。password_updated_at = 最近一次设置/修改的 unix 秒；0 = 从未设置。
ALTER TABLE users ADD COLUMN password_hash TEXT NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN password_updated_at INTEGER NOT NULL DEFAULT 0;
```

store 层（`internal/store/auth.go`）新增，**不得**把 hash 放进 `store.User`
（`/v1/auth/me`、admin 列表都会序列化 User，泄露即事故）：

```go
type UserPassword struct {
    Hash      string
    UpdatedAt int64
}

// GetUserPassword 读密码列。用户不存在 → ErrNotFound。
func (d *DB) GetUserPassword(userID string) (*UserPassword, error)
// SetUserPassword 写 hash + updatedAt（0 行受影响 → ErrNotFound）。
func (d *DB) SetUserPassword(userID, hash string, updatedAt int64) error
// ClearUserPassword 清空（hash=''、updated_at=0）。对不存在用户无副作用。
func (d *DB) ClearUserPassword(userID string) error
```

## 2. HTTP 契约（冻结）

| 方法 | 路径 | 鉴权 | body | 200 响应 |
| --- | --- | --- | --- | --- |
| POST | `/v1/auth/password/login` | 匿名（限速） | `{"username","password"}` | `{"ok":true}` + Set-Cookie |
| GET | `/v1/auth/password` | 会话 Cookie | — | `{"hasPassword":bool,"updatedAt":int}` |
| POST | `/v1/auth/password` | 会话 Cookie | `{"currentPassword","newPassword"}` | `{"ok":true,"hasPassword":true,"updatedAt":int}` |
| DELETE | `/v1/auth/password` | 会话 Cookie | `{"currentPassword"}` | `{"ok":true,"hasPassword":false}` |

约定：
- 字段全部 camelCase；错误统一 `{"error":"中文文案"}`（`writeErr`）。
- 登录错误码：缺字段 400；凭据错/未设置密码/账户禁用 401；限速 429（`Retry-After`，
  走 `rateLimited`）。
- 设置/修改：未登录 401；新密码不合规或当前密码错 400；成功 200。
- 移除：未登录 401；当前密码错 400；无 Passkey → **409**（`ErrLastAuthMethod`）。
- 匿名登录限速：IP 10/min + 用户名（小写）10/min，两维都过才放行。
  常量命名：`rlPasswordLoginIPPerMin = 10`、`rlPasswordLoginUserPerMin = 10`，
  包级 `rlPasswordLoginIP` / `rlPasswordLoginUser`。
- 路由注册（`internal/server/mux.go`，必须显式注册，保证语义化状态码）：
  - `mux.Handle("/v1/auth/password/login", noStore(http.HandlerFunc(authH.passwordLogin)))`
  - `mux.Handle("/v1/auth/password", noStore(sessMW(http.HandlerFunc(authH.password))))`
- 日志事件（不得记录明文/哈希）：
  `auth.password.login_success` / `auth.password.login_fail`（含 ip）/
  `auth.password.set`（含 user_id、created=bool）/ `auth.password.removed`（含 user_id）。

## 3. 认证服务语义（`internal/auth/password.go`）

```go
const PasswordMinLen = 8   // 按 rune 计
const PasswordMaxLen = 128 // 按 rune 计

var (
    ErrPasswordNotSet     = errors.New("auth: 该账户未设置密码")
    ErrInvalidCredentials = errors.New("auth: 用户名或密码错误")
    ErrLastAuthMethod     = errors.New("auth: 至少保留一种登录方式：请先添加 Passkey 再移除密码")
)

type PasswordStatus struct {
    HasPassword bool
    UpdatedAt   int64
}

func ValidatePassword(pw string) error
func (s *Service) PasswordStatus(userID string) (*PasswordStatus, error)
func (s *Service) SetPassword(userID, currentPassword, newPassword string) (*PasswordStatus, error)
func (s *Service) RemovePassword(userID, currentPassword string) error
func (s *Service) LoginWithPassword(username, password, deviceName string) (sessionID string, err error)
```

细则：
- `ValidatePassword`：rune 数 8–128；全空白拒绝；错误文案中文
  （`密码至少 8 个字符` / `密码最多 128 个字符` / `密码不能全为空白`）。
- `SetPassword`：用户不存在 → `用户不存在`；已有密码时**必须**校验
  `currentPassword`，错误 → `当前密码不正确`（措辞固定，前端直接展示）；
  无密码时忽略 `currentPassword`。校验新密码 → `hashKey` → `SetUserPassword(now)`。
  返回最新状态。**不吊销已有会话**（本轮契约）。
- `RemovePassword`：用户不存在 → `用户不存在`；未设置 → 幂等成功
  （`hasPassword=false`）；已设置时校验 `currentPassword`；
  `ListPasskeysByUser` 为空 → `ErrLastAuthMethod`；然后 `ClearUserPassword`。
- `LoginWithPassword`：
  - 空 username/password → `ErrInvalidCredentials`；`ValidateUsername` 失败同样。
  - 查无用户 / 查库出错：**先对一个进程内惰性预计算的 dummy argon2id 哈希做一次
    `verifyKey`**（抹平计时差），再返回 `ErrInvalidCredentials`。dummy 用
    `sync.Once` + `hashKey("anotify-dummy-password")`，失败时降级为不校验但不 panic。
  - `user.Disabled` → `errors.New("auth: 账户已被禁用")`。
  - 未设置密码 → `ErrPasswordNotSet`（文案：`该账户未设置密码，请使用 Passkey 登录`）。
  - `verifyKey` 失败 / 哈希格式错 → `ErrInvalidCredentials`。
  - 成功 → `s.sess.Create(user.ID, deviceName)`；deviceName 为空时用
    `DeviceNameFromUA("")` 的既有行为（直接透传即可）。返回 sess.ID。

## 4. Passkey 删除护栏改造（`internal/server/auth_handlers.go`）

`deletePasskey`：`len(list) <= 1` 时不再无条件 409，而是查 `PasswordStatus(uid)`：
- 已设置密码 → 允许删除（200）。
- 未设置密码 → 409，文案改为：
  `至少保留一种登录方式：请先设置密码，再删除最后一个 Passkey。`

## 5. Go 测试（必须新增/覆盖）

- `internal/store/password_test.go`：新用户默认 `("",0)`；set → get 往返一致
  （hash 原文 + updatedAt）；clear → 归零；set 不存在用户 → `ErrNotFound`。
- `internal/auth/password_test.go`：
  `ValidatePassword` 边界（7/8/128/129、全空白、中文 rune）；
  set→status→login 成功（会话可 `Validate`）；登录失败（错密码/无密码账号/禁用账号/
  不存在用户）错误映射；改密码需当前密码（错则失败、对则成功且旧密码失效）；
  remove：无 Passkey → `ErrLastAuthMethod`、有 Passkey → 成功、幂等。
  测试内用 `newTestService` / `createUser`（recovery_test.go 已有）与
  `svc.db.CreatePasskey(&store.Passkey{...})`。
- `internal/server/password_test.go`：按 `passkeys_test.go` 的 env 装配方式：
  - 未登录 GET/POST/DELETE `/v1/auth/password` → 401；
  - 登录：错密码 401、无密码账号 401、正确密码 200 + Cookie 可访问 `/v1/auth/me`；
  - 设置密码后 `hasPassword=true`；改密码旧密码登录 401、新密码 200；
  - 移除密码：无 Passkey → 409；有 Passkey → 200；
  - 登录限速：同一 username 连续 11 次错误 → 第 11 次 429（测试包内可直接重置
    `rlPasswordLoginIP/rlPasswordLoginUser` 为新窗口，避免与其它用例串扰）；
  - `deletePasskey`：设置密码后删除唯一 Passkey → 200；未设置 → 409。

## 6. 前端（冻结 DOM / i18n）

### 6.1 登录页 `web-src/pages/login.html`

在 `#login-panel` 的 Passkey 按钮**之后**加入（Passkey 仍是主路径）：

```html
<div class="auth-or"><span>{{t "login.password.or"}}</span></div>
<button type="button" id="password-login-toggle" class="btn btn-ghost auth-submit" aria-expanded="false" aria-controls="password-panel">
  {{t "login.password.toggle"}}
</button>
<form id="password-panel" class="auth-password hidden" novalidate>
  <div class="field">
    <label class="field-label" for="password-username">{{t "login.password.username_label"}}</label>
    <input id="password-username" class="input" type="text" autocomplete="username" autocapitalize="none" autocorrect="off" spellcheck="false" maxlength="32" placeholder="{{t "login.password.username_placeholder"}}" />
  </div>
  <div class="field">
    <label class="field-label" for="password-password">{{t "login.password.password_label"}}</label>
    <input id="password-password" class="input" type="password" autocomplete="current-password" placeholder="{{t "login.password.password_placeholder"}}" />
  </div>
  <button type="submit" id="password-login-btn" class="btn btn-primary auth-submit">
    {{t "login.password.button"}}
  </button>
  <p class="field-hint">{{t "login.password.hint"}}</p>
</form>
```

行为：
- 点 `#password-login-toggle`：切换 `#password-panel.hidden`；`aria-expanded` 同步；
  按钮文字在 `toggle` / `toggle_hide` 间切换；展开后 focus `#password-username`；
  收起后 focus 回 Passkey 按钮。
- 提交：两字段 trim 后任一为空 → `showStatus(t("login.password.err_empty"), "warn")`；
  否 POST `/v1/auth/password/login`（`credentials:"include"`）。
  成功：`showStatus(t("login.password.success"), "success")` → `navigate(nextTarget())`。
  失败：429 → `err_rate_limited`；401/其它 → `err_failed` 模板（`{msg}` 用服务端 error 或
  `HTTP <status>`，与恢复码表单一致）。提交期间按钮 disabled + `busy` 文案，结束后恢复。
- 不新增依赖、不引入自定义未定义类；`.auth-or` / `.auth-password` 必须写进本页 `<style>`，
  颜色只用 tokens。不要改 `#login-btn`（Passkey）与 tab 结构（e2e 锚点）。

### 6.2 安全页 `web-src/pages/security.html`

在「① Passkey 凭证」与「② 登录会话」之间新增「密码登录」区块，复用现有
`.sec-sec-head` / `.sec-sec-title` / `.sec-sec-sub` / `.card` / `.btn` / `.modal-*`：

- `#password-status`：`未设置` / `已设置`（已设置时第二行/小字显示
  `security.password.updated_at` 模板，日期 `YYYY-MM-DD`）。
- `#password-edit-btn`：未设置 → `设置密码`；已设置 → `修改密码`。
- `#password-remove-btn`：未设置时带 `hidden`。
- 弹层 `#password-modal`（`class="modal-backdrop hidden"`，`role="dialog"`，`aria-modal="true"`）：
  - `#password-modal-title`（设置/修改/移除三态文案）
  - `#pwd-current-field`（`.field`，仅修改/移除时显示）→ `#pwd-current`
  - `#pwd-new-field`（仅设置/修改）→ `#pwd-new`
  - `#pwd-confirm-field`（仅设置/修改）→ `#pwd-confirm`
  - `#password-modal-error`（`.field-error hidden`，`role="alert"`）
  - `#password-modal-submit`、`#password-modal-cancel`
- 行为：
  - 载入时 GET `/v1/auth/password` → 渲染状态；失败按现有失败态处理（不伪造）。
  - 设置/修改：POST（`{currentPassword,newPassword}`；设置时 current 省略）→ 成功 toast
    `security.password.toast.saved`、刷新状态。失败把错误写进
    `#password-modal-error`。
  - 移除：DELETE（`{currentPassword}`）→ 成功 toast `toast.removed`；409 显示服务端文案
    （没有 Passkey 时的护栏）。
  - 前端校验：新密码 8–128（按字符）、两次输入一致；不满足就地报错、不发请求。
  - 弹层用 `Anotify.openModal` / `Anotify.closeModal`；点遮罩、Esc、取消都能关闭
    （Esc 与页面已有的 enroll-modal Esc 处理合并，不要互相抢）。
- 页面 `security.intro` / `security.subtitle` 更新为四件套（Passkey · 密码 · 会话 · 恢复码）。

### 6.3 i18n（四语齐全：zh-CN / en / ja / es）

`make sitegen` 缺 key 会 WARN 并回退默认语言 —— 本任务要求**零 WARN**，四语都要填，
英文/日文/西语要地道，不是机翻。新增 key（值自拟，语义按注释）：

- `login.password.or`（分隔词「或」）
- `login.password.toggle` / `.toggle_hide`
- `login.password.username_label` / `.username_placeholder`
- `login.password.password_label` / `.password_placeholder`
- `login.password.button` / `.busy`
- `login.password.hint`
- `login.password.err_empty` / `.err_failed`（含 `{msg}`）/ `.err_rate_limited`
- `login.password.success`
- `security.password.title` / `.subtitle` / `.status_set` / `.status_unset`
- `security.password.updated_at`（含 `{date}`）/ `.never`
- `security.password.set_button` / `.change_button` / `.remove_button`
- `security.password.modal.set_title` / `.change_title` / `.remove_title`
- `security.password.modal.current_label` / `.new_label` / `.confirm_label`
- `security.password.modal.save` / `.remove_submit` / `.cancel`
- `security.password.modal.hint`（8–128 位说明）
- `security.password.err_empty` / `.err_short` / `.err_long` / `.err_mismatch`
- `security.password.toast.saved` / `.removed` / `.failed`（含 `{msg}`）
- `security.password.confirm.remove`（二次确认文案）

不许删除/改名任何既有 key。

## 7. E2E 冻结锚点（供 e2e 套件使用）

- 登录页：`#login-panel`、`#login-btn`、`#password-login-toggle`、`#password-panel`、
  `#password-username`、`#password-password`、`#password-login-btn`、`#status-box`。
- 安全页：`#password-status`、`#password-edit-btn`、`#password-remove-btn`、
  `#password-modal`、`#pwd-current`、`#pwd-new`、`#pwd-confirm`、
  `#password-modal-submit`、`#password-modal-error`、`#toast`。
- API：见 §2；e2e 用 `H.req(base, "/v1/auth/password", { session })` 断言状态码/JSON。

## 8. 文件所有权（并行时不得越界）

**be-password（后端）**：
`internal/store/schema.sql`、`internal/store/migrate.go`、`internal/store/auth.go`、
`internal/store/password_test.go`、`internal/auth/password.go`、
`internal/auth/password_test.go`、`internal/server/auth_handlers.go`、
`internal/server/mux.go`、`internal/server/ratelimit.go`、
`internal/server/password_test.go`、`api/openapi.yaml`。

**fe-password（前端）**：
`web-src/pages/login.html`、`web-src/pages/security.html`、
`web-src/locales/{zh-CN,en,ja,es}.yaml`。

**coordinator**：`scripts/e2e/**`、`DEVELOPMENT.md`、`README*`、`.pi-orchestrator/**`、
最终合并与全量验证。**worker 不要改 `web/`、`internal/server/dist/`（生成物）**，
也不要 commit。

## 9. 验证命令

后端：
```bash
export GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto
gofmt -l internal/store internal/auth internal/server
go vet ./internal/store ./internal/auth ./internal/server
go test ./internal/store/ ./internal/auth/ ./internal/server/
python3 scripts/check_openapi.py
```

前端：
```bash
node scripts/check-classes.mjs
make sitegen   # 必须 0 WARN（缺 key 会打 warning）
make fe
```

全量（协调者）：`make ci`、`make e2e`、`web_verify` dark/light 截图。

---

# 追加：用密码注册（第二套注册方式）

> 首轮已交付「密码登录 + Passkey 注册」。本轮让注册也能选「用户名 + 密码」，
> 与 Passkey 注册并存。**不改变首轮任何已冻结项**。

## A1. 目标与不变量

- 注册两种方式：Passkey ceremony（现有）**或** 用户名 + 密码（新增）。
- 首个注册用户（无论哪种方式）自动成为 admin —— 判定仍必须发生在
  `store.RegisterUser*` 的**同一个事务**里（`RoleAuto`），不得先查后写。
- 密码注册必须**原子**：用户行 + 默认命名空间 + 内置能力播种 + 密码哈希，
  一个事务全成或全不成（不能留下「有账号但没有任何登录方式」的半成品）。
- 密码注册成功后同样签发 8 个一次性恢复码（明文仅此一次返回），
  前端复用现有 `#codes-modal` 展示。
- 会话与设备名语义同 Passkey 注册（`SessionManager.Create` + `DeviceNameFromUA`）。

## A2. HTTP 契约（冻结）

```
POST /v1/auth/password/register      # 匿名 + 限速
body: {"username":"...","password":"...","displayName":"可选"}
200:  {"ok":true,"recoveryCodes":["XXXX-XXXX-XXXX-XXXX", ...]}   + Set-Cookie
400:  {"error":"中文文案"}   # 用户名/密码不合规、用户名已存在
429:  限速（IP 10/min，带 Retry-After）
```

- 路由必须**显式注册**（与 password/login 同族）：
  `mux.Handle("/v1/auth/password/register", noStore(http.HandlerFunc(authH.passwordRegister)))`
- 限速：新增 `rlPasswordRegisterPerMin = 10` + 包级 `rlPasswordRegisterIP`；
  IP 维先于读 body（与 recovery/password login 一致）。本轮只做 IP 维
  （注册不是秘密猜测接口；username 维留给登录）。
- 日志：`auth.register.success` / `auth.register.fail`（含 ip；**不得**记密码/哈希）。
- `api/openapi.yaml` 增加该 path，`security: []`。

## A3. 服务层（`internal/auth/password.go`）

```go
// RegisterWithPassword 用用户名 + 密码注册（不创建 Passkey）。
// 返回会话 ID 与一次性恢复码（明文仅此一次）。
func (s *Service) RegisterWithPassword(
    ctx context.Context, username, displayName, password, deviceName string,
) (sessionID string, recoveryCodes []string, err error)
```

细则：
1. `ValidateUsername(username)` → 原样返回其错误；`ValidatePassword(password)` → 原样。
2. `displayName` 为空时用 `username`。
3. 预检 `GetUserByUsername`：已存在 → `errors.New("auth: 用户名已存在")`（与
   `BeginRegister` 同文案）。
4. 先生成 `hashKey(password)`（失败直接返回，不建用户）。
5. 调 `store.RegisterUserWithPassword(ctx, u, hash, store.Now())`，`u.Role = store.RoleAuto`。
   若返回错误：再查一次用户名是否已存在（并发注册输家）→ 「用户名已存在」；
   否则原样包装返回。失败时不允许留下用户行。
6. `IssueRecoveryCodes(user.ID)`（8 个）→ 失败则返回错误（用户已建，与 Passkey
   注册的既有语义一致）。
7. `s.sess.Create(user.ID, deviceName)` 建会话，返回 sess.ID。
8. 首个用户升 admin 的日志沿用 `ensureRegisteredUser` 的
   `auth.register.first_admin` 语义（抽公共函数或复制同样的 slog，字段一致）。
   提示：`ensureRegisteredUser` 现在绑定「无密码」路径，可把它重构成
   「接受一个 register 回调」或改成调用方传入 `registerFn`；**不要**破坏
   Passkey 路径现有测试。

## A4. store 层（`internal/store/identity.go`）

```go
// RegisterUserWithPassword 与 RegisterUser 完全同语义（RoleAuto / 命名空间 /
// 内置能力播种），额外在**同一个事务**里写入密码列。
func (d *DB) RegisterUserWithPassword(
    ctx context.Context, u *User, passwordHash string, passwordUpdatedAt int64,
) (*Namespace, error)
```

实现方式：把 `RegisterUser` 的主体抽成内部 `registerUser(ctx, u, passwordHash, passwordUpdatedAt)`，
两个导出函数都调它；INSERT 的列增加 `password_hash, password_updated_at`。
`RegisterUser` 传 `("", 0)`，行为与列默认值完全一致（现有单测必须继续绿）。

## A5. 前端（`web-src/pages/login.html` + 四语 locale）

在 `#signup-panel` 内、现有 `#signup-btn`（创建 Passkey 并注册）**之后**追加：

```html
<div class="auth-or"><span>{{t "login.signup.password.or"}}</span></div>
<button type="button" id="password-signup-toggle" class="btn btn-ghost auth-submit"
        aria-expanded="false" aria-controls="password-signup-panel">
  {{t "login.signup.password.toggle"}}
</button>
<form id="password-signup-panel" class="auth-password hidden" novalidate>
  <div class="field">
    <label class="field-label" for="signup-password">{{t "login.signup.password.password_label"}}</label>
    <input id="signup-password" class="input" type="password" autocomplete="new-password"
           placeholder="{{t "login.signup.password.password_placeholder"}}" />
  </div>
  <div class="field">
    <label class="field-label" for="signup-password-confirm">{{t "login.signup.password.confirm_label"}}</label>
    <input id="signup-password-confirm" class="input" type="password" autocomplete="new-password"
           placeholder="{{t "login.signup.password.confirm_placeholder"}}" />
  </div>
  <button type="submit" id="password-signup-btn" class="btn btn-primary auth-submit">
    {{t "login.signup.password.button"}}
  </button>
  <p class="field-hint">{{t "login.signup.password.hint"}}</p>
</form>
```

行为：
- toggle 切换 `.hidden` / `aria-expanded` / 文案（toggle ↔ toggle_hide），展开后聚焦
  `#signup-password`，收起后聚焦 `#signup-btn`（与登录页的密码 toggle 同款）。
- 提交：先 `refreshUsername(true)` 校验用户名；密码按码点 8–128；两次一致。
  不满足就地报错（不发请求）。
- POST `/v1/auth/password/register` body `{username, displayName: username, password}`。
  成功：`recoveryCodes` 非空 → 复用 `showCodesPanel(codes, () => navigate(nextTarget()))`；
  为空 → `showStatus(success)` + `navigate(nextTarget(), 600)`。
  失败：429 → `login.signup.password.err_rate_limited`（warn）；其它 →
  `login.status.signup_failed` 模板（error，`{msg}` 用服务端 error）。
- 请求期间按钮 disabled + `busy` 文案，结束恢复。
- 全局 Enter 处理器：在 `#password-signup-panel` 内部的回车交给表单原生 submit，
  不得再触发一次 Passkey 注册 ceremony（与密码登录同一处守卫，写法一致）。
- **不改** `#new-username` / `#signup-btn` / `#username-hint` / `#username-error` /
  `#codes-modal` / `showCodesPanel` 的契约与行为；`.auth-or` / `.auth-password` 复用
  已定义的页内样式。
- 密码注册**不要求** WebAuthn 支持（无 Passkey 的浏览器也能注册）。

i18n（四语：zh-CN / en / ja / es，全部非空）：
`login.signup.password.or` / `.toggle` / `.toggle_hide` / `.password_label` /
`.password_placeholder` / `.confirm_label` / `.confirm_placeholder` / `.button` /
`.busy` / `.hint` / `.err_empty` / `.err_short` / `.err_long` / `.err_mismatch` /
`.err_rate_limited` / `.success`。

## A6. 测试要求

- store：`RegisterUserWithPassword` 往返（用户 + 密码列 + 命名空间 + 内置能力）、
  Passkey 路径 `RegisterUser` 仍写空密码、并发首注册仍恰好一个 admin。
- auth：`RegisterWithPassword` 成功（可立即用密码登录、会话可 `Validate`、8 个恢复码、
  密码只存哈希）；重复用户名 / 弱密码 / 非法用户名错误；首个用户 admin、第二个 member。
- server：端点 200 + Cookie + `recoveryCodes` 长度 8、重复用户名 400、短密码 400、
  限速第 11 次 429、注册后 `GET /v1/auth/password` `hasPassword=true` 且无 Passkey。
- e2e（`scripts/e2e/suites/password_login.mjs`，由协调者追加）：
  登录页切注册 tab → 用密码注册新用户 → `#codes-modal` 出现且 8 条码 → 继续进入工作台
  → `/v1/auth/me` 用户名正确、`/v1/auth/passkeys` 为 `[]`、密码状态 true
  → 登出后密码登录该账号 200；重复用户名注册 400。
