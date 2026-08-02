The prototype is minimal — subscriptions.json is raw Web Push subscriptions, no status filter yet in code (the task describes the target Go backend design). Good — my design stands on the task's stated model. I'll write the design now, decisive and under ~500 words, then the acceptance report.The prototype is minimal (raw push subscriptions; status filter lives in the described Go backend design), so the design below stands on the stated model. Naming avoids collision with the payload's existing `tags` field.

---

# Anotify device-tag routing — design decision

## 1. The one rule

> **A notification is delivered to a device iff the device is enabled AND (the notification has no tags OR the device has no tags OR they share ≥1 tag).**

Tag filtering **only activates when the notification carries tags.** An untagged device is a catch-all that never filters.

Edge cases, resolved:

- **Notification with NO tags** → broadcast to **all** enabled devices. This preserves today's behavior exactly and eliminates the worst footgun: an agent that never sets tags can never dead-letter a notification (which *would* happen if untagged notifications only matched untagged devices and every device were tagged).
- **Device with NO tags** → receives **everything** (broadcasts + all tagged traffic). This *is* the "receive everything" option — no extra checkbox. Empty filter = no filtering, the same philosophy as the existing status filter's `all`.
- **Multi-tag × multi-tag** → **ANY** match (non-empty intersection). ALL-match is rejected: it makes tags conjunctive narrowing, which no user expects from "subscribed to these channels," and the owner can always send twice.
- **Strict exclusivity** ("Agent picks `告警`, *only* 告警 devices get it"): true by default **as long as every device has ≥1 tag**. If a catch-all device exists, it also receives — that's its stated purpose. Document: *"want hard partitioning? Tag every device."*

Rejected alternative: an explicit `receiveAll` flag + strict partition (untagged device gets only untagged traffic). It adds a UI concept and reintroduces the dead-letter footgun for untagged notifications.

## 2. Interaction with the status filter

Fully **independent AND**. Delivery predicate:

```
deliver = device.enabled AND statusMatch(device.statusFilter, notif.status) AND tagMatch(rule above)
```

Pure predicates — evaluation order is semantically irrelevant (check `enabled` first operationally). Status filter answers *"which severities do I care about"*; tags answer *"which channels am I subscribed to."* Orthogonal dimensions, both must pass. No precedence, no overrides, no coupling.

## 3. Data model

- **Device**: `tags TEXT` — JSON array of strings, e.g. `["手机","工作"]`, default `[]`. (For the future Go backend: `[]string` + JSONB/JSON column; a normalized `device_tags` table is over-engineering at this scale — no cross-device tag queries are needed.)
- **Notify request**: new field **`deviceTags: []string`**, optional. Omitted or `[]` = broadcast. **Do not** reuse the existing payload `tags` (those are notification labels) — the name `deviceTags` makes the routing target unambiguous.
- **Validation/normalization**: trim, dedupe, match case-insensitively (normalize on write); ≤10 tags/device, ≤32 chars/tag. Unknown tags are legal — they just match nothing.
- **Observability**: have `POST /v1/notify` return per-device delivery results (the prototype already collects `{endpoint, status}`). An agent must be able to see "0 devices matched" instead of silent loss.

## 4. Examples

Devices: **A** phone `[手机]` (status: all), **B** work browser `[工作]` (status: error), **C** tablet `[]` (catch-all, status: all).

1. `{status: success}` (no deviceTags) → **A, B*, C** — broadcast, unchanged from today. (*B receives it only if its status filter passes; with `error` it doesn't — status filter still applies to broadcasts.)
2. `{deviceTags: ["手机"], status: error}` → **A** (tag match + status ok), **C** (catch-all). **B** excluded (no tag match).
3. `{deviceTags: ["手机","工作"], status: error}` → **A, B** (ANY match; B's `error` filter passes), **C** (catch-all). To keep C out of targeted alerts, tag it (e.g. `[生活]`).