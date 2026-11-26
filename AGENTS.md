# AGENTS.md · Anotify 子 Agent 专属约定（必读）

> 你是 Anotify 实施团队的一个子 Agent，在协调者（主 pi Agent）编排下工作。
> 开工前必读：本文件（仓库级红线）+ `DEVELOPMENT.md`（开发总纲）+ **你所改 area 的模块约定**（见 §0）+ 相关包的 `*_test.go`。
>
> **编排层事实**（2026-11-16 校准）：本仓库可用的子 agent 角色是
> **scout（只读侦察）/ worker（实现）/ centurion（带队的隔离交付单元）**。
> 旧文档提到的 pm / designer / frontend / tester / reviewer、workspace `.pi/agents/`、
> workspace 级 `AGENTS.md` **都不存在**（仓库上级目录没有这个文件）。不要按它们安排职责，也不要引用。
>
> **为什么需要 §0 这张表**：pi 只从「工作目录 + 祖先目录」加载 `AGENTS.md`，**不会**扫子目录。
> 子目录里的模块约定必须显式读，否则写了没人看。

## 0. 模块索引（改哪块就读哪份 AGENTS.md）

| area | 目录 | 模块约定 | 状态 |
| --- | --- | --- | --- |
| `store` | `internal/store/` | [`internal/store/AGENTS.md`](internal/store/AGENTS.md) | ✅ 已写 |
| `server` | `internal/server/` | [`internal/server/AGENTS.md`](internal/server/AGENTS.md) | ✅ 已写 |
| `node` | `cmd/anode/` | `cmd/anode/`（模块约定待写：AGENTS.md） | 待写 |
| `edge` | `internal/protocol/`、`internal/ws/` | `internal/protocol/AGENTS.md` | 待写 |
| `auth` | `internal/auth/`、`internal/authn/` | `internal/auth/AGENTS.md` | 待写 |
| `api` | `api/`（五份 OpenAPI 真源） | `api/AGENTS.md` | 待写 |
| `web` | `web-src/`（源）、`web/`（手写 css/js + 生成物） | `web-src/AGENTS.md` | 待写 |
| `e2e` | `scripts/e2e/` | `scripts/e2e/`（模块约定待写：AGENTS.md） | 待写 |

**硬规则（写成可检查的动作，因为宽泛说法会被窄任务绕过）**：

1. **本次会话的第一条工具调用**必须是「读本 area 的模块约定」；若该 area 还没有模块约定，就读本文件 §0 这一行并明确声明「该 area 暂无约定」。
2. 模块约定与本文件冲突时**模块约定优先**（它更具体）；两者都不能覆盖 `docs/contracts/` 下的契约与 `api/` 五份 OpenAPI（语义/接口判据）。
3. 派发卡的第一行固定写：`0. 第一步读 <area>/AGENTS.md`。

> **防漂移守卫**：`make check-agents`（已接进 `make ci`）校验三件——本表「已写/待写」与真实文件是否一致、
> 模块约定里的 `path:line` 引用是否还落在真实行范围内、模块文件是否超 80 行。
> 它红了就是文档过期了：**改文档，不要改守卫**（确属举例的行加 `agents-ref-ignore` 标记）。

> **依据（2026-11-16 实测 5 个子 agent）**：3 个探索型任务（出方案）全部把「读模块 AGENTS.md」作为第一个动作；
> 1 个窄任务（改一行注释）虽然上下文里有本文件，却直接跳到代码、一条约定都没读 —— 说明声明式索引会被窄任务绕过。
> 于是把说法改成本节第 1 条那几个**可执行的字**，再用同形状的窄任务重测：
> 第一条工具调用就是 `read internal/server/AGENTS.md`（不再跳过）。
> 验证方式：查子 agent 的 session JSONL（`<cwd>/.pi-subagents/runs/*/<name>.jsonl`），看第一条 `read` 的路径。

## 1. 环境与工具链

- Go 拉依赖统一用：`GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto`
  （直连不经过镜像代理；官方校验和；需要时自动补 go1.25 工具链）
- Node 22 可用；系统 Chrome 可供 `web_verify` / Playwright 无头验证
- Docker 可用（仅用于最终镜像构建，开发期用 git worktree 隔离）

## 2. 工作方式

- 你在协调者分配的 **git worktree**（如 `wt-store`）里工作，对应独立分支
- **不要**改动你任务范围之外的文件；不要动别人的 worktree
- 代码遵循各包契约（`api/` 五份 OpenAPI 真源 + `internal/store/schema.sql` + `internal/protocol/doc.go`）
- 模块级红线见 §0 表里各 area 的 `AGENTS.md`；本文件是仓库级约定

## 3. 代码规范

