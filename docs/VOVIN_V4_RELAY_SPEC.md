# VOVIN V4 — Relay and WebSocket Architecture Specification

**Status:** Specification — not yet implemented  
**Target:** Sprint 2, Milestone M1 (critical — unblocks real-time messaging)

---

## Current State (V3)

```
Browser → WSS → Caddy → Nantar (Go, :8081) → WS proxy → Vovin (:8092)
                                               httputil.ReverseProxy
```

Vovin's push registry is an in-RAM `Mutex<HashMap<identity, broadcast::Sender>>`. Problems:

- **Dies on restart**: all WS connections lost, subscribers gone
- **Single-instance only**: cannot scale horizontally
- **No offline queue depth management**: messages sent to offline users are dropped silently after 90-day DB retention
- **No cross-brain events**: only Vovin can push to browsers; Nantar cannot push feed updates

---

## V4 Target Architecture

```
Browser ─── WSS ──→ Caddy ──→ Nantar WS Gateway (:8081/api/ws)
                                     │
                    ┌────────────────┼─────────────────────┐
                    │                │                      │
              Redis Pub/Sub    Redis Pub/Sub          Redis Pub/Sub
              message:{pial}   notify:{pial}         feed:{pial}
                    │                │                      │
                  Vovin           Nantar               AethyrRank
              (publishes)      (publishes)            (publishes)
```

**Key change**: Nantar becomes the sole WebSocket gateway. All brains publish events to Redis. Nantar subscribes to channels per connected user and pushes to browser.

This is a Sprint 2 task (`S2.2`–`S2.4`). The V3 Vovin WS proxy continues to work during migration; V4 WS gateway ships alongside it.

---

## Part 1 — WebSocket Lifecycle

### 1.1 Connect

```
Client:  GET /api/ws  (Upgrade: websocket)
         Cookie: session=...
Nantar:  authenticate session → get PIAL
         subscribe to Redis channels:
           message:{PIAL}
           notify:{PIAL}
           feed:{PIAL}
         register in Redis: vovin:conn:{PIAL} → {ws_id, connected_at}
```

### 1.2 Authenticate

Session cookie authenticated by Nantar before upgrade. If session invalid: `HTTP 401` (no upgrade). After upgrade, re-auth is not required — the session is valid for the connection lifetime.

### 1.3 Subscribe

On connection, Nantar subscribes to Redis pub/sub channels for the user's PIAL. Goroutine model:

```go
// One goroutine per connected WebSocket client
func handleWSClient(conn *websocket.Conn, pial string) {
    sub := redis.Subscribe("message:"+pial, "notify:"+pial, "feed:"+pial)
    defer sub.Close()
    defer redis.HDel("vovin:conn:"+pial, connID)

    for {
        select {
        case msg := <-sub.Channel():
            conn.WriteMessage(websocket.TextMessage, msg.Payload)
        case <-clientDisconnect:
            return
        case <-heartbeatTick:
            conn.WriteMessage(websocket.PingMessage, nil)
        }
    }
}
```

### 1.4 Heartbeat

- Client pings every 25 seconds (browser `setInterval`)
- Server refreshes Redis presence TTL on ping
- 30 seconds no ping: connection marked stale
- 60 seconds no ping: connection removed from registry, goroutine exits

```js
// Client heartbeat
setInterval(() => {
  if (_ws && _ws.readyState === WebSocket.OPEN) {
    _ws.send(JSON.stringify({type: 'ping'}));
  }
}, 25000);
```

### 1.5 Drain Offline Queue

On reconnect, after subscribing to Redis:

```go
// Drain offline queue — deliver messages that arrived while user was offline
msgs := redis.LRange("vovin:offline:"+pial, 0, -1)
for _, msg := range msgs {
    conn.WriteMessage(websocket.TextMessage, []byte(msg))
}
redis.Del("vovin:offline:"+pial)
// Then switch to live Redis pub/sub
```

