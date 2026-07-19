# VOVIN V4 — IndexedDB Client Specification

**Status:** Specification — partially implemented (V3 IDB exists)  
**Scope:** Complete IDB schema, transaction patterns, search index, backup restore

---

## Overview

All message content is stored client-side in IndexedDB, namespaced per PIAL UUID:

```js
const IDB_NAME = `vovin_v1_${MY_PIAL}`;  // current — keep in V4
const IDB_VER  = 4;  // bump from 3
```

The vault key (private key) is stored separately in IDB `kv` store, encrypted at rest. Decrypted message content exists only in RAM during a session.

---

## Store Definitions

### `kv` — Key-Value Store
keyPath: none (explicit key parameter)

Current use: vault metadata, ratchet sessions, glyph keys, sent message cache.

```js
// Existing keys (V3, preserved)
'meta'                          → { method, pubB64, identId }
'pk_plain' | 'pk_wrapped' | 'pk_passkey'  → wrapped private key
'glyph_priv_plain' | 'glyph_priv_wrapped' → glyph signing key
'glyph_pub_b64'                 → glyph public key
'ratchet_{pialId}'              → ratchet session state
'sent_{pialId}'                 → [{id, text, ts, status}]

// New V4 keys
'backup_state'                  → { backup_enabled, last_backup_at, backup_id_hash, recovery_key_confirmed }
'spqr_state_{pialId}'           → { spqr_pub, spqr_priv_jwk, messages_since_spqr }
'search_index_version'          → int (for full rebuild detection)
```

---

### `messages` — Received Message Store
keyPath: `id`
Index: `by_sender` on `sender` field

```js
// Message object (stored as received from server, still encrypted)
{
  id:            "uuid",           // server-assigned message ID
  sender:        "pial_id",        // sender's PIAL (used as IDB index key)
  recipient:     "pial_id",        // always MY_PIAL
  conversation_id: "uuid",         // V4 only — not present in V3 messages
  ciphertext:    "base64",
  iv:            "base64",
  sender_pub:    "base64",         // V1 only — sender's public key
  ratchet_pub:   "base64",         // V2/V4 — ratchet DH public key
  msg_version:   2,                // 1=V1 ECDH, 2=V2 DR, 4=V4 Triple Ratchet
  created_at:    "ISO8601",
  read_at:       "ISO8601" | null
}
```

**V4 change**: Add `conversation_id` index for faster thread lookup (currently uses `sender` as conversation key, which breaks for group chats).

---

### `conversations` — Conversation Metadata Store
keyPath: `pialId` (V3) → `conversation_id` (V4)

```js
// V3 (current)
{
  pialId:    "pial_id",   // the OTHER person's PIAL (for DMs)
  handle:    "string",
  lastAt:    "ISO8601",
  unread:    0,
  preview:   "string"
}

// V4 (new schema)
{
  conversation_id: "uuid",      // server-assigned conversation UUID
  conversation_type: "dm"|"group",
  participants: ["pial_id", ...],  // for DMs: [my_pial, other_pial]
  handle:      "string",           // display name (other person for DMs, group name for groups)
  lastAt:      "ISO8601",
  last_msg_id: "uuid",
  unread:      0,
  preview:     "string",           // last decrypted message preview (truncated)
  pinned_at:   "ISO8601" | null,
  disappearing_timer: "off"|"1h"|"24h"|"7d"|"30d",
  pinned_message_ids: ["uuid", ...],  // max 3
  muted:       false,
  muted_until: "ISO8601" | null,
  e2e_verified: false              // safety number verified
}
```

Indexes:
- `by_last_at`: `lastAt DESC` for sorted conversation list
- `by_pinned_at`: `pinned_at DESC NULLS LAST` for pinned-first ordering

---

### `conversation_keys` — Ratchet Session Store (new in V4)
keyPath: `[conversation_id, device_id]`

Replaces `kv['ratchet_{pialId}']`. Keyed by conversation + device combination for multi-device correctness.

```js
{
  conversation_id:  "uuid",
  device_id:        "string",
  session:          { /* ratchet session state — see CRYPTO_SPEC */ },
  protocol_version: 4,
  updated_at:       "ISO8601"
}
```

Session state is stored in plaintext in IDB (the IDB database itself is protected by the vault unlock requirement). If vault is locked, session state is not accessible.

---

### `media_cache` — Media Reference Store (new in V4)
keyPath: `message_id`

```js
{
  message_id:    "uuid",
  storage_path:  "string",   // Vovin file ID or voice URL
  local_blob_url: "string",  // object URL after decryption (ephemeral, not persisted)
  cached_at:     "ISO8601",
  mime_type:     "string"
}
```

Eviction: entries older than 7 days are deleted on IDB open (in `onupgradeneeded` or on `boot()`).