- **Go**：gofmt；包注释；错误用 `fmt.Errorf("...: %w", err)` 包装；表名/字段严格按 `internal/store/schema.sql`
- **前端**：纯静态 HTML + Tailwind CDN + `tokens.css`（颜色只用 tokens 变量，不硬编码色值）；无构建框架
  - **class 纪律（硬红线）**：HTML 里的类只允许三种来源——① `ui.css`/`tokens.css` 组件类、② `ui.css`/页面内联 `<style>` 里定义的类、③ Tailwind 工具类。**禁止自造无定义的类**（如 `input-field` 这种不在任何 CSS 里的类，Tailwind CDN 会静默不生效，导致风格脱节）。`make fe`/`make sitegen`/`make dev` 已接入 `scripts/check-classes.mjs` 死类守卫，会拦住这种类；手写新类前先确认它落在上述三个来源内（若想要新组件类，就加进 `ui.css`）。
- **API 契约**：JSON 字段统一 camelCase（给结构体加 `json` tag）；空列表返回 `[]` 而非 `null`
- **v4 开发期全量切换（用户已批准）**：以 `docs/contracts/v4/CONTRACT.md`（含 V4-D1～D6 决策索引）为本轮契约；不保留 v3 API/协议/旧数据库迁移兼容层。允许把 `schema.sql` 直接重建为 v4 baseline、移除旧迁移 hooks/专属测试；仍须保留新库结构、重开幂等、错误拒绝和字段往返测试。旧库不可用时明确报错，禁止自动删除用户数据。此例外覆盖下面的历史 baseline 追加规则，仅适用于本次尚未发布的 v4 全量开发。
- **store 层**：新增字段必加"往返一致性"单测（存什么读什么）。schema 变更的当前规则见
  [`internal/store/AGENTS.md §3.2`](internal/store/AGENTS.md)：**迁移引擎目前只支持单块**（`baselineLayout()` 硬校验），
  所以 v4 未发布期加表/加列是在 baseline 块内原地改、`headVersion` 保持 1；要引入第 2 块必须先改引擎（单独授权）。
  已发布之后回到「baseline 不再改、只追加 `-- +migration N` 块」。老库不会被自动迁移或删数据（`Open` 明确报错）。
- 提交信息：`type(scope): 中文描述`，`scope` 用 §0 的 area 名，如 `feat(store): 事件分页改 keyset`

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

你报 DONE ≠ 完成。协调者会**独立验证**（跑 `make e2e` / 契约 / web_verify）。通过才在协调者的本地任务台账（协调者工作区，不入库）标 ✅；不通过会退回并注明原因，你需返工。

**铁律**：

- 发现产品 bug 时**不要改测试断言迁就**，明确上报"发现产品 bug：xxx"，由协调者决定修产品还是调测试
- 不要为"让测试通过"而弱化断言

## 6. 关键陷阱（必读，避免重蹈）

详见 `DEVELOPMENT.md` 第 8 节。要点：

- **BackupEligible**：注册时必须持久化 `cred.Flags.BackupEligible`，否则登录校验不一致报错
- **推送域已退役（V4-D7）**：`internal/push`、`devices` 表、`/v1/devices*`、VAPID、
  `notify:receive`/`devices:read` 都已删除，别再按旧记忆去改这些文件（它们不存在了）
- **store 只依赖叶子包**：`protocol`/`artifact`/`sourcespec`/`playbook`/`route`/`source`；
  不得 import `server`/`ws`/`lease`/`auth`/`api`（分层倒挂 + import 循环）
- **空列表返回 `[]`**：别返回 `null`，否则前端误判"未连接"
- **schema 变更按 `internal/store/AGENTS.md` §3.2 走**：当前引擎只支持单块，v4 未发布期的加列就是改 baseline；
  写成 `-- +migration 2` 而不先改引擎 → 每次 `Open` 报 `expected one v4 baseline`。
  `CREATE TABLE IF NOT EXISTS` 不改已存在的表，老库缺列会被 `ErrSchemaInvalid` 具名拒绝（不迁移、不删数据）。

## 7. 测试要求

- **改 Go 代码**：相关包 `go test` 必须通过
- **改前端**：`web_verify` 逐页无 JS 错误/溢出；改完 `make build` 重新指纹
- **纯视觉/样式微调（不改 DOM 契约、Go、API）**：`make fe` + 目标页 dark/light 截图即可迭代；需要时单跑 `./scripts/e2e/run_all.sh --serial frontend`，**不必每轮全量**
- **push / 合并前**：`make e2e` 全绿（固化门禁）；全量 e2e 只在这里跑
- **同机并发**：e2e 矩阵入口有机器级 flock（`${TMPDIR:-/tmp}/anotify-e2e-<uid>.lock`），等锁上限 `E2E_LOCK_WAIT`（默认 900s）、超时 exit 75 并打印持锁者；`E2E_LOCK=0` 是**显式**逃生口（只在确认没有共享资源冲突时用）。锁在**矩阵**层，`--parallel` 的套件照旧并发；语义与自测见 `E2E_TESTING.md`「机器级互斥」
- **改 Go / API / DOM 契约 / 新功能**：仍跑相关包测试 + 全量
- **新功能**：必须加对应 E2E 套件或单测覆盖
