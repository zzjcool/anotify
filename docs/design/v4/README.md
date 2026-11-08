# v4 设计文档

| 文件 | 内容 | 状态 |
|---|---|---|
| `capability.md` | 能力：`params` / `next` / `run` + 派生 `status` | 讨论中 |
| `source.md` | 源（触发器）：四种 kind、config、targets | 讨论中（D1–D5 待定） |
| `playbook.md` | 安装包：能力 + 触发器 | 待写 |

## 命名与二进制（v4 一并改）

| 现在 | v4 |
|---|---|
| `anotify-agent` | **`anode`**（和 AI 的 agent 撞名，改掉） |
| `cmd/agent/` | `cmd/anode/` |
| `node.yaml` | **`anode.yaml`** |
| systemd 单元 `anotify-agent.service` | `anode.service` |

**只有一个二进制**，三个子命令：

```bash
anode serve        # 守护进程（systemd 里跑这个）
anode install      # 装成系统服务
anode emit <name>  # 能力脚本里唯一的命令
```

全仓约 460 处 / 71 个文件需要同步替换（含 Dockerfile、install skill、e2e 夹具、文档）。
漏一处的后果是"新环境下装不上"，所以它必须在 P9 的第一刀里做完、且由 e2e 验证。

v3 的文档仍在上级目录（`capability-spec-v3.md`），**已落地并推送**，作为现状参考。
v4 是 breaking change：`spec.outputs` 改名 `next`、边不再是用户实体、
`handler` 实现形式删除。见 `capability.md §7`。
