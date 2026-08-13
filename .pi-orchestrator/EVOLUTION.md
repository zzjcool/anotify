# Anotify 编排回顾（项目级）

> **自我升级机制已上提至 workspace 层**，见 workspace `.pi-orchestrator/EVOLUTION.md`。
> 那里包含：回顾三问、四层落点（L1 记忆 / L2 提示词 / L3 配置 / L4 流程）、模型升级判断、agent 新陈代谢、子 agent 派发契约、worktree 纪律、已知待进化点。
>
> 本文件改为 **anotify 项目级的回顾记录**：每个非琐碎任务完成后，协调者在此写「三问」（哪里顺/哪里卡、根因、怎么改）。anotify 专属的踩坑沉淀进 `AGENTS.md` §6 / `DEVELOPMENT.md` §8。

---

## 回顾 2026-08-12 · 开发流程上提至 workspace（三问）

### 做对了什么

- **先调研再动手**：逐个读 7 个 agent 文件 + AGENTS.md §0 + EVOLUTION，把"通用职责"和"anotify 专属"切清楚，泛化时不丢关键红线（派发契约/自测红线/flaky 诊断）。
- **职责与业务上下文解耦**：agent 文件只留通用职责入 workspace，anotify 专属（Go 规范/tokens/partials/make e2e/架构地图）留在 anotify AGENTS.md/DEVELOPMENT.md，靠 inheritProjectContext 桥接。避免 agent 文件在两处维护漂移。

### 踩了什么坑

- **MD041 lint advisory 干扰**：pi 的 agent md 约定首行是 YAML frontmatter，但 markdownlint 报"首行应为标题"（MD041）。是 false positive，需判断为可忽略，不能被它带偏去改文件结构。
- **write 后需 re-read**：autofix 会改文件，直接续写会 mismatch。每次 autofix 后必须重读。

### 怎么改（四层落点）

1. **L4 流程**：流程上提本身完成。workspace AGENTS.md §3 + EVOLUTION + 7 agent 成为单一事实源，anotify 瘦身为只放专属约定。
2. **L2 提示词**：泛化后的 agent 文件已写入 workspace，后续 anotify 任务的 agent 派发会用新名（pm/scout/worker/...，无 anotify 前缀）。
3. **观察项**：需在真实 anotify 任务里验证「inheritProjectContext 能否正确让 workspace agent 拿到 anotify AGENTS.md 上下文」——若继承不到，需补 agent 文件的 defaultReads 指向各仓库 AGENTS.md。
