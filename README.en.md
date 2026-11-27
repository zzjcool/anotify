# Anotify · Get notified when your agents finish

[中文](README.md) | **[English](README.en.md)**

When an Agent finishes a task it reports one event; the hub runs the chain you
defined on your nodes (analysis, evidence capture, alert scripts, …) and the whole
run stays inspectable in the console: event → task chain → logs.
Passkey passwordless login (or an optional password), single binary — easy to self-host.

> This repository evolved from a prototype into a full implementation.
> **The Web Push delivery domain has been retired entirely** (decision V4-D7, see
> [`docs/contracts/v4/CONTRACT.md`](docs/contracts/v4/CONTRACT.md)) — we no longer push
> notifications to phones; "notification" now means the event stream and task chain,
> and where it goes is decided by your own capability scripts. The only authority for behaviour is **code + tests +
> `docs/contracts/`**; start at [`docs/README.md`](docs/README.md).

## 📚 Documentation

| Doc | Audience | Content |
| --- | --- | --- |
| **[DEVELOPMENT.md](DEVELOPMENT.md)** | Developers / Agents | Architecture decisions, dev workflow, test system, gotchas (must-read) |
| **[AGENTS.md](AGENTS.md)** | Sub-agents | Conventions, specs, pitfalls |
| **[docs/README.md](docs/README.md)** | Everyone | Doc index: what is authoritative, what is a snapshot, how to change behaviour |
| **[E2E_TESTING.md](E2E_TESTING.md)** | Developers | End-to-end test system (`make e2e` gate) |
| **[examples/playbooks/](examples/playbooks/)** | Users | Installable playbook examples (event → verdict → alert/audit), with empty-environment steps |

## Architecture

```
External systems ─HTTP/WS─▶ Hub anotify (single binary) ─▶ WS dispatch ─▶ node anode ─▶ run capability scripts
                                     SQLite
```

- **Two products**: hub `anotify` (`cmd/server`: resource API + dispatch, **never executes
  capabilities**) and node `anode` (`cmd/anode`: `serve` / `install` / `emit`, the machine that
  actually runs them)
- **Single binary**: Go + `go:embed` bundles frontend + SQLite (pure Go, no CGO)
- **The hub never executes**: execution lives only on nodes (mechanically checked: no `os/exec`
  in central packages)
- **Path separation**: `/v1/*` dynamic API (no-store), static assets via CDN cache
  tiering (hash-fingerprinted files are immutable)

> v4 behaviour and API follow the code and the five specs under `api/`; the v3 `cmd/agent` is retired
> together with the Broker abstraction (no compatibility layer).

## Quick Start

### First-time setup (once per machine)

```bash
# 1. Install deps (Go ≥1.25, Node 22, cloudflared, Chrome)
brew install go node@22 cloudflared

# 2. Clone
git clone git@github.com:zzjcool/anotify.git && cd anotify

# 3. Configure local env (.env.local is gitignored, holds secrets)
cp .env.example .env.local
# optionally adjust ANOTIFY_RP_ID / ANOTIFY_RP_ORIGIN (Passkey RP must equal the access domain)
```

Key fields in `.env.local` (pre-configured for dev.openaaas.org):

```
ANOTIFY_ADDR=:8080
ANOTIFY_STATIC=./web
ANOTIFY_RP_ID=dev.openaaas.org
ANOTIFY_RP_ORIGIN=https://dev.openaaas.org
```

### Cloudflare named tunnel (dev.openaaas.org, optional but recommended)

A fixed domain (instead of a temporary trycloudflare URL) as WebAuthn RP_ID makes Passkeys reusable across restarts.

```bash
cloudflared tunnel login                       # browser auth (creates ~/.cloudflared/cert.pem)
cloudflared tunnel create anotify              # note the returned UUID
```

Create `~/.cloudflared/config.yml` (replace `<UUID>`):

```yaml
tunnel: <UUID>
credentials-file: /Users/<you>/.cloudflared/<UUID>.json

ingress:
  - hostname: dev.openaaas.org
    service: http://localhost:8080
  - service: http_status:404
```

Bind DNS:

```bash
cloudflared tunnel route dns anotify dev.openaaas.org
```

> Skip this if you don't need a tunnel — `make dev-local` starts only the local server (RP_ID defaults to localhost).

### Daily start

