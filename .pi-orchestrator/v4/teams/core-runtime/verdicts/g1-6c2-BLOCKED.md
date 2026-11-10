C2 **阻塞，未改代码**。

冻结卡要求 Source `displayName` 更新 CAS，但：
- `internal/store/source.go:57–79`：结构体及列清单均无 `DisplayName`。
- `internal/store/schema.sql:545–559`：`sources` 表无 `display_name`。
- C2 禁止修改 schema，并明确“必需字段不存在时停止报告”。

无法在当前文件权限内真实持久化该字段；未擅自扩范围、塞入 config 或删减需求。

**产出文件**
- `/tmp/anotify-core-recovery-c2-blocked.log`：证据、假设、建议下一步与验证状态。

**验收状态**
- 新增测试/公共方法：无。
- 四个固定门禁：全部 **NOT RUN，无退出码**（实施前发现契约冲突）。
- 红绿日志：无；未伪造失败/通过证据。
- 原始三门禁及前轮 194 测试证据保持不变。
- 无提交、push、merge、MR。

建议父会话先授权精确 schema 前置卡并完成布局/往返验证，或明确修订并延期 Source displayName 范围，再重派 C2。

DONE V4-CORE-RUNTIME-RECOVERY / C2 — BLOCKED，**非 T1 DONE**。

{"ok":false,"reason":"C2 必需的 Source displayName 在 source.go:57–79/schema.sql:545–559 中不存在，而 schema 只读；未改代码，需父会话修订契约或授权前置实现。"}
