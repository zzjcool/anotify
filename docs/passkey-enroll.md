# passkey-enroll · 新设备补建 Passkey 的时序

> 行为判据是代码、测试与 `api/auth.yaml` 里 passkey-enroll 端点族的描述；
> 本文只画流程与「谁有权做什么」，供 review 与排障对照。

## 参与者与凭证

- **旧设备** = 发起者：持 `anotify_session` cookie，`user_id` 在**建会话时**写入。
- **新设备** = 匿名：只能凭短码/二维码找到会话。
- `secret`：`knock` 返回，仅给新设备，`poll` 时作为门控。
- `registrationToken`：`poll` 在 `approved` 时返回的一次性 token，`complete` 时校验。

## 时序（当前实现）

```text
旧设备(发起者, cookie)         服务端                        新设备(匿名)
        │                       │                              │
        │ POST /sessions        │ 建会话：user_id = 发起者      │
        │──────────────────────▶│ status=pending，短 TTL       │
        │◀──────────────────────│ {sessionId, secret, userCode, authUrl}
        │                       │                              │
        │                       │◀────── GET by-code?code= ────│ 输短码 / 扫码
        │                       │──────▶ {status, initiatorName, deviceHint?}
        │                       │◀────── POST /{id}/request ───│ 敲门（deviceHint 自报→裁剪）
        │                       │ pending→requested 只接受一次 │
        │                       │──────▶ {secret} ─────────────│
        │ GET /{id}/watch       │                              │
        │──────────────────────▶│ 属主校验（先于惰性过期）      │
        │◀──────────────────────│ {status:requested, deviceHint}
        │ POST /{id}/approve    │                              │
        │        或 /deny       │ 属主 → kind → 过期 → 状态     │
        │──────────────────────▶│                              │
        │◀──────────────────────│ {status:approved|denied}     │
        │                       │◀────── GET /poll?secret= ────│
        │                       │──────▶ {attestationOptions, registrationToken, initiatorName}
        │                       │◀────── POST /complete ───────│ 一次性 token + attestation
        │                       │ approved→consumed，写 Passkey（归属发起者）
        │ GET /{id}/watch       │                              │
        │◀──────────────────────│ {status:consumed}            │
```

## 谁有权做什么（#625 的核心修复）

| 端点 | 属主校验 | 非发起者 | 说明 |
| --- | --- | --- | --- |
| `watch` | 发起者 | `404` | 属主校验先于惰性过期，非发起者不替别人推进状态机 |
| `approve` | 发起者 | `404` | 批准**不写** `user_id` |
| `deny` | 发起者 | `404` | 否则任何登录用户都能拒掉别人的授权 |
| `DELETE`（取消） | 发起者 | `404` | 属主校验先于 kind guard |

- `user_id` 只有一义：**建会话时写入的发起者**。改动前 `approve` 会把它覆盖成批准者，
  于是新设备被绑进批准者的账号、发起者再 `watch` 反而 `404`。
- 校验顺序固定为 **属主 → kind → 惰性过期 → 状态**：非发起者即使拿到已过期或
  apikey-kind 会话的 id，看到的也是 `404`（不是 `409` / `400`，少一层存在性泄露）；
  发起者操作过期会话才是 `409`，操作 apikey-kind 会话是 `400`。
- `user_id` 为空（cli-auth 的匿名会话）时，任何登录用户都不是发起者 → `404`。
- 新设备侧只能拿到「给哪个账号加设备」的 `initiatorName` 与自报的 `deviceHint`；
  它不参与任何授权判断。

## 尚未实现（#625 剩余项）

- **首次匿名抢占（`pending` 窗口）**：先敲门的人拿到 `secret`，属主批准后该设备拿到的是
  给**发起者账号**的 `attestationOptions` + `registrationToken`，完成注册即等于拿到发起者的
  Passkey 登录凭证。当前只做了两件事：`requested` 之后不再接受 `knock`（`409`，且不重写
  `secret_hash`）、`deviceHint` 落库与日志前裁剪控制字符与长度。
  真正防抢占需要新设备生成一次性 nonce 放在 URL fragment（不进日志），批准页展示其前几位
  做 SAS 核对——**未实现**。
- **cli-auth 批准页的带外信息**：仍只展示 CLI 自报的 `deviceName`，没有创建时间 / 来源 IP /
  User-Agent。

## 相关

- 契约：`api/auth.yaml` 的 passkey-enroll 端点族；`api/` 是 HTTP 形状的正本。
- 测试：`internal/auth/enroll_test.go`、`internal/server/passkey_enroll_test.go`、
  `scripts/e2e/suites/passkey_enroll.mjs`。
