# #525：internal/ws 夹具的落盘竞争 —— 收尾等「会话真的退完」，不等时间

snapshot-doc: 2026-11-22 本机交付快照（注入的输出、门禁数字都是当次运行的定格；每处都附了复跑命令，复跑以新输出为准）

- 基线：`origin/main` = `584b0cd8`；分支 `pi-subagent/hw-nj/anotify-525-ws-flake-997a3dc8`（独立 worktree）
- 结论：`internal/ws` 夹具（`fixture_test.go`）的收尾从「关连接就算完」改成「等每条会话的
  `ServeHTTP` 真的返回」（= `detach` 的两笔库写已落），上限 `sessionSettleLimit`（5s），超时
  `t.Fatal` 并打印剩余条数。**产品代码零改动**（`hub.go` / `session.go` / `handler` 一行未动）。
- 没放宽任何断言，没有 sleep 兜底，没有 skip，`-shuffle` 照开。

## 1. 现象与根因

CI 的 `quick（make ci）`（run 44934355658 / job 136937743770，PR #523 纯前端）红了：

```
--- FAIL: TestMalformedResultFrameClosesBeforeIdentityCheck (0.10s)
    --- FAIL: .../bad_field_type (0.07s)
        testing.go:1369: TempDir RemoveAll cleanup: unlinkat
            /tmp/.txHYtxc/.../001: directory not empty
```

与改动无关，是**夹具的收尾竞态**。`t.TempDir()` 的 cleanup 注册得最早（LIFO ⇒ 最后跑），
它前面只有三道收尾，**三道都不能证明服务端会话已经退出**：

| 收尾 | 为什么不够 |
| --- | --- |
| `conn.Close()`（客户端） | 只是客户端侧动作；服务端读完 close 帧后还要跑 `detach` |
| `httptest.Server.Close()` | 被 hijack 的 WebSocket 会从 `s.wg` 摘掉（`http.StateHijacked`）⇒ 既不关那条连接，也不等它的 handler |
| `hub.Online()==false` | `detach` **先**摘注册表**再**写库（`MarkNodeOffline` / `MarkInstancesDead`），false 落在写库之前 |
| `db.Close()` | `database/sql` 只关**空闲**连接，不等在飞的查询（`sql.go` 的 `(*DB).Close` 不等待在用连接） |

于是 `RemoveAll` 会与 `detach` 的写并行：SQLite 的 `-shm` 在被 unlink 后按「共享内存被删」
的既定逻辑重建，`RemoveAll` 的 `rmdir` 就报 `directory not empty`。`-shuffle` 只改变顺序，
所以它是偶发而不是必现。

## 2. 修法（判据：唯一等价于「库写已落」的观测点是 `ServeHTTP` 已返回）

新增 `internal/ws/settle_test.go`（**测试专用**，test hook）：`sessionSettle` 记录「还有几条
`ServeHTTP` 没返回」，`enter/exit` 在 `env.handle` 里配对，`wait(d)` 在计数归零时被唤醒
（channel 语义，不是轮询）。`fixture_test.go` 的收尾随之变成：

```
各 dial 的 conn.Close → server.Close → 等会话退完（≤5s）→ db.Close → TempDir 删除
```

超时直接失败并打印剩余条数；随后再断言 hub 注册表为空（`sessionCount()==0`）。
细节三条：

- **挂在 `env.handle` 上，不挂裸 `hub.ServeHTTP`**：`session_lifecycle_test.go` 的
  `newCountingServer` 会把 `e.server` 换掉，换过的 server 也必须进同一个屏障。
- **不做 seal/拒绝新会话**（`internal/server` 的 `settleGroup` 有这一档）：本夹具的 dial
  都发生在测试体里，收尾阶段没有新会话来源；等待是「有界 + 计数」而不是
  `sync.WaitGroup`（后者的 `Add` 与 `Wait` 并发是非法用法）。
- **产品代码不动**：屏障是夹具的事，`Hub` 的语义（detach 顺序、写库时机）一个字都没改。

参考形状：`internal/server/hub_fixture_test.go` 用同样的屏障修过同类竞态（#259 / #340 r3，
出处是更早的 `internal/ws/node_test.go`）—— 那个包当时修了，ws 自己的夹具没跟上。

## 3. 稳定性证据（修复后）

