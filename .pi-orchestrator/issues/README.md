# 派发卡（`.pi-orchestrator/issues/`）

给协调者和子 agent 用。**一卡一个可派发单元**，不再把 issue 正文散在聊天里。

## 为什么不直接读 GitHub

子 agent 里 `gh issue view` 会挂死（OSC 11 / CSI 6n 查询没人回应）。所以本地留卡，
读 issue 用这里的文件；必须跑 gh 时**一定重定向**：`gh issue list > /tmp/x.txt 2>&1`。

历史快照在 `../archive/epic-369-issues/`（**已关闭，勿当活 issue 读**）。

## frontmatter 必填

```yaml
---
id: 001                       # 顺序号，三位
kind: bug                     # bug|feat|ui|i18n|perf|security|test|docs|chore|infra
area: [server]                # 8 个之一或多个：store|server|node|edge|auth|api|web|e2e|contract
size: S                       # S(≤1 文件/不触 chokepoint) | M(单 area 单 PR) | L(跨 area) | XL(epic)
severity: P3                  # P0 安全/数据/阻断 | P1 主流程 | P2 体验 | P3 打磨
state: open                   # open|working|done|blocked|needs-decision
needs_recon: false            # true = 根因不明，先派 scout
contract: []                  # 触及的 chokepoint：schema.sql|mux.go|protocol|api-yaml|locales|ui.css|partials.js|Makefile
depends_on: []
route: [worker]               # 派生结果：scout/worker/centurion（可人工覆盖）
gates: ["go test ./internal/server/ -run TestOpenAPIParity -count=1"]
opened: 2026-11-16
---
```

## 硬规则

1. **一个卡只有一个 `kind`**。两个就拆 —— 否则门禁不确定。
2. `area` 必须能对应到路径白名单（见根 `AGENTS.md` §0）。归不到 = `needs_recon: true`，先侦察再定。
3. `area` 命中 `contract` 里的任何一个 → 无论 size 都要 contract owner 会签。
4. 派发时任务文本第一行固定写：`0. 第一步读 <area>/AGENTS.md`。
5. 明确写「不做」的部分，防止下一轮以「没要求做」为由重开。

## 基线红

`make ci` 在本机基线上就是红的（`internal/sitegen` docs test 确定性失败、`cmd/anode` 5 个
bootstrap 因无 sudo 失败、`ws`/`runner` 负载下 flaky）。门禁口径是「记录基线红 vs 新增红」，
不是「让它变绿」。
