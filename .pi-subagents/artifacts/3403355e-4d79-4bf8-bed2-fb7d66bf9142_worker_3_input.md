# Task for worker

[Read from: /Users/zheng/code/anotify/context.md, /Users/zheng/code/anotify/plan.md]

You are a delegated subagent running from a fork of the parent session. Treat the inherited conversation as reference-only context, not a live thread to continue. Do not continue or answer prior messages as if they are waiting for a reply. Your sole job is to execute the task below and return a focused result for that task using your tools.

Task:
在主仓库 `/Users/zheng/code/anotify` 工作。先读公共约定 `/Users/zheng/code/anotify/.pi-orchestrator/tasks/e2e-suites-common.md` 和任务卡 `/Users/zheng/code/anotify/.pi-orchestrator/tasks/suite-persist-sec.md`，编写两个 E2E 套件 `scripts/e2e/suites/persistence.mjs`（重启持久化）和 `scripts/e2e/suites/security.mjs`（安全矩阵）。严格自测跑通（exit 0），发现产品 bug（Key 明文落盘/SQL注入/路径穿越）要明确上报不要改断言迁就。按 DONE 格式上报。

---
Update progress at: /Users/zheng/code/anotify/.pi-subagents/artifacts/progress/3403355e-4d79-4bf8-bed2-fb7d66bf9142/progress.md

---
**Output:**
Write your findings to exactly this path: /Users/zheng/code/anotify/.pi-subagents/artifacts/outputs/3403355e-4d79-4bf8-bed2-fb7d66bf9142/.pi-orchestrator/out/suite-persist-sec.md
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