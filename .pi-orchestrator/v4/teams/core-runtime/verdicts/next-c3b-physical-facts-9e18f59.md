## 事实清单 / 调用链（draft）

1. **指定三个 Go 文件没有 `DELETE FROM capability_versions`**：`ref.go` 是引用解析；`orphan.go` 是孤儿查询/任务放弃；`retention.go` 删除任务、事件、去重记录、安装回执。证据：`9e18f59bfd0f405bb66d3692852cb53a88e71e78:internal/store/ref.go:1-179`、`9e18f59bfd0f405bb66d3692852cb53a88e71e78:internal/store/orphan.go:1-235`、`9e18f59bfd0f405bb66d3692852cb53a88e71e78:internal/store/retention.go:1-307`。

2. **一次定点 git grep 的显式物理 DELETE 命中**：
   `DELETE FROM capability_versions WHERE namespace_id = ?`
   位于 `9e18f59bfd0f405bb66d3692852cb53a88e71e78:internal/store/namespace.go:357`。**所属函数待验证**：该文件正文不在允许读取范围内。

3. **FK 不是完整引用保护**：
   - capability→namespace：`ON DELETE CASCADE`。证据：`9e18f59bfd0f405bb66d3692852cb53a88e71e78:internal/store/schema.sql:256`。
   - deployment→capability：`ON DELETE CASCADE`，删除能力会删除部署行，而非阻止删除。证据：`9e18f59bfd0f405bb66d3692852cb53a88e71e78:internal/store/schema.sql:496`。
   - task 的 `capability_id` 没有 FK；attempt→task 是级联删除。证据：`9e18f59bfd0f405bb66d3692852cb53a88e71e78:internal/store/schema.sql:370`、`9e18f59bfd0f405bb66d3692852cb53a88e71e78:internal/store/schema.sql:442`。

## 已有防护 / 风险

4. `RunRetention→DeleteExpiredTasks(ctx, cutoff, limit)`：同条 DELETE 检查终态、时间及不存在未 reaped attempt；**不是 capability GC guard**。证据：`9e18f59bfd0f405bb66d3692852cb53a88e71e78:internal/store/retention.go:84-85,147-155`。

5. 回执删除在显式事务中检查 capability/source 的 `installed_from`、event/task 的 `trace_id`；没有完整 Task/attempt/manifest/deploy 引用检查。证据：`9e18f59bfd0f405bb66d3692852cb53a88e71e78:internal/store/retention.go:246-269`。

6. 孤儿查询明确识别“pending task 的能力行已不存在”；这是事后诊断，不是删除阻挡。证据：`9e18f59bfd0f405bb66d3692852cb53a88e71e78:internal/store/orphan.go:79-85`。

## 所有权 / 待验证

- **不能确认现有 capability 删除是否同事务检查全部引用，也不能断言仅检查 live/版本计数**：实际命中的函数正文范围外。
- 最小**已证实涉及物理 DELETE 的额外路径**是 `internal/store/namespace.go`（上述357行）；没有证据支持把 `ref.go/orphan.go`列为物理删除入口。完整 guards 所有权闭合仍待验证，所需补充范围为该 DELETE 所属函数正文。
- 无文件改动、无新增测试；固定提交 `git show` 与一次 `git grep` 成功；未跑测试、未读 mutable execution_auth。

{"ok": false, "reason": "实际物理DELETE命中范围外的internal/store/namespace.go:357；所属函数及同事务完整引用guards待验证"}