Order: drain offline queue first, then subscribe to live. Ensures no gap between offline and online delivery.

### 1.6 Reconnect (Client)

Exponential backoff with jitter, max 30 seconds:

```js
function connectWS() {
  if (_ws && _ws.readyState <= 1) return;
  const scheme = location.protocol === 'https:' ? 'wss:' : 'ws:';
  _ws = new WebSocket(`${scheme}//${location.host}/api/ws`);

  _ws.onopen = () => { _wsRetry = 0; drainAndSubscribe(); };
  _ws.onclose = () => {
    const delay = Math.min(1000 * (2 ** Math.min(_wsRetry, 6)), 30000);
    _wsRetry++;
    setTimeout(connectWS, delay + Math.random() * 500);
  };
  _ws.onerror = () => _ws?.close();
}
```

---

## Part 2 — Redis Key Schema

All keys use the prefix `vovin:` to namespace within the shared Redis instance.

```
vovin:conn:{pial}                → HASH { ws_id, connected_at, last_ping }
                                   TTL: 35 seconds (refreshed on ping)
                                   Indicates user is currently connected

vovin:offline:{pial}             → LIST of JSON event payloads
                                   Max depth: 1000 (oldest dropped if exceeded)
                                   TTL: 30 days
                                   Drained on reconnect

vovin:typing:{conv_id}           → SET of currently-typing pial_ids
                                   TTL: 10 seconds (refreshed on typing_start)

vovin:presence:{pial}            → HASH { status, last_seen_at }
                                   TTL: 5 minutes (refreshed on ping)
                                   status: "online" | "away" | "offline"

vovin:device:{pial}              → SET of active device_ids
                                   TTL: 30 days
```

### 2.1 Channel Names

```
Redis Pub/Sub channels:

message:{pial}    — DM events for this user (from Vovin)
notify:{pial}     — notification events (from Nantar, AethyrRank, etc.)
feed:{pial}       — feed updates (from Nantar, future use)
```

---

## Part 3 — Fan-Out Algorithm

### 3.1 DM Fan-Out (Vovin → Redis → Nantar → Browser)

```
1. Vovin receives POST /v1/dm (send message)
2. Store encrypted message in DB
3. Publish to Redis: PUBLISH message:{recipient_pial} {event_json}
4. Redis delivers to Nantar WS gateway (subscriber on channel)
5. Nantar finds active WS connection for recipient_pial
6. Push event to browser: { type: "new_message", conversation_id, message_id }
7. If recipient offline: LPUSH vovin:offline:{pial} {event_json}
                         LTRIM vovin:offline:{pial} 0 999  // max 1000
```

Event published to Redis:
```json
{
  "type": "new_message",
  "conversation_id": "uuid",
  "message_id": "uuid",
  "ts": 1746900000
}
```

No plaintext content is included in the Redis event. The browser receives the event, then calls `GET /vovin/v1/dm/{pial}` to fetch the encrypted ciphertext.

### 3.2 Group Chat Fan-Out

For group conversations with N participants:

```
For each participant p in conversation_participants WHERE p != sender:
    PUBLISH message:{p.pial} {event_json}
    // If p offline: LPUSH vovin:offline:{p.pial} ...
