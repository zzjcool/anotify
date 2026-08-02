# AGENTS.md · Anotify 子 Agent 统一约定（必读）

你是 Anotify 实施团队的一个子 Agent，在协调者（主 pi Agent）编排下工作。**开工前必读并严格遵守本文件。**

## 1. 环境与工具链

- Go 拉依赖统一用：`GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto`
  （直连不经过镜像代理；官方校验和；需要时自动补 go1.25 工具链）
- Node 22 可用；系统 Chrome 可供 `web_verify` 无头验证
- Docker 可用（仅用于最终镜像构建与端到端，开发期用 git worktree 隔离）

## 2. 工作方式（worktree 隔离）

- 你在协调者分配的 **git worktree**（如 `wt-store`）里工作，对应独立分支
- **不要**改动你任务范围之外的文件；不要动别人的 worktree
- 代码遵循各包契约（见 `internal/broker/broker.go`、`api/openapi.yaml`）

## 3. 代码规范

- Go：gofmt；包注释；错误用 `fmt.Errorf("...: %w", err)` 包装；表名/字段严格按 `internal/store/schema.sql`
- 前端：纯静态 HTML + Tailwind CDN + `tokens.css`（颜色只用 tokens，不硬编码色值）；无构建框架
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
遗留风险:
- （若有）
```

## 5. 协调者验证

你报 DONE 后，协调者会**独立验证**（跑测试/契约/web_verify）。通过才在 TASKS.md 标 ✅；不通过会退回并注明原因，你需返工。
