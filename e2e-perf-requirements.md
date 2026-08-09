# 重构：e2e 测试体系性能与架构（长期方案）

> 协调者定（需求边界清晰，直接产出）。事实源：本文件 + scout-context.md。
> worktree：`.pi-orchestrator/worktrees/wt-e2e-perf`，分支 `refactor/e2e-perf`（off main 269981a）。

## 1. 问题陈述

全量 `make e2e` 实测 **491 秒（8 分钟）**，远超 subagent bash 120s 超时。用户要求「长期彻底方案，打好基础」。

scout 已定位根因（事实见 scout-context.md）：

1. **固定 sleep 代替事件等待**：9 个套件 55 处 `waitForTimeout` 共 62.1s 纯 sleep。
2. **同套件重复遍历**：i18n 对 28 页面遍历 3 遍；i18n_coverage 每页 newContext。
3. **每页 newContext**：i18n AC-3.1 段 28 个 context，i18n_coverage ~29 个。
4. **无并行**：run_all.sh 纯串行 for + sleep 1.5。
5. **构建缺口**：`run_all.sh` 不生成 `internal/server/dist`（gitignore 产物），全新环境 `go build` 直接失败。
6. **单测拖累**：`go test ./...` 50s（含 28.6s 固定 sleep）被算进 e2e。
7. **无结构化结果**：无 JSON/TAP，并行聚合靠解析 stdout，脆弱。
8. **无 CI**：e2e 只在本地门禁跑。

## 2. 目标（长期基础）

- **快**：全量 e2e 目标 **≤ 180s**（3 分钟内，subagent 可单次跑完），从 491s 降 63%+。
- **稳**：消除固定 sleep，全部改事件等待；消除随机端口冲突。
- **可并行**：套件可并行跑，结果可聚合。
- **可观测**：结构化结果输出（JSON），失败可定位。
- **自包含**：`make e2e` 从干净仓库可直接跑通（自动生成 dist）。
- **不弱化断言**：所有现有断言保留，不为了快而删 case。

## 3. 范围

### 必做（本次）
1. **harness 增强**：
   - 引入 `waitForAppReady(page)` helper（基于 `#sidebar`/`#lang-switcher` 等稳定锚点），替代所有 `waitForTimeout`。
   - 确定性端口分配（避免并行撞端口）+ 端口冲突检测重试。
   - 结构化结果：每套件输出 JSON 结果文件（`{suite, passed, failed, failures[], durationMs}`）。
2. **套件改造**（消除 sleep + 合并遍历）：
   - i18n：28 页遍历 3 遍 → 1 遍（可访问性 + switcher + 渲染 + 溢出一次完成）；AC-3.1 的 28 ctx → 4 ctx（每 lang 1 个）。
   - i18n_coverage：与 i18n 共享页面遍历或合并，消除 ~29 次 newContext。
   - frontend / lang_hint / cli_auth / passkey_enroll / admin_flow / deeplink / auth_flow：所有 waitForTimeout 换事件等待。
3. **run_all.sh 重构**：
   - 前置 `make fe` 生成 dist（检测不存在才生成，避免重复）。
   - 并行执行器：套件分组并行（Chrome 类一组、HTTP 类一组），可控并发数（默认按 CPU）。
   - 结果聚合：读各套件 JSON 结果，统一汇总报告。
   - `go test ./...` 拆出独立目标（`make test`），不再阻塞 e2e（e2e 只跑端到端套件）。
4. **Makefile**：新增 `make test`（go 单测）、`make e2e`（端到端，不含单测）、`make e2e-parallel`（并行模式）。
5. **macOS 兼容**：用 `perl -e 'alarm N; exec @ARGV'` 或 node 原生超时替代不存在的 `timeout` 命令。

### 不做（明确排除）
- 不引入 Jest/Vitest 等测试框架（保持原生 node .mjs，零新增依赖）。
- 不改产品代码逻辑（只改测试代码 + harness + run_all.sh + Makefile）。
- 不改 Go 单测里的固定 sleep（那是单测问题，本次只把它从 e2e 时长里摘出去；单测 sleep 优化另立任务）。
- 不加 CI（本次先把本地基础打牢，CI 是后续任务，但结果格式要为 CI 友好预留）。

## 4. 验收标准

1. **全量 `make e2e` ≤ 180s**（本机实测，同等硬件）。
2. **单套件 `make e2e-one S=<name>` 无一超过 60s**（含 i18n，目前 174s）。
3. **`grep -rn waitForTimeout scripts/e2e/` 零命中**（或仅剩无法避免的、带注释说明的极少数）。
4. **干净仓库可跑**：删除 `internal/server/dist` 和 `web/` 后 `make e2e` 仍能跑通（自动生成）。
5. **并行模式 `make e2e-parallel`** 比串行快且结果一致（通过数不减少）。
6. **结构化结果**：每套件在 `.e2e-bin/results/<suite>.json` 输出结果，run_all 汇总读 JSON。
7. **断言零弱化**：所有现有断言保留，通过数 ≥ 改造前（562+ 静态断言）。
8. **端口无冲突**：并行跑 3 次全量，无端口冲突失败。
9. **macOS 无 `timeout` 依赖**：不依赖 GNU coreutils。

## 5. 风险

- **并行下 Chrome 内存**：多个 Chrome + server 同跑可能吃内存。需并发上限（默认 4，可配）。
- **事件等待的稳定性**：`#sidebar` 等锚点若在某些页面不出现（如 login 页无 sidebar），需分页面类型给不同锚点。designer 需定义清晰的「页面就绪」判定策略。
- **i18n 合并遍历的断言顺序**：合并后断言执行顺序变化，若有依赖需保留。designer 需梳理。
- **并行下 server 端口**：需确定性分配 + 冲突检测，否则随机段仍有小概率撞。

## 6. 分层

- pm（本文件）：需求/边界/验收。
- designer：测试架构设计（并行模型、结果格式、harness API、页面就绪策略、套件分组）。
- worker：实现 harness 增强 + run_all.sh 重构 + Makefile。
- frontend/worker：改造各套件（i18n/frontend/lang_hint/cli_auth 等）。
- tester：性能基准 + 全量回归。
- reviewer：终审对照验收。