```

Fan-out happens in Vovin's request handler. For groups > 100 members, fan-out should be moved to a Kafka consumer worker (future work).

### 3.3 Multi-Device Fan-Out

User may have up to 5 devices. Each device has its own WS connection key (`vovin:conn:{pial}:{device_id}`). Fan-out delivers to ALL connected devices of a user simultaneously:

```go
devices := redis.SMembers("vovin:device:"+pial)
for _, device := range devices {
    connKey := "vovin:conn:"+pial+":"+device
    if redis.Exists(connKey) {
        // Device is online — Redis pub/sub handles delivery
    }
}
// Single PUBLISH to message:{pial} delivers to all subscribers
// Each Nantar instance that has a connection for this user will deliver it
```

---

## Part 4 — Offline Queue

### 4.1 Enqueue

On PUBLISH when recipient has no active connection:

```go
// Vovin calls after PUBLISH finds no subscribers
if deliveredCount == 0 {
    redis.LPush("vovin:offline:"+pial, eventJSON)
    redis.LTrim("vovin:offline:"+pial, 0, 999)  // max 1000
    redis.Expire("vovin:offline:"+pial, 30*24*3600)  // 30 day TTL
}
```

### 4.2 Queue Overflow Notification

When queue depth reaches 1000 (oldest messages dropped), a system message is added when user reconnects:

```json
{ "type": "system", "body": "Some messages were not delivered while you were offline." }
```

### 4.3 Drain on Reconnect

On WS connect (Nantar WS gateway):

```go
func drainOfflineQueue(pial string, conn *websocket.Conn) {
    msgs := redis.LRange("vovin:offline:"+pial, 0, -1)
    for _, msg := range msgs {
        conn.WriteJSON(json.RawMessage(msg))
    }
    redis.Del("vovin:offline:"+pial)
}
```

Drain before subscribing to live channel to prevent duplicate delivery.

---

## Part 5 — Presence State Machine

### 5.1 States

```
online  — WS connected AND pinged within 25 seconds
away    — WS connected but no ping for 25–300 seconds
offline — no WS connection OR no ping for > 300 seconds
```

### 5.2 Transitions

```
connect      → online
ping recv    → online (reset TTL)
25s no ping  → away
300s no ping → offline (Redis key expires, presence cleared)
disconnect   → offline
```

### 5.3 Visibility

Presence is visible **only within active conversations**, not globally. A user's online status is only visible to people they have an active conversation with. Not on profiles, not in feeds.

"Last seen" timestamp is user-configurable: `Settings → Privacy → Show last seen`.

### 5.4 Typing Indicator State

```
typing_start received → add pial to vovin:typing:{conv_id} SET, EXPIRE 10s
typing_stop received  → remove from SET
```

Typing indicator is shown only in the active conversation. Polling or pub/sub on the SET is implementation-specific; the simplest approach is to deliver typing events via the existing `message:{pial}` channel.

---

## Part 6 — CCX23 Capacity Targets

Hetzner CCX23: 4 vCPU, 16 GB RAM, 400 Mbps network.

### Vovin Resource Budget

| Resource | Budget | Notes |
|----------|--------|-------|
| Memory   | 2 GB   | Hard ceiling in Docker compose |
| WebSocket connections | 10,000 | Target max |
| Redis pub/sub channels | 10,000 | One per connected user |

### Memory Per Connection

WebSocket connection overhead in Rust/Axum (tokio):
- Stack per task: 8 KB default
- Buffers: ~4 KB per connection
- Redis subscription: ~1 KB
- **Total: ~14 KB per connection**
- 10,000 × 14 KB = 140 MB — well within 2 GB budget

### Upgrade Trigger

Upgrade from CCX23 → AX52 (8 vCPU, 32 GB RAM) when ANY of:

- Vovin RSS > 1.5 GB sustained for > 30 minutes
- Active WebSocket connections > 8,000 sustained for > 10 minutes
- Message delivery p99 latency > 500 ms sustained (measured by `vovin_delivery_latency_ms`)

Alert: `VovinConnectionsHigh` fires at 8,000 connections (20% headroom before upgrade trigger).

---

## Part 7 — Linux Kernel Tuning

Apply to all production hosts running Vovin. See `docs/LINUX_TUNING.md` for complete sysctl values.

Summary:
- `net.core.somaxconn = 65535` — connection backlog
- `fs.file-max = 200000` — file descriptor limit (WebSocket = 1 fd each)
- `net.ipv4.tcp_tw_reuse = 1` — reuse TIME_WAIT sockets
- Docker nofile ulimit: `65536`

---

*Document version: 2026-05-10. Review required before implementation begins.*
