# T1 fresh-context 安全接续安排（父批准，待 S 检查点）

## 原因与边界

原 core-runtime centurion 会话约278K上下文，主动请求在 G1-6S 安全收口后以新鲜上下文继续，防止后续大任务遗漏契约。批准恢复编排，不压缩功能、不减测试、不自动合并部分完成代码。

当前父工具没有强制 compact 子会话入口；resume/continue保留原上下文。用受支持 subagent 新centurion隔离worktree接已保存分支，不手动启动Pi或改harness/settings。

## 旧团队交接前必须完成

1. 当前唯一S writer收口：实际task_attempts/机器snapshot/reaped槽位事实及具名反例；若T3共享Go delta还未交则明确实际代码依赖，不借缺旧字段造compat绿。
2. 所有writer/review结果收齐；无裸长命令/未结写入任务。commit当前团队分支（禁止push/merge），确保dirty为空或列每个保护文件。
3. 保存 `.pi-orchestrator/v4/teams/core-runtime/RECOVERY-CHECKPOINT.md`：HEAD、阶段与实测、剩余范围、可直接接续的小任务卡、实际接口vs未冻结部分、测试旧→新映射、HTTP/A1/A2/A3接缝、所有权。
4. 只有交接安全完成才返回明确 `BLOCKED needs fresh-context continuation`，不是DONE或“继续等writer”的final。阶段不能伪造done/verification_passed。

## 现已记录的代码检查点（新团队需取最终HEAD而非此旧表）

- 61c7629：protocol/能力artifact基础。
- 38cc153：v4baseline与能力版本规则。
- a39ce4e：三层配置/clusterNode/投影。
- 52b17a6：task/emit/source/GC渐进实现。
- c654058：纯Playbook目录/render/plan。
- 8b1b95c：第一轮取消/输入修复（后续复审仍有attempt机器pin/历史槽major，不能当最终）。
- 1b420e6：T3权威63570df E1/A1纯协议支持。
- 8497d7f：Store安装事务/回执实例历史。父独立阶段包验证PASS，不是全Go门禁。
- 9a2b57d：父A2文档原样支持。

## 必须接续的完整范围

T1仍负责store/route/ws/source/trigger/lease/server/API/OpenAPI/契约/构建/seed/非前端E2E/旧agent退役。protocol、cmd/anode归T3；web/sitegen/前端专属tests归T2，不得抢写。

未完成至少：S后复审、原store测试闭包、原子CAS接口、A1typedattemptmanifest/GC引用；G2中枢调度与WS/日志部署/emit；G3namespace资源API与Status/status派生/OpenAPI；G4工具链/seed/e2e及真实最终门禁。具体以旧团队最终checkpoint逐项确认，不把此表替代实际验收。

最终固定：`go test ./... -count=1`、`make contract`、`make e2e-quick`。父另跑fresh review及跨T1/T3/T2完整CI/build/E2E/页面验证；不能因fresh恢复放宽。

## 协议状态

A3物理slot/reap**技术裁决已closed**，父4af336f和SLOT-HANDOFF.md权威；等待的是T3实际Go delta，不是父未决方案。Nodeaccept真slot→grant echo、lease/slot历史ACK、presence、slot_unavailable启动前不计attempt。A2产物新EnvArtifactDir在0a5c551明确；已有protocol63570df还须接T3后续A2/A3/headerguard纯delta。

## 尚未授权的动作

旧团队未safe checkpoint前不启动第二个T1 writer；不改/删旧worktree、不强推/删除已违规发布远端分支、不动用户settings、不合默认分支。新team不全量重读旧JSONL，依文件checkpoint与精确必要事实补水。
