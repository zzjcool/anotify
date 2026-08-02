# Task for worker

[Read from: /Users/zheng/code/anotify/context.md, /Users/zheng/code/anotify/plan.md]

You are a delegated subagent running from a fork of the parent session. Treat the inherited conversation as reference-only context, not a live thread to continue. Do not continue or answer prior messages as if they are waiting for a reply. Your sole job is to execute the task below and return a focused result for that task using your tools.

Task:
你在 git worktree `/Users/zheng/code/anotify/.pi-orchestrator/worktrees/wt-notify`（分支 wt-notify）工作。请严格按照任务卡 `/Users/zheng/code/anotify/.pi-orchestrator/tasks/t12-notify.md` 完成 T12：实现 /v1/notify 上报 + 标签路由 + WS/WebPush 双派发器。先读任务卡和 AGENTS.md，完成后在 worktree 内 git commit，并按 DONE 格式上报。环境：GOPROXY=direct GOSUMDB=sum.golang.org GOTOOLCHAIN=auto。

---
Update progress at: /Users/zheng/code/anotify/.pi-subagents/artifacts/progress/7ba75e66-8615-4639-bfc6-6abffe9db470/progress.md

---
**Output:**
Write your findings to exactly this path: /Users/zheng/code/anotify/.pi-subagents/artifacts/outputs/7ba75e66-8615-4639-bfc6-6abffe9db470/.pi-orchestrator/out/t12.md
This path is authoritative for this run.
Ignore any other output filename or output path mentioned elsewhere, including output destinations in the base agent prompt, system prompt, or task instructions.

## Acceptance Contract
Acceptance level: checked
Completion is not accepted from prose alone. End with a structured acceptance report.

Criteria:
- criterion-1: Implement the requested change without widening scope
- criterion-2: Return evidence sufficient for an independent acceptance review

Required evidence: changed-files, tests-added, commands-run, residual-risks, no-staged-files

Review gate: required by reviewer.

Finish with a fenced JSON block tagged `acceptance-report` in this shape:
Use empty arrays when no items apply; array fields contain strings unless object entries are shown.
`criteriaSatisfied[].status` must be exactly one of: satisfied, not-satisfied, not-applicable.
`commandsRun[].result` must be exactly one of: passed, failed, not-run.
`manualNotes` and `notes` are optional strings; an empty string means no note and does not satisfy `manual-notes` evidence.
```acceptance-report
{
  "criteriaSatisfied": [
    {
      "id": "criterion-1",
      "status": "satisfied",
      "evidence": "specific proof"
    },
    {
      "id": "criterion-2",
      "status": "satisfied",
      "evidence": "specific proof"
    }
  ],
  "changedFiles": [
    "src/file.ts"
  ],
  "testsAddedOrUpdated": [
    "test/file.test.ts"
  ],
  "commandsRun": [
    {
      "command": "command",
      "result": "passed",
      "summary": "short result"
    }
  ],
  "validationOutput": [
    "validation output or concise summary"
  ],
  "residualRisks": [
    "none"
  ],
  "noStagedFiles": true,
  "diffSummary": "short description of the diff",
  "reviewFindings": [
    "blocker: file.ts:12 - issue found, or no blockers"
  ],
  "manualNotes": "anything else the parent should know"
}
```