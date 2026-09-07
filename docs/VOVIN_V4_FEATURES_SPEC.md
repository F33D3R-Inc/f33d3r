# VOVIN V4 — Feature Specification

**Status:** Specification — not yet implemented  
**Scope:** 15 messaging features matching 2026 iMessage/Signal standard  
**All features are E2E encrypted unless explicitly noted.**

---

## Wire Format Convention

All message content is a JSON object encrypted via the session ratchet. The server sees only the ciphertext. The type field drives rendering.

```js
// Example decrypted message body
{ "type": "text", "body": "Hello!" }
{ "type": "reaction", "target_message_id": "uuid", "emoji": "❤️" }
{ "type": "voice_note", "url": "/vovin/v1/voice/uuid", "duration_secs": 23, "waveform": [0.1, 0.4, ...] }
```

All types are listed in the `message_type` DB enum: `text | reaction | reply | edit | delete | pin | unpin | voice_note | media | forward | system | typing_start | typing_stop`

---

## Feature 1 — Emoji Reactions (Tapbacks)

### Wire Format
```js
{ type: "reaction", target_message_id: "uuid", emoji: "❤️" }
```

### Behaviour
- Any Unicode emoji, from a quick-picker of 6 defaults + "more" button
- Long-press (500ms) on mobile, right-click on desktop → opens picker
- Toggle: sending the same emoji removes the reaction
- Max 8 unique emoji per message (client enforces before sending)
- Aggregated client-side from message log — server stores ciphertext

### IDB State Machine
```
state: S_reactions = Map<msgId, Map<emoji, {count, mine}>>

On receive reaction message:
  if emoji already in map && sender === my_pial:
    mine = true
  increment count
  re-render reaction pills

On send reaction:
  optimistic: update S_reactions immediately, re-render
  send to server
  on server error: revert S_reactions, show toast
```

### UI Render
- Reaction pills appear below the message bubble
- Pill: `{emoji} {count}` with accent background if `mine`
- Tap pill → sends same emoji (toggle)
- Long-tap pill in group chat → sheet showing who reacted

### Backup Integration
Reaction messages stored as regular messages in backup payload. Aggregation state is derived on restore.

---

## Feature 2 — Message Replies (Threaded)

### Wire Format
```js
{ type: "reply", reply_to_id: "uuid", body: "Yes, exactly." }
```

### Behaviour
- Long-press → context menu → "Reply"
- Compose bar shows quoted preview of original message
- If original deleted: preview shows "[Message deleted]"

### UI Render
```
┌─────────────────────────────────────────┐
│ ▏ @handle                              │  ← quoted preview
│ ▏ The original message text…           │
├─────────────────────────────────────────┤
│ Yes, exactly.                           │
└─────────────────────────────────────────┘
```
Tap quote → scroll to original message (if in IDB).

### IDB State Machine
`reply_to_id` stored with message. On render: look up original in IDB, render quote. If not found, show `[Message not in history]`.

### Backup Integration
`reply_to_id` is preserved. On restore, if original is also in backup, quote renders correctly.

---

## Feature 3 — Message Editing

### Wire Format
```js
{ type: "edit", target_id: "uuid", new_body: "Corrected text." }
```

### Behaviour
- 15-minute edit window (enforced server-side by `server_timestamp` check on edit message)
- Long-press on own message → "Edit"
- Inline editing in compose bar with original text pre-filled
- "Edited" label appears on message after edit (tapping shows edit history client-side)
- Original preserved in message log (append-only); edit message references it

### IDB State Machine
```
On receive edit:
  find original in IDB by target_id
  create derived "displayed" version with new_body
  store: { ...original, _edited_body: new_body, _edit_count: prev + 1 }
  re-render thread
```

### Server Enforcement
Server rejects edit messages where `NOW() - original.server_timestamp > 15 minutes`.

### UI Render
```
The original text → Corrected text.   Edited ·
```
"Edited ·" is a tappable label (opens edit history sheet, client-side only).

---

## Feature 4 — Message Deletion

### Delete For Me
Local IDB delete only. No server message sent. Other participant sees nothing different.

### Delete For Everyone
```js
{ type: "delete", target_id: "uuid" }
```
- 60-minute window (server enforces)
- Server creates tombstone: `is_tombstone = true, ciphertext = b''`
- Both clients delete from IDB on receipt
- After 60 min: "Delete for me" only (client-side IDB delete, no wire message)

### UI Render
When tombstone received: replace bubble with `[Message deleted]` in muted style.

