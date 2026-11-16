# 会话 01a1c041 接续账目

用户授权：继续会话 `01a1c041-ddc8-74da-bcfb-d4a907ed097c` 的 v4 全量改造。派发 worker/scout 不传 `model` 或模型 preset，采用默认配置；传递给组长的子任务亦遵守此要求。

## 只读恢复事实

- 原会话：`/root/.pi/agent/sessions/--root-code-anotify--/2026-11-09T12-21-44-521Z_01a1c041-ddc8-74da-bcfb-d4a907ed097c.jsonl`。最新压缩记录 L2619，最后用户继续请求 L2665。
- 父分支 `feat/p9-capability-v4`，HEAD `b5adeda8c766019c6ffc5d7f89d10be6ea323e7d`。v4 生产代码尚未集成到父分支。
- 原任务持有者已经中断，未发现活 writer；新派工按现有独立 WT 接管，不对原孤儿反复 resume/collect、不删除证据。
- Node 已冻结：`/root/code/anotify/.pi-subagents/runs/r-57adb51b/worktrees/v4-anode`，HEAD `5a3dae3879662a7487f7248ab83af0776178a9cd`；本轮只读。
- 核心 G1 已收口：`/root/code/anotify/.pi-subagents/runs/r-57adb51b/worktrees/v4-core-delivery`，HEAD `50fb9ac7c79d07c9a6c439c8d685898ea17483d8`，代码 epoch `32ec26b`。G1 收口不等于完整 T1 DONE；G2/G3/G4 和原最终三门禁仍待。
- 前端：`/root/code/anotify/.pi-subagents/runs/r-57adb51b/worktrees/v4-frontend-delivery`，HEAD `4db5b35646402a01cf14e2ec3998258eabfb1ad5`。第二次完整五套件为 191 PASS / 4 FAIL；观察 run `final-28bbef29-cbf1-48f6-9227-1aa83486c330` 只跑 UI/coverage，82/1 与 40/0，不是产品最终验收。
- 最新真实 Trace 失败：同 document API 200、7 tasks，图库 defer 未完成时访问 `window.AnotifyGraphs.taskChain` 抛 TypeError；已有 `TRACE-STARTUP-HANDOFF.md` 冻结最小授权。原 trace-fix 子任务没有实际代码写入。

## 接续工作所有权

1. Trace 修复：唯一 worker，直接使用既有前端 WT。授权仅 Trace 启动依赖、相关确定性回归及必要注释/证据；按 `TRACE-STARTUP-HANDOFF.md`。旧 Node199、完整195组、304矩阵和旧失败均保留。代码冻结后 fresh review 与新唯一 epoch/READY 的五套件各一次，最多两 suite 同时运行。
2. 核心 G2：使用既有 core-delivery WT，按 `teams/core-runtime/task-cards/G2.md`。组长仅调度一个源码 writer，route/ws/lease/source/trigger/logging、精确退役旧 executor/cmd-agent；store/protocol/artifact/server/API/工具/Node/前端只读。原 G1/C3/Node 不重做；契约接口缺口明确阻断上报。
3. G3/G4：G2 验收与独立 review 后再序列化推进；不提前跨 G2 所有权写入。
4. 父最终集成仍待独立 review、跨队验收、`make ci`、`make build`、完整 `make e2e`；局部门禁不冒称整体 v4 完成。

## 安全与证据

- 父 `.pi/settings.json` 是既有未提交修改，禁止纳入提交或改动；不碰 `.env*`、`.git`、`node_modules`、用户数据。
- 只使用 canonical `.pi-subagents` 路径，命令必须核对 cwd/root/GOMOD；禁止误写 `.pi/subagents`。
- 不 push/MR/merge/rebase/reset/squash/forcepush，不删旧 worktree/session/日志，不复用已执行 run/ID，不弱化断言或延长原等待阈值迁就产品 bug。
- 本轮恢复报告：新 scout `resume-state` 与 `resume-worktree` 的 session/collected output；实际启动句柄与结果在后续 STATUS 中登记。
