# AGENTS.md · Anotify 子 Agent 统一约定（必读）

> 你是 Anotify 实施团队的一个子 Agent，在协调者（主 pi Agent）编排下工作。
> **开工前必读 `DEVELOPMENT.md`（开发总纲）+ 本文件 + 相关包的 `*_test.go`。**

## 0. 编排架构（默认流程）

所有**非琐碎**改动都走三层编排流程，由协调者（主 Agent）编排、各专路子 Agent 执行：

```
【定义层 · 该做什么】(kimi-k3)
  anotify-pm        产品：需求/价值/边界/验收标准 → requirements.md
  anotify-designer  设计：信息架构/视觉方案/交互规格 → design.md
【侦察/规划层】
  anotify-scout     侦察现状 → context.md        (deepseek-v4-flash)
  (内置 planner)   拆任务 → plan.md              (kimi-k3)
【实现层 · 怎么做】(glm-5.2)
  anotify-worker    后端实施
  anotify-frontend  前端实现（照 designer 的稿）
  anotify-tester    测试把关（发现产品 bug 上报，不改断言）
【终审层】(kimi-k3)
  anotify-reviewer  对照需求&设计稿终审 → APPROVE / REQUEST_CHANGES
```

**何时必须编排**：改动 >3 个文件、或涉及 ≥2 个独立模块（broker/server/store/push/ws/前端）、或新增页面/契约。琐碎单点修复（改个文案/小 bug）协调者可直接做，不必兴师动众。

**分层原则**：定义层（pm/designer）与实现层（worker/frontend/tester）严格分离——定义层不写实现代码，实现层不擅自改需求/设计；tester 与 worker 分离（写码的不自测自夸）；reviewer 独立于所有实现者做终审。

**模型分级**：kimi-k3 守定义层与终审（重推理/决策），glm-5.2 执实现层主力（长上下文执行），deepseek-v4-flash 跑侦察（快/省）。

## 1. 环境与工具链

- Go 拉依赖统一用：`GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto`
  （直连不经过镜像代理；官方校验和；需要时自动补 go1.25 工具链）
- Node 22 可用；系统 Chrome 可供 `web_verify` / Playwright 无头验证
- Docker 可用（仅用于最终镜像构建，开发期用 git worktree 隔离）

## 2. 工作方式

- 你在协调者分配的 **git worktree**（如 `wt-store`）里工作，对应独立分支
- **不要**改动你任务范围之外的文件；不要动别人的 worktree
- 代码遵循各包契约（见 `internal/broker/broker.go`、`api/openapi.yaml`）
- 各专路 Agent 的详细职责/红线见 `.pi/agents/anotify-*.md`（那里是事实源）；本文件是跨角色的公共约定

## 3. 代码规范

- **Go**：gofmt；包注释；错误用 `fmt.Errorf("...: %w", err)` 包装；表名/字段严格按 `internal/store/schema.sql`
- **前端**：纯静态 HTML + Tailwind CDN + `tokens.css`（颜色只用 tokens 变量，不硬编码色值）；无构建框架
- **API 契约**：JSON 字段统一 camelCase（给结构体加 `json` tag）；空列表返回 `[]` 而非 `null`
- **store 层**：新增字段必加"往返一致性"单测（存什么读什么）；新增列要在 `store.Open` 里加幂等 `ALTER TABLE` 迁移
- 提交信息：`type(scope): 中文描述`，如 `feat(broker): 实现 Publish/Replay`

## 4. 完成后上报（必须）

在输出末尾给出：

```
DONE <任务ID>
产出文件:
- path/to/file1
- path/to/file2
自测命令与结果:
- go test ./internal/... → PASS (xx tests)
发现的产品 bug（若有，不要改断言迁就）:
- xxx
遗留风险:
- （若有）
```

## 5. 协调者验证（你不算完成）

你报 DONE ≠ 完成。协调者会**独立验证**（跑 `make e2e` / 契约 / web_verify）。通过才在 `.pi-orchestrator/TASKS.md` 标 ✅；不通过会退回并注明原因，你需返工。

**铁律**：

- 发现产品 bug 时**不要改测试断言迁就**，明确上报"发现产品 bug：xxx"，由协调者决定修产品还是调测试
- 不要为"让测试通过"而弱化断言

## 6. 关键陷阱（必读，避免重蹈）

详见 `DEVELOPMENT.md` 第 8 节。要点：

- **VAPID subject**：webpush-go 会自动加 `mailto:`，配置若已带要 `TrimPrefix`，否则双重前缀致 Apple BadJwtToken
- **BackupEligible**：注册时必须持久化 `cred.Flags.BackupEligible`，否则登录校验不一致报错
- **UpsertDevice vs UpdateDevice**：前者是订阅刷新（只更新密钥），后者是改配置（全字段）；PATCH 用 Update
- **store 不依赖 broker**：用本地 `MessageRow`，避免 import 循环
- **空列表返回 `[]`**：别返回 `null`，否则前端误判"未连接"
- **新增列要迁移**：`CREATE TABLE IF NOT EXISTS` 不改已存在的表，需显式 `ALTER TABLE ADD COLUMN`

## 7. 测试要求

- **改 Go 代码**：相关包 `go test` 必须通过
- **改前端**：`web_verify` 逐页无 JS 错误/溢出；改完 `make build` 重新指纹
- **改完任何东西**：最终 `make e2e` 全绿才算完成（固化门禁）
- **新功能**：必须加对应 E2E 套件或单测覆盖
