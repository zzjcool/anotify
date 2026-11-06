# W7 / N1 · 并发首注册双 admin（安全修复）

- 提交：`ab37f77` fix(auth): 首用户判定移进 RegisterUser 同一事务，修并发首注册双 admin
- 分支/PR：`power` → PR #309 https://github.com/zzjcool/anotify/pull/309 （已含本提交）
- 改动文件：`internal/auth/webauthn.go`、`internal/auth/first_admin_test.go`（新）、`internal/store/identity.go`

## 1. 根因

`FinishRegister` 里的「首个用户 → admin」判定是**两次独立的连接池操作**：
`db.UserCount()` 一次，随后的 `db.RegisterUser()` 又一次，中间没有事务。
webauthn.go:219-220 的注释把安全性归于「SQLite 单写（SetMaxOpenConns(1)）使两者串行化」——
那是 #240 之前的事实。`store.go:38 defaultMaxOpenConns = 8` 之后连接池放开，
两个并发「首个注册」都能读到 `count == 0`，于是双双拿到 `RoleAdmin`
（凭空多出一个互不知情的超管账户）。

## 2. 修法

- `store`：新增入参专用值 `RoleAuto`（永不落库）。`RegisterUser` 在自己的
  `BEGIN IMMEDIATE` 事务里 `SELECT COUNT(*) FROM users`（读走 `tx`，同一快照）定角色：
  0 ⇒ `RoleAdmin`，否则 `RoleMember`。后一个注册者的 `BeginTx` 必须等前一个提交，
  因此它必然读到已提交的那一行 ⇒ 只可能有一个 admin（跨进程也成立）。
- `auth`：删掉先查后写与那条假注释，改传 `store.RoleAuto`；把落库步骤抽成
  `ensureRegisteredUser(ctx, u)`，让单测能直接驱动角色判定（真 `FinishRegister`
  需要构造可验证 attestation，写不出并发用例）。日志与错误包装语义不变
  （`user.Role` 由 `RegisterUser` 回填，`auth.register.first_admin` 仍只在真正提权时打）。

## 3. 测试

`internal/auth/first_admin_test.go`：

| 用例 | 作用 |
|---|---|
| `TestRegisterRole_FirstUserAdminSecondMember` | 顺序注册的确定性底座：第一个 admin、第二个 member、已存在用户复用不改角色 |
| `TestConcurrentFirstRegister_ExactlyOneAdmin` | 回归护栏：**文件库** + 起跑门 + 20 轮全新库，断言恰好一个 admin、库里 admin 计数 = 1、两个用户都落库 |

必须文件库：`:memory:` 被 `openWithOptions` 强制压到单连接（`store.go`），池操作全被串行化，
竞态测不出来（会永远绿）。

### 修复前（红）

单轮命中率约 1/16（20 轮/次）。

```
--- FAIL: TestConcurrentFirstRegister_ExactlyOneAdmin (5.06s)
    first_admin_test.go:169: round 13: 并发首注册产生 admin 2 个（库里 2 个），应恰好 1 个 ——
    「首个用户」判定必须与 INSERT 在同一事务内完成，否则两个注册者都会看到 count==0
FAIL	github.com/zzjcool/anotify/internal/auth	5.417s
```

连跑 3 次全红：round 0 / round 15 / round 16 分别命中（日志里能看到同轮两个
`event=auth.register.first_admin`）。只把 `ensureRegisteredUser` 里的
`u.Role = store.RoleAuto` 换回旧的「UserCount 后赋角色」两块即可复现（round 0 即红）：

```
--- FAIL: TestConcurrentFirstRegister_ExactlyOneAdmin (0.05s)
    first_admin_test.go:169: round 0: 并发首注册产生 admin 2 个（库里 2 个），应恰好 1 个 —— ...
```

### 修复后（绿）

```
$ go test ./internal/auth/... -race -count=1
ok  	github.com/zzjcool/anotify/internal/auth	68.994s
```

## 4. 验证输出

```
$ go test ./... -count=1          # 在 git archive ab37f77 的干净副本里跑
ok  	github.com/zzjcool/anotify/internal/api	1.305s
ok  	github.com/zzjcool/anotify/internal/auth	35.710s
ok  	github.com/zzjcool/anotify/internal/lease	0.880s
ok  	github.com/zzjcool/anotify/internal/route	2.087s
ok  	github.com/zzjcool/anotify/internal/server	55.611s
ok  	github.com/zzjcool/anotify/internal/store	17.195s
ok  	github.com/zzjcool/anotify/internal/ws	25.902s
--- FAIL: TestHandleUpgradeNeedRootDoesNotExit (0.00s)
    upgrade_frame_test.go:257: ack = &{Type:upgrade_ack OK:true Version:1.5.0 ...}, want ok=false error=need_root
FAIL	github.com/zzjcool/anotify/cmd/agent	18.418s
```

最后这条是**既有环境失败**，与本改动无关：用例靠 `chmod 0555` 造「目录不可写 → need_root」，
而本机以 root 运行（`id -u` = 0），root 无视目录权限。同一用例在未改动的 main
（`/root/code/anotify`，7cb2e88）上同样失败：

```
$ (cd /root/code/anotify && go test ./cmd/agent/ -count=1 -run TestHandleUpgradeNeedRootDoesNotExit)
--- FAIL: TestHandleUpgradeNeedRootDoesNotExit (0.02s)
FAIL	github.com/zzjcool/anotify/cmd/agent	0.044s
```

另：

```
$ go vet ./...        # 无输出
$ make contract
✅ 契约静态断言（A2/A4/A5/A6/A7 + openapi 一致性）通过
```

## 5. 边界说明（请裁决/知悉）

任务卡写「不要碰 internal/store/*（另一轮在改）」，但同一张卡的「实现要求 1」要求把
`SELECT COUNT(*)` 挪进 `RegisterUser` 的同一事务——那只能改 `internal/store/identity.go`。
已核实该文件**不在**任何其它 bundle 的在飞改动里（W2 的提交 e28beed 只碰
migrate.go / node.go / schema.sql / schema_baseline_test.go / seq_in_trace_test.go / task.go /
authz_test.go + lease/sweeper + server/admin_handlers.go），提交时无冲突面，故按实现要求 1 执行。

签名未改（`RegisterUser(ctx, u)` 不变），也没有新增 store 导出方法：角色由**入参值**
`RoleAuto` 表达，其它 8 处调用点（devseed / route 测试 / store 测试 / bench）零改动，
`Role==""` 仍兜底成 `RoleMember`，语义不变。

若裁决要求 W7 严格不碰 store，回退方式：`git revert ab37f77`（本改动是单一 commit，
auth 与 store 两半必须一起回退——只回退 store 会让 `role='auto'` 落库）。

## 6. 未决问题

- `internal/server/admin_handlers.go` 的 PUT 用户角色校验只接受 `admin|member`（:215），
  所以 `auto` 不可能从 HTTP 注入；反过来说 `RoleAuto` 是对外可见的 store 常量，
  将来若有第三方直接调 `RegisterUser` 也需知道它「不落库」。已在常量注释里写明。
- 未新增 store 侧单测（`internal/store` 只改了 identity.go 一处），行为由 auth 侧
  两条用例端到端覆盖（含 RoleAuto 的两个分支）。若要求 store 侧补一条
  `TestRegisterUser_RoleAuto`，需要授权我改 `internal/store/*_test.go`。
