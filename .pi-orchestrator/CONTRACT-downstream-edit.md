# 冻结契约 · 在能力编辑里改下游（替代出口页）

> 唯一事实源。日期 2026-11-06。用户原话：「这个不应该在能力的编辑上可以直接做吗，只是现在没有优先级」。

## 0. 为什么

现状（已侦察）：

| 操作 | 能力侧 | 出口页 |
|---|---|---|
| 建下游 | ✅ 新建/批量/发版 `links[]`、详情「+ 下游」 | ✅ 第 5 条重复路径 |
| 删下游 | ❌ 没有 | ✅ 唯一入口（`DELETE /v1/edges`） |
| 改优先级 | ❌ **连 HTTP 端点都没有**（`store.UpdateEdgePriority` 零调用方） | ❌ 也没有 |

结果：用户在能力页加了 3 个下游，想删掉 1 个 → 必须去另一页；而能力页还写着「点『编辑』可调优先级」（`capabilities.html:2131`）——**承诺了不存在的功能**。

目标：**能力的下游只能在能力里改**，出口页删除。

## 1. 边怎么表达

一条能力边 = `(from_ref=能力名, from_port='out', to_ref, priority, literals)`。

- `from_ref` 是**能力名**（不是版本）：一个能力的所有版本共用同一组出边（升级不改拓扑）。
- `from_port` 固定隐式出口 `out`（`store.ImplicitPort`）——界面与 API 都不出现它。
- `to_ref` 是 `name`（跟随最新）或 `name@version`（钉死）。

## 2. 新端点：整体替换下游

```
PUT /v1/capabilities/{name}/downstreams?namespaceId=…
Content-Type: application/json

{
  "links": [
    {"toRef":"script.echo",        "priority":0},
    {"toRef":"notify.phone@1",     "priority":10, "literals":{"to":"+8613800000000"}}
  ]
}

→ 200 {"name":"x","links":[{"toRef":"script.echo","priority":0,"toCapabilityId":"cap_…","resolvedVersion":1}]}
```

语义是 **replace（整体替换）**，不是 append：

1. 校验（全部先做，任一失败 → 400，**一个字节都不写**）：
   - `toRef` 必须解析得到（`store.ResolveRef`），否则 400 `unknown_target`（带名字）
   - `toRef` 的名字不能等于 `{name}` → 400 `self_link`
   - 条目数 ≤ 100 → 400 `too_many_links`
   - `priority` ∈ [0, 1000]，缺省 0
   - `literals` 必须满足目标 `params_schema`（复用 `checkEdgeLiterals`）
   - 重复 `toRef` → 400 `duplicate_target`
2. 一个事务里：
   - `DELETE FROM edges WHERE from_is_source=0 AND from_ref=? AND to_ref NOT IN (…)`（**只删这个能力的出边**，不动别人、不动入口边）
   - 其余 `INSERT … ON CONFLICT(from_ref,from_port,to_ref) DO UPDATE SET priority=?, literals=?`
3. **空数组是合法的**：清空＝这个能力没有下游（终端能力）。界面留空提交就是清空。
4. 返回落库后的完整集合（含 `toCapabilityId` / `resolvedVersion`，方便前端回显）。

为什么是 `PUT` 而不是 `PATCH`：语义是「这组就是全部」，不是增量。PATCH 会让「删掉一个」只能靠哨兵值表达。

## 3. 关掉旧入口

- `POST /v1/edges`：**能力边**（`fromIsSource=false`）→ `405`，`{"code":"edge_write_removed","error":"能力的下游请在能力里改（PUT /v1/capabilities/{name}/downstreams）"}`。
  **入口边**（`fromIsSource=true`）保持可用：源页面自己走 `replaceEntryEdgesTx`，但 e2e/高级用法仍需给「没有 sources 行的来源名」接入边。
- `DELETE /v1/edges`：保留（运维纠正用），但控制台不再有入口。
- `GET /v1/edges`：保留（图、详情、trace 都靠它）。
- `store.CreateEdge` / `DeleteEdge` / `UpdateEdgePriority`：保留（内部实现与测试造数）。

## 4. 界面

能力**编辑**（发版弹层）里的「下游能力（候选）」：

- 不再因为「已有下游」而隐藏；进入编辑时**预填当前下游**（名字 + 优先级）
- 每行：能力名（可搜索候选，已有 `#cap-down-targets` datalist + 候选胶囊）+ **优先级数字输入** + **删除按钮**
- 保存 → 调 `PUT /v1/capabilities/{name}/downstreams`（与发版解耦：改下游不产生新版本，因为拓扑不属于产物）
- 新建能力时仍是「`links[]` 随能力一起创建」（一条链路建完），语义一致
- 优先级留空 = 0；数字越小越优先（已有的候选选择规则：`to_ref` 字典序 → priority → 组内轮盘）

## 5. 删掉出口页

`web-src/pages/edges.html` 整页删除 + 导航项 + 4 语言 `edges.*` 文案 + `caps.edges.moved_*` 引导卡 + mock 的三个 handler + e2e 的 `checkEdgesPage`。
**保留**：`docs.openapi.tag.edges`、`pb.edges.*`（预案页）、`NS_PATHS` 里的 `/v1/edges`。

## 6. 分组

| 组 | 范围 |
|---|---|
| **13（后端）** | `internal/server`（新端点 + 封 POST 能力边 + openapi）、`internal/store`（replace 事务 + 单测）、`internal/server/*_test.go` 对应调整 |
| **14（前端）** | 能力编辑下游行编辑器（名 + 优先级 + 删除）、删出口页/导航/文案/mock、frontend e2e |
| 协调者 | 13 与 14 之间冻结 API；e2e 造数迁移（12 个套件用 `POST /v1/edges` 建能力边） |

## 7. 门禁

13：gofmt / vet / build / `go test ./...` / `make contract`。
14：`check-classes` / `check-tailwind-css` / `make fe` / `run_all.sh frontend`。