```
go test ./internal/ws -run TestMalformedResultFrameClosesBeforeIdentityCheck -count=100 -shuffle=on
ok  github.com/zzjcool/anotify/internal/ws  10.0s      # 200/200 子用例 PASS（2 子用例 × 100 轮）

go test ./internal/ws -count=10 -shuffle=on
ok  github.com/zzjcool/anotify/internal/ws  44.5s      # 39 个用例 × 10 轮
```

对照组（CI 同款压力面：整套 `go test ./... -count=1 -shuffle=on` 并行在跑，同时目标用例
`-count=100 -shuffle=on`）：见 §4 的表。

## 4. 变异：把等待删掉（= #525 之前的夹具），能不能复现红

变异做法：`t.Cleanup(func(){ server.Close(); 等归零 })` → `t.Cleanup(server.Close)`
（只删等待，其余不动；`settle`/`sessionCount` 保留但不再被等待路径使用）。

| 实验 | 命令 | 结果 |
| --- | --- | --- |
| A 孤立 ×5 轮 | `-run ... -count=100 -shuffle=on` | 0/500 轮红（本机太干净） |
| B 轻负载 ×3 轮 | 同上，另起 `go test ./internal/store -count=2` | 0/300 红 |
| C CI 同款压力面 ×2 轮 | 同上，同时 `go test ./... -count=1 -shuffle=on` | **第 2 轮红 1 次**，签名与 CI 一致 |
| D 修复后同款压力面 ×2 轮 | 与 C 完全相同 | **0/200 红**（两轮整套 `./...` 也全绿） |

C 的红（`mutC-2.log`，与 CI 的失败同一行号、同一形态）：

```
--- FAIL: TestMalformedResultFrameClosesBeforeIdentityCheck (0.18s)
    --- FAIL: .../truncated_json (0.08s)
        testing.go:1369: TempDir RemoveAll cleanup: unlinkat
            /tmp/TestMalformedResultFrameClosesBeforeIdentityChecktruncated_json823467265/001: directory not empty
FAIL	github.com/zzjcool/anotify/internal/ws	16.708s
```

为什么 A/B 打不中而 C 打得中：这个 bug 要的是一次**文件系统级**的交错（`-shm` 的 unlink 与
SQLite 的重建撞在同一微秒里），单进程时窗口太窄；CI（和 C）里几十个测试进程并行，调度抖动
才把窗口暴露出来。**它正是「重跑就过」的那种红**，所以不能靠「多跑几遍看看」。

### 4.2 第二个红样本（另一个变异体 `wait(0)`，包级 `-count=10`）

另一个等价变异体（把等待上限改成 0 ⇒ 立即返回，并把「仍有 N 条在飞」印成日志而不是 Fatal）
在**包级** `-count=10 -shuffle=on` 上直接红了 —— 这次死的是另一条用例（`rotated_token`），
而且红之前变异体自己先把条件说出来了：

```
--- FAIL: TestRejectAssignmentAuthorizedSuccessAndCredentialMatrix (0.09s)
    --- FAIL: .../rotated_token (0.04s)
        fixture_test.go:102: MUTATION: db.Close 前仍有 1 条会话在飞
        testing.go:1369: TempDir RemoveAll cleanup: unlinkat /tmp/…rotated464552874/001: directory not empty
FAIL	github.com/zzjcool/anotify/internal/ws	51.514s
```

`db.Close 前仍有 1 条会话在飞` 与 `directory not empty` 在同一次运行里前后脚出现，把
§1 的因果链钉死了：**在飞的会话**是原因，**删不干净的目录**只是它被看见的方式。
（这个变异体不是本 PR 的脚本产生的：它在核对阶段被从 worktree 外部注入，我发现后
恢复了原码、核了 sha256 并重跑了全部证据 —— 详见 §7。）

### 4.1 直接量到那个窗口（比偶发红更确定）

A/B 全绿说明「偶发」这条证据不可靠，于是直接测根因条件：**在 `db.Close()` 那一刻，服务端
会话 goroutine 还在不在**。做法（**仅实验**，未入库）：在 `newEnv` 里注册一个探针 cleanup
（LIFO 排在 `db.Close` 之前），读 `settle.pending`。

| 夹具 | `-count=20 -shuffle=on`（40 个子用例迭代） | 命中「db.Close 时仍有会话在跑」 |
| --- | --- | --- |
| 变异（无等待） | 40 次 | **31 次** |
| 修复后 | 40 次 | **0 次** |

