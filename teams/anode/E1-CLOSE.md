# E1-CLOSE · V4-ANODE N1/E1 纯协议补充 + N2A 独立机械 close

- 分支：`pi-subagent/v4-anode-d0006ee5`；基线 HEAD：`4a8b2f6`（N2A checkpoint）
- 权威：父 `/tmp/anotify-v4-emit-ws-approval.md`（E1 全文）、`.pi-legion/contract.md` rev3、父 `.pi-orchestrator/v4/CONTRACT.md` rev2 §2/§7
- 本任务只补 protocol；**未改/未实现任何 node、artifact、server、ws、store 代码**

## 1. 交付

| 内容 | 位置 |
|---|---|
| 产品 commit（**仅** `internal/protocol/`） | **179e999** `feat(protocol): 补齐 E1 emit 转发两帧与节点 WS 端点常量` |
| 唯一最终交接 | `teams/anode/PROTOCOL-HANDOFF.md`（覆盖旧版，无相互矛盾快照） |
| 本文件（工艺/验证记录） | `teams/anode/E1-CLOSE.md` |
| 累计纯 protocol 补丁 | `/tmp/anotify-v4-anode-final/protocol-only.patch`（`f7e9a6c` → `179e999`） |
| E1 增量纯 protocol 补丁 | `/tmp/anotify-v4-anode-final/protocol-emit.patch`（`caecd9d` → `179e999`） |
| 补丁校验和 | `/tmp/anotify-v4-anode-final/SHA256SUMS` |

`teams/anode/protocol-only.patch` 已 `git rm`：补丁正文的 diff 上下文行含 `空格+tab`，留在 repo 会让生产 `git diff --check` 误报；生成物迁移到 `/tmp`，repo 内保留 HANDOFF 说明 + 外部 SHA256。

改动文件（产品提交）：

- `internal/protocol/emit_frames.go`（新增：`TaskEmitFrame`/`TaskEmitAckFrame`/`Validate`/`IsEmitErrorCode`）
- `internal/protocol/emit_frames_test.go`（新增 13 个具名 Test，见 HANDOFF §7）
- `internal/protocol/node_endpoints_test.go`（新增 `TestNodeStreamPath`）
- `internal/protocol/codec.go`、`internal/protocol/doc.go`、`internal/protocol/ops_frames.go`、`internal/protocol/protocol_test.go`（Decode/方向/端点常量/帧集合更新）
- `teams/anode/PROTOCOL-HANDOFF.md`、`teams/anode/E1-CLOSE.md`（本提交，文档）
- 移除：`teams/anode/protocol-only.patch`

## 2. E1 协议点（父批准逐条落地）

- `task.emit` node→server：`type,requestId,taskId,attempt,leaseId,attemptToken,to,id?,data?`，匿名嵌入 `EmitRequest` → 与 HTTP 入口同一 payload 解释；缺 `to` 仍是 `missing_target` 400 / CLI 2（`Decode`/`Validate` 都不拒），非对象 `data` 由 `Validate` 拒绝。
- `task.emit.ack` server→node：`type,requestId,taskId,attempt,httpStatus,response?,error?`；成功**只** `response`（200，剪枝 `taskId:null` + `pruned` 也是 200），错误**只** `error`；`Validate` 强制互斥且 `httpStatus == EmitHTTPStatus(error.Error)`，错误码封闭。
- ACK 不含 `attemptToken`/`leaseId`/`namespace`/业务 `id`/`seq`；`requestId` 只做 RPC 关联（与业务 `id` 是两个字段，具名测试固定）。
- 帧无 `namespace`：未知字段被忽略，调用方无法覆盖中枢按 task/node 权威推导的 namespace。
- 两帧进统一 `Decode` + 方向表（`task.emit` 上行、`task.emit.ack` 下行）；帧总数 21 → **23**。
- `NodeStreamPath = "/v1/node/stream"` 与 `NodeBinaryPath` 并列导出；HANDOFF 说明中枢空 path/`/` 归一、显式反代 path 原样、无 legacy alias；Artifact GET 200 直接完整 `artifact.Artifact` JSON（无 wrapper/status），本包不造镜像。
- 网关有界等待/timeout/迟到 ACK/CLI5 未知结果**不在本包**，留给 N2B 节点网关。

## 3. N2A 独立机械验证（不是引用旧自报）

