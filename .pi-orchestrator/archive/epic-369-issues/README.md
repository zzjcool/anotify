# #369 子 issue 原文（历史快照，2026-11-13）· **已归档，不要当活 issue 读**

> ⚠️ **这 16 份文件是 2026-11-13 的快照，其中 `state: OPEN` 是当时的文本，不代表现在。**
> Epic #369 已由提交 `7b9749e`（`merge: UI/UX 评审总账（#369）—— 16 个子 issue 全部落地`）合入
> `main`，这些 issue 已全部关闭。
>
> **为什么要留档**：事后追查「某一行为什么这么改」时，快照里有当时的判断依据。
> **不要做的事**：不要把这里的 `state: OPEN` 当成待办派发；不要按这里的内容判断代码现状（行号会漂）。
>
> 判断当前是否还有活 issue 的唯一方式（在仓库根目录）：
>
> ```bash
> gh issue list --state open --limit 100 > /tmp/gh-open.txt 2>&1   # 必须重定向，见下
> ```

## 为什么读 issue 必须用本地文件/重定向

在子 agent 里通过 `gh issue view` 读 issue 会**挂起** —— gh 会向终端发 OSC 11 / CSI 6n 查询
（背景色、光标位置），pty 没人回应就永远等下去。实测：`script -qec 'gh issue view 372'` 超时；
而 `gh issue view 372 > /tmp/x.txt 2>&1` 立即返回。

## 立新 issue 的规矩

新建/派发一律走 `.pi-orchestrator/issues/<N>.md`（带 frontmatter：`kind` / `area` / `size` / `gates`），
不要把新快照塞回本目录 —— 本目录只读。
