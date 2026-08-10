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

**模型与职责解耦**：agent 文件（`.pi/agents/anotify-*.md`）只写**角色职责**（入仓库），不写死模型；**模型映射集中在 `.pi/settings.json`** 的 `agentOverrides`（也入仓库，团队统一）。想换模型/模型升级过时，只改 settings 一处；个人想本地用别的模型，在 `~/.pi/agent/settings.json` 覆盖同名 agent 即可（用户级优先于项目级）。

**自我升级**：每次完成非琐碎任务或踩坑后，协调者必须按 `.pi-orchestrator/EVOLUTION.md` 做回顾（三问），把教训沉淀到四层落点（记忆/提示词/配置/流程）。写型 agent（worker/frontend/tester）带 `memory` 持久记忆，可自我沉淀经验。

### 0.1 协调者纪律（踩坑换来的铁规，2026-08-05 i18n 任务后固化）

用户只认「跑完的结果」，不认「说了要做」。协调者必须遵守：

1. **回合结束三条件**（满足其一才可结束回合，否则继续干）：
   - 任务完成（门禁绿、产物交付、验证过）；或
   - 真正阻塞在用户才能拍的板上（且必须明确说清等什么）；或
   - 真正阻塞在外部事件上（且该事件有显式的等待/检查安排，见第 2 条）。
   - 「汇报一下进度」**不是**结束回合的理由。用户已委派的任务，不要停下来等确认。
2. **后台任务不许点火就跑**：凡起后台进程（e2e、服务、隧道、子 agent），结束回合前必须要么等它出结果（前台跑/subagent_wait/轮询日志），要么明确交付「它在跑、我何时如何验收」。无人等待的后台任务 = 丢了。
3. **原子交付**：一个环境/服务类任务（如「起测试环境」= 隧道+服务+健康验证+可用 URL）必须整体验证可用后才交付，半成品（只起了隧道没起服务）不许交付。
4. **子 agent 被截断 ≠ 完成**：subagent 超时/截断后，协调者必须核实其产物落盘情况，补齐未完成部分或重派，不得当作已交付。
5. **多窗口并发隔离**（2026-08-06 固化）：用户开多个 pi 窗口同时跑任务时，pi-subagents 的进程/通信/artifacts 是 session 隔离的（supervisor 消息按 session id 隔离、artifacts 文件名带 runId 前缀不碰、`resume` 有跨进程 session lease），但**文件系统层共享**——同一 git 工作区、同一 `anotify.db`、同一 `internal/server/dist/` 构建产物、同一 `.pi-orchestrator/TASKS.md` 任务板。协调者派任务时必须：
   - **写代码类任务必须各走独立 git worktree**（`worktree: true` 或 `cwd` 指向 `.pi-orchestrator/worktrees/wt-<任务名>`，从 main 切独立分支），不得在主工作区并发改同一批文件；
   - **跑服务/e2e 类任务必须用各自独立的 DB 与端口**（`-db` 指向各自临时文件、端口不撞），否则两个 `make e2e` 会互相冲 WAL/端口；
   - **改 `TASKS.md`/`EVOLUTION.md` 等共享编排文档要串行**——两窗口同时改同一 md 会后写覆盖先写；需要登记进展时先 `git pull`/重读再写，或约定只有一个窗口维护任务板；
   - **构建产物 `internal/server/dist/`、`web/*.html` 是共享的**：两窗口同时 `make build`/`make fe` 会互相覆盖指纹文件，并发构建类任务要错开或各自 worktree。

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
  - **class 纪律（硬红线）**：HTML 里的类只允许三种来源——① `ui.css`/`tokens.css` 组件类、② `ui.css`/页面内联 `<style>` 里定义的类、③ Tailwind 工具类。**禁止自造无定义的类**（如 `input-field` 这种不在任何 CSS 里的类，Tailwind CDN 会静默不生效，导致风格脱节）。`make fe`/`make sitegen`/`make dev` 已接入 `scripts/check-classes.mjs` 死类守卫，会拦住这种类；手写新类前先确认它落在上述三个来源内（若想要新组件类，就加进 `ui.css`）。
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