```js
async function evictOldMedia() {
  const db = await IDB.open();
  const tx = db.transaction('media_cache', 'readwrite');
  const store = tx.objectStore('media_cache');
  const cutoff = new Date(Date.now() - 7*24*3600*1000).toISOString();
  const range = IDBKeyRange.upperBound(cutoff);
  const index = store.index('by_cached_at');
  // Delete all entries where cached_at < cutoff
}
```

---

### `draft_messages` — Unsent Draft Store (new in V4)
keyPath: `conversation_id`

```js
{
  conversation_id: "uuid",
  body:            "string",
  reply_to_id:     "uuid" | null,
  attachments:     [],         // pending file refs
  last_modified:   "ISO8601"
}
```

Drafts survive page reload. Loaded when conversation is opened.

---

### `offline_queue` — Outbound Message Queue (new in V4)
keyPath: autoIncrement

```js
{
  queue_id:        autoIncrement,
  conversation_id: "uuid",
  payload:         { /* encrypted message body ready to POST */ },
  created_at:      "ISO8601",
  retry_count:     0
}
```

Used when network is unavailable. On reconnect, drain in order.

```js
async function drainOutboundQueue() {
  const all = await IDB.getAllFromStore('offline_queue');
  for (const item of all) {
    try {
      await fetch('/vovin/v1/dm', { method:'POST', body: JSON.stringify(item.payload) });
      await IDB.deleteFromStore('offline_queue', item.queue_id);
    } catch {
      item.retry_count++;
      if (item.retry_count > 5) await IDB.deleteFromStore('offline_queue', item.queue_id);
      else await IDB.putInStore('offline_queue', item);
    }
  }
}
```

---

### `search_index` — Full-Text Search Index (new in V4)
keyPath: `token`

```js
// token → sorted array of {message_id, conversation_id, timestamp}
{
  token: "hello",
  entries: [
    { message_id: "uuid1", conversation_id: "uuid2", timestamp: 1746900000 },
    ...
  ]
}
```

Built from decrypted message content. Rebuilt completely on backup restore.

---

### `backup_state` — Singleton Backup Status
keyPath: `"singleton"` (fixed key in `kv` store — not a separate object store)

```js
{
  backup_enabled:           boolean,
  last_backup_at:           "ISO8601",
  backup_id_hash:           "hex_string",   // HKDF(recovery_key, "backup-id")
  recovery_key_confirmed:   boolean
  // NOT: recovery_key itself — never stored
}
```

---

## Transaction Patterns

### Reading a conversation thread

```js
async function loadThread(conversation_id) {
  const db = await IDB.open();
  const tx = db.transaction(['messages', 'conversations'], 'readonly');
  const msgStore = tx.objectStore('messages');
  const convStore = tx.objectStore('conversations');

  // Get conversation metadata
  const conv = await promisify(convStore.get(conversation_id));

  // Get messages for this conversation
  const msgIndex = msgStore.index('by_conversation_id');  // V4 new index
  const msgs = await promisify(msgIndex.getAll(conversation_id));

  return { conv, msgs: msgs.sort((a,b) => new Date(a.created_at) - new Date(b.created_at)) };
}
```

### Storing an incoming message

```js
async function storeIncomingMessage(msg) {
  const db = await IDB.open();
  const tx = db.transaction(['messages', 'conversations'], 'readwrite');

  // Store message
  tx.objectStore('messages').put(msg);

  // Update conversation metadata
  const convStore = tx.objectStore('conversations');
  const conv = await promisify(convStore.get(msg.conversation_id))
    ?? { conversation_id: msg.conversation_id, unread: 0 };
  conv.lastAt = msg.created_at;
  conv.last_msg_id = msg.id;
  conv.unread = (conv.unread || 0) + 1;
  convStore.put(conv);

  await promisify(tx);
}
```

### Updating search index (background)

```js
async function indexMessage(messageId, decryptedText) {
  const tokens = tokenise(decryptedText);
  const db = await IDB.open();
  const tx = db.transaction('search_index', 'readwrite');
  const store = tx.objectStore('search_index');

  for (const token of tokens) {
    const existing = await promisify(store.get(token)) ?? { token, entries: [] };
    existing.entries.push({ message_id: messageId, timestamp: Date.now() });
    existing.entries = existing.entries.slice(-500);  // max 500 per token
    store.put(existing);
  }
}

function tokenise(text) {
  return text.toLowerCase()
    .split(/[^a-z0-9]+/)
    .filter(t => t.length >= 3 && !STOP_WORDS.has(t));
}
```

---

## Search Query Algorithm

