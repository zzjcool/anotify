# v4 剩余交付与发布前验收

当前 E1 emit 失败 Task 回执修复已经完成并通过 worker、父级固定四项门禁与三路独立复审。按用户要求，暂停继续源码实施，将剩余工作转为可分派、范围与验收清晰的 issues。

## 已完成，不重新开工
- G1/C3 store 与结果授权事实；S1 原子 ingress/failedTask/proposal/reject/deploy 事务接口。
- Node/anode 产品及真实日志采集、可证索引完成性、publication 修复。
- G2-R1 配置 revision/提案稳定/授权顺序/日志关联/next 引用与有限测试返修。
- E1 真实 failed Task ID 回执、回放、严格 ref 与未知 version presence。
- 前端 UI、五文件 sitegen reader，mock 模式完整195组/353视图/304矩阵已独立验收。

这些是**局部交付**，并不意味着完整 HTTP/五API/工具链/真实 Node E2E/前端真 API/发布门禁已完成。

## 基线交接（重要）
- 核心本地集成源码：`b30e998767544f475363815e5c03037cf9de0a1f`，工作树 `.pi-subagents/runs/r-57adb51b/worktrees/v4-core-delivery/.pi-subagents/runs/r-01be4aa7/worktrees/core-g2-resume`，分支 `pi-subagent/core-g2-resume-52fc54d4`。
- 前端本地交付：`bf21f18e433b48d093a5d597cc26ca5aa7353700`（code `1abc042`），工作树 `.pi-subagents/runs/r-57adb51b/worktrees/v4-frontend-delivery`。
- 当前交付尚未 push/merge 到默认分支；旧 main、PR #329 草稿不代表上述完整基线。分派前协调者必须提供这些集成 commit/补丁与冻结契约，不能让后续 worker 从旧 main 重做已经完成部分。
- 交接索引：`.pi-orchestrator/v4/issues/`；契约 D1–D6、W1–W6、A1/A3、S1、E1、G3-HTTP-SPEC-RESUME-HANDOFF。正文已给出必要范围与规则，不以“只看不到的文档路径”代目标。

## 统一红线
无 v3 API/协议/旧库迁移兼容层，旧库明确拒绝且不删除用户数据；旧中央 executor/notify.phone Capability 刻意退役，独立 auth/notify/devices/broker/push 必须保留。Resource 使用 camelCase/轻量信封/只读 status/opaque CAS，空列表 `[]`。Node 当前 credential/ns/attempt/lease/token/cancel 授权须同事务；日志/产物/Task 事实不能伪造。

同一工作树只有一个源码 writer；server/mux、Makefile、五 spec、共享 harness/FE suite 按 issue 所有权序列化。worker/scout 不显式指定模型/preset。发现产品 bug 修产品，不弱化断言/超时/错误白名单；机械 full log/真实 exit 与 fresh review 均为完成条件。

## 子任务与实施顺序
- [ ] https://github.com/zzjcool/anotify/issues/331 — [v4][G3a] 装配 HTTP 资源 API 并保留独立认证与通知域；可从当前验收基线启动
- [ ] https://github.com/zzjcool/anotify/issues/332 — [v4][G3b] 打通 Node 受权产物上传、结果托管与私有下载；依赖 #331
- [ ] https://github.com/zzjcool/anotify/issues/333 — [v4][G3c] 落地 exact-five OpenAPI 与引用、鉴权、DTO 契约门禁；依赖 #331、#332
- [ ] https://github.com/zzjcool/anotify/issues/334 — [v4][G4a] 切换服务入口、开发播种与构建镜像工具链；依赖 #331、#332、#333
- [ ] https://github.com/zzjcool/anotify/issues/336 — [v4][G4b] 迁移非前端 E2E 底座并验证真实 Node 全链路；依赖 #331、#332、#334
- [ ] https://github.com/zzjcool/anotify/issues/335 — [v4][FE] 接入真实五 API 并校准失败 Task 与历史日志场景；依赖 #331、#332、#333、#334、#336
- [ ] https://github.com/zzjcool/anotify/issues/337 — [v4][HARDEN] 补强严格结构拒错与确定性安全回归；可从当前验收基线启动
- [ ] https://github.com/zzjcool/anotify/issues/338 — [v4][FINAL] 完成跨团队集成与发布前全量门禁；依赖 #331、#332、#333、#334、#336、#335、#337

主线为 HTTP资源 → 产物传输 → exact-five API → 工具链 → 真实 Node/E2E共享底座 → 前端真实集成 → 最终全门禁。先冻结共享 harness 再由前端消费，避免循环依赖；加固可在隔离分支并行，最终集成必须等所有前置完成。

## 总完成标准
`go test ./... -count=1`、`make contract`、`make e2e-quick`、`make ci`、`make build`、完整 `make e2e` 及真实 Node/HTTP/前端 dark-light桌面手机验收全部通过；唯一源码与 spec/binary/指纹身份可对账。未经进一步授权不 push/MR/merge/deploy；issue 创建不等于发布源码。