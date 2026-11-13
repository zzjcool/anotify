# #369 子 issue 原文（本地快照，2026-11-13）

**为什么有这份快照**：在子 agent 里通过 `gh issue view` 读 issue 会**挂起** —— gh 会向终端发
OSC 11 / CSI 6n 查询（背景色、光标位置），pty 没人回应就永远等下去。实测：
`script -qec 'gh issue view 372'` 超时；而 `gh issue view 372 > /tmp/x.txt 2>&1` 立即返回。

**规矩**：

1. 读 issue 用这里的本地文件（`issues/<N>.md`），不要跑 `gh issue view`。
2. 万不得已要跑 gh，**必须重定向**：`gh ... > /tmp/x.txt 2>&1` 之后再用 read 读文件。
3. 建 PR / 合并由协调者做；车道只需要把分支 push 上去。
