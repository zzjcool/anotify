## 状态说明：本 PR 作为历史 checkpoint 保留，最终以 superseded 关闭（不 merge）

本 PR 是 v4 节点（anode）的 **Draft checkpoint**（N1/E1 协议冻结 + N2A 及后续节点工作），基于早期 `main`。后续 v4 全量交付已在另一条集成线上完成，**本 PR 不合并进 `main`**，等 release PR 落地后 close 为 superseded。

### 代码没有丢：逐文件比对结果

基线：`v4/integration` 起点 `b30e998`（E1 checkpoint），其中节点交付由 `80eb523 feat(v4): 集成已验收节点真实日志记录与完成性补口` 落盘。

```text
$ git merge-base --is-ancestor 5a3dae3 4c822a7 && echo YES
YES                                  # 本 PR head 5a3dae3 是节点日志分支 4c822a7 的祖先
$ git diff --stat 4c822a7 b30e998 -- cmd/anode
（空）                                # b30e998 的 cmd/anode 树与 4c822a7 完全一致
```

- 与本 PR head `5a3dae3` 相比，`cmd/anode` 57 个共有文件里 **53 个字节相同**；
  其余 4 个（`internal/ops/ops.go`、`ops_test.go`、`internal/resultstore/store.go`、`internal/runner/attempt.go`）
  是本 PR 之后、`4c822a7` 内的节点日志完整性后续修复，**已全部包含**在 `b30e998`。
- `b30e998` 相对 `4c822a7` 只在 4 个协议/artifact 文件上继续演进（不是回退）：
  - `internal/artifact/artifact.go`：`paramEnvName` 归一规则去重到共享 `internal/protocol`；
  - `internal/protocol/emit.go`：E1 的 `version,omitempty`（未解析版本省略键，不公开哨兵 0）及回执语义注释/测试。
- `git cherry` 显示 38 个 commit 均非 patch-equivalent，原因只是交付线以 `80eb523` **单亲提交**重新落盘，未保留分支 ancestry；**树内容一致**。

### 为什么本 PR 不能直接 merge

本 PR 分支缺 v4 核心（早于 v4 runtime 重写）：

```text
internal/store/resource_cas.go      missing
internal/route/route.go             missing
internal/server/api_envelope.go     missing
```

直接合进 `main` 会得到「有 anode、没有完整 v4 服务端/store/API」的半套 v4。

### 收尾计划

1. `v4/integration` 上完成 #331–#338（HTTP 资源、产物传输、exact-five OpenAPI、工具链、真实 Node E2E、前端、全量门禁）。
2. #338 全绿后开 release PR：`v4/integration → main`（携带本 PR 的节点代码 + 完整 v4）。
3. release PR 合并后，本 PR 以 **superseded** 关闭；需要追溯节点交付时看 `b30e998` / `80eb523` / `4c822a7`。

在此之前本 PR 保持 open（Draft），作为历史证据，不参与本轮代码评审。