事实：`4a8b2f6` 的 launch 漏了 verify 数组，旧 writer 只有 attested、不算 verified。本次在**最终协议 179e999** 上独立重跑固定三条（同一命令同时覆盖 protocol/artifact/cmd-anode 与新增协议），未为通过而改 node 代码。

```sh
GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto go test ./internal/protocol/... ./internal/artifact/... ./cmd/anode/... -count=1
# ok  	github.com/zzjcool/anotify/internal/protocol	0.057s
# ok  	github.com/zzjcool/anotify/internal/artifact	2.597s
# ok  	github.com/zzjcool/anotify/cmd/anode/internal/applied	0.021s
# ok  	github.com/zzjcool/anotify/cmd/anode/internal/capability	0.036s
# ok  	github.com/zzjcool/anotify/cmd/anode/internal/config	0.007s
# ok  	github.com/zzjcool/anotify/cmd/anode/internal/hostenv	0.005s
# ok  	github.com/zzjcool/anotify/cmd/anode/internal/hub	0.027s
# ok  	github.com/zzjcool/anotify/cmd/anode/internal/listen	0.006s
# ok  	github.com/zzjcool/anotify/cmd/anode/internal/node	0.028s
# exit=0

GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto go vet ./internal/protocol/... ./cmd/anode/...
# （无输出）exit=0

git diff --check
# （无输出）exit=0

gofmt -l internal/protocol/
# （空）exit=0
```

结论：N2A 的既有 69 个顶层测试 + 新协议测试在同一固定命令下独立通过，N2A checkpoint 由本次机械验证 close；**不表示完整 N2 完成**（N2B grant 执行/lease/emit 网关/env/files/GC、N3 仍未做）。

## 4. 纯 protocol 补丁导出与校验（当前 checkout + /tmp，未读写其他 worktree）

生成：

```sh
git diff f7e9a6c 179e999 -- internal/protocol > /tmp/anotify-v4-anode-final/protocol-only.patch   # 13 files
git diff caecd9d 179e999 -- internal/protocol > /tmp/anotify-v4-anode-final/protocol-emit.patch   # 7 files
cd /tmp/anotify-v4-anode-final && sha256sum protocol-only.patch protocol-emit.patch > SHA256SUMS
```

校验和：

```
3c7d7f3761ba5eac129bb0a2b0e7a079a4aba351e48e3f0943e466f5e1aa01ed  protocol-only.patch
738bbdd8531e50156b404e31df14051ac45bbef1f229a83cb3ead6974973bb69  protocol-emit.patch
```

archive apply-check / apply / 逐字相同（仅当前 checkout 的 `git archive` + `/tmp`）：

```sh
git archive f7e9a6c internal/protocol | tar -x -C /tmp/.../verify/only-base
git archive 179e999 internal/protocol | tar -x -C /tmp/.../verify/only-head
(cd only-base && git apply --check protocol-only.patch)   # PASS
(cd only-base && git apply protocol-only.patch)           # PASS
diff -r only-base/internal/protocol only-head/internal/protocol   # 无差异（byte-identical）

git archive caecd9d internal/protocol | tar -x -C /tmp/.../verify/emit-base
(cd emit-base && git apply --check protocol-emit.patch)   # PASS
(cd emit-base && git apply protocol-emit.patch)           # PASS
diff -r emit-base/internal/protocol emit-head/internal/protocol   # 无差异（byte-identical）

sha256sum -c SHA256SUMS
# protocol-only.patch: OK
# protocol-emit.patch: OK
```

两个补丁都只含 `internal/protocol/` 的 diff（`grep '^diff --git'`：累计 13 个文件、增量 7 个文件），不含 docs/cmd/artifact。产品提交 `179e999` 相对 `4a8b2f6` 也只动 `internal/protocol/`（7 files, +766/−4）。

## 5. 未决（如实上报，不假最终完成）

- N2B：真实 grant 执行、lease TTL/续租/回收、HTTP emit 网关 + WS 转发运行器（有界 in-flight/timeout/迟到 ACK/CLI5）、env/files/GC。
- N3：serve/install/emit 三命令收尾、安装器/脚本/Docker/docs。
- 冻结设计文档 `capability.md §8.5` 尚未收录 deploy/emit 帧（父权威在 CONTRACT §7 + `/tmp` 批准；T3 不改冻结设计）。
- 本任务**不宣称**完整节点或 N2 全量完成；fresh review / 父跨队 E2E 未做。