```js
async function search(query) {
  const tokens = tokenise(query);
  if (!tokens.length) return [];

  const db = await IDB.open();
  const tx = db.transaction('search_index', 'readonly');
  const store = tx.objectStore('search_index');

  // Fetch entry lists for all tokens
  const lists = await Promise.all(tokens.map(t => promisify(store.get(t))));

  // Intersection of message_ids for AND search
  let result = new Set(lists[0]?.entries.map(e => e.message_id) ?? []);
  for (let i = 1; i < lists.length; i++) {
    const ids = new Set(lists[i]?.entries.map(e => e.message_id) ?? []);
    result = new Set([...result].filter(id => ids.has(id)));
  }

  // Load matched messages from messages store
  const msgStore = db.transaction('messages','readonly').objectStore('messages');
  const matched = await Promise.all([...result].map(id => promisify(msgStore.get(id))));
  return matched.filter(Boolean).sort((a,b) => new Date(b.created_at) - new Date(a.created_at));
}
```

Performance target: `< 100ms` for 10,000 messages, 500 tokens per token entry.

---

## IDB Upgrade Handler (V3 → V4)

```js
r.onupgradeneeded = e => {
  const db = e.target.result;
  const oldVersion = e.oldVersion;

  // V1 stores (always create if missing)
  if (!db.objectStoreNames.contains('kv'))
    db.createObjectStore('kv');

  // V2 stores
  if (!db.objectStoreNames.contains('messages')) {
    const ms = db.createObjectStore('messages', {keyPath:'id'});
    ms.createIndex('by_sender', 'sender', {unique:false});
  }
  if (!db.objectStoreNames.contains('conversations'))
    db.createObjectStore('conversations', {keyPath:'pialId'});

  // V4 new stores (version 4 upgrade)
  if (oldVersion < 4) {
    if (!db.objectStoreNames.contains('conversation_keys')) {
      db.createObjectStore('conversation_keys', {keyPath:['conversation_id','device_id']});
    }
    if (!db.objectStoreNames.contains('media_cache')) {
      const mc = db.createObjectStore('media_cache', {keyPath:'message_id'});
      mc.createIndex('by_cached_at', 'cached_at');
    }
    if (!db.objectStoreNames.contains('draft_messages'))
      db.createObjectStore('draft_messages', {keyPath:'conversation_id'});
    if (!db.objectStoreNames.contains('offline_queue'))
      db.createObjectStore('offline_queue', {autoIncrement:true, keyPath:'queue_id'});
    if (!db.objectStoreNames.contains('search_index'))
      db.createObjectStore('search_index', {keyPath:'token'});

    // Add conversation_id index to messages store (requires recreating in IDB)
    // IDB cannot add indexes to existing stores — requires data migration
    // Strategy: create messages_v4 store, migrate on next full sync
    if (!db.objectStoreNames.contains('messages_v4')) {
      const msv4 = db.createObjectStore('messages_v4', {keyPath:'id'});
      msv4.createIndex('by_sender', 'sender', {unique:false});
      msv4.createIndex('by_conversation_id', 'conversation_id', {unique:false});
    }
  }
};
```

---

## Backup Restore: IDB Rebuild Sequence

On backup restore (`restoreFromBackup()`):

```js
async function restoreIDB(payload) {
  const db = await IDB.open();

  // 1. Clear existing data
  await clearStore(db, 'messages');
  await clearStore(db, 'conversations');
  await clearStore(db, 'conversation_keys');
  await clearStore(db, 'search_index');
  await clearStore(db, 'media_cache');

  // 2. Restore messages
  const msgTx = db.transaction('messages', 'readwrite');
  for (const msg of payload.messages) {
    msgTx.objectStore('messages').put(msg);
  }
  await promisify(msgTx);

  // 3. Restore conversations
  const convTx = db.transaction('conversations', 'readwrite');
  for (const conv of payload.conversations) {
    convTx.objectStore('conversations').put(conv);
  }
  await promisify(convTx);

  // 4. Restore ratchet sessions
  const kvTx = db.transaction('kv', 'readwrite');
  for (const [pialId, session] of Object.entries(payload.ratchet_sessions)) {
    kvTx.objectStore('kv').put(session, 'ratchet_' + pialId);
  }
  await promisify(kvTx);

  // 5. Rebuild search index in background (do not await)
  rebuildSearchIndex().catch(console.warn);
}

async function rebuildSearchIndex() {
  const msgs = await IDB.getAllFromStore('messages');
  for (const msg of msgs) {
    const text = S.decrypted.get(msg.id);  // use already-decrypted cache
    if (text) await indexMessage(msg.id, text);
  }
}
```

---

## Cache Eviction Policies

| Store | Eviction Rule |
|-------|--------------|
| `media_cache` | Entries older than 7 days, on page load |
| `search_index` | Rebuilt on backup restore; pruned to 500 entries per token |
| `offline_queue` | Deleted after successful send; dropped after 5 retries |
| `draft_messages` | Cleared after message send |
| `kv['sent_{pial}']` | Trimmed to last 200 messages per conversation |

---

*Document version: 2026-05-10. Review required before implementation begins.*
