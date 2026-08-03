DONE edge_cases + frontend

# E2E 套件交付：edge_cases.mjs + frontend.mjs

## 自测结果（真实跑通，exit 0）

| 套件 | 结果 | 退出码 |
| --- | --- | --- |
| `scripts/e2e/suites/edge_cases.mjs` | **18 通过 / 0 失败** | 0 |
| `scripts/e2e/suites/frontend.mjs` | **45 通过 / 0 失败** | 0 |

## 覆盖 case

### edge_cases.mjs（边界/并发/Unicode，18 断言）

1. 并发上报 10 条 → 全 200，Replay 后 seq=1..10 无重复无缺口（验证 broker 并发 seq 事务）
2. Unicode/Emoji/换行/引号/`<script>` → 200 且 Replay 内容逐字一致（按文本存储）
3. 超长 title(5000)/body(50000) → 不 500（行为明确）
4. 空 deviceTags `[]` vs 省略 → 均为广播（matched 一致）
5. deviceTags 含空串/纯空格 → 归一化剔除不报错、按广播
6. 重复 endpoint 设备 → Upsert，列表只保留一个
7. limit=99999 → 钳制不 500、返回数组
8. sinceSeq=-5 / =abc → 不 500，负数按 0 处理返回全部

### frontend.mjs（前端渲染 + 路由守卫 + 真实数据，45 断言）

- **路由守卫**：未登录访问 index/receivers/keys/security → 全部自动跳 login.html
- **login 公开页**：未登录停留本页不跳、正常渲染
- **已登录真实数据**：注入会话访问 index → 不显示「演示数据」徽章、显示真实通知
- **6 页 × 桌面1280/移动390 双视口**：无 JS pageerror、无横向溢出、可滚动到底（/v1/* 401/404 为预期降级，区分处理）

## 发现并修复的 3 个真实前端 bug（非测试问题）

1. **index.html `normalize()` 字段名不匹配**：原代码读 camelCase（`n.title/n.status/n.payload`），但 `/v1/notifications` 直接序列化 `broker.Message`（Go 无 json tag → 返回 PascalCase `Title/Status/Payload`）。真实通知被渲染成「（无标题）/信息/0/0 设备已送达」。
   修复：`web/index.html` 的 `normalize()` 每个字段兼容 PascalCase（真实 API）与 camelCase（演示/契约）。

2. **keys.html + security.html 路由守卫失效**：两页用自有裸 `fetchApi`（任何非 200 都返回 null 降级演示数据，**无 401 处理**），未登录不跳登录页（index/receivers 用的 `Anotify.api` 有守卫所以正常）。
   修复：两页 `fetchApi` 在 401 时委托给 `Anotify.api`（内置守卫会跳登录），其余非 200 仍降级演示数据。

3. ~~docs.html 移动 390px 横向溢出 26 元素~~ → **诊断为非产品 bug**：溢出元素是 `.code-body`（`overflow-x:auto; white-space:pre`）内的语法高亮 `<span>`，属合法横向滚动代码内容。是测试断言的排除清单未覆盖 `.code-body`/`code`，已修正测试（web_verify 同源逻辑同样会误报此类）。

## 产出文件

- `scripts/e2e/suites/edge_cases.mjs`（新建）
- `scripts/e2e/suites/frontend.mjs`（新建）
- `web/index.html`（修 normalize PascalCase 兼容）
- `web/keys.html`（修 fetchApi 401 守卫）
- `web/security.html`（修 fetchApi 401 守卫）

## 遗留风险 / 需协调者决策

- **产品一致性（建议）**：`/v1/notifications` 返回 PascalCase 字段，与 `api/openapi.yaml` 暗示的 camelCase 不一致。我已在 index.html 前端做兼容（向后安全）。建议协调者决定是否给 `broker.Message` 加 json tag 统一为 camelCase；其他消费方（如有）也需同步。**这是潜在的一致性 bug 源头。**
- **集成说明**：`web/index.html`、`web/keys.html` 的修复在开发过程中被协调者收入提交 `013284a`；`web/security.html` + `frontend.mjs` 的后续 refinement 当前未提交（git 工作区），留给协调者统一合并。我未提交任何文件（暂存区为空）。
- **未触碰**：persistence/routing/security 等其他套件（兄弟子 Agent 负责），harness 底座未改。

## 自测命令

```bash
cd /Users/zheng/code/anotify
ANOTIFY_VAPID_PUBLIC_KEY=$(python3 -c "import json;print(json.load(open('vapid.json'))['publicKey'])") \
ANOTIFY_VAPID_PRIVATE_KEY=$(python3 -c "import json;print(json.load(open('vapid.json'))['privateKey'])") \
node scripts/e2e/suites/edge_cases.mjs   # → 18 通过 / 0 失败, exit 0
node scripts/e2e/suites/frontend.mjs     # → 45 通过 / 0 失败, exit 0
```
