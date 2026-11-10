## implementationGate: false — 原包闭包未完成

局部 checkpoint：`8baf0ae216463a22c950f5dc15e7235708859fed`

**提交文件仅两项：**
- `internal/auth/password_test.go`
- `.pi-orchestrator/v4/teams/core-runtime/TEST-MAPPING.md`

### 已完成
- 仅替换 `TestRegisterWithPassword_Success` 的退役 builtin 指向，改用真实 namespace 授权和脚本能力 setup；原 PHC、用户、恢复码、会话、角色、错密码断言保留。
- 该同名测试已红→绿，完整 auth 包通过。
- 从本仓库 `0d4b975` 登记 **460 个原测试/benchmark 名**，包含已删除 builtin 文件的逐名清单。未迁移项明确标为 pending。

### 未完成与回滚
原 store 仍存在旧测试类型编译阻塞：
- `capability_links_test.go:59`：`ArtifactBuilder`
- `playbook_test.go:17/51/67/77`：旧 Playbook DTO
- `reconcile_test.go:49`：`AppliedRef`
- `session_lease_safety_test.go:50/56`：`Lifecycle`

**这些不是新发现的产品 bug，而是本次未完成的原测试迁移。** 两次 vet 均失败，未继续第三次猜测。未验证的迁移及 7 个内部白盒草案已回滚；草案保存在 `/tmp/anotify-core-next-g1tests-*`，不计交付测试或 PASS。

### 固定三项实测
| 门禁 | Exit |
|---|---:|
| format | 0 |
| 完整三个包域 vet | 1 |
| 完整三个包域 test `-count=1` | 1 |

最后一次完整 scoped test 输出：
```text
FAIL	github.com/zzjcool/anotify/internal/store [build failed]
ok  	github.com/zzjcool/anotify/internal/store/v4test	34.654s
ok  	github.com/zzjcool/anotify/internal/auth	20.790s
ok  	github.com/zzjcool/anotify/internal/push	0.006s
FAIL
EXIT 1
```

完整命令、pwd、验证时 HEAD、原始编译输出及红绿日志均在 `TEST-MAPPING.md` 和 `/tmp/anotify-core-next-g1tests-*.log`；最终 receipt：
`/tmp/anotify-core-next-g1tests-final-receipt.json`。

下一步应继续 **serial test-only 原包迁移**，再恢复并执行白盒草案。G1、G2–G4、最终 full Go/contract/e2e-quick 均未闭合。未改产品，未派子 agent，未 push/merge/MR。

DONE V4-CORE-RUNTIME-NEXT/G1TEST（失败交接，未通过验收）

{"ok":false,"reason":"原 store in-package 测试迁移未完成，fixed vet/test exit 1；仅提交 auth 局部闭包及逐名库存 8baf0ae，未验证 store 草案已回滚，不声明 G1/T1 完成。"}
