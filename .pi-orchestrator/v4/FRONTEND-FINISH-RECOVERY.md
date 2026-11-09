# T2 C2 后 fresh-context 收尾安排（父批准，安全检查点已完成）

原T2已safe BLOCKED/retired，clean HEAD `767c27c962d60250e34603aef7c24d2490a8409d`（C2生产 `c052230`），最后延迟通知确认C2 accepted(verified)。全部writer/命令已收齐，F3与终review未做；不是T2DONE。父已只读保存 `/tmp/anotify-v4-frontend-finish-recovery/frontend-products-and-evidence.patch`（base0e0ff0f、仅T2产品/团队证据）+SHA/checkpoint，旧worktree不动。

原恢复centurion上下文约287K，实施期180分钟为advisory，当前Node/config/保留功能仍推进，不应停止writer或伪造phase通过。

## 安全接续条件

1. 当前唯一c2 writer收齐实际Node/config/保留功能验收：root/config同一CAS、Token明文一次、namespace交集、vars来源、limits两层、retention0永久、真实busy与unknown、保留认证设备入口。
2. clean阶段提交（禁止push/merge），确认无正在写入或裸后台command；完整截图/日志/JSON/lateACK与工具acceptance差异文件化。
3. 团队 `RECOVERY-CHECKPOINT.md` 列所有实际阶段commit与通过结果、不能算verified的收集项、下一小任务卡；保存F3-FACTS/F3-TASK/TEST-MAP。
4. 若需要fresh接续，c2后不要再在大上下文扩写F3，mail父准确HEAD后明确BLOCKED needs fresh-context continuation（安全settled后，非DONE/非继续等writer的final）。父启动受支持subagent新隔离finish团队接同范围，不重做已完成页面、不丢代码。

## 尚未完成

至少F3：sitegen/cmdsitegen/cmdpreview五spec读取与fixture（T1真实spec后集成，不回退旧openapi）；前端专属五E2E套件重写与保留功能断言；fresh correctness/coverage/simplicity review；必要返工；最终固定makefe/sitegen测试/Node测试，工具unknown项真实机械close。

父最终仍须T1/T3/T2真实跨队build/CI/全量E2E/页面验证。mock浏览器分阶段通过不等产品整体通过。

## 所有权与恢复来源

不变：T2仅web-src/web静态、sitegen/fe-preview、规定的前端tests，T1 server/GoAPI/tooling、T3protocol/anode，不合他队分支。恢复以原团队最终HEAD+dirty证据快照为准，不重读287K旧JSONL/全仓。

参考已见阶段：F1支持+修复恢复aad4b15、能力ca450e7、Source5d79bbf、诊断7a7e96e、Playbook090365e；A2docs588e34c。C2已提交c052230，99Node/13Chrome含44checks/49shots阶段通过；早期apiList.items误消费与docs内部滚动已准确修复/验收。最后checkpoint767c27c，F3未开工，后续以文件完整恢复。A3技术已848正文，可能需原样同步支持；没有父强制compact能力，resume不是清空context。