```bash
make dev          # starts server + cloudflared tunnel; Ctrl-C stops both
```

This single command: reads `.env.local` → ensures `web/*.html` → checks port → starts tunnel in background → runs `go run ./cmd/server` in foreground (uses `web/` source, frontend changes are live).

After start:

- **Public**: <https://dev.openaaas.org>
- **Local**: <http://localhost:8080>
- Homepage redirects to login → create the first account with a Passkey or a username + password (first user auto-admin)
- Prefer a password? Pick password sign-up on the form, or set one later under **Security & login** (Passkey and password coexist)
- server log: `tail -f /tmp/anotify-dev.log`; tunnel log: `tail -f /tmp/anotify-tunnel.log`

| Command | Purpose |
| --- | --- |
| `make dev` | dev: server + tunnel (dev.openaaas.org) |
| `make dev-local` | dev: local server only, no tunnel |
| `make build` | build both product binaries: `anotify` (embeds fingerprinted frontend) + `anode` (node) |
| `make test` | Go unit tests |
| `make ci` | PR gate: gofmt / vet / build / unit tests (same as GitHub Actions; no e2e) |
| `make e2e` | full end-to-end tests (~57s, 968 assertions) |
| `make e2e-one S=auth_flow` | run a single suite |

## Docker

```bash
make docker                              # hub image (ships the node upgrade source /app/anode)
docker run -p 8080:8080 \
  -e ANOTIFY_RP_ID=your-domain \
  -e ANOTIFY_RP_ORIGIN=https://your-domain \
  anotify
```

## Node (anode)

The hub only dispatches — **the node is what actually executes capabilities**. `anode` is a
separate binary you run on your own machine: it connects to the hub, pulls capabilities,
runs them, and reports results/emits. The default path is **one command** — the line the
hub's Nodes page hands you after you register a node (it uses `--token`, and the plaintext token
is shown only once):

```bash
curl -fsSL https://<hub-domain>/install-anode.sh | sh -s -- \
  --name node-a --server https://<hub-domain> --token <node-token>
```

`--token` stays in your shell history, so prefer a 0600 file for day-to-day use:
`--token-file /root/anode.token` (only one of the two; upgrades and token rotation work the same).

It downloads the binary and verifies its sha256, pre-flights the token with
`anode serve --check`, stages files with `anode install`, installs a systemd service
(`system` / `user` / `none`), polls until it is `active`, then prints a summary. Afterwards the
node should show as online on the hub's Nodes page. **Do not add `sudo` to the command** —
the script escalates by itself through your terminal.

- upgrade = **re-run the same command** (keeps the existing `anode.yaml`, swaps the binary, restarts)
- uninstall = `sh install-anode.sh --uninstall` (removes the config too; `--keep-config` keeps it, `--purge` also deletes the data dir)
- full walkthrough (download priority, service modes and privilege escalation, pre-flight diagnostics, security) lives in `docs/anode-install.md`

Without the script (you drive the binary, unit and service yourself):

```bash
make anode                                  # build ./anode (with version metadata)
./anode install --name node-a --server wss://<hub-domain> --token-file /root/anode.token
./anode serve --config /etc/anode/anode.yaml --data /var/lib/anode
./anode --version                           # {"version":"<version>+<commit>","commit":"<sha>"}

make docker-anode                           # node image (a container is a node)
```

The token is issued after registering the node in the hub
(`POST /v1/nodes` → `POST /v1/nodes/{name}/tokens`, plaintext returned once).
`anode install` only writes files (no `sudo`, no `systemctl`); see `docs/anode-cli.md` for the
full CLI (three subcommands and their flags).

## Public Exposure

See "Quick Start → Cloudflare named tunnel" above: `make dev` already starts `cloudflared tunnel run anotify` (fixed dev.openaaas.org → localhost:8080). Temporary tunnels (`cloudflared tunnel --url`) are for one-off real-device testing only — not recommended for daily dev (URL changes each time, invalidating Passkeys).

## Testing

```bash
make ci                  # same as CI: gofmt + embed frontend + vet + build + unit tests
make test                # Go unit tests (independent of e2e; default build does not embed dist)
make e2e                 # full end-to-end tests (~57s, parallel, 968 assertions)
make e2e-one S=auth_flow # run a single suite
make integration         # integration tests (health / cache tiers / auth matrix)
node scripts/ws_test.mjs # WS receiver (needs SEND_KEY)
go run ./cmd/devseed     # seed test user + keys + sessions
```

