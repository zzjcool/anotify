# 编排流程自我升级机制（协调者职责）

> 目标：让这套编排流程**越用越好**，而不是一次配完就僵化、过时。
> 触发时机：每次完成一个非琐碎编排任务后、或流程中踩到坑/发现阻塞后。

## 一、何时做回顾（Retrospective）

满足任一条件，协调者就要做一次简短回顾（写进 TASKS.md 的「回顾」小节）：

- 一个编排任务走完（无论成败）
- 某个 agent 明显没干好（返工 >1 次、产出被 reviewer 打回、踩了已知坑）
- 发现了新的"关键陷阱"（DEVELOPMENT.md / AGENTS.md 还没收录的）
- 流程本身卡住（角色边界不清、上下文没传到、模型明显不胜任）

## 二、回顾三问（每次必填）

1. **哪里顺 / 哪里卡**？（具体到某个 agent / 某个环节 / 某次交接）
2. **根因是什么**？（是 agent 职责定义问题？模型不够？上下文缺失？红线没写进 prompt？还是流程缺环节？）
3. **怎么改能避免复发**？（落到下面四层的具体动作）

## 三、升级的四个落点（按改动成本从低到高）

| 层 | 改什么 | 何时改 |
| --- | --- | --- |
| **L1 记忆层** | 更新 `.pi/agent-memory/<agent>/MEMORY.md`（agent 自己也会写） | 每次踩坑后，把教训沉淀给该 agent |
| **L2 提示词层** | 更新 `.pi/agents/anotify-*.md` 的职责/红线/约束；更新 `AGENTS.md`/`DEVELOPMENT.md` 陷阱清单 | 某 agent 反复犯同类错，说明 prompt 没写透 |
| **L3 配置层** | 更新 `.pi/settings.json` 的 agentOverrides（模型/thinking）；增删 agent | 模型明显不胜任/过时；出现新角色缺口 |
| **L4 流程层** | 更新本文件的流程、TASKS.md 模板、分层原则 | 流程结构本身有问题（如缺了某层把关） |

**原则：能用低成本层解决就不动高成本层。** 大部分进化靠 L1/L2（记忆+提示词）就够。

## 四、模型过时/升级的判断

- 当某 agent 在同一类任务上**连续 2 次**产出质量不达标（被 reviewer 打回、或协调者需大量返工），先查 prompt（L2），再考虑换模型（L3）。
- 新模型出现时，用**单一真实小任务**做 A/B 验证（同一任务分别用旧/新模型跑，对比产出），别凭感觉换。
- 换模型只改 `.pi/settings.json` 一处（这就是当初把模型从 agent 文件抽出来的意义）。

## 五、Agent 角色的新陈代谢

- **会过时的不只是模型，还有角色。** 当某 agent 的职责长期用不上、或与另一 agent 高度重叠，协调者应在回顾中提出"合并/下线"建议，由用户拍板。
- 新增角色前，先确认现有 6 个 agent 是否真的覆盖不了——避免角色膨胀导致编排成本超过收益。

## 六、当前已知待进化点（持续维护）

- [x] 协调者「回合结束/后台任务/原子交付」纪律 → 已固化到 AGENTS.md §0.1（2026-08-05 i18n 任务回顾）
- [x] **子 agent 被硬超时/EOF 杀掉丢产出**（根因级修复，2026-08-06 诊断）：见下方「子 agent 派发契约」专节。此前只归因为「tester 跑 e2e 超时」，漏掉了更普遍的诱因——provider 网络抖动（503/unexpected EOF/rate limit）会随时杀死普通前台子 agent，且 `partialOutputPath: null` 导致产出全丢。
- [ ] tester 跑全量 e2e 易超时：后续给 tester 派任务时要求「先写套件+单跑新套件自验，全量 e2e 放最后且留足预算」，或考虑拆分「写套件」与「跑门禁」两阶段（观察 1-2 次再定是否入 prompt）
- [ ] reviewer 终审被 turn 预算截断过一次：派终审任务时明确「先落报告（结论+分级问题清单）再深挖细节」，保证截断也有可用产出（观察再定）
- [ ] 用户实测反馈会穿透 designer 的原设计（如平铺→统一下拉）：设计稿应预留「实测后快速迭代」环节，designer 对「用户直接反馈」的响应流程已验证可行（v1.0→v1.1）

---

## 七、子 agent 派发契约（2026-08-06 固化，防「执行到一半不执行」）

### 根因复盘

排查 `~/.pi/agent/run-history.jsonl` + `.pi-subagents/artifacts/*_meta.json` + 主 session 日志，发现子 agent「执行到一半不执行」有两类诱因，**同一套机制**：

1. **长任务撞 30min 默认前台硬超时**：worker/frontend/tester 跑全量 e2e 时，默认前台 30min wall-clock 到点 SIGKILL，`partialOutputPath: null`，产出全丢。meta 直接写 `"error": "Subagent timed out after 1800000ms."`。
2. **普通任务被 provider 网络抖动杀死**：codebuddy provider（copilot.tencent.com）频繁返回 503 / unexpected EOF / rate limit，命中前台阻塞子 agent 时进程异常退出（exit=1），同样 `partialOutputPath: null`。session 日志实锤：`Post "https://copilot.tencent.com/v2/chat/completions": unexpected EOF — cut off early (only got to reading files)`。

**共同根因**：前台阻塞 + 默认 30min 硬墙 + 无产出落盘 + 无重试。普通任务遇抖动「突然消失」，长任务遇超时「做到一半被杀」，两者都因无落盘而让协调者感觉「什么都没留下」。

### 固化契约（已写入 worker/frontend/tester/reviewer 的 agent md frontmatter）

协调者派**实现层 + 终审层**子 agent 时必须：

1. **`async: true`** —— 不走前台阻塞默认 30min 硬墙（async 默认无超时）。
2. **显式 `timeoutMs`** —— worker/frontend/reviewer ≥ 3600000（1h）；tester 跑全量 e2e ≥ 3600000 且任务里指示「先写套件+单跑自验，全量放最后」。
3. **`output` 落盘到指定文件** —— 被截断/失败时 `partialOutputPath` 有值，协调者先读 output 抢救已完成部分再决定重派，而非从头重跑。
4. **协调者用 `subagent_wait` / `nonBlocking` 订阅收尾**，不干等一个突然的 timeout。
5. **provider 抖动重试**：收到 `unexpected EOF` / 503 / rate limit 导致的 error 时，直接重派（产线已落盘则只重派未完成部分），不要当成「任务完成」。

### 上下文瘦身（同步执行）

- **scout 关 `inheritProjectContext`**：scout 是 deepseek-flash + thinking:low 的纯侦察角色，吃 19KB 的 AGENTS+DEVELOPMENT 是浪费 token。
- **`.pi-orchestrator/archive/` 归档历史任务产物**：i18n/lang-hint 等 ~100KB 已完成任务文档移入 archive，避免被新任务的 defaultReads 误扫，膨胀子 agent 上下文、挤占有效预算。
