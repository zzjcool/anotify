I've reviewed the current mockups. Here is my recommendation as a design document (no code changes needed for this task — it's a design/IA recommendation).

---

# Anotify — Information Architecture Recommendation

## 1. Left-Sidebar Navigation

Group by the **two axes** (delivery vs auth) plus a utility tier:

```
工作台 (Workspace)
  ├─ 总览                    (Dashboard)
  ├─ 通知接收                 (Receivers — formerly "设备与会话")
  └─ API Keys

集成 (Integration)
  ├─ 接入 Agent
  ├─ API 文档
  └─ 技术方案

账户 (Account)
  ├─ 安全与登录               (Security — NEW, passkey/sessions/recovery)
  └─ 返回首页
```

Order matters: `通知接收` and `安全与登录` are now clearly separated by the grouping labels themselves.

---

## 2. Per-Page Responsibilities

| Page | Single responsibility |
|------|----------------------|
| **总览** | Aggregate health: recent notifications, delivery stats, quick actions |
| **通知接收** (Receivers) | Where notifications get delivered. Two channel tabs (Push + WebSocket), tags, per-receiver filters. **No auth concerns.** |
| **API Keys** | Key issuance/revocation/scoping for the REST API and WebSocket auth |
| **安全与登录** (Security) | Everything about proving "who you are": Passkeys (multiple), active login sessions, recovery codes. **No delivery concerns.** |
| **接入 Agent / API 文档 / 技术方案** | Static integration docs |
| **首页** | Public landing |

---

## 3. Should sessions move OUT of Devices?

**Yes — unequivocally.** The current mockup itself admits the two are independent axes. Keeping them together forces users to mentally disambiguate on every visit.

- **Sessions belong to the auth axis.** They are created by Passkey auth, are revoked via auth, and their "last active" is an auth event. They answer: *"Where am I logged in?"*
- **Devices/Receivers belong to the delivery axis.** They answer: *"Where do notifications go?"*

The current page has to carry an entire explainer section ("为什么是两条独立的轴？") just to justify its own grouping — a clear smell that the grouping is wrong. Splitting eliminates that cognitive tax. The new Security page absorbs sessions, and the Receivers page drops the sessions panel and the dual-axis explainer.

---

## 4. Naming the new Passkey page

The current login screen is `mockup-passkey.html` — a standalone, unauthenticated page. The new management page must not collide with that.

**Recommendation: 安全与登录 (Security & Sign-in)**, file name `security.html`.

- **Why not "Passkey"?** Too narrow — it also holds sessions and recovery codes. "Passkey" as a page name also reads like the login screen itself.
- **Why "安全与登录"?** It scopes the page to auth concerns, matches the user's mental model of "account security," and is a standard pattern (GitHub/Google both use "Security" as the umbrella).
- **Disambiguation from login screen:** rename the login screen's file to `login.html` (or `auth.html`). The sidebar label "登录 / Passkey" currently points to the login screen; it should be removed from the sidebar entirely — an authenticated workspace shouldn't carry a link to its own login page. "返回首页" already exists for that exit.

---

## 5. Presenting two receiving channels on the Devices page

Rename the page **通知接收 (Receivers)** and use a **tabbed channel switcher** at the top:

```
[推送订阅] [WebSocket 长连接]
```

- **Tab 1 — 推送订阅:** current device list (browser push subscriptions). This is the default.
- **Tab 2 — WebSocket 长连接:** list of live connections keyed by API key, showing connected client name, uptime, tags, last message. Empty state links to API docs.

**Why tabs, not a merged list:** the two channel types have different lifecycles (push persists when offline; WS is ephemeral), different management actions (push: rename/pause/filter; WS: disconnect/inspect), and different metadata. Merging would force a lowest-common-denominator UI.

**Tags** live on each receiver (both channels) as a filter dimension, managed inline per receiver and summarized in a tag filter bar at the top of the Receivers page.

---