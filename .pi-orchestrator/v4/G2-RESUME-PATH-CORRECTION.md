# G2 接续路径校正（不改变行为/测试门禁）

事实：`core-g2-resume` 由合法 subagent/centurion 调度实际建立并使用了隔离子 WT，尽管父 launch 显式传 `worktree:false`。工具返回 supervisor=terminal；磁盘实测为：

- 直接父交付树只读：`/root/code/anotify/.pi-subagents/runs/r-57adb51b/worktrees/v4-core-delivery`，HEAD `50fb9ac`。
- G2 唯一执行树：`/root/code/anotify/.pi-subagents/runs/r-57adb51b/worktrees/v4-core-delivery/.pi-subagents/runs/r-01be4aa7/worktrees/core-g2-resume`，分支 `pi-subagent/core-g2-resume-52fc54d4`，冻结文档 checkpoint `5441426`。

父接受工具实际隔离结果，以此树为 G2 唯一源码 writer 的 canonical cwd/root/GOMOD。不搬目录，不删除/另建 WT，不让 worker 写回直接父树，不 merge/push。父原恢复文档“直接使用既有 core WT、不另起”的路径预期在这里由实际工具隔离规则校正，不改变 G2 所有权与验收。

## 验证口径

父 launch 的首条命令硬编码原直接父路径，会与工具隔离 cwd 不一致。若因此 rejected，原记录完整保留，并明确是入口路径预期错误、不是源码/格式通过，也不能掩盖尚未执行的后续三个 gate。

G2 worker 在新执行树的固定首项必须是只读的 `test -z "$(gofmt -l internal/route internal/ws internal/lease internal/source internal/trigger internal/logging)"` + `git diff --check`，而不是门禁执行 `gofmt -w` 后再宣告通过。formatter 写操作只在实现阶段。

其余三项仍为完整六 runtime 包 vet、相同六包 test count1、store/artifact/protocol test count1；所有命令先核真实 canonical cwd/root/GOMOD。任何子任务验收必须传父完整四项数组，不省略、不复用旧无匹配路径 receipt。最终代码冻结且无 writer 后，父按真实子树路径另外启动完整四项 mechanical carrier 独立验收，原错误 receipt 不改写为 accepted。

## 已知日志阻塞

新两路侦察均证：Node 的档案只 raw stdout/stderr + attempt-level时间；没有逐行 at/sequence，不能从原文恢复跨流顺序。G2 日志真实结构化项仍 PENDING；其余运行链可继续。父会单独冻结日志采集语义和协议/Node所有权，不授权 G2 偷改 protected protocol/Node，也不接受私有DTO/空成功冒替 W6。
