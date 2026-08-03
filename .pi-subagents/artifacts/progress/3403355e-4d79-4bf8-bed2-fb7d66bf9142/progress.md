# 进度：E2E 套件 edge_cases + frontend

## 状态：✅ 两个套件均全绿（exit 0）

| 套件 | 断言 | 结果 |
| --- | --- | --- |
| edge_cases.mjs | 18 通过 / 0 失败 | ✅ exit 0 |
| frontend.mjs | 45 通过 / 0 失败 | ✅ exit 0 |

## 已完成

- 编写 `scripts/e2e/suites/edge_cases.mjs`（边界/并发/Unicode，18 断言）
- 编写 `scripts/e2e/suites/frontend.mjs`（前端渲染 + 路由守卫 + 真实数据，45 断言）

## 发现并修复的 3 个真实前端 bug

1. **index.html normalize() 字段名不匹配**：读 camelCase，但 /v1/notifications 直接序列化 broker.Message（Go 无 json tag → PascalCase）。真实通知渲染成「（无标题）/信息/0/0」。已修 normalize 兼容 PascalCase。
2. **keys.html + security.html 路由守卫失效**：用自有裸 fetchApi（任何非 200 都返回 null 降级演示数据，无 401 处理），未登录不跳登录。已修：401 委托给 Anotify.api（内置守卫）。
3. docs.html 移动 390px 横向溢出 26 元素 → 诊断确认是 .code-body 内语法高亮代码（overflow-x:auto，合法横向滚动），**非产品 bug**，是测试断言排除清单不全，已修正测试。

## 遗留（上报协调者决策）

- **产品一致性**：/v1/notifications 返回 PascalCase 字段，与 openapi.yaml 暗示的 camelCase 不符。已在 index.html 前端做兼容。建议协调者决定是否给 broker.Message 加 json tag 统一为 camelCase（届时前端兼容层仍向后兼容）。
- web/index.html、web/keys.html 的修复已被协调者集成进提交 013284a；web/security.html + frontend.mjs 的后续 refinement 未提交（留给协调者合并）。

## 更新于

本次运行（edge_cases 18/18 + frontend 45/45 全绿）
