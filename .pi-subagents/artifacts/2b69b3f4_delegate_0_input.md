# Task for delegate

You are a senior distributed-systems architect reviewing a design decision for **Anotify**, an open-source self-hostable "Agent notification platform" (Go backend, embedded frontend).

PRODUCT CONTEXT:
- Flow: An AI Agent (e.g. pi, Claude Code) finishes a task → POST /v1/notify with an API key → Go backend validates → pushes the notification to the user's devices → system notification appears.
- Currently the ONLY receiving channel is **Web Push** to browsers (Safari/Chrome) via VAPID + service worker. iOS requires add-to-homescreen.
- Entities: User (Passkey auth), API Key (ant_live_..., used by Agent to REPORT notifications; server stores only the hash), Device (a push subscription), Notification.

NEW REQUIREMENT:
The owner wants a SECOND receiving channel so that **arbitrary devices / future "receiver plugins"** can receive notifications in real-time, authenticated by an **API key** — because normal request/response HTTP is not suitable for server→client message delivery. Browser Web Push must be RETAINED as a parallel channel.

YOUR TASK — give a reasoned, opinionated recommendation:
1. Compare **WebSocket vs gRPC server-streaming vs SSE** for this "receive notifications over a persistent connection" use case. Consider: plugin-developer ergonomics, browser compat, proxy/firewall traversal, need for client→server messages (acks, dynamic tag subscription, heartbeats), self-host simplicity. Pick ONE as the primary recommendation and justify; note when the alternatives are better.
2. Specify the full protocol for your recommended channel: connection URL + auth handshake, message frame types and their JSON fields, how a client subscribes/unsubscribes to tags, heartbeat/keepalive, reconnection + resume (missed-event replay), and connection lifecycle.
3. Security: should receiving use the SAME API key as reporting, or a separate scoped key? Recommend a key-scope model (e.g. notify:send vs notify:receive) and justify.
4. Briefly: how does this enable the future "receiver plugin" ecosystem (desktop menubar app, CLI daemon, smart-home bridge)?

Be concrete and specific (give example JSON frames). Keep it under ~600 words. This is a design discussion, not code.

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