# Anotify · Get notified when your agents finish

[中文](README.md) | **[English](README.en.md)**

Push notifications to all your devices (iOS / Mac / PC / Android) automatically
when an Agent finishes a task. Passkey passwordless login, no polling, single
binary — easy to self-host.

> This repository evolved from a prototype (iOS Web Push proof-of-concept) into
> a full implementation. See `design/` for mockups and `design/tech-scheme.html`
> for the technical spec.

## 📚 Documentation

| Doc | Audience | Content |
| --- | --- | --- |
| **[DEVELOPMENT.md](DEVELOPMENT.md)** | Developers / Agents | Architecture decisions, dev workflow, test system, gotchas (must-read) |
| **[AGENTS.md](AGENTS.md)** | Sub-agents | Conventions, specs, pitfalls |
| **[E2E_TESTING.md](E2E_TESTING.md)** | Developers | End-to-end test system (`make e2e` gate) |
| **[IOS_TESTING.md](IOS_TESTING.md)** | QA | iOS real-device push checklist (only manual step) |
| `.pi-orchestrator/TASKS.md` | Coordinator | Implementation task log (history) |

## Architecture

```
Agent ─POST /v1/notify─▶ Go backend ─▶ Broker(SQLite) ─┬─▶ WS dispatcher(consumer 1) ─▶ WebSocket clients
 (ant_send Key)         single binary  queue+history   └─▶ Push dispatcher(consumer 2) ─▶ Web Push(APNs/FCM)
```

- **Single binary**: Go + `go:embed` bundles frontend + SQLite (pure Go, no CGO)
- **Broker abstraction**: Message queue interface (Publish/Subscribe/Ack/Replay);
  SQLite implementation now, swappable to NATS/Redis/Kafka with zero business changes
- **Two receive channels = two consumers**: WebSocket long-poll + Web Push
- **Path separation**: `/v1/*` dynamic API (no-store), static assets via CDN cache
  tiering (hash-fingerprinted files are immutable)

## Quick Start

```bash
# 1. Generate VAPID keys (first time)
make keys        # outputs ANOTIFY_VAPID_PUBLIC_KEY / ANOTIFY_VAPID_PRIVATE_KEY

# 2. Build single binary (includes frontend fingerprint + embed)
make build

# 3. Set environment and run
export ANOTIFY_VAPID_PUBLIC_KEY=... ANOTIFY_VAPID_PRIVATE_KEY=...
./anotify        # default :8080, embeds frontend

# Dev mode (frontend changes don't rebuild binary)
make dev         # uses web/ local directory, no fingerprinting
```

## Docker

```bash
make docker                              # build image (~20MB)
docker run -p 8080:8080 \
  -e ANOTIFY_VAPID_PUBLIC_KEY=$ANOTIFY_VAPID_PUBLIC_KEY \
  -e ANOTIFY_VAPID_PRIVATE_KEY=$ANOTIFY_VAPID_PRIVATE_KEY \
  -e ANOTIFY_RP_ID=your-domain \
  -e ANOTIFY_RP_ORIGIN=https://your-domain \
  anotify
```

## Public Exposure (Cloudflare tunnel, for iOS / real-device testing)

```bash
# After the server is running on :8080, use a tunnel domain as RP_ID and restart
# (Passkey requires domain matching)
cloudflared tunnel --url http://localhost:8080
# → Get https://xxx.trycloudflare.com, use it as ANOTIFY_RP_ID / ANOTIFY_RP_ORIGIN
```

## Testing

```bash
make test                # all unit tests
make integration         # integration tests (health / cache tiers / auth matrix)
node scripts/ws_test.mjs # WS receiver (needs RECV_KEY/SEND_KEY)
node scripts/push_e2e.mjs# Desktop Chrome push E2E (needs SESSION/API_KEY)
go run ./cmd/devseed     # seed test user + keys + sessions
```

## API (/v1, see api/openapi.yaml)

| Endpoint | Description |
| --- | --- |
| `POST /v1/auth/register[/options]` | Passkey registration |
| `POST /v1/auth/login[/options]` | Passkey login |
| `POST /v1/notify` | **Agent reporting** (Bearer Key, scope=notify:send) |
| `GET /v1/stream` | WebSocket receiving (Bearer Key, scope=notify:receive) |
| `GET/POST /v1/devices` | Push subscription device management |
| `GET/POST /v1/keys` | API Key management |
| `GET /v1/notifications` | Notification history |
| `GET /v1/vapid-public-key` | VAPID public key (for frontend subscription) |

### Agent reporting example

```bash
curl -X POST https://your-domain/v1/notify \
  -H "Authorization: Bearer ant_send_..." \
  -H "Content-Type: application/json" \
  -d '{"title":"Deploy done","status":"success","body":"Build succeeded","deviceTags":["ops"]}'
```

Delivery rules: device enabled ∧ status filter passes ∧ tags match
(no-tag message = broadcast; no-tag device = catch-all; otherwise intersection).

## Environment Variables

| Variable | Default | Description |
| --- | --- | --- |
| `ANOTIFY_ADDR` | `:8080` | Listen address |
| `ANOTIFY_DB` | `./anotify.db` | SQLite path |
| `ANOTIFY_STATIC` | empty (embed) | Local static directory (dev: `./web`) |
| `ANOTIFY_VAPID_PUBLIC_KEY/PRIVATE_KEY` | — | VAPID keys (required for push) |
| `ANOTIFY_RP_ID` | `localhost` | WebAuthn RP domain (= access domain) |
| `ANOTIFY_RP_ORIGIN` | `http://localhost:8080` | WebAuthn Origin (with protocol) |
| `ANOTIFY_CDN_PREFIX` | empty | CDN prefix (production static acceleration) |

## File Structure

```
cmd/server/         single binary entrypoint
cmd/devseed/        test seeding tool
internal/
  store/            SQLite data access + schema
  broker/           message queue abstraction + SQLiteBroker
  auth/             Passkey(WebAuthn) + API Key(argon2id) + sessions
  api/              /v1/notify reporting
  ws/               WebSocket dispatcher
  push/             Web Push dispatcher(VAPID)
  route/            tag/status delivery filter (shared pure logic)
  server/           route assembly + static assets(CDN cache tiers) + embed
web/                frontend (static HTML + Tailwind CDN + tokens.css)
scripts/hash.mjs    fingerprint script (content-hash + reference rewrite + manifest)
scripts/integration.sh / ws_test.mjs / push_e2e.mjs   test scripts
design/             mockups + technical spec
```

## Security

- API Keys are stored as **argon2id hashes only**; plaintext shown once at creation,
  with scope (notify:send / notify:receive / devices:read)
- Passkey (WebAuthn) passwordless; sessions via httpOnly cookies
- Push payloads end-to-end encrypted (p256dh/auth), VAPID private key stays server-side
- `vapid.json` / `*.db` / `.env.local` are gitignored — do not commit
