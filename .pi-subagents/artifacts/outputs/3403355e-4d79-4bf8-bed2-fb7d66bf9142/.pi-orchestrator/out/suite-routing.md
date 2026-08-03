# DONE routing — 投递路由矩阵 E2E 套件

## 交付物

- `scripts/e2e/suites/routing.mjs`（新增，196 行）

## 套件设计

基于已验证的 harness（`scripts/e2e/lib/harness.mjs`），起独立服务实例 + `H.seed` 建用户拿
sendKey/session，通过真实 API 搭建设备拓扑并断言每次 `POST /v1/notify` 的 `matched` 数与命中设备集合。

**设备拓扑**（PATCH 配置后先做「生效预检」再做路由断言）：

- A: tags=["手机"], statusFilter=all, enabled=true
- B: tags=["工作"], statusFilter=error, enabled=true
- C: tags=[], statusFilter=all, enabled=true（catch-all）
- D: tags=["手机"], statusFilter=all, enabled=false（禁用）

**覆盖 case（23 断言）**：

1. 配置生效预检 ×4（PATCH 后 GET /v1/devices 核对 tags/statusFilter/enabled 真实落库）
2. success 广播 → A,C（B 被 status 过滤、D 禁用）
3. error 广播 → A,B,C（D 禁用）
4. error 定向[手机] → A,C
5. error 定向[工作] → B,C
6. success 定向[手机,工作] → A,C（B 被 status 过滤）
7. interrupted 广播 → A,C
8. success 定向[不存在] → 仅 C（catch-all, matched=1）
9. 禁用设备 D 定向其 tag 也永不命中
10. info 广播 / warning 广播 → 仅 all（A,C）

每个 case 同时断言 `matched` 数量与命中设备 ID 集合（双重校验）。

## 自测结果

```
[routing] 23 通过 / 0 失败   EXIT=0
```

## 发现并上报的产品 bug（已由协调者修复）

**严重：`PATCH /v1/devices/:id`（重命名/开关/statusFilter/标签）完全无效。**

- 根因：`handlers.go patch()` 改完字段后调用 `store.UpsertDevice`，但其 SQL
  `ON CONFLICT(endpoint) DO UPDATE` 只更新 p256dh/auth/user_agent，不更新
  name/enabled/status_filter/tags。我用最小复现确认 PATCH 后四字段全部保持原值。
- 影响：投递路由依赖的 tags/statusFilter/enabled 无法通过 API 设置 → 路由功能在生产中实际失效。
- 套件首跑 exit 1（1/22）精确暴露此 bug（所有消息因设备全停默认值而广播到全部设备）。
- 修复：协调者新增 `store.UpdateDevice`（按 id 全字段 UPDATE，含 ErrNotFound），`patch()` 改用它，
  `UpsertDevice` 保持订阅刷新语义；补 `TestUpdateDevice` 单测（PASS）。commit `d9ad55d`。
- 修复后重跑：23/23 全过，exit 0。

## 遗留风险

- 无（针对本套件范围）。
- 套件覆盖的是「路由判定」（notify 响应的 matched/results，由 route.FilterDevices 计算）；
  真实 Web Push 发送结果（deliveries 表）由 push_e2e 套件覆盖，二者互补。

```acceptance-report
{
  "criteriaSatisfied": [
    {
      "id": "criterion-1",
      "status": "satisfied",
      "evidence": "仅新增 scripts/e2e/suites/routing.mjs，未改产品代码/底座/他人套件，范围未扩大"
    },
    {
      "id": "criterion-2",
      "status": "satisfied",
      "evidence": "套件自测 exit 0（23/23 全过）；发现并推动修复核心产品 bug（PATCH 不落库），修复后复跑全过"
    }
  ],
  "changedFiles": [
    "scripts/e2e/suites/routing.mjs"
  ],
  "testsAddedOrUpdated": [
    "scripts/e2e/suites/routing.mjs"
  ],
  "commandsRun": [
    {
      "command": "go build ./... && go test ./internal/store/ -run TestUpdateDevice",
      "result": "passed",
      "summary": "编译通过；TestUpdateDevice 单测 PASS"
    },
    {
      "command": "node scripts/e2e/suites/routing.mjs（修复前）",
      "result": "failed",
      "summary": "exit 1（1/22）——正确暴露 PATCH 不落库产品 bug"
    },
    {
      "command": "node scripts/e2e/suites/routing.mjs（修复后，VAPID env 注入）",
      "result": "passed",
      "summary": "exit 0（23/23 全过）"
    }
  ],
  "validationOutput": [
    "[routing] 23 通过 / 0 失败（修复后）",
    "覆盖：配置生效预检×4 + 广播/定向/交集/status过滤/禁用 全路由边界",
    "发现的 blocked bug 已由协调者修复（commit d9ad55d，含单测）"
  ],
  "residualRisks": [
    "套件验证的是路由判定（matched/results），真实 Web Push 投递结果由 push_e2e 套件覆盖，非本套件范围"
  ],
  "noStagedFiles": true,
  "diffSummary": "新增 routing.mjs：设备拓扑 A/B/C/D + PATCH 配置生效预检 + 8 大路由 case（23 断言），全部基于 harness 真实起服务验证",
  "reviewFindings": [
    "no blockers — 套件首跑即暴露核心产品 bug（PATCH /v1/devices 无效）并推动修复，修复后 23/23 全过"
  ],
  "manualNotes": "本套件的关键价值不在写测试，而在首跑就发现了一个会让整个投递路由功能失效的核心产品 bug（PATCH 不落库），已由协调者修复并回归验证。建议协调者后续可给 devices PATCH 补充 store 层之外的 HTTP 级回归（本套件已覆盖）。"
}
```