> See `E2E_TESTING.md` for the e2e suite system (17 suites, parallel runner, structured JSON results).

## API (/v1)

> The authoritative v4 definition is the five specs under `api/` (agent / auth / events /
> ingress / resource); the docs page is generated by `make sitegen` (visit `/docs`).
> The table below lists v3 flat endpoints, some retired by the v4 resource API
> (`/v1/notify`, `/v1/stream`, `/v1/notifications`) — kept for historical reference only.

| Endpoint | Description |
| --- | --- |
| `POST /v1/auth/register[/options]` | Passkey registration |
| `POST /v1/auth/password/register` | Password sign-up (username + password) |
| `POST /v1/auth/login[/options]` | Passkey login |
| `POST /v1/auth/password/login` | Password login (username + password) |
| `GET/POST/DELETE /v1/auth/password` | Password status / set / change / remove (session required) |
| `POST /v1/notify` | **Deleted** — v4 uses `POST /v1/namespaces/{namespace}/events` (Bearer Key, scope=notify:send) |
| `GET /v1/stream` | **Deleted** — v4 long-lived connections are the node stream in `api/agent.yaml`; the console reads events via `GET /v1/console/events` |
| `GET/POST /v1/keys` | API Key management |
| `GET /v1/notifications` | **Deleted** — in v4 the event stream *is* the notification history: `GET /v1/console/events` |

### Agent reporting example (v4 event ingress)

`source` must be the name of an **existing** source: create a Manual source first
(the example uses `agent`, as in step 1 of `docs.html#agent`), otherwise the request
below returns `404 source not found` when copied.

```bash
curl -X POST https://your-domain/v1/namespaces/{namespace}/events \
  -H "Authorization: Bearer ant_live_..." \
  -H "Content-Type: application/json" \
  -d '{"source":"agent","type":"agent_done","id":"delivery-1","payload":{"repo":"acme/api"}}'
```

> v3's `/v1/notify` (`title` / `agentState` / `deviceTags` payload) is **deleted**: calling it returns 404,
> not "the old behaviour". Same for every flat endpoint marked *Deleted* in the table above;
> the v3 tag/level delivery rules are kept as history only.

## Environment Variables

| Variable | Default | Description |
| --- | --- | --- |
| `ANOTIFY_ADDR` | `:8080` | Listen address |
| `ANOTIFY_DB` | `./anotify.db` | SQLite path |
| `ANOTIFY_STATIC` | empty (embed) | Local static directory (dev: `./web`) |
| `ANOTIFY_RP_ID` | `localhost` | WebAuthn RP domain (= access domain) |
| `ANOTIFY_RP_ORIGIN` | `http://localhost:8080` | WebAuthn Origin (with protocol) |
| `ANOTIFY_CDN_PREFIX` | empty | CDN prefix (production static acceleration) |

## File Structure

```
cmd/server/         single binary entrypoint
cmd/devseed/        test seeding tool
internal/
  store/            SQLite data access + schema
  auth/             Passkey(WebAuthn)/password sign-up + password(argon2id) + API Key(argon2id) + sessions
  api/              event ingress + resource routes (see api/*.yaml)
  ws/               node long-poll connections + dispatch
  route/            event → task routing and pruning (shared pure logic)
  protocol/         hub ↔ node wire protocol
  playbook/         playbook parse / render / install
  yamldoc/          YAML document layer (single-document rule)
  server/           route assembly + static assets(CDN cache tiers) + embed
web/                frontend (static HTML + Tailwind CDN + tokens.css)
scripts/hash.mjs    fingerprint script (content-hash + reference rewrite + manifest)
scripts/integration.sh / ws_test.mjs                  test scripts
```

## Security

- API Keys are stored as **argon2id hashes only**; plaintext shown once at creation,
  with a scope set that currently holds only `notify:send` (it guards the event ingress
  `POST /v1/namespaces/{ns}/events`)
- Passkey (WebAuthn) passwordless; an optional password can be set at sign-up or later under Security & login. Sessions via httpOnly cookies
- **Every account keeps at least one login method**: deleting the last Passkey requires a password, removing the password requires a Passkey
- `*.db` / `.env.local` / `.env.dev` are gitignored — do not commit
