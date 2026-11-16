# v4 剩余 issue 派发 · 通用执行契约（所有 centurion 必读）

> 你是协调者（root）为本轮 GitHub issue 交付派出的**实现 centurion**。
> 本文件 + 你的 issue 卡 + 冻结契约文档是唯一权威；与旧 `.pi-orchestrator` 里「不 push/MR」的约定冲突时，以本文件为准（用户已显式授权提交 PR）。

## 0. 权限边界（硬红线）

- **允许**：在你自己 worktree 里写代码、commit、`git push -u origin <你的 feature 分支>`。
- **禁止**：`gh pr create` / `gh pr merge` / push 到 `main` 或 `v4/integration` / 强推 / rebase 共享分支 / 改其他 worktree / 直接动 legion.db。
- PR 由协调者创建；合并权在协调者。你只交付**可审查的分支**。
- 禁止交互式命令、禁止 sudo、禁止写 `.env`/`.git`/`node_modules`、禁止删除用户数据、禁止 `rm -rf` 家目录。
- 不向 main checkout（`/root/code/anotify`）写任何文件；它只用于**只读**读取冻结契约文档。

## 1. 基线校正（第一步，必须做且必须报告结果）

pi 从仓库 HEAD（`b5adeda`，旧 v3 代码）给你建 worktree，**这不是你的基线**。开工第一条命令：

```bash
cd <你的 worktree>
git fetch origin v4/integration
BASE=$(git rev-parse origin/v4/integration)
git checkout -B <你的 feature 分支> "$BASE"
git rev-parse HEAD   # 必须等于 $BASE
```

在报告中写明 `base=<BASE sha>`。若 fetch 失败，停下来上报，不要从 b5adeda 开工。

## 2. 冻结契约文档（只读，绝对路径）

- issue 原文：`/root/code/anotify/.pi-orchestrator/v4/issues/V4-0X-*.md`
- G3 总契约：`/root/code/anotify/.pi-orchestrator/v4/G3-HTTP-SPEC-RESUME-HANDOFF.md`（D1–D6）
- wire/CAS/资源字段：`/root/code/anotify/.pi-orchestrator/v4/WIRE-HANDOFF.md`（W1–W6）
- 产物/授权：`/root/code/anotify/.pi-orchestrator/v4/ARTIFACT-HANDOFF.md`、`RESULT-WIRE-AUTH.md`
- S1/E1：`STORE-INGRESS-FAILURE-HANDOFF.md`、`EMIT-FAILED-TASK-RECEIPT-HANDOFF.md`
- 设计：`docs/design/v4/{decisions,capability,source,anode,playbook,config,api-conventions}.md`（worktree 内）
- 机械边界：`docs/contracts/CONTRACT.md` §9

**冲突裁决顺序**：用户本轮指令 > issue 原文 > G3/W1–W6/A1/S1/E1 > 旧代码注释。契约矛盾时写进报告由协调者裁决，不要自行发明语义。

## 3. 工作方式

- 单个源码 writer 纪律：同一时刻只允许一个 worker 写共享文件；`mux.go`、`schema.sql`、Makefile 等共享文件必须串行分配给唯一 owner。多个 worker 只能拆**互不重叠的新文件/测试文件**。
- 内部至少做一轮独立 review（新 worker，只读，附冻结决定与「刻意不做」清单），critical/major 必须修完再收队。
- 长任务写进度到 worktree 内 `.legion-notes.md`（**不提交**），防上下文丢失。
- 提交信息：`type(scope): 中文描述`；`git add` 明确列文件，禁止 `git add -A` 把 evidence/临时文件带进来。
- 提交 `.pi-legion/`、证据日志、临时脚本一律不进 git。

## 4. 证据纪律（真实、可复核）

- 所有长命令：`set -o pipefail; timeout 900 <cmd> 2>&1 | tee <worktree>/evidence/<name>.log; echo EXIT=${PIPESTATUS[0]}`（至少保留 exit code）。
- Go 一律：`GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto`。
- 测试临时目录：`export TMPDIR=/root/tmp/<issue>; mkdir -p "$TMPDIR"`，结束清理 owned 进程/临时目录。
- **禁止**：为绿灯删测试/skip/放宽断言/放宽超时/加全局错误白名单；伪造或复用旧日志；用 `|| true` 吞错误。
- 产品 bug：本 issue 范围内修产品并加回归；范围外只记录（文件:行号 + 现象），不越界。
- 同 root-cause 最多重试 2 次，仍失败则 SAFE_BLOCKED 上报（附已尝试、失败输出、假设、涉及文件）。

## 5. 交付报告（最终消息必须给全）

```
DONE <issue id>
分支: <branch> @ <sha>   base=<v4/integration sha>
改动摘要: <按模块 3-8 条>
公开接口清单: <新增/变更的 method+path / DTO / 错误码>
测试映射: <旧用例 → 新用例 / 具名退役；统计 kept/migrated/retired>
验证命令与真实 exit: <逐条命令 + EXIT + log 路径>
未完成 / SAFE_BLOCKED: <具体 owner 与原因，没有写「无」>
风险与待裁决: <契约矛盾、缺失底层接口、可疑产品 bug>
```
