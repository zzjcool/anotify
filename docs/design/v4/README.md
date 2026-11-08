# v4 设计文档

| 文件 | 内容 | 状态 |
|---|---|---|
| `capability.md` | 能力：`params` / `next` / `run` + 派生 `status` | 讨论中 |
| `source.md` | 源（触发器）：四种 kind、config、targets | 讨论中（D1–D5 待定） |
| `playbook.md` | 安装包：能力 + 触发器 | 待写 |

v3 的文档仍在上级目录（`capability-spec-v3.md`），**已落地并推送**，作为现状参考。
v4 是 breaking change：`spec.outputs` 改名 `next`、边不再是用户实体、
`handler` 实现形式删除。见 `capability.md §7`。
