# E1 · 已创建失败 Task 的 emit 回执身份（E1-only 接口卡）

> 父契约：`/root/code/anotify/.pi-orchestrator/v4/EMIT-FAILED-TASK-RECEIPT-HANDOFF.md`（D1-D3 冻结）。
> 本卡只记 E1 的**新接缝**与当前 code 的事实（actual ids / 权威顺序 / result 字节来源），
> 不改父契约、不新增 wire 字段/数组、不重做 G1/S1/Node。

## 1. 问题（修复前真实行为）

- `downstream_missing` 已在 `EmitAuthorized` 同事务落 `phase=failed` Task + 错误事件；
- 但 `store.DispatchFailureInputs` 没有 caller ID，`newDispatchFailureTask` 现场 `NewTaskID()`，
  `writeEmitPlannedFactsOn` 只回 error → route 200 回执 `taskId:null,eventId,to,version` 且无 `pruned`；
- `cmd/anode/emit.go` 成功判据 `To != "" && (TaskID != nil || Pruned != nil)` → 严格 CLI 视作
  `result unknown`（exit 5）；同 emit id 回放只重放这份坏 body。

## 2. store 接缝（`internal/store/dispatch_failure.go` / `emission.go`，均非 wire）

| 名字 | 形状 | 语义 |
| --- | --- | --- |
| `DispatchFailureInputs.TaskID` | `string`（可选，新增） | 调用方预分配 id；`""` → `NewTaskID()`；只接受 `tsk_<base62>` 形状 |
| `EmitFailureFact.TaskID` | `string`（可选，新增） | 计划事实把预分配 id 传进同一 typed input |
| `writeEmitPlannedFactsOn` | 返回 `([]string, error)` | 返回**真实创建**的失败 Task id（按计划顺序），供 result 核对 |
| `emitResult(rec, tasks, failureTaskIDs)` | 内部 | `rec.Result` 非空 → 原字节；但核对其中 `taskId`（非 null 时）∈ 本事务真实创建行，否则整事务失败 |
| `validSuppliedTaskID` | 内部 | 只校验形状；唯一性由 `tasks` 主键判（碰撞 → INSERT 失败 → 整事务回滚，不覆盖既有行） |

`EmitAuthorized` 顺序不变（授权 → 幂等回放 → 建正常子任务 → 计划事实 → result → emit 行 → commit），
只多了一步「失败 Task 用调用方 id 落库 + result 身份核对」。

## 3. route 接缝（`internal/route/emit.go` / `route.go`）

- 单目标 missing：先 `store.NewTaskID()` → 放进 `EmitFailureFact.TaskID`，**同时**放进
  `EmitResponse.TaskID`；`Pruned=nil`；`To=plan.Targets[0].Ref`（真实请求引用，不改其它状态 To）；
  `Version=plan.Targets[0].Version`（只保明确声明的正版本）。
- `len(failures) > 1` 直接报错（单目标计划不可能；不挑第一条伪造完整回执，不扩数组协议）。
- 失败 Task 的 `capability` 存**请求 basename**（`ghost`），`version` 单独一列（不再把 `ghost@2` 整体塞进 name）。
- 严格 `@` 正整数字符引用：`@2junk` / `@+2` / `@ 2` / `@02` / `@` / `@0` 都不是精确引用，
  一律沿现 `target_not_declared`（400）拒绝；坏写法永不匹配声明（含两个坏写法互相命中）。
  source 侧共用 `splitRef`，坏写法不再前缀解析成版本事实（`ghost@3junk` → version 0）。
- 回放：`EmitAuthorized` 命中已存行 → 原字节返回（route 只 unmarshal 成 Response）；不新建行、不重算 body。

## 4. 共享 DTO（`internal/protocol/emit.go`，同 5 字段，无新字段）

- `TaskID *string json:"taskId"`：语义注释明确「非 null = 本次真实创建的任务（含 phase=failed 的派发失败 Task）」；
  剪枝仍显式 `null`。
- `Version int json:"version,omitempty"`：未知/未声明（0）省略，不公开哨兵；已解析成功与显式正版本恒返回。
- Node CLI 成功判据 / 退出码 / 执行帧一字未改（`cmd/anode` 只读）。

## 5. 需要与父契约对齐的**既有事实**

- `EmitAuthorized` 的历史 result 字节由 route 在写前 marshaled（store 不重算），E1 只加身份核对；
  「同 id 回放字节相同」来自 `emits.result` 原列回读。
- 失败 Task 的 `version` 取「父 next 的唯一钉版声明」（route 现有语义），裸 ref + next 裸声明才为 0（省略）。
- 本 WT 的 `internal/server/mux.go` 引用 `internal/executor`（该包不在本分支）→ 全仓
  `go build ./...` / `make ci` 在 HEAD 6ecb938 上已是红的；E1 未触碰，也不在本卡范围。
