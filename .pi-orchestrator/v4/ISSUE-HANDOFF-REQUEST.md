# 用户新指令：当前任务验收后转 issue 拆分交接

用户要求：当前任务完成之后，把剩余任务拆分成 issue，定义目标、验收标准、实施范围等，便于后续拆分。

## 当前收口点

当前唯一源码writer为 `emit-failed-task-receipt-fix`（E1，已创建失败Task的emit回执身份/声明version/strictref小接缝），先完成源码freeze、fresh有限复审、固定机械验收。原G2-R1/S1/Node日志/前端195局部门禁成果保，不因此重新开发。

## 后续行为调整

- E1完成验收后**不自动派发G3/G4大实施**。已冻结 `G3-HTTP-SPEC-RESUME-HANDOFF.md` 和原G3/G4卡作为事实/范围基础转 issue。
- 先只读并行scout梳理剩余delivery/fileowner/实际验收命令与repo issue惯例，主会话不串行探仓。
- 每个 issue 要有：明确目标与背景、纳入/刻意不做项、文件/模块所有权、输入与输出契约（引用父D/S/R编号）、实施范围/最小产出、机械验收命令+新case、依赖/集成顺序、完成定义与风险。共享server/mux/Makefile/spec/parity之类只能给一个writer序列化，不假称都可并行。
- 按真实剩余工作拆：HTTP资源与独立auth/notify/device装配；真实Node受权上传/结果spool/私有下载；exact-fiveOpenAPI/checkers；sitegen五文件消费者与前端真实新Task/日志契约适配；工具链/serverCLI/seed/Docker/Makefile；非前端E2Eharness与真实Node场景；父最终跨队集成/fullgate。具体issue数量按scope依赖事实裁定，不重新做已完成的G1/C3/Node/R9/E1。
- 用户授权目标是issue交接，优先在仓库真实tracker创建；若缺tracker工具/登录或平台不可达，先完整保存可发布issue草稿和索引、明确未发布原因，不把本地文件冒真实issue链接。
- 不push/MR/merge/rebase/reset/squash/清旧WT或日志/修改env/settings，所有worker/scout默认模型不显式model/preset。