也就是说：旧夹具**七成以上**的迭代都带着在飞的会话进 `db.Close`/`RemoveAll`（有时就是这样
偶发红），而修复后一次都没有。红线由此变成结构性的：`RemoveAll` 严格发生在 `detach` 的两笔
库写之后。

## 5. 门禁

| 门 | 结果 |
| --- | --- |
| `make ci` | **exit 0** —— 在干净副本（detached `5375f830`，`/tmp/ws525/verify`）上跑，4m53s；43 个包 `ok`（`internal/ws` 14.5s）、契约 48/48、注释密度「与基线完全一致」、临时目录零残留（143.6s） |
| `make e2e-affected` | 本改动被判为「二进制风险面（server←cmd/server）⇒ 全量 e2e 30/30 套」。本机两次全量跑都是 **29/30**，且**每次红的套件不同**（routing 2 条断言 / vars_ui 1 条断言），各自的单套复跑都 0 失败（routing 295/0、vars_ui 74/0）→ 与本改动无关的环境性假红（本机同时有别的 worktree 在跑全量 e2e：`/proc` 里 `anotify-522-router` 的 `run_all.sh`）。**权威判据是 PR #526 的 CI**：同一份 affected 计划分 4 片跑，全部 pass（quick（make ci）3m45s / 4 个 e2e 分片 3m23s–4m9s / `e2e（affected）` gate pass） |
| `scripts/e2e/known-env-failures.json` | `entries` 保持 `[]`，本 PR 未动、也**不**把上面两条环境性假红登记进去 |
| 注释密度棘轮（双向） | 新增 `settle_test.go` 的注释行由同批压缩抵消；净 −1 行 ⇒ 同批 `--update`（`internal/ws` 669→668） |

## 6. 边界（别把这份报告读多）

- **没有改产品代码**：`detach` 的写库顺序、`Hub` 的会话语义未变；这不是「用生产改动掩盖测试竞态」。
- **没有把偶发红当证据**：本机 A/B 打不中，所以补了 §4.1 的确定性探针；C 与 §4.2 的红只作为
  「症状确实由缺失的等待造成」的旁证。
- **`internal/server` 的同类夹具**（`hub_fixture_test.go`）已经有屏障，本 PR 没碰它。
- 本 PR **不**处理其它包的 `TempDir` 收尾（`internal/route` 等夹具不挂 WebSocket，没有这条竞态）。

## 7. 交付过程中的外部改动（如实记录）

核对期间，本 worktree 的 `internal/ws/fixture_test.go` **两次**被从外部改写成变异体：

| 时间 | 注入的变异 | 发现方式 |
| --- | --- | --- |
| 02:55（`make ci` exit 0 之后） | 收尾块换成 `e.settle.wait(0)` + `t.Logf("MUTATION: …")`（等待上限 0 = 立即返回，断言降成日志） | 紧接着的包级 `-count=10 -shuffle=on` 直接变红，失败行里带 `MUTATION:`（§4.2 的样本） |
| 03:07（commit `5375f830` 之后） | 收尾块换成「只读 `pending` 打日志、完全不等待」 | 重跑 `make ci` 时 `staticcheck U1000` 报 `wait` / `sessionCount` 未使用（变异体绕过了它们） |

两次都不是本 PR 的脚本产生的（本 PR 的变异只用 `/tmp` 下的脚本，且每次都 `revert` + `diff -q` 自检）。
处置：

1. 从 `HEAD` 恢复被改的文件，重核 sha256（`fixture_test.go` = `05ccd033…`，`settle_test.go` = `ddcffd7f…`）；
2. 把交付分支先推到远端，让「分支」这个交付物与本地工作区解耦；
3. 门禁改在**干净副本**上跑：`git worktree add --detach /tmp/ws525/verify 5375f830`，
   在那里跑 `make ci` / `make e2e-affected`（跑前跑后各核一次 sha256）。

这一段保留在报告里而不是悄悄抹掉：读者需要知道 `make ci` 的绿是在**哪个树**上跑出来的。
副作用：第二个变异体被 `staticcheck` 的 `U1000` 抓出来，等于额外证了一次
「交付版本真的在用那个等待」——把等待拆掉，编译期就能看见两个函数变成死代码。
