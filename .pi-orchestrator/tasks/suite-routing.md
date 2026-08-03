# 任务：编写 E2E 套件 `scripts/e2e/suites/routing.mjs`

先读 `/Users/zheng/code/anotify/.pi-orchestrator/tasks/e2e-suites-common.md`（公共约定）。

写投递路由矩阵套件。核心：用 H.seed 拿 sendKey+session，通过 /v1/devices 创建**不同 tags/statusFilter/enabled 组合**的设备，然后 POST /v1/notify 并断言响应里的 `matched` 数量和 `results` 命中了正确的设备。

投递规则（必须逐条验证）：
> 投递 ⟺ enabled ∧ statusMatch(device.statusFilter, msg.status) ∧ tagMatch
>
> - 消息无 deviceTags → 广播到所有 enabled 设备
> - 设备无 tags → catch-all 收一切（含定向消息）
> - 双方有 tags → 交集≥1（ANY，非 ALL）
> - statusFilter=all 全过；=error 仅 error；=success 仅 success；interrupted/info/warning 仅 all 时过

## 测试拓扑（建议）

为一个用户创建这些设备（用 session 调 /v1/devices POST，再 PATCH 设 tags/statusFilter/enabled）：

- A: tags=["手机"], statusFilter=all, enabled=true
- B: tags=["工作"], statusFilter=error, enabled=true
- C: tags=[], statusFilter=all, enabled=true（catch-all）
- D: tags=["手机"], statusFilter=all, enabled=false（禁用）

## 断言矩阵（每条上报后核对 matched + 命中设备集合）

1. {status:success} 无 deviceTags → A,B*,C（广播；B 的 statusFilter=error 不过 success → 不命中；D 禁用不命中）→ 实际命中 A,C（B 被 status 过滤）。*注意 B 的 statusFilter=error 过滤掉 success*
2. {status:error} 无 deviceTags → A,B,C（B 的 error 通过；D 禁用）
3. {status:error, deviceTags:["手机"]} → A（tag+status 都过）；C（catch-all）；B 不命中(tag不符)
4. {status:error, deviceTags:["工作"]} → B(tag+status 过), C(catch-all)；A 不命中
5. {status:success, deviceTags:["手机","工作"]} → A(tag 过+status all 过), C；B 不命中（statusFilter=error 过滤 success）
6. {status:interrupted} 无 deviceTags → A,C（B=error filter 不过 interrupted）
7. {status:success, deviceTags:["不存在"]} → 仅 C（catch-all）；A/B 不命中 → matched=1
8. 禁用设备 D 任何情况都不接收（在上面各 case 中验证 D 从不出现在 results）

注意：matched 是 route.FilterDevices 计算的结果；results 含每命中设备的 {device,status}。设备 endpoint 用 H.makeDevice 生成（推送不会真发，因 endpoint 是假的/example.com——push dispatcher 会失败写 deliveries，但不影响 matched 断言）。自测跑通（exit 0）后上报。
