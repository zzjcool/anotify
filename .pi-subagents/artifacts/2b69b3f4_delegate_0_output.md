No existing protocol decisions in the repo — clean slate. Here is the recommendation.

---

## Anotify — Receiver Channel Design Recommendation

### 1. WebSocket vs gRPC-streaming vs SSE

| Criterion | WebSocket | gRPC server-stream | SSE |
|---|---|---|---|
| Client→server msgs (acks, subscribe, heartbeat) | Native bidirectional | Client-stream/bidi exists | **Impossible** (server→client only) |
| Plugin ergonomics (Go/Rust/Python/Node) | Std-lib/one-dep clients everywhere | Needs proto toolchain + codegen | Trivial (`EventSource`/curl) |
| Proxy/firewall traversal | Upgrade on :443, standard | HTTP/2 required; breaks on naive L7 proxies | Plain HTTP/1.1, best traversal |
| Browser support | Full | Needs gRPC-web shim | Native |
| Self-host simplicity | Gorilla/coder `websocket` + one HTTP route | Extra protobuf/HTTP2 stack | Simplest |

**Recommendation: WebSocket.** The killer requirement is client→server messages: acks for replay, dynamic tag subscribe/unsubscribe, and ping/pong. SSE cannot do this without a parallel POST channel (two paths, ordering headaches); gRPC makes hobbyist plugin authors fight protoc and HTTP/2 proxies. Use SSE only for a read-only "tail mode" (`curl -N` debugging), and gRPC only if a future high-throughput fleet plugin needs typed contracts.

### 2. Protocol

**Connect:** `wss://host/v1/stream` with header `Authorization: Bearer ant_recv_...` (query-param token accepted for constrained clients, but header preferred to keep keys out of logs).

**Handshake → server sends:**
```json
{"type":"hello","conn_id":"c_9f2","protocol":1,"heartbeat_sec":30,
 "resume_token":"evt_01J8X2KQ","subscribed_tags":["ops"]}
```

**Client frames:** `subscribe`/`unsubscribe`/`ack`/`ping`:
```json
{"type":"subscribe","tags":["ops","builds"]}
{"type":"ack","event_ids":["evt_01J8X3A1"]}
```

**Server frames:** `subscribed` (echo + resume_token), `notification`, `pong`, `error{code,message}`, `bye{reason}`:
```json
{"type":"notification","event_id":"evt_01J8X3A1","seq":1042,
 "title":"Task finished","body":"refactor complete","url":"...",
 "tags":["ops"],"sent_at":"2025-01-15T09:41:02Z","ttl_sec":86400}
```

**Heartbeat:** client pings every `heartbeat_sec`; server closes after 2 missed.

**Reconnect/resume:** reconnect with `Last-Event-Id: evt_01J8X3A1` header (or `resume_token` in a `resume` frame as first message). Server replays retained unacked events (e.g. 24 h ring buffer), sends `{"type":"replay_end"}`, then live traffic. Events beyond retention → `error{code:"resume_expired"}` + full snapshot guidance. Lifecycle: `connect → hello → active → (close|bye)`, one connection per device, `Device` row keyed by `conn_id`.

### 3. Key scoping

**Separate scopes on one key model.** Issue keys as `ant_{prefix}_{secret}` with a scope set: `notify:send`, `notify:receive`, `devices:read`. The reporter agent (runs on untrusted CI/dev boxes, most likely to leak) should get **send-only** keys; receivers get **receive-only**. A leaked send key can't eavesdrop on your notifications (content is the sensitive part); a leaked receive key can't forge notifications. Server stores argon2/bcrypt hash + scopes; scope enforced at the route middleware. Never mint one all-powerful key.

### 4. Plugin ecosystem

The frame protocol is JSON-over-WS with no browser assumptions: a desktop menubar app (Tauri/Go) receives `notification` → native toast; a CLI daemon (`anotifyd`) acks and runs a shell hook; a smart-home bridge subscribes to tag `home` and maps bodies to MQTT/Home Assistant. New plugins implement one documented WS contract — no Go dependency, no protobuf — and tagging + ack/replay give them exactly-once-ish semantics for free.