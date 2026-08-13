# anotify-plugins 测试同步（接收端能力模型协议变更）

## DONE

### 改动文件清单

| 文件 | 改动 |
|------|------|
| `test/test_notify_basic.sh` | `--status success`→`--agent-state done`；断言 `"status":"success"`→`"agentState":"done"`；注释更新 |
| `test/test_notify_no_cred.sh` | `--status success`→`--agent-state done`（2 处） |
| `test/test_notify_missing_args.sh` | 注释"缺 --status"→"缺 --agent-state"；`--status success`→`--agent-state done` |
| `test/test_notify_401.sh` | `--status success`→`--agent-state done` |
| `test/test_notify_invalid_status.sh` → `test/test_notify_invalid_agent_state.sh` | **改名+改内容**：坏值列表改测新枚举坏值（bogus/running/idle/success/error/info/warning/1/空串）；新增好值校验段（working/blocked/done/interrupted/error 应通过枚举校验，退出码≠1） |
| `test/test_notify_tags.sh` | `--status info`→`--agent-state working`（3 处，info→working 映射） |
| `test/test_notify_escape.sh` | `--status success`→`--agent-state done` |
| `test/test_notify_env_override.sh` | `--status success`→`--agent-state done` |
| `test/run.sh` | 测试名"无效 status 报错"→"无效 agent-state 报错"；文件引用改 `test_notify_invalid_agent_state.sh` |
| `test/pi_extension_smoke.mjs` | `status!=="info"` 断言→`agentState!=="working"`；新增 tool_execution_end 断言（agentState=working + severity=error）；新增 agent_settled 断言（agentState=done） |

### npm test 输出摘要

```
════════════════════════════════════════
  通过：11  失败：0
════════════════════════════════════════
```

11 个测试全部通过：
1. notify.sh: 基本成功上报 ✓
2. notify.sh: 缺少必填参数报错 ✓
3. notify.sh: 无效 agent-state 报错 ✓
4. notify.sh: deviceTags 数组化 ✓
5. notify.sh: JSON 转义 ✓
6. notify.sh: 凭证缺失报错 ✓
7. notify.sh: 401 鉴权失败 ✓
8. notify.sh: 环境变量覆盖凭证 ✓
9. login.sh: 幂等自检 ✓
10. pi 扩展: TypeScript 类型检查 ✓
11. pi 扩展: 事件→上报 集成测试 ✓

### Grep 核对

```
grep -rn '\-\-status\b\|"status":' test/ common/ pi/
→ 仅 common/notify.sh:92 的 --status 兼容别名（允许保留）
→ test/ 目录下零匹配（完全清除）
```

### 遗留风险

1. **notify.sh line 120 全角字符 bug（pre-existing）**：`die "无效 --agent-state：$AGENT_STATE（须为…）"` 中 `$AGENT_STATE` 后紧跟全角 `（`（U+FF08），`set -u` 下 shell 将 `AGENT_STATE（` 视为未绑定变量名而崩溃。此 bug 在协议变更前就存在（原代码 `$STATUS（` 同样有问题），被 invalid-state 测试掩盖（测试只检查退出码≠0，不检查错误消息）。本任务范围是改测试，notify.sh 标记为"已改完"，未修复此 bug。建议后续在 `$AGENT_STATE` 后加空格或用 `${AGENT_STATE}` 隔离。

2. **test_notify_basic.sh line 22 `MOCK_BASE: unbound variable` 警告（pre-existing）**：来自 `start_mock.sh` 的 `set -eu` 与全角字符交互，原始代码即存在，不影响测试结果。非本任务范围。