---

## Feature 5 — Pinned Messages

### Wire Format
```js
{ type: "pin",   target_id: "uuid" }
{ type: "unpin", target_id: "uuid" }
```

### Behaviour
- Up to 3 pinned messages per conversation
- Long-press → "Pin message"
- Pinned messages shown in collapsible banner at top of conversation
- Tapping banner scrolls to pinned message
- Stored in conversation metadata (encrypted, in backup)

### IDB State Machine
```
conversation_meta.pinned_ids = ["uuid1", "uuid2"]  // max 3
On pin message: prepend, trim to 3
On unpin: remove from list
```

### UI Render
```
┌──────────────────────────────────────────────────────┐
│ 📌 3 pinned messages                            [✕] │
│    "The original message text…"                      │
└──────────────────────────────────────────────────────┘
```
Tap row → scroll to message. Tap ✕ → collapse banner (doesn't unpin).

---

## Feature 6 — Pinned Conversations

### Behaviour
- Up to 5 conversations pinned at top of conversation list
- Long-press conversation row → "Pin" / "Unpin"
- Stored in local IDB `conversation_participants.pinned_at`
- Synced to backup

### UI Render
Pinned conversations appear above unpinned in the sidebar, sorted by `pinned_at DESC`. A subtle 📌 icon appears on pinned conversation rows.

### Server Storage
`conversation_participants.pinned_at TIMESTAMPTZ` — already in V4 schema.

---

## Feature 7 — Disappearing Messages

### Wire Format
Setting change is a system message:
```js
{ type: "system", body: "@handle set messages to disappear after 24h." }
```
System message itself does NOT disappear.

### Options
`off | 1h | 24h | 7d | 30d`

### Timer Semantics
Timer starts at **read time** (not send time). A message sent at noon with 24h timer disappears at noon the next day if read immediately, or 24h after it is first opened.

### Implementation
- Client: tracks `first_read_at` per message in IDB. Schedules `setTimeout` to delete from IDB at `first_read_at + timer`.
- Server: runs deletion job every 5 minutes. Deletes `ciphertext` and sets `is_tombstone = true` for messages where `expires_at < NOW()`.
- On restore from backup: messages past expiry are not included in backup payload.

---

## Feature 8 — Voice Notes

### Wire Format
```js
{
  type: "voice_note",
  url: "/vovin/v1/voice/uuid",
  duration_secs: 23,
  mime: "audio/webm;codecs=opus",
  waveform: [0.0, 0.3, 0.7, 0.4, ...]  // 40 amplitude samples, 0.0–1.0
}
```

### Recording
- MediaRecorder API, Opus/WebM encoding
- Hold-to-record on mobile (current implementation)
- Click-to-start / click-to-stop on desktop (new — remove hold requirement)
- Waveform: `AnalyserNode` samples during recording, down-sampled to 40 bars

### Waveform in Wire Format
The 40-sample amplitude array is computed client-side during recording and included in the encrypted message body. The server never sees it. Recipients render the waveform from the decrypted body without additional fetches.

### Playback UI
```
[▶]  ▁▃▅▇▅▃▁▂▆▇▅▄▃▂▁▃▅▇▆▅▃▂▁  0:23
```
Progress fills bars left-to-right as audio plays. Duration counts down.

### Max Duration
3 minutes. Client stops recording at 180 seconds.

### Voice URL Fix (already shipped)
Voice files are at `/vovin/v1/voice/{id}`, which routes through Nantar's proxy at `/vovin/v1/voice/{id}`. The `url` field in the message body must include the `/vovin` prefix. (Already fixed in messages.js v20260510e.)

---

## Feature 9 — Typing Indicators

### Wire Format (NOT stored, fire-and-forget)
```js
{ type: "typing_start" }   // sent on first keystroke
{ type: "typing_stop"  }   // sent after 3s pause or message send
```

Server relays via WebSocket to recipient(s). Not encrypted (metadata: "someone is typing"). Not stored in DB. Server auto-clears after 10 seconds if no stop received.

### Current State
Already implemented in V3 (messages.js `sendTypingStart()` / `handleTypingEvent()`). No V4 changes needed.

---

## Feature 10 — Link Previews

### Flow
1. Client sends text message containing a URL
2. Nantar proxy detects URL in plaintext request? — **NO.** Content is already encrypted before it leaves the browser.
3. Correct approach: **Client** fetches OG tags via a dedicated Nantar endpoint that proxies the fetch server-side.

```
GET /api/link-preview?url=https%3A%2F%2Fexample.com
→ { title, description, image_url, favicon_url }
```

4. Client receives preview metadata, includes it in the encrypted message body:
```js
{
  type: "text",
  body: "Check this out https://example.com",
  link_preview: {
    url: "https://example.com",
    title: "Example Domain",
    description: "...",
    image_url: "https://example.com/og.jpg"
  }
}
```

5. All preview data is in the encrypted message body. Server sees only ciphertext.

### Nantar Endpoint (new)
```go
// GET /api/link-preview?url=...
// - SSRF protection: block 127.0.0.0/8, 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16
// - Timeout: 3 seconds
// - Cache: Redis, key=SHA256(url), TTL 1 hour
// - Returns: {title, description, image_url} or 204 on failure
```

### UI
Preview card rendered below message text, above reactions. User can dismiss ("Don't show link preview" per-send option in compose bar).

---

## Feature 11 — Read Receipts

### Default
- DMs: ON by default
- Group chats: OFF by default
- User can toggle per-conversation in conversation settings

### Wire Format (fire-and-forget, NOT stored as messages)
```
POST /vovin/v1/dm/read { reader, message_ids }  → server pushes receipt via WS
```

Already implemented in V3 (`markMessagesRead()`). V4 adds per-conversation toggle.

### UI
- One check: sent to server
- Two checks: delivered (server received from sender's perspective — not yet implemented)
- Two filled checks: read (recipient called `/dm/read`)
- In group chats: tap read receipt → sheet showing who has read (client-side from delivered receipt messages)

---

## Feature 12 — Message Search

**100% client-side. Server cannot search E2E content.**

### Index Structure (IDB `search_index` store)
```js
// Built from decrypted message content
// key: word token, value: [{ message_id, conversation_id, timestamp }]

Example: "hello"
→ [{ message_id: "uuid1", conversation_id: "uuid2", timestamp: 1746900000 }]
```

### Index Build
- Runs in background after vault unlock
- Tokenise: lowercase, split on non-alphanumeric, filter stop words
- Incremental: new messages indexed as they arrive
- Rebuild from scratch on backup restore

### Search UX
- Search bar at top of conversation list (tap magnifier icon)
- Results grouped by conversation, sorted by recency
- Snippet: excerpt around match, match term highlighted

### Performance Target
`< 100ms` for 10,000 messages. IDB trigram or prefix search over token → message_id map.

---

## Feature 13 — Message Forwarding

### Wire Format
```js
{ type: "forward", body: "<original decrypted text>", forwarded: true }
```
Re-encrypted for the new conversation's session key (client-side). Server stores new ciphertext.

### UX
- Long-press → "Forward"
- Conversation picker (multi-select, max 5)
- "Forwarded" label rendered on forwarded message bubbles
- Forwarded media: references original AFF file_id + key_b64 (key is re-encrypted for new session)

---

## Feature 14 — Mentions in Group Chats

### Wire Format
```js
{ type: "text", body: "hey @handle check this", mentions: ["pial_shard_id"] }
```

### Behaviour
- Type `@` → participant picker dropdown (from group member list)
- Mention stored as pial_id in `mentions` array (encrypted in body)
- On receive: if `mentions` contains `MY_PIAL`, show notification even if conversation is muted

### UI Render
Mentioned handles highlighted in blue within the bubble text.

---

## Feature 15 — Conversation Muting

### Options
`8h | 1 week | Always`

### Storage
`conversation_participants.muted_until TIMESTAMPTZ` — `NULL` with `notifications_muted = true` means "always muted".

### Behaviour
- Muted conversations still receive messages (IDB syncs, WS receives)
- No push notification while muted
- Exception: mentions still notify even when muted
- Muted conversations show a 🔕 icon in the conversation list

### Wire
No wire message needed — mute is local state stored in IDB and synced to backup. The server's `conversation_participants` table stores it for multi-device consistency.

---

## Multi-Device Delivery

All features must work consistently across multiple devices (up to 5 per user). Key invariants:

1. **Messages fan out**: When a message is delivered, server pushes to all online devices of the recipient simultaneously
2. **Reactions aggregate per-device**: Each device independently computes reaction aggregates from received reaction messages
3. **Typing indicators**: Delivered to all devices of recipient; displayed only on the device with the active conversation
4. **Read receipts**: Any device opening a conversation sends a read receipt; all other devices of the reader see "read" state cleared

---

*Document version: 2026-05-10. Review required before implementation begins.*
