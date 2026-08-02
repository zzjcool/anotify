# Task for delegate

You are a senior product designer / information architect reviewing the navigation + page structure of **Anotify**, an open-source self-hostable "Agent notification platform" (dark premium UI, left-sidebar workspace).

CURRENT STATE:
- Pages: 总览 (dashboard), 设备与会话 (devices page — currently holds BOTH push-subscription "receiving devices" AND "login sessions"), API Keys, 登录/Passkey (the login screen), API 文档, plus a landing 首页.
- Entities: User (Passkey auth), API Key, Device (push subscription), Notification, login Session.

NEW REQUIREMENTS from the owner:
1. **Passkey should have its OWN dedicated management page** — separate from devices. It should manage: Passkey credentials (multiple per account, e.g. iCloud-synced, hardware key, per-device), and likely also login sessions + recovery codes.
2. Devices get a **Tag** capability (tag-based notification routing).
3. Devices page gains a second **receiving channel**: besides browser Web Push, also long-connection receiving via API key (WebSocket) for arbitrary devices / future receiver plugins.

YOUR TASK — recommend a clean information architecture:
1. Propose the left-sidebar navigation structure (grouped, ordered).
2. For EACH page, define its single clear responsibility and what belongs on it.
3. Decide specifically: should **login sessions** move OUT of the devices page into the new Passkey/security page? Justify (hint: sessions come from Passkey auth; devices are about delivery). Consider the "two independent axes" idea: auth-axis (passkey/session/recovery) vs delivery-axis (push receivers/channels/tags).
4. Recommend names for the new Passkey page and how to avoid confusion with the existing login screen (currently mockup-passkey.html).
5. Suggest how the devices page should present TWO receiving channels (Web Push subscriptions vs WebSocket connections) without confusion.

Be decisive and concrete. Under ~500 words. Design discussion, not code.

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