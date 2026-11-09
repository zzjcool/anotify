# T2 F3 收尾的新受管入口（父批准）

## 运行时原因

新v4-frontend-finish恢复成果后，第一次planning→implementing实际工具回执却为`failed→implementing refused: terminal phase failed cannot transition`；它未主动进入failed、未派writer。根因未确认，不能伪造数据库或忽略已声明终态。该节点已safe BLOCKED/retired，clean c7503fa。

父使用工具支持的**不声明阶段机的新centurion交付模式**：新隔离节点v4-frontend-delivery，按文件记录planning/implementation/review/verification，不调用legion_phase、不恢复旧终态；所有worker仍显式verify，fresh三角review和父机械验收完全保留。此为重新创建合法任务入口，不把旧failed修改成done、不绕过旧机器条件。

## 恢复成果

复用 `/tmp/anotify-v4-frontend-finish-recovery/frontend-products-and-evidence.patch`（base0e0ff0f、SHA），它已含全部F1/F2产品及证据。c7503fa新增只有收尾契约/阻塞事实，分别只读其FINISH-CONTRACT.md/FINISH-RECOVERY-STATUS.md，不重做任何页面或已完成测试。旧WT全部保留，没有活writer。

## F3与最终门禁不削

- sitegen/cmdsitegen/cmdpreview五spec解析与明确missingwarn/allmissingempty、无旧openapi自动fallback；T1真spec和harness由父集成。
- 仅T2允许的5前端E2E新语义，显式mockonly可完整运行、默认真服务不能失败自动mock，保留19page/认证device/权限/UX行为与测试映射。
- fresh正确性/回归、覆盖、简洁性三角review，critical/major返修；只有最终代码冻结后makefe/sitegen测试/Node测试全过并收齐才报告完整本队完成。
- 父独立检查正确WT/HEAD/真实exit，工具verifier若错cwd如实rejected，不伪造verified。最终真实T1T3T2 CI/build/fullE2E仍独立门禁。
- 同worktree单writer，无PUSH/MERGE/MR，无修改settings/env/node_modules/他队文件。

进度只mail/file；只有完整交付或确切无法续的安全BLOCKED才final。新模式不需要等待不存在reviewer角色或阶段机root状态，不重读旧300Ksession。
