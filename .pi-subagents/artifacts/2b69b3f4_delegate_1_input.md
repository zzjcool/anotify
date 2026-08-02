# Task for delegate

You are a senior product/architecture designer reviewing a notification-routing design for **Anotify**, an open-source self-hostable "Agent notification platform".

PRODUCT CONTEXT:
- An AI Agent finishes a task → POST /v1/notify (API key) → Go backend → pushes notification to the user's devices.
- Device = a notification receiver (currently a browser Web Push subscription). Each device already has: enabled (on/off) and a per-device **status filter** (all / error / success) so e.g. a phone can be set to "only failures".
- The notify payload currently has: title, status (success|error|interrupted|info|warning), body, link, agentId, sessionId, cwd, durationMs, model, priority, ttl, collapseKey, silent, tags, metadata.

NEW REQUIREMENT:
The owner wants **device Tags**. A device can be tagged (e.g. "手机", "工作", "告警"). When the Agent reports a notification it can **choose which tag(s) to send to**, so the notification only reaches devices subscribed to that tag.

YOUR TASK — design a clean, unambiguous routing model:
1. Specify EXACT delivery semantics covering ALL edge cases:
   - notification with NO tags → who receives it?
   - device with NO tags → what does it receive?
   - notification with multiple tags; device with multiple tags → match rule (ANY vs ALL)?
   - should there be a "receive everything" device option, and how does it interact with tags?
2. How does tag-routing interact with the EXISTING per-device status filter (all/error/success)? (Are they independent AND-conditions? Order of evaluation?)
3. Recommend the data model for Device.tags and the notify request field(s).
4. Give 2-3 concrete routing examples that make the semantics obvious.
5. Keep it the SIMPLEST model that is still predictable and covers the owner's use case ("Agent picks a tag, only those devices get it"). Avoid over-engineering.

Be decisive and concrete. Under ~500 words. Design discussion, not code.

## Acceptance Contract
Acceptance level: attested
Completion is not accepted from prose alone. End with a structured acceptance report.

Criteria:
- criterion-1: Return a concise result and residual risks when applicable

Required evidence: manual-notes, residual-risks

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