# 历史任务归档

本目录存放**已完成编排任务**的产物（requirements / design / plan / review / test-report）。
保留供回溯，但不再进入新任务的 `defaultReads` 扫描范围——新任务的产物写到 `.pi-orchestrator/` 根。

## 已归档任务

- **i18n 站点多语言**（2026-08-05 完成，merge a507cbf）：4 语言 + 切换器 + hreflang
  - `i18n-requirements.md` / `i18n-switcher-design.md` / `i18n-review.md`
- **i18n 覆盖率补全**（2026-08-06）：缺 key 回退与覆盖率门禁
  - `i18n-coverage-spec.md` / `i18n-coverage-review.md` / `i18n-coverage-test-report.md`
- **语言提示 lang-hint**（2026-08-06）：根据 Accept-Language 首选语言提示
  - `lang-hint-requirements.md` / `lang-hint-design.md` / `lang-hint-review.md` / `lang-hint-test-report.md`

> 协调者注意：派新任务时，任务产物的 `defaultReads`（requirements.md/design.md/plan.md/context.md）
> 指向的是**当前任务在根目录或 worktree 内的产物**，不要把这里的历史文档读进来——会膨胀子 agent 上下文。
